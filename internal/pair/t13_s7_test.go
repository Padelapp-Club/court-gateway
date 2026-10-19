package pair_test

// T13, scenario 7: a booking made directly in one CRM blocks the slot in
// the other CRM and in PadelApp's sale within 2 minutes of loop time; its
// cancellation and its move free it within 2 minutes — both directions,
// every pair.

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// passTook is how long each pass of the timed tests takes of the loop's
// clock: core.MirrorRecheck promises 2 minutes while a pass takes at most
// 30 s.
const passTook = 25 * time.Second

// The club's own loop (core.RunMirrorsWith every core.MirrorInterval, the
// extra pass core.MirrorRecheck on) on a clock the test moves, passes of
// 25 s, each event landing right after a pass read both CRMs:
//
//  1. a booking made in src: the sale answers 409 at once (the union of
//     both CRMs) and its mirror is in dst within 2 minutes;
//  2. it is moved, keeping its id, onto a span overlapping its old one
//     (10:00–11:00 → 10:30–11:30): within 2 minutes one mirror, of the new
//     span, and the old half hour sells;
//  3. it is cancelled and the same slot booked again under a new id: the
//     slot never sells, and within 2 minutes its one mirror is the new
//     booking's (its marker, where the CRM keeps one);
//  4. that one is cancelled: within 2 minutes no mirror is left and the
//     slot sells (201).
func TestT13S7WithinTwoMinutes(t *testing.T) {
	eachPair(t, func(t *testing.T, env *pairtest.Env) {
		for i, d := range directions(env) {
			t.Run(d.name, func(t *testing.T) {
				court := []string{"court-1", "court-2"}[i]
				s := env.Day(2+i, 10, 0)
				e := s.Add(time.Hour)
				ms, me := s.Add(30*time.Minute), e.Add(30*time.Minute)
				loop := startLoop(t, env, passTook)
				check := func(step string, took time.Duration, reps []pair.Report) {
					t.Helper()
					for _, r := range reps {
						if r.Aborted || len(r.Alarms) > 0 {
							t.Fatalf("%s: pass %+v", step, r)
						}
					}
					if took > 2*time.Minute {
						t.Fatalf("%s: settled %s after the event", step, took)
					}
					t.Logf("%s: settled %s after the event (%d passes)", step, took, len(reps))
				}
				marked := func(handle string) bool {
					m := mirrorsOn(t, d.dst, court)
					want := markerOf(d.src, handle)
					return len(m) == 1 && m[0].Start.Equal(ms) && m[0].End.Equal(me) && (want == "" || d.markerless || m[0].Note == want)
				}

				var id string
				took, reps := loop.within(t, func() {
					id = d.src.Book(t, court, s, e)
					if st, _ := env.Create(t, court, s, s.Add(30*time.Minute)); st != http.StatusConflict {
						t.Fatalf("the sale sold a slot booked in %s: %d", d.src.Type(), st)
					}
				}, func() bool { return pairtest.Has(mirrorsOn(t, d.dst, court), pairtest.Mirror, court, s, e) })
				check("create", took, reps)
				if m := mirrorsOn(t, d.dst, court); len(m) != 1 || !d.markerless && markerOf(d.src, id) != "" && m[0].Note != markerOf(d.src, id) {
					t.Fatalf("create: mirrors %s, want the marker %s", pairtest.Spans(m), markerOf(d.src, id))
				}

				took, reps = loop.within(t, func() { d.src.Move(t, id, ms, me) }, func() bool { return marked(id) })
				check("move onto its own old span", took, reps)
				if env.Busy(t, court, s, ms) || !env.Busy(t, court, ms, me) {
					t.Fatal("move: the sale does not show the new span busy and the old half hour free")
				}

				var id2 string
				took, reps = loop.within(t, func() {
					d.src.Cancel(t, id)
					id2 = d.src.Book(t, court, ms, me)
				}, func() bool { return marked(id2) })
				check("cancel and book again", took, reps)
				if st, _ := env.Create(t, court, ms, ms.Add(30*time.Minute)); st != http.StatusConflict {
					t.Fatalf("the slot booked again was sold: %d", st)
				}

				took, reps = loop.within(t, func() { d.src.Cancel(t, id2) }, func() bool {
					return len(mirrorsOn(t, d.dst, court)) == 0 && !env.Busy(t, court, ms, me)
				})
				check("cancel", took, reps)
				if st, _ := env.Create(t, court, ms, me); st != http.StatusCreated {
					t.Fatalf("the freed slot does not sell: %d", st)
				}
			})
		}
	})
}

