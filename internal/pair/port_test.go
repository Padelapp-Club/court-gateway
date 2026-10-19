package pair_test

// Regressions of the merge of the two builds of wave 2 (D8–D20 of the
// dual-build comparison, 2026-10-20), on the CRM stubs.

import (
	"net/http"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c/fitness1cstub"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// gridAfter is the first step of the 30-minute grid of t's day after t.
func gridAfter(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	day := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	step := day.Add(l.Sub(day).Truncate(30 * time.Minute))
	for !step.After(t) {
		step = step.Add(30 * time.Minute)
	}
	return step
}

// D8 on the stubs (the comparison's probe TestProbeO_Lead): a club booking
// that starts in 3 minutes, and one under way since 10 minutes ago, get
// their mirrors in the other CRM by the next pass — the one under way from
// the next step of the 30-minute grid, never from the past — both ways.
func TestBookingAboutToStartOrUnderWayIsMirrored(t *testing.T) {
	for _, dir := range []string{"viva to lunda", "lunda to viva"} {
		t.Run(dir, func(t *testing.T) {
			env := pairtest.VivaLunda(t, pairtest.Options{})
			src, dst := env.Primary, env.Mirror
			if dir == "lunda to viva" {
				src, dst = env.Mirror, env.Primary
			}
			n := time.Now().In(env.Location).Truncate(time.Minute)
			soon, end := n.Add(3*time.Minute), n.Add(90*time.Minute)
			src.Book(t, "court-1", soon, soon.Add(time.Hour))
			src.Book(t, "court-2", n.Add(-10*time.Minute), end)
			before := gridAfter(time.Now(), env.Location)
			rep := env.Pass(t)
			after := gridAfter(time.Now(), env.Location)
			if rep.Aborted || len(rep.Alarms) > 0 || len(rep.Created) != 2 {
				t.Fatalf("pass: %+v", rep)
			}
			live := dst.Live(t)
			if !pairtest.Has(live, pairtest.Mirror, "court-1", soon, soon.Add(time.Hour)) {
				t.Fatalf("the booking starting in 3 minutes is not blocked: %s", pairtest.Spans(live))
			}
			if !pairtest.Has(live, pairtest.Mirror, "court-2", before, end) && !pairtest.Has(live, pairtest.Mirror, "court-2", after, end) {
				t.Fatalf("the booking under way is not blocked from %s: %s", before.Format("15:04"), pairtest.Spans(live))
			}
			if rep := env.Pass(t); len(rep.Created)+len(rep.Removed) > 0 || len(rep.Alarms) > 0 {
				t.Fatalf("the next pass: %+v", rep)
			}
		})
	}
}

// D9: Lunda's horizon is its club's booking depth, read every pass. With a
// depth of 7 days a pass reads 7 days, is complete and mirrors; a booking
// of the primary beyond the depth waits for the window (no alarm); when the
// club lengthens its depth, the next pass mirrors it.
func TestLundaHorizonIsTheBookingDepth(t *testing.T) {
	env := pairtest.VivaLunda(t, pairtest.Options{})
	env.LundaStub.BookingDepth = 7
	near := env.Day(1, 15, 0)
	env.Mirror.Book(t, "court-1", near, near.Add(time.Hour))
	far := env.Day(10, 15, 0)
	env.Primary.Book(t, "court-2", far, far.Add(time.Hour))
	for i := range 2 {
		if rep := env.Pass(t); rep.Aborted || len(rep.Alarms) > 0 {
			t.Fatalf("pass %d with a booking depth of 7 days: %+v", i+1, rep)
		}
	}
	if !pairtest.Has(env.Primary.Live(t), pairtest.Mirror, "court-1", near, near.Add(time.Hour)) {
		t.Fatalf("the Lunda booking within the depth is not blocked in the primary: %s", pairtest.Spans(env.Primary.Live(t)))
	}
	if n := len(pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, "")); n != 0 {
		t.Fatalf("a booking beyond Lunda's depth was written into Lunda: %s", pairtest.Spans(env.Mirror.Live(t)))
	}
	// the pair sells no further than its shorter horizon
	if st, _ := env.Create(t, "court-1", far, far.Add(time.Hour)); st == http.StatusCreated {
		t.Fatal("a partner's booking beyond Lunda's depth was sold")
	}
	if st, _ := env.Create(t, "court-2", near, near.Add(time.Hour)); st != http.StatusCreated {
		t.Fatalf("a partner's booking within Lunda's depth: %d", st)
	}
	env.LundaStub.BookingDepth = 14
	// a depth that cannot be read is a read that failed: nothing written
	env.LundaStub.FailNext(http.MethodGet, "/club/settings", http.StatusServiceUnavailable, "{}")
	if rep := env.Pass(t); !rep.Aborted || !alarmed(rep, pair.AlarmIncomplete) || len(rep.Created) > 0 {
		t.Fatalf("pass whose depth could not be read: %+v", rep)
	}
	if rep := env.Pass(t); rep.Aborted || len(rep.Created) != 1 {
		t.Fatalf("pass with a depth of 14 days: %+v", rep)
	}
	if !pairtest.Has(env.Mirror.Live(t), pairtest.Mirror, "court-2", far, far.Add(time.Hour)) {
		t.Fatalf("the far booking is not blocked once within the depth: %s", pairtest.Spans(env.Mirror.Live(t)))
	}
}

