package pair_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
)

// rollbackCRM is a fake primary whose partner's creates succeed (e-1,
// e-2, …) and whose cancel answers cancelErr, recording the ids asked;
// onCancel, when set, runs once inside the next cancel.
type rollbackCRM struct {
	*fakeCRM
	cancelErr error
	cancels   int
	asked     []string
	onCancel  func()
	n         int
}

func (f *rollbackCRM) Create(_ context.Context, req driver.CreateRequest) (driver.Booking, error) {
	f.n++
	return driver.Booking{ID: fmt.Sprintf("e-%d", f.n), Court: req.Court, Start: req.Start, End: req.End}, nil
}

func (f *rollbackCRM) Cancel(_ context.Context, id, _ string) error {
	f.cancels++
	f.asked = append(f.asked, id)
	if h := f.onCancel; h != nil {
		f.onCancel = nil
		h()
	}
	return f.cancelErr
}

// refusingCRM is a fake mirror that refuses every partner's create.
type refusingCRM struct{ *fakeCRM }

func (f *refusingCRM) Create(context.Context, driver.CreateRequest) (driver.Booking, error) {
	return driver.Booking{}, errors.New("refused")
}

func newRollbackLoop(t *testing.T) (*rollbackCRM, *pair.Pair, *time.Time, time.Time) {
	t.Helper()
	loc := time.FixedZone("MSK", 3*3600)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, loc)
	a := &rollbackCRM{fakeCRM: &fakeCRM{typ: "viva"}, cancelErr: errors.New("upstream down")}
	b := &refusingCRM{&fakeCRM{typ: "lunda"}}
	o := pair.Options{PrimaryType: "viva", MirrorType: "lunda", Courts: []string{"court-1", "court-2", "court-3"}, Location: loc,
		Clock: func() (time.Time, bool) { return now, true }}
	for i := range 7 {
		o.Hours[i] = &[2]int{7 * 60, 23 * 60}
	}
	p, err := pair.New(a, b, o)
	if err != nil {
		t.Fatal(err)
	}
	start := now.Add(2 * time.Hour)
	if _, err := p.Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: start, End: start.Add(time.Hour),
		Kind: driver.KindGame, Partner: "padelapp"}); err == nil {
		t.Fatal("the partner was not refused")
	}
	if a.cancels != 1 || !hasAlarm(p.Alarms(), pair.AlarmHalfBooking) {
		t.Fatalf("after the failed rollback: cancels %d, alarms %+v", a.cancels, p.Alarms())
	}
	return a, p, &now, start
}

func hasAlarm(al []pair.Alarm, code string) bool {
	for _, a := range al {
		if a.Code == code {
			return true
		}
	}
	return false
}

// A failed rollback is tried again by every pass until it goes through;
// then it is not tried again and its alarm is gone.
func TestRollbackRetriedUntilDone(t *testing.T) {
	a, p, _, _ := newRollbackLoop(t)
	p.Reconcile(context.Background())
	if a.cancels != 2 {
		t.Fatalf("the pass did not retry the rollback: cancels %d", a.cancels)
	}
	a.cancelErr = nil
	rep := p.Reconcile(context.Background())
	if a.cancels != 3 || hasAlarm(rep.Alarms, pair.AlarmHalfBooking) || hasAlarm(p.Alarms(), pair.AlarmHalfBooking) {
		t.Fatalf("after the retry went through: cancels %d, alarms %+v", a.cancels, rep.Alarms)
	}
	p.Reconcile(context.Background())
	if a.cancels != 3 {
		t.Fatalf("a rollback done is tried again: cancels %d", a.cancels)
	}
}