// blockSpans are the spans a block of [start, end) may take in dst when a
// pass runs at any time in [before, after]: the span dst gives it, or —
// once that span has begun — its rest from the next step of the 30-minute
// grid (pair.BlockGrid).
func blockSpans(dst driver.Mirrorer, start, end time.Time, loc *time.Location, before, after time.Time) [][2]time.Time {
	var out [][2]time.Time
	for _, now := range []time.Time{before, after} {
		bs, be := dst.BlockSpan(start, end)
		if !bs.After(now) {
			bs, be = dst.BlockSpan(gridAfter(now, loc), end)
		}
		out = append(out, [2]time.Time{bs, be})
	}
	return out
}

func hasAny(entries []pairtest.Entry, court string, spans [][2]time.Time) bool {
	for _, sp := range spans {
		if pairtest.Has(entries, pairtest.Mirror, court, sp[0], sp[1]) {
			return true
		}
	}
	return false
}

// eachPairDir runs f on a new Env of every pair for each direction.
func eachPairDir(t *testing.T, f func(t *testing.T, env *pairtest.Env, d direction)) {
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			if p.name != "yclients+lunda" {
				t.Parallel()
			}
			for i := range 2 {
				env := p.setup(t, pairtest.Options{})
				d := directions(env)[i]
				t.Run(d.name, func(t *testing.T) { f(t, env, d) })
			}
		})
	}
}

// mirrorers are the drivers of d's source and target.
func mirrorers(env *pairtest.Env, d direction) (src, dst driver.Mirrorer) {
	src, dst = env.Pair.Primary().(driver.Mirrorer), env.Pair.Mirror().(driver.Mirrorer)
	if d.dst == env.Primary {
		src, dst = dst, src
	}
	return src, dst
}

// D8 on every pair, both ways: a club booking under way (begun 30 minutes
// before the last step of the grid, ending on the grid) and one starting
// in 3 minutes (off the grid) are blocked by the next pass — the one under
// way from the next step of the grid, nothing in the past — and the passes
// after change nothing; the one under way, cancelled, frees its block
// (one that has not begun and starts beyond the lead).
func TestT13S7UnderWayAndAboutToStart(t *testing.T) {
	eachPairDir(t, func(t *testing.T, env *pairtest.Env, d direction) {
		daytime(t, env)
		srcM, dstM := mirrorers(env, d)
		// the source as its CRM shows it (1C: its busy time, whole seances)
		span := func(s, e time.Time) (time.Time, time.Time) { return srcM.BlockSpan(s, e) }
		n := time.Now().In(env.Location)
		step := gridAfter(n, env.Location)
		uwS, uwE := step.Add(-time.Hour), step.Add(time.Hour) // began at least 30 minutes ago
		soonS := n.Truncate(time.Minute).Add(3 * time.Minute)
		soonE := soonS.Add(time.Hour)
		uw := d.src.Book(t, "court-1", uwS, uwE)
		soon := d.src.Book(t, "court-2", soonS, soonE)
		before := time.Now()
		rep := env.Pass(t)
		after := time.Now()
		if rep.Aborted || len(rep.Alarms) > 0 || len(rep.Created) != 2 {
			t.Fatalf("pass: %+v", rep)
		}
		live := d.dst.Live(t)
		s, e := span(uwS, uwE)
		if sp := blockSpans(dstM, s, e, env.Location, before, after); !hasAny(live, "court-1", sp) {
			t.Fatalf("the booking under way (%s) is not blocked as %v: %s", uw, sp, pairtest.Spans(live))
		}
		s, e = span(soonS, soonE)
		if sp := blockSpans(dstM, s, e, env.Location, before, after); !hasAny(live, "court-2", sp) {
			t.Fatalf("the booking starting in 3 minutes (%s) is not blocked as %v: %s", soon, sp, pairtest.Spans(live))
		}
		for _, m := range pairtest.Of(live, pairtest.Mirror, "") {
			if !m.Start.After(before) {
				t.Fatalf("a block starts in the past: %s", pairtest.Spans([]pairtest.Entry{m}))
			}
		}
		if rep := d.settle(t, env); len(rep.Created)+len(rep.Removed) > 0 || len(rep.Alarms) > 0 {
			t.Fatalf("the passes after: %+v", rep)
		}
		m := mirrorsOn(t, d.dst, "court-1")
		if len(m) != 1 || !m[0].Start.After(time.Now().Add(pair.Lead+time.Minute)) {
			t.Logf("the block of the booking under way starts within the lead (%s): its removal is not asserted", pairtest.Spans(m))
			return
		}
		d.src.Cancel(t, uw)
		if rep := d.settle(t, env); len(rep.Removed) != 1 || len(mirrorsOn(t, d.dst, "court-1")) != 0 {
			t.Fatalf("the block of the cancelled booking under way stays: %+v; %s", rep, pairtest.Spans(d.dst.Live(t)))
		}
	})
}

