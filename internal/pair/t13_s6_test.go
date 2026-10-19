package pair_test

// T13, scenario 6: a PadelApp booking lands in both CRMs or in neither —
// the cells the scenario tests before T13 left open, on every pair.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

func partners(t testing.TB, env *pairtest.Env) (primary, mirror []pairtest.Entry) {
	return pairtest.Of(env.Primary.Live(t), pairtest.Partner, ""), pairtest.Of(env.Mirror.Live(t), pairtest.Partner, "")
}

// The first write is refused (the primary's): nothing is written to Lunda
// at all, the partner is refused, the slot sells again at once.
func TestT13S6PrimaryRefusesNeither(t *testing.T) {
	eachPair(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(2, 12, 0)
		env.Primary.Fail(t, "create")
		if st, _ := env.Create(t, "court-2", start, start.Add(time.Hour)); st == http.StatusCreated {
			t.Fatal("the booking was sold although the primary refused it")
		}
		if p, m := partners(t, env); len(p)+len(m) > 0 || len(env.Mirror.Live(t)) > 0 {
			t.Fatalf("written although the primary refused: primary %s / mirror %s", pairtest.Spans(p), pairtest.Spans(env.Mirror.Live(t)))
		}
		if st, _ := env.Create(t, "court-2", start, start.Add(time.Hour)); st != http.StatusCreated {
			t.Fatalf("the slot does not sell after the refusal: %d", st)
		}
		if p, m := partners(t, env); len(p) != 1 || len(m) != 1 {
			t.Fatalf("the booking is not in both: primary %s / mirror %s", pairtest.Spans(p), pairtest.Spans(m))
		}
	})
}

// RR-W2-1 on the hub: Lunda refuses and the rollback of the primary's half
// fails. The partner is refused; the club's readiness carries
// MIRROR_HALF_BOOKING with a detail naming the court, the time and the
// booking (no CRM words, no client data); the alarm stays pass after pass
// (the sticky alarm hands over to the pass's own half-booking rule); no
// pass repairs anything — the half is neither cancelled nor completed.
func TestT13S6RollbackFailsReachesTheHub(t *testing.T) {
	eachPair(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(2, 14, 0)
		env.Mirror.Fail(t, "create")
		env.Primary.Fail(t, "delete")
		st, id := env.Create(t, "court-1", start, start.Add(time.Hour))
		if st == http.StatusCreated || id != "" {
			t.Fatalf("the partner was not refused: %d %q", st, id)
		}
		p, m := partners(t, env)
		if len(p) != 1 || len(m) != 0 {
			t.Fatalf("the half left: primary %s / mirror %s", pairtest.Spans(p), pairtest.Spans(m))
		}
		half := p[0].Handle
		err := env.Built.Clubs.Ready(context.Background())
		d := core.ReadyDetail(err)
		if core.ReadyReason(err) != pair.AlarmHalfBooking || !strings.Contains(d, "корт court-1") || !strings.Contains(d, "только в") ||
			!strings.Contains(d, start.In(env.Location).Format("2006-01-02 15:04")) || strings.Contains(d, "PRIVACY-MARKER") {
			t.Fatalf("readiness %q, detail %q", core.ReadyReason(err), d)
		}
		before := env.Primary.Raw(t, half)
		for i := range 5 {
			rep := env.Pass(t)
			if !alarmed(rep, pair.AlarmHalfBooking) || len(rep.Created)+len(rep.Removed) > 0 || rep.Aborted {
				t.Fatalf("pass %d after the failed rollback: %+v", i+1, rep)
			}
			if r := core.ReadyReason(env.Built.Clubs.Ready(context.Background())); r != pair.AlarmHalfBooking {
				t.Fatalf("pass %d: readiness %s", i+1, r)
			}
		}
		if now := env.Primary.Raw(t, half); now != before {
			t.Fatalf("a pass touched the half:\nbefore %s\nafter  %s", before, now)
		}
		if n := len(env.Mirror.Live(t)); n != 0 {
			t.Fatalf("a pass completed the half in Lunda: %s", pairtest.Spans(env.Mirror.Live(t)))
		}
	})
}