// D14 on the stubs (F6): the gateway's mirror on a court of the CRM that
// the club's mapping does not name — Lunda's service-client booking with a
// mirror's marker, 1C's rental of the mirror client — raises
// MIRROR_UNMAPPED and is never touched.
func TestUnmappedMirrorOnStubs(t *testing.T) {
	t.Run("lunda", func(t *testing.T) {
		env := pairtest.VivaLunda(t, pairtest.Options{})
		const court3 = "00000000-0000-4000-8000-00000000a1c3"
		env.LundaStub.AddCourt(court3, "Корт 3")
		s := env.Day(2, 10, 0)
		id := env.LundaStub.SeedBooking(lundastub.Booking{Court: court3, Begin: s, End: s.Add(time.Hour), Customer: pairtest.LundaService,
			Price: 0, Notes: "[cg-mirror:viva:e-00000000-0000-4000-8000-0000000000aa]"})
		for i := range 2 {
			if rep := env.Pass(t); !alarmed(rep, pair.AlarmUnmapped) || len(rep.Removed) > 0 {
				t.Fatalf("pass %d: %+v", i+1, rep)
			}
		}
		if b := env.LundaStub.Booking(id); b.Status == "CANCELLED" {
			t.Fatal("a mirror off the mapping was cancelled")
		}
	})
	t.Run("fitness1c", func(t *testing.T) {
		env := pairtest.Fitness1CLunda(t, pairtest.Options{})
		f := env.Primary.(*pairtest.Fitness1CStub)
		const room3 = "00000000-0000-4000-8000-00000000c1b3"
		f.Stub.AddRoom(room3, "Корт 3")
		s := env.Day(2, 10, 0)
		id := f.Stub.SeedClientRental(pairtest.F1CMirrorTech, room3, pairtest.F1CService, s, s.Add(time.Hour))
		for i := range 3 {
			if rep := env.Pass(t); !alarmed(rep, pair.AlarmUnmapped) || len(rep.Removed) > 0 {
				t.Fatalf("pass %d: %+v", i+1, rep)
			}
		}
		for _, r := range f.Stub.RentalViews() {
			if r.ID == id && r.Canceled {
				t.Fatal("a mirror off the mapping was cancelled")
			}
		}
	})
}

// D15: a 1C mirror is told by the mirror client alone. A mirror whose
// service the club edits is still the gateway's: kept while its source
// stands, never taken for the club's busy time (nor mirrored back into
// Lunda), removed once its source is gone.
func TestFitness1CMirrorSurvivesServiceEdit(t *testing.T) {
	env := pairtest.Fitness1CLunda(t, pairtest.Options{})
	f := env.Primary.(*pairtest.Fitness1CStub)
	const other = "00000000-0000-4000-8000-00000000c1e9"
	f.Stub.AddService(fitness1cstub.Service{ID: other, Title: "Аренда корта", Seance: 30, Price: "2000"})
	start := env.Day(3, 10, 0)
	src := env.Mirror.Book(t, "court-1", start, start.Add(time.Hour))
	if rep := env.Pass(t); len(rep.Created) != 1 {
		t.Fatalf("first pass: %+v", rep)
	}
	mirrors := pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "court-1")
	if len(mirrors) != 1 {
		t.Fatalf("1C mirrors: %s", pairtest.Spans(env.Primary.Live(t)))
	}
	m := mirrors[0]
	f.Stub.MoveRental(m.Handle, pairtest.F1CRoom1, other, m.Start, m.End) // the club edits its service
	for i := range 2 {
		if rep := env.Pass(t); len(rep.Created)+len(rep.Removed) > 0 || len(rep.Alarms) > 0 || rep.Aborted {
			t.Fatalf("pass %d after the service edit: %+v", i+1, rep)
		}
	}
	if n := len(pairtest.Of(env.Mirror.Live(t), pairtest.Mirror, "")); n != 0 {
		t.Fatalf("the edited mirror was mirrored back into Lunda: %s", pairtest.Spans(env.Mirror.Live(t)))
	}
	env.Mirror.Cancel(t, src)
	if rep := env.Settle(t); len(rep.Removed) != 1 {
		t.Fatalf("passes after the source's cancel: %+v", rep)
	}
	if n := len(pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "")); n != 0 {
		t.Fatalf("the edited mirror outlived its source: %s", pairtest.Spans(env.Primary.Live(t)))
	}
}