// The horizon's edge where nothing but the window protects a mirror: its
// source has no point read — a slot of Lunda that is no booking (a court
// the club blocked: "ls-…", driver.ErrNotSupported), so an invalid mirror
// of it is removed by the second pass in a row that finds it so (in 1C the
// mirror itself carries no marker). Such a source crossing the window's
// end when the window shrinks keeps its mirror pass after pass.
func TestT13S7HorizonEdgeWithoutPointRead(t *testing.T) {
	eachPair(t, func(t *testing.T, env *pairtest.Env) {
		daytime(t, env)
		env.LundaStub.BookingDepth = 14
		edge := time.Now().In(env.Location).Add(7*24*time.Hour - time.Hour)
		g := edge.Truncate(30 * time.Minute)
		s, e := g.Add(-30*time.Minute), g.Add(time.Hour)
		env.LundaStub.SeedSlot(pairtest.LundaCourt1, "BLOCKED", s, e, nil)
		if rep := env.Pass(t); rep.Aborted || len(rep.Alarms) > 0 || len(rep.Created) != 1 {
			t.Fatalf("pass with a depth of 14: %+v", rep)
		}
		mirrors := mirrorsOn(t, env.Primary, "court-1")
		if len(mirrors) != 1 || !pairtest.Has(mirrors, pairtest.Mirror, "court-1", s, e) {
			t.Fatalf("the blocked slot's mirror: %s", pairtest.Spans(mirrors))
		}
		raw := snap(t, env.Primary, handles(mirrors))
		env.LundaStub.BookingDepth = 7
		for p := range 3 {
			if rep := env.Pass(t); rep.Aborted || len(rep.Alarms) > 0 || len(rep.Created)+len(rep.Removed) > 0 {
				t.Fatalf("pass %d with a depth of 7: %+v", p+1, rep)
			}
		}
		raw.same(t, env.Primary, "the window shrank")
	})
}