// The primary's half fails to cancel: the partner's cancel is an error,
// both halves stay (both, not one), and a retry cancels both.
func TestT13S6CancelPrimaryHalfFails(t *testing.T) {
	eachPair(t, func(t *testing.T, env *pairtest.Env) {
		start := env.Day(3, 12, 0)
		st, id := env.Create(t, "court-2", start, start.Add(time.Hour))
		if st != http.StatusCreated {
			t.Fatalf("create: %d", st)
		}
		env.Primary.Fail(t, "delete")
		if st := env.CancelBooking(t, id); st == http.StatusNoContent {
			t.Fatal("a cancel answered success with the primary's half still there")
		}
		if p, m := partners(t, env); len(p) != 1 || len(m) != 1 {
			t.Fatalf("after the failed cancel the booking is not whole: primary %s / mirror %s", pairtest.Spans(p), pairtest.Spans(m))
		}
		if st := env.CancelBooking(t, id); st != http.StatusNoContent {
			t.Fatalf("the retry of the cancel: %d", st)
		}
		if p, m := partners(t, env); len(p)+len(m) > 0 {
			t.Fatalf("half a booking stays: primary %s / mirror %s", pairtest.Spans(p), pairtest.Spans(m))
		}
	})
}

// The gateway restarts (a new process: config, sessions and the pair's
// memory new) between a partner's two writes — the half in the primary
// only — or the club cancelled the primary's half — the half in Lunda
// only. Either half is found by the second pass after the restart, not the
// first (nothing remembered from before it), and a restart between the two
// passes starts the count again; nothing is ever cancelled, completed or
// mirrored for it.
func TestT13S6HalfBookingAfterRestart(t *testing.T) {
	eachPair(t, func(t *testing.T, env *pairtest.Env) {
		for _, c := range []struct {
			name  string
			court string
			write func(start time.Time) (driver.Booking, error)
			half  func() pairtest.CRM
		}{
			{"primary only", "court-1", func(start time.Time) (driver.Booking, error) {
				return env.Pair.Primary().Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: start, End: start.Add(time.Hour),
					Kind: driver.KindGame, Partner: env.Partner, Note: "Игра"})
			}, func() pairtest.CRM { return env.Primary }},
			{"lunda only", "court-2", func(start time.Time) (driver.Booking, error) {
				return env.Pair.Mirror().Create(context.Background(), driver.CreateRequest{Court: "court-2", Start: start, End: start.Add(time.Hour),
					Kind: driver.KindGame, Partner: env.Partner, Note: "Игра"})
			}, func() pairtest.CRM { return env.Mirror }},
		} {
			start := env.Day(4, 15, 0)
			if _, err := c.write(start); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			half := pairtest.Of(c.half().Live(t), pairtest.Partner, c.court)
			if len(half) != 1 {
				t.Fatalf("%s: the half: %s", c.name, pairtest.Spans(c.half().Live(t)))
			}
			before := c.half().Raw(t, half[0].Handle)
			env.Restart(t)
			if rep := env.Pass(t); alarmed(rep, pair.AlarmHalfBooking) || len(rep.Created)+len(rep.Removed) > 0 {
				t.Fatalf("%s: first pass after the restart: %+v", c.name, rep)
			}
			env.Restart(t) // between the two passes: the count starts again
			if rep := env.Pass(t); alarmed(rep, pair.AlarmHalfBooking) || len(rep.Created)+len(rep.Removed) > 0 {
				t.Fatalf("%s: first pass after the second restart: %+v", c.name, rep)
			}
			rep := env.Pass(t)
			if !alarmed(rep, pair.AlarmHalfBooking) || len(rep.Created)+len(rep.Removed) > 0 {
				t.Fatalf("%s: second pass after the restart: %+v", c.name, rep)
			}
			if r := core.ReadyReason(env.Built.Clubs.Ready(context.Background())); r != pair.AlarmHalfBooking {
				t.Fatalf("%s: readiness: %s", c.name, r)
			}
			if now := c.half().Raw(t, half[0].Handle); now != before {
				t.Fatalf("%s: a pass touched the half:\nbefore %s\nafter  %s", c.name, before, now)
			}
			if p, m := partners(t, env); len(p)+len(m) != 1 || len(mirrorsOn(t, env.Primary, ""))+len(mirrorsOn(t, env.Mirror, "")) > 0 {
				t.Fatalf("%s: a pass completed or mirrored the half: primary %s / mirror %s", c.name, pairtest.Spans(env.Primary.Live(t)), pairtest.Spans(env.Mirror.Live(t)))
			}
			// clean up by hand for the next case: the club cancels the half
			if c.half() == env.Primary {
				env.Primary.Cancel(t, half[0].Handle)
			} else {
				env.Mirror.Cancel(t, half[0].Handle)
			}
			env.Restart(t)
		}
	})
}
