package pair_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// pairs are the pairs every scenario runs over (T13 adds the rest of the
// matrix the same way).
var pairs = []struct {
	name  string
	setup pairtest.Setup
}{
	{"viva+lunda", pairtest.VivaLunda},
}

func each(t *testing.T, f func(t *testing.T, env *pairtest.Env)) {
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) { f(t, p.setup(t, pairtest.Options{})) })
	}
}

func alarmed(rep pair.Report, code string) bool {
	return slices.ContainsFunc(rep.Alarms, func(a pair.Alarm) bool { return a.Code == code })
}

// Scenario 6: a partner's booking is in both CRMs or in neither.
func TestScenario6BothOrNeither(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(2, 10, 0)
		end := start.Add(time.Hour)
		st, id := env.Create(t, "court-1", start, end)
		if st != http.StatusCreated {
			t.Fatalf("create: %d", st)
		}
		p, m := env.Primary.Live(t), env.Mirror.Live(t)
		if !pairtest.Has(p, pairtest.Partner, "court-1", start, end) || !pairtest.Has(m, pairtest.Partner, "court-1", start, end) {
			t.Fatalf("the booking is not in both CRMs: primary %s / mirror %s", pairtest.Spans(p), pairtest.Spans(m))
		}
		if lunda, ok := env.Mirror.(*pairtest.LundaStub); ok {
			lunda.CheckMirrorBookings(t)
		}
		// the loop mirrors none of it: it is in both already
		for range 2 {
			if rep := env.Pass(t); len(rep.Created) > 0 || len(rep.Removed) > 0 || len(rep.Alarms) > 0 {
				t.Fatalf("a pass touched a partner's booking: %+v", rep)
			}
		}
		// cancelled: in neither
		if st := env.CancelBooking(t, id); st != http.StatusNoContent {
			t.Fatalf("cancel: %d", st)
		}
		if len(pairtest.Of(env.Primary.Live(t), pairtest.Partner, "")) > 0 || len(pairtest.Of(env.Mirror.Live(t), pairtest.Partner, "")) > 0 {
			t.Fatalf("a cancelled booking stays: primary %s / mirror %s", pairtest.Spans(env.Primary.Live(t)), pairtest.Spans(env.Mirror.Live(t)))
		}
	})
}

// Scenario 6: the mirror refuses — the primary's half is cancelled again
// and the partner refused: in neither.
func TestScenario6MirrorRefusesNeither(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(2, 12, 0)
		env.Mirror.Fail(t, "create")
		st, _ := env.Create(t, "court-2", start, start.Add(time.Hour))
		if st == http.StatusCreated {
			t.Fatal("the booking was sold although the mirror refused it")
		}
		if p, m := pairtest.Of(env.Primary.Live(t), pairtest.Partner, ""), pairtest.Of(env.Mirror.Live(t), pairtest.Partner, ""); len(p)+len(m) > 0 {
			t.Fatalf("half a booking stays: primary %s / mirror %s", pairtest.Spans(p), pairtest.Spans(m))
		}
		// and the slot sells again at once
		if st, _ := env.Create(t, "court-2", start, start.Add(time.Hour)); st != http.StatusCreated {
			t.Fatalf("the slot does not sell after the refusal: %d", st)
		}
	})
}

// Scenario 6, residual risk RR-W2-1: the mirror refuses and the cancel of
// the primary's half fails too — the partner is refused, the hub alarmed.
func TestScenario6RollbackFailsAlarms(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(2, 14, 0)
		env.Mirror.Fail(t, "create")
		env.Primary.Fail(t, "delete")
		st, _ := env.Create(t, "court-1", start, start.Add(time.Hour))
		if st == http.StatusCreated {
			t.Fatal("the booking was sold although the mirror refused it")
		}
		if !slices.ContainsFunc(env.Pair.Alarms(), func(a pair.Alarm) bool { return a.Code == pair.AlarmHalfBooking }) {
			t.Fatalf("no alarm for a booking left in one CRM: %+v", env.Pair.Alarms())
		}
		err := env.Built.Driver.Ready(context.Background())
		if core.ReadyReason(err) != pair.AlarmHalfBooking {
			t.Fatalf("readiness does not carry the alarm to the hub: %v (%s)", err, core.ReadyReason(err))
		}
	})
}