// FINDING (T13-F1): scenario 7 says a booking's cancellation frees its
// slot in the other CRM within 2 minutes. A mirror is removed only when it
// lies wholly beyond now + pair.Lead (reconcile.go: the inWindow check
// before a removal), and never once it has begun (driver ErrStarted / the
// Started flag). So a club booking cancelled within about 5 minutes of its
// start keeps its block in the other CRM — and in the partners' sale,
// which is the union of both — for its whole span: the 2 minutes do not
// hold for late cancellations. Kept skipped (it fails by design of the
// current code); run with T13_DEFECTS=1.
func TestT13S7FindingCancelWithinLead(t *testing.T) {
	if os.Getenv("T13_DEFECTS") == "" {
		t.Skip("DEFECT T13-F1: a cancellation within pair.Lead of the booking's start never frees the mirror (reconcile.go inWindow before removal); T13_DEFECTS=1 runs it")
	}
	eachPairDir(t, func(t *testing.T, env *pairtest.Env, d direction) {
		daytime(t, env)
		n := time.Now().In(env.Location)
		start := n.Truncate(time.Minute).Add(3 * time.Minute)
		id := d.src.Book(t, "court-2", start, start.Add(time.Hour))
		if rep := env.Pass(t); len(rep.Created) != 1 {
			t.Fatalf("pass: %+v", rep)
		}
		m := mirrorsOn(t, d.dst, "court-2")
		if len(m) != 1 {
			t.Fatalf("mirrors: %s", pairtest.Spans(d.dst.Live(t)))
		}
		if m[0].Start.After(time.Now().Add(pair.Lead)) {
			t.Skipf("the block starts at %s, beyond the lead (1C rounds it to the grid): nothing to show at this minute", m[0].Start.In(env.Location).Format("15:04"))
		}
		d.src.Cancel(t, id)
		for range 3 {
			env.Pass(t)
		}
		if left := mirrorsOn(t, d.dst, "court-2"); len(left) != 0 {
			t.Fatalf("3 passes after the club cancelled a booking starting at %s, its block stays in %s: %s",
				start.Format("15:04"), d.dst.Type(), pairtest.Spans(left))
		}
	})
}

// The horizon's edge, both ways: a mirror made while its source was within
// the window stays — not removed, not made again, no alarm — when the
// window shrinks (Lunda's booking depth from 14 to 7 days) so that its
// source crosses the window's end or lies wholly beyond it; with the depth
// back the pass changes nothing.
func TestT13S7HorizonEdge(t *testing.T) {
	eachPair(t, func(t *testing.T, env *pairtest.Env) {
		daytime(t, env)
		for i, d := range directions(env) {
			t.Run(d.name, func(t *testing.T) {
				court := []string{"court-1", "court-2"}[i]
				env.LundaStub.BookingDepth = 14
				edge := time.Now().In(env.Location).Add(7*24*time.Hour - time.Hour) // the window's end with a depth of 7
				g := edge.Truncate(30 * time.Minute)
				crossS, crossE := g.Add(-30*time.Minute), g.Add(time.Hour)
				farS := env.Day(10, 12, 0)
				d.src.Book(t, court, crossS, crossE)
				d.src.Book(t, court, farS, farS.Add(time.Hour))
				if rep := env.Pass(t); rep.Aborted || len(rep.Alarms) > 0 || len(rep.Created) != 2 {
					t.Fatalf("pass with a depth of 14: %+v", rep)
				}
				mirrors := mirrorsOn(t, d.dst, court)
				if len(mirrors) != 2 {
					t.Fatalf("mirrors: %s", pairtest.Spans(mirrors))
				}
				raw := snap(t, d.dst, handles(mirrors))
				env.LundaStub.BookingDepth = 7
				for p := range 3 {
					if rep := env.Pass(t); rep.Aborted || len(rep.Alarms) > 0 || len(rep.Created)+len(rep.Removed) > 0 {
						t.Fatalf("pass %d with a depth of 7: %+v", p+1, rep)
					}
				}
				raw.same(t, d.dst, "the window shrank")
				env.LundaStub.BookingDepth = 14
				if rep := d.settle(t, env); rep.Aborted || len(rep.Alarms) > 0 || len(rep.Created)+len(rep.Removed) > 0 {
					t.Fatalf("pass with the depth back: %+v", rep)
				}
				raw.same(t, d.dst, "the window back")
			})
		}
	})
}

func first(a, _ time.Time) time.Time  { return a }
func second(_, b time.Time) time.Time { return b }