// A booking that has started is never cancelled by a retry, even when the
// primary would take the cancel: it is dropped, its alarm stays.
func TestRollbackNotRetriedOnceStarted(t *testing.T) {
	a, p, now, start := newRollbackLoop(t)
	a.cancelErr = nil
	*now = start
	rep := p.Reconcile(context.Background())
	if a.cancels != 1 {
		t.Fatalf("a started booking was cancelled by a retry: cancels %d", a.cancels)
	}
	if !hasAlarm(rep.Alarms, pair.AlarmHalfBooking) {
		t.Fatalf("the alarm of a half left for good is gone: %+v", rep.Alarms)
	}
	*now = start.Add(-time.Minute) // even with the clock back, it is not tried again
	p.Reconcile(context.Background())
	if a.cancels != 1 {
		t.Fatalf("a dropped rollback was tried again: cancels %d", a.cancels)
	}
}

// A refusal for good (not owned) drops the rollback; a booking already
// gone ends it as a success.
func TestRollbackDroppedOnRefusalOrGone(t *testing.T) {
	for _, c := range []struct {
		err   error
		alarm bool
	}{{driver.ErrNotOwned, true}, {driver.ErrStarted, true}, {driver.ErrNotFound, false}} {
		a, p, _, _ := newRollbackLoop(t)
		a.cancelErr = c.err
		rep := p.Reconcile(context.Background())
		p.Reconcile(context.Background())
		if a.cancels != 2 || hasAlarm(rep.Alarms, pair.AlarmHalfBooking) != c.alarm {
			t.Fatalf("%v: cancels %d, alarms %+v", c.err, a.cancels, rep.Alarms)
		}
	}
}

// refuse makes a partner's booking on court that the mirror refuses and
// whose rollback fails.
func refuse(t *testing.T, p *pair.Pair, court string, start time.Time) {
	t.Helper()
	if _, err := p.Create(context.Background(), driver.CreateRequest{Court: court, Start: start, End: start.Add(time.Hour),
		Kind: driver.KindGame, Partner: "padelapp"}); err == nil {
		t.Fatal("the partner was not refused")
	}
}

// A pass retries one rollback, so a CRM that hangs on cancels cannot eat
// the pass; one that fails again goes behind the others.
func TestRollbackOnePerPass(t *testing.T) {
	a, p, _, start := newRollbackLoop(t)
	refuse(t, p, "court-2", start)
	refuse(t, p, "court-3", start)
	a.asked = nil
	for range 4 {
		p.Reconcile(context.Background())
	}
	if want := []string{"e-1", "e-2", "e-3", "e-1"}; !slices.Equal(a.asked, want) {
		t.Fatalf("retries %v, want %v", a.asked, want)
	}
}

// A rollback a partner's create adds while a pass retries another is not
// lost.
func TestRollbackAddedDuringRetryKept(t *testing.T) {
	a, p, _, start := newRollbackLoop(t)
	a.onCancel = func() { refuse(t, p, "court-2", start) }
	p.Reconcile(context.Background())
	a.cancelErr = nil
	p.Reconcile(context.Background())
	p.Reconcile(context.Background())
	if want := []string{"e-1", "e-1", "e-2", "e-2", "e-1"}; !slices.Equal(a.asked, want) {
		t.Fatalf("cancels %v, want %v", a.asked, want)
	}
}

// While a rollback waits its alarm stays, however many passes go by (here
// passes that cannot read the CRMs, which see no half themselves).
func TestRollbackAlarmKeptWhileWaiting(t *testing.T) {
	a, p, _, _ := newRollbackLoop(t)
	a.invErr = errors.New("read unavailable")
	for i := range 6 {
		if rep := p.Reconcile(context.Background()); !hasAlarm(rep.Alarms, pair.AlarmHalfBooking) {
			t.Fatalf("pass %d: the half's alarm is gone while its rollback waits: %+v", i+1, rep.Alarms)
		}
	}
}

// The Report of a pass is the caller's: the next pass, clearing an alarm
// it carries, does not change it.
func TestRollbackReportOfPassBeforeUnchanged(t *testing.T) {
	a, p, _, _ := newRollbackLoop(t)
	first := p.Reconcile(context.Background())
	want := slices.Clone(first.Alarms)
	a.cancelErr = nil
	p.Reconcile(context.Background())
	if !reflect.DeepEqual(first.Alarms, want) {
		t.Fatalf("the Report before changed: %+v, was %+v", first.Alarms, want)
	}
}