// Scenario 6: the mirror's half fails to cancel — the partner's cancel is
// an error, and a retry cancels both.
func TestScenario6CancelBothOrError(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(3, 10, 0)
		st, id := env.Create(t, "court-1", start, start.Add(90*time.Minute))
		if st != http.StatusCreated {
			t.Fatalf("create: %d", st)
		}
		env.Mirror.Fail(t, "delete")
		if st := env.CancelBooking(t, id); st == http.StatusNoContent {
			t.Fatal("a cancel answered success with the mirror's half still there")
		}
		if st := env.CancelBooking(t, id); st != http.StatusNoContent {
			t.Fatalf("the retry of the cancel: %d", st)
		}
		if p, m := pairtest.Of(env.Primary.Live(t), pairtest.Partner, ""), pairtest.Of(env.Mirror.Live(t), pairtest.Partner, ""); len(p)+len(m) > 0 {
			t.Fatalf("half a booking stays: primary %s / mirror %s", pairtest.Spans(p), pairtest.Spans(m))
		}
	})
}

// Scenario 7: a booking made in the mirror (Lunda) is blocked in the
// primary within one pass, its cancel unblocks it.
func TestScenario7MirrorToPrimary(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(2, 18, 0)
		end := start.Add(90 * time.Minute)
		id := env.Mirror.Book(t, "court-2", start, end)
		if !env.Busy(t, "court-2", start, end) {
			t.Fatal("the sale shows a slot booked in Lunda as free")
		}
		rep := env.Pass(t)
		if len(rep.Created) != 1 || len(rep.Alarms) > 0 {
			t.Fatalf("pass: %+v", rep)
		}
		live := env.Primary.Live(t)
		if !pairtest.Has(live, pairtest.Mirror, "court-2", start, end) {
			t.Fatalf("no mirror in the primary: %s", pairtest.Spans(live))
		}
		mirror := pairtest.Of(live, pairtest.Mirror, "court-2")[0]
		if mirror.Note != "[cg-mirror:lunda:l-"+id+"]" || strings.Contains(mirror.Note, "PRIVACY") {
			t.Fatalf("the mirror's marker: %q", mirror.Note)
		}
		if st, _ := env.Create(t, "court-2", start, start.Add(time.Hour)); st != http.StatusConflict {
			t.Fatalf("the slot was sold over the club's booking: %d", st)
		}
		// a second pass changes nothing: a mirror is never mirrored back
		if rep := env.Pass(t); len(rep.Created)+len(rep.Removed) > 0 {
			t.Fatalf("second pass: %+v", rep)
		}
		env.Mirror.Cancel(t, id)
		rep = env.Pass(t)
		if len(rep.Removed) != 1 {
			t.Fatalf("pass after the cancel: %+v", rep)
		}
		if live := env.Primary.Live(t); len(pairtest.Of(live, pairtest.Mirror, "")) > 0 {
			t.Fatalf("the mirror stays after its source's cancel: %s", pairtest.Spans(live))
		}
		if env.Busy(t, "court-2", start, end) {
			t.Fatal("the slot does not sell again")
		}
		if st, _ := env.Create(t, "court-2", start, start.Add(time.Hour)); st != http.StatusCreated {
			t.Fatalf("the slot does not sell again: %d", st)
		}
	})
}

// Scenario 7: a booking made in the primary is blocked in Lunda within one
// pass, its cancel unblocks it.
func TestScenario7PrimaryToMirror(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(4, 9, 0)
		end := start.Add(time.Hour)
		id := env.Primary.Book(t, "court-1", start, end)
		rep := env.Pass(t)
		if len(rep.Created) != 1 || len(rep.Alarms) > 0 {
			t.Fatalf("pass: %+v", rep)
		}
		live := env.Mirror.Live(t)
		if !pairtest.Has(live, pairtest.Mirror, "court-1", start, end) {
			t.Fatalf("no mirror in Lunda: %s", pairtest.Spans(live))
		}
		if n := pairtest.Of(live, pairtest.Mirror, "court-1")[0].Note; !strings.HasPrefix(n, "[cg-mirror:"+env.Primary.Type()+":") || strings.Contains(n, "PRIVACY") {
			t.Fatalf("the mirror's marker: %q", n)
		}
		if lunda, ok := env.Mirror.(*pairtest.LundaStub); ok {
			lunda.CheckMirrorBookings(t)
		}
		env.Primary.Cancel(t, id)
		if rep := env.Pass(t); len(rep.Removed) != 1 {
			t.Fatalf("pass after the cancel: %+v", rep)
		}
		if live := env.Mirror.Live(t); len(pairtest.Of(live, pairtest.Mirror, "")) > 0 {
			t.Fatalf("the mirror stays after its source's cancel: %s", pairtest.Spans(live))
		}
	})
}

