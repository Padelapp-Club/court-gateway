package pair_test

// Regressions of scenarios found by the probes of the dual-build
// comparison (2026-10-20).

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

func noAlarms(t *testing.T, label string, rep pair.Report) {
	t.Helper()
	if rep.Aborted || len(rep.Alarms) > 0 {
		t.Fatalf("%s: %+v", label, rep)
	}
}

// A club booking of the primary moved onto a span overlapping its old one
// (10–11 → 10:30–11:30), and an unrelated cancellation on another court,
// in one pass: the old mirror goes first, the new one is made, the
// cancelled one's mirror goes — no alarm, one mirror left.
func TestOverlappingMoveOfThePrimary(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(5, 10, 0)
		id := env.Primary.Book(t, "court-1", start, start.Add(time.Hour))
		other := env.Primary.Book(t, "court-2", start.Add(3*time.Hour), start.Add(4*time.Hour))
		noAlarms(t, "first pass", env.Pass(t))
		env.Primary.Move(t, id, start.Add(30*time.Minute), start.Add(90*time.Minute))
		env.Primary.Cancel(t, other)
		rep := env.Pass(t)
		noAlarms(t, "pass after the move", rep)
		if len(rep.Created) != 1 || len(rep.Removed) != 2 {
			t.Fatalf("pass after the move: %+v", rep)
		}
		mirrors := pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, "")
		if len(mirrors) != 1 || !pairtest.Has(mirrors, pairtest.Mirror, "court-1", start.Add(30*time.Minute), start.Add(90*time.Minute)) {
			t.Fatalf("Lunda mirrors: %s", pairtest.Spans(mirrors))
		}
	})
}

// The club cancels a Lunda booking and books the same slot again (a new
// id), and cancels another one: the slot stays blocked in the primary, the
// cancelled one is freed — within the passes the pair needs, no alarm.
func TestSameSlotBookedAgain(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(5, 10, 0)
		l1 := env.Mirror.Book(t, "court-1", start, start.Add(time.Hour))
		l2 := env.Mirror.Book(t, "court-2", start.Add(5*time.Hour), start.Add(6*time.Hour))
		noAlarms(t, "first pass", env.Pass(t))
		env.Mirror.Cancel(t, l1)
		env.Mirror.Book(t, "court-1", start, start.Add(time.Hour))
		env.Mirror.Cancel(t, l2)
		noAlarms(t, "passes after", env.Settle(t))
		mirrors := pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "")
		if len(mirrors) != 1 || !pairtest.Has(mirrors, pairtest.Mirror, "court-1", start, start.Add(time.Hour)) {
			t.Fatalf("primary mirrors: %s", pairtest.Spans(mirrors))
		}
	})
}

// 1C: a club rental added right after one already mirrored makes one busy
// stretch of the grid: its mirror is replaced by one of the whole stretch;
// when the added rental is cancelled, the mirror is back to the first.
func TestFitness1CAdjacentRentals(t *testing.T) {
	env := pairtest.Fitness1CLunda(t, pairtest.Options{})
	start := env.Day(5, 10, 0)
	env.Primary.Book(t, "court-1", start, start.Add(time.Hour))
	noAlarms(t, "first pass", env.Pass(t))
	r2 := env.Primary.Book(t, "court-1", start.Add(time.Hour), start.Add(2*time.Hour))
	rep := env.Pass(t)
	noAlarms(t, "pass after the adjacent rental", rep)
	if m := pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, ""); len(m) != 1 || !pairtest.Has(m, pairtest.Mirror, "court-1", start, start.Add(2*time.Hour)) {
		t.Fatalf("Lunda mirrors after the adjacent rental: %s", pairtest.Spans(m))
	}
	env.Primary.Cancel(t, r2)
	noAlarms(t, "pass after its cancel", env.Pass(t))
	if m := pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, ""); len(m) != 1 || !pairtest.Has(m, pairtest.Mirror, "court-1", start, start.Add(time.Hour)) {
		t.Fatalf("Lunda mirrors after its cancel: %s", pairtest.Spans(m))
	}
}

