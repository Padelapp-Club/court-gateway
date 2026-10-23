package pair_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
)

// rollbackCRM is a fake primary whose partner's create succeeds and whose
// cancel answers cancelErr, counting the calls.
type rollbackCRM struct {
	*fakeCRM
	cancelErr error
	cancels   int
}

func (f *rollbackCRM) Create(_ context.Context, req driver.CreateRequest) (driver.Booking, error) {
	return driver.Booking{ID: "e-1", Court: req.Court, Start: req.Start, End: req.End}, nil
}

func (f *rollbackCRM) Cancel(context.Context, string, string) error {
	f.cancels++
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
	o := pair.Options{PrimaryType: "viva", MirrorType: "lunda", Courts: []string{"court-1"}, Location: loc,
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
	}{{driver.ErrNotOwned, true}, {driver.ErrNotFound, false}} {
		a, p, _, _ := newRollbackLoop(t)
		a.cancelErr = c.err
		rep := p.Reconcile(context.Background())
		p.Reconcile(context.Background())
		if a.cancels != 2 || hasAlarm(rep.Alarms, pair.AlarmHalfBooking) != c.alarm {
			t.Fatalf("%v: cancels %d, alarms %+v", c.err, a.cancels, rep.Alarms)
		}
	}
}