// Scenario 7: a booking moved in its CRM (its id kept) gets a mirror at
// its new time, and loses the old one, in one pass.
func TestScenario7Move(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(5, 11, 0)
		id := env.Mirror.Book(t, "court-1", start, start.Add(time.Hour))
		if rep := env.Pass(t); len(rep.Created) != 1 {
			t.Fatalf("pass: %+v", rep)
		}
		// moved to overlap its old time: the old mirror goes first
		moved := start.Add(30 * time.Minute)
		env.Mirror.Move(t, id, moved, moved.Add(time.Hour))
		rep := env.Pass(t)
		if len(rep.Created) != 1 || len(rep.Removed) != 1 || len(rep.Alarms) > 0 {
			t.Fatalf("pass after the move: %+v", rep)
		}
		live := env.Primary.Live(t)
		if mirrors := pairtest.Of(live, pairtest.Mirror, ""); len(mirrors) != 1 || !pairtest.Has(live, pairtest.Mirror, "court-1", moved, moved.Add(time.Hour)) {
			t.Fatalf("mirrors after the move: %s", pairtest.Spans(live))
		}
	})
}

// Scenario 7: within 2 minutes without anyone calling a pass — the loop
// runs every core.MirrorInterval (1 minute) by itself.
func TestScenario7LoopRunsByItself(t *testing.T) {
	if core.MirrorInterval > time.Minute {
		t.Fatalf("the loop runs every %s: a booking would wait longer than 2 minutes", core.MirrorInterval)
	}
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(2, 20, 0)
		env.Mirror.Book(t, "court-1", start, start.Add(time.Hour))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); core.RunMirrors(ctx, env.Built.Clubs, 50*time.Millisecond, nil) }()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(10 * time.Second)
		for !pairtest.Has(env.Primary.Live(t), pairtest.Mirror, "court-1", start, start.Add(time.Hour)) {
			if time.Now().After(deadline) {
				t.Fatalf("the loop made no mirror: %s", pairtest.Spans(env.Primary.Live(t)))
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// Scenario 8: no pass cancels or changes a club's booking — of either CRM,
// whatever happens to the mirrors around it.
func TestScenario8ClubBookingsUntouched(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		d := func(h int) time.Time { return env.Day(6, h, 0) }
		env.Primary.Book(t, "court-1", d(8), d(9))
		env.Primary.Book(t, "court-2", d(9), d(11))
		gone := env.Mirror.Book(t, "court-1", d(12), d(13))
		env.Mirror.Book(t, "court-2", d(14), d(15))
		// the club's booking carrying a copy of a mirror's marker of no
		// existing source: still the club's (F4)
		copied := env.Primary.Book(t, "court-1", d(16), d(17))
		env.Primary.Annotate(t, copied, "[cg-mirror:lunda:l-00000000-0000-4000-8000-0000000000ff]")
		// two club bookings overlapping across the CRMs: an alarm, nothing touched
		env.Primary.Book(t, "court-2", d(18), d(19))
		env.Mirror.Book(t, "court-2", d(18), d(20))
		clubBefore := func() ([]pairtest.Entry, []pairtest.Entry) {
			return pairtest.Of(env.Primary.Live(t), pairtest.Club, ""), pairtest.Of(env.Mirror.Live(t), pairtest.Club, "")
		}
		p0, m0 := clubBefore()
		rep := env.Pass(t)
		if !alarmed(rep, pair.AlarmOverlap) {
			t.Fatalf("no overlap alarm: %+v", rep)
		}
		env.Mirror.Cancel(t, gone)
		for range 3 {
			env.Pass(t)
		}
		p1, m1 := clubBefore()
		// the only change to the club's bookings is the cancel the club made
		m0 = slices.DeleteFunc(m0, func(e pairtest.Entry) bool { return e.Handle == gone })
		if !sameEntries(p0, p1) || !sameEntries(m0, m1) {
			t.Fatalf("a pass changed a club's booking:\nprimary before %s\nprimary after  %s\nmirror before %s\nmirror after  %s",
				pairtest.Spans(p0), pairtest.Spans(p1), pairtest.Spans(m0), pairtest.Spans(m1))
		}
		// mirrors: one per club booking but the overlapping two; never a mirror of a mirror
		pm, mm := pairtest.Of(env.Primary.Live(t), pairtest.Mirror, ""), pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, "")
		if len(pm) != 1 || len(mm) != 3 {
			t.Fatalf("mirrors: primary %s / mirror %s", pairtest.Spans(pm), pairtest.Spans(mm))
		}
	})
}