// The gateway stops between a partner's write to the primary and the one
// to Lunda (here: the primary's half made alone): the loop never mirrors
// or completes it; the second pass raises MIRROR_HALF_BOOKING as the
// club's readiness; the partner's cancel removes it (204).
func TestRestartBetweenTheTwoWrites(t *testing.T) {
	each(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(4, 15, 0)
		b, err := env.Pair.Primary().Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: start, End: start.Add(time.Hour),
			Kind: driver.KindGame, Partner: env.Partner, Note: "Игра"})
		if err != nil {
			t.Fatal(err)
		}
		noAlarms(t, "first pass", env.Pass(t))
		rep := env.Pass(t)
		if !alarmed(rep, pair.AlarmHalfBooking) || len(rep.Created)+len(rep.Removed) > 0 {
			t.Fatalf("second pass: %+v", rep)
		}
		if r := core.ReadyReason(env.Built.Driver.Ready(context.Background())); r != pair.AlarmHalfBooking {
			t.Fatalf("readiness: %s", r)
		}
		if n := len(env.Mirror.Live(t)); n != 0 {
			t.Fatalf("written into Lunda: %s", pairtest.Spans(env.Mirror.Live(t)))
		}
		if st := env.CancelBooking(t, b.ID); st != http.StatusNoContent {
			t.Fatalf("partner's cancel: %d", st)
		}
		if n := len(pairtest.Of(env.Primary.Live(t), pairtest.Partner, "")); n != 0 {
			t.Fatalf("partner bookings left: %d", n)
		}
	})
}

// A partner's Lunda half loses its notes (it reads as the club's booking
// on the service client): it is neither mirrored back over the primary's
// half nor cancelled — MIRROR_OVERLAP and, by the second pass,
// MIRROR_HALF_BOOKING — while an unrelated cancellation in the same passes
// is still freed.
func TestUnmarkedLundaHalf(t *testing.T) {
	env := pairtest.VivaLunda(t, pairtest.Options{})
	start := env.Day(4, 15, 0)
	l2 := env.Mirror.Book(t, "court-2", start.Add(5*time.Hour), start.Add(6*time.Hour))
	noAlarms(t, "first pass", env.Pass(t))
	if st, _ := env.Create(t, "court-1", start, start.Add(time.Hour)); st != http.StatusCreated {
		t.Fatalf("create: %d", st)
	}
	halves := pairtest.Of(env.Mirror.Live(t), pairtest.Partner, "")
	if len(halves) != 1 {
		t.Fatalf("Lunda halves: %s", pairtest.Spans(env.Mirror.Live(t)))
	}
	env.Mirror.Annotate(t, halves[0].Handle, "")
	env.Mirror.Cancel(t, l2)
	var all pair.Report
	for range 2 {
		rep := env.Pass(t)
		all.Alarms = append(all.Alarms, rep.Alarms...)
		all.Created = append(all.Created, rep.Created...)
		all.Removed = append(all.Removed, rep.Removed...)
	}
	if !alarmed(all, pair.AlarmOverlap) || !alarmed(all, pair.AlarmHalfBooking) || len(all.Created) != 0 || len(all.Removed) != 1 {
		t.Fatalf("passes: %+v", all)
	}
	if n := len(pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "")); n != 0 {
		t.Fatalf("primary mirrors left: %s", pairtest.Spans(env.Primary.Live(t)))
	}
	if b := env.LundaStub.Booking(halves[0].Handle); b.Status == "CANCELLED" {
		t.Fatal("the unmarked half was cancelled")
	}
	if n := len(pairtest.Of(env.Primary.Live(t), pairtest.Partner, "")); n != 1 {
		t.Fatalf("the primary's half: %d", n)
	}
}

// A YCLIENTS club with 200 records over two weeks: one pass reads it by
// lists (no read per record) well within the production pass timeout and
// makes MaxCreates mirrors.
func TestYclientsVolumeWithinOnePass(t *testing.T) {
	env := pairtest.YclientsLunda(t, pairtest.Options{})
	for i := range 200 {
		s := env.Day(1+i/14, 8+(i/2)%7, 0)
		env.Primary.Book(t, []string{"court-1", "court-2"}[i%2], s, s.Add(time.Hour))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	t0 := time.Now()
	rep := env.Pair.Reconcile(ctx)
	took := time.Since(t0)
	if rep.Aborted || len(rep.Created) != pair.DefaultMaxCreates || took > 25*time.Second {
		t.Fatalf("one pass over 200 records took %s: aborted %v, created %d, alarms %+v", took, rep.Aborted, len(rep.Created), rep.Alarms)
	}
	t.Logf("one pass over 200 YCLIENTS records: %s, %d mirrors", took, len(rep.Created))
}