func sameEntries(a, b []pairtest.Entry) bool {
	key := func(e pairtest.Entry) string {
		return e.Handle + "|" + e.Court + "|" + e.Start.UTC().String() + "|" + e.End.UTC().String() + "|" + e.Note
	}
	ka, kb := make([]string, len(a)), make([]string, len(b))
	for i := range a {
		ka[i] = key(a[i])
	}
	for i := range b {
		kb[i] = key(b[i])
	}
	slices.Sort(ka)
	slices.Sort(kb)
	return slices.Equal(ka, kb)
}

// Scenario 8: a club's booking on Lunda's service client without a marker
// (the club's own tournament) is the club's: mirrored, never removed.
func TestScenario8ServiceClientWithoutMarkerIsTheClubs(t *testing.T) {
	env := pairtest.VivaLunda(t, pairtest.Options{})
	lunda := env.Mirror.(*pairtest.LundaStub)
	start := env.Day(3, 15, 0)
	id := lunda.BookOnService("court-1", start, start.Add(2*time.Hour), "Турнир клуба")
	for range 3 {
		env.Pass(t)
	}
	if b := lunda.Stub.Booking(id); b.Status == "CANCELLED" || b.Notes != "Турнир клуба" {
		t.Fatalf("the club's tournament was changed: %+v", b)
	}
	if !pairtest.Has(env.Primary.Live(t), pairtest.Mirror, "court-1", start, start.Add(2*time.Hour)) {
		t.Fatalf("the club's tournament is not blocked in the primary: %s", pairtest.Spans(env.Primary.Live(t)))
	}
}

// Scenario 8, a damaged marker: a booking of the service client whose
// notes start like a mirror's but do not read is touched by nobody.
func TestScenario8DamagedMarkerUntouched(t *testing.T) {
	env := pairtest.VivaLunda(t, pairtest.Options{})
	lunda := env.Mirror.(*pairtest.LundaStub)
	start := env.Day(3, 9, 0)
	id := lunda.BookOnService("court-2", start, start.Add(time.Hour), "[cg-mirror:viva:b-0000")
	rep := env.Pass(t)
	if !alarmed(rep, pair.AlarmDamaged) || len(rep.Created)+len(rep.Removed) > 0 {
		t.Fatalf("pass: %+v", rep)
	}
	if b := lunda.Stub.Booking(id); b.Status == "CANCELLED" {
		t.Fatal("a damaged mirror was cancelled")
	}
}

// Scenario 6, F2: a partner's booking found in one CRM only, in two
// passes in a row, raises an alarm; nothing is cancelled or made.
func TestHalfBookingDetected(t *testing.T) {
	env := pairtest.VivaLunda(t, pairtest.Options{})
	start := env.Day(2, 16, 0)
	st, _ := env.Create(t, "court-1", start, start.Add(time.Hour))
	if st != http.StatusCreated {
		t.Fatalf("create: %d", st)
	}
	half := pairtest.Of(env.Mirror.Live(t), pairtest.Partner, "")[0]
	env.Mirror.Cancel(t, half.Handle) // the club cancelled the Lunda half
	if rep := env.Pass(t); alarmed(rep, pair.AlarmHalfBooking) {
		t.Fatalf("an alarm at the first sight: %+v", rep)
	}
	rep := env.Pass(t)
	if !alarmed(rep, pair.AlarmHalfBooking) || len(rep.Created)+len(rep.Removed) > 0 {
		t.Fatalf("second pass: %+v", rep)
	}
	if len(pairtest.Of(env.Primary.Live(t), pairtest.Partner, "")) != 1 {
		t.Fatal("the primary's half was touched")
	}
}
