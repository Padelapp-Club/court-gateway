package core

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/pair"
)

// MirrorInterval is how often the reconciliation loop of a club served by
// two CRMs runs: within it, a booking made directly in one CRM is blocked
// in the other and leaves the partners' sale (scenario 7: at most 2 min).
const MirrorInterval = time.Minute

// MirrorRecheck: a pass that leaves a mirror to be removed by a second pass
// (pair.Report.Recheck: a 1C mirror, which carries no marker, found invalid
// for the first time) is followed by one more pass of its club this long
// after it started, not a whole interval later (D11). A cancellation then
// frees its slot within MirrorInterval + MirrorRecheck + the second pass:
// within 2 minutes while each pass takes at most 30 s. Only that club runs
// the extra pass: halving the interval of every club instead would double
// the load on every CRM (YCLIENTS paces an account at 180 requests a
// minute) for the few removals that need two passes.
const MirrorRecheck = 30 * time.Second

// mirrorPassTimeout bounds one club's pass: the next is due a minute on.
const mirrorPassTimeout = 50 * time.Second

// reconciler is a club driver with a reconciliation loop (*pair.Pair).
type reconciler interface {
	Reconcile(ctx context.Context) pair.Report
}

// MirrorSchedule is when the passes run (RunMirrorsWith); zero values are
// the defaults.
type MirrorSchedule struct {
	// Interval between the passes of every club (MirrorInterval).
	Interval time.Duration
	// Recheck: the extra pass of a club whose pass reported Recheck runs
	// so long after that pass started (MirrorRecheck, at most half the
	// interval).
	Recheck time.Duration
	// PassTimeout bounds a pass (50 s).
	PassTimeout time.Duration
	// Now and After are the schedule's clock (time.Now, time.After).
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// Passed, if set, is told of every pass (after it, before the next is
	// scheduled).
	Passed func(club string, rep pair.Report)
}

// RunMirrors runs the passes of every served club that has a loop, at
// once and then every interval, until ctx is done (RunMirrorsWith).
func RunMirrors(ctx context.Context, clubs *Clubs, interval time.Duration, log *slog.Logger) {
	RunMirrorsWith(ctx, clubs, MirrorSchedule{Interval: interval}, log)
}

// RunMirrorsWith runs the reconciliation of the clubs served by two CRMs:
// every interval a pass of each such club, the clubs' passes in parallel
// (D20) — a slow CRM delays only its own club — each bounded by
// PassTimeout, and a club's passes one at a time: a club whose pass is
// still running when the interval comes runs its next pass as soon as
// that one ends. A pass that reports Recheck is followed by another of its
// club Recheck after it started (D11). It is the gateway's only scheduled
// write. The current club table is read for every pass and nothing here
// holds it: a new config (Clubs.Set) never waits for a pass, and the next
// pass of the club runs on the new config's pair (one pass of a club at a
// time across its pairs: pair.Memory). A club whose config the CRM refused
// (blocked) is left out; its alarms reach the hub with its readiness.
func RunMirrorsWith(ctx context.Context, clubs *Clubs, sc MirrorSchedule, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if sc.Interval <= 0 {
		sc.Interval = MirrorInterval
	}
	if sc.Recheck <= 0 {
		sc.Recheck = min(MirrorRecheck, sc.Interval/2)
	}
	if sc.PassTimeout <= 0 {
		sc.PassTimeout = mirrorPassTimeout
	}
	if sc.Now == nil {
		sc.Now = time.Now
	}
	if sc.After == nil {
		sc.After = time.After
	}
	type club struct{ running, due bool }
	state := map[string]*club{}
	done := make(chan string)
	var wg sync.WaitGroup
	defer wg.Wait()
	start := func(id string) {
		c := state[id]
		if c == nil {
			c = &club{}
			state[id] = c
		}
		if c.running {
			c.due = true // its pass is still running: the next one right after it
			return
		}
		c.running, c.due = true, false
		wg.Go(func() {
			runClubPasses(ctx, clubs, id, sc, log)
			select {
			case done <- id:
			case <-ctx.Done():
			}
		})
	}
	for {
		for _, id := range clubs.IDs() {
			start(id)
		}
		tick := sc.After(sc.Interval)
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case id := <-done:
				c := state[id]
				c.running = false
				if c.due {
					start(id)
				} else {
					delete(state, id)
				}
			case <-tick:
				break wait
			}
		}
	}
}

// runClubPasses runs a pass of club id on its current driver and, while a
// pass reports Recheck, another one Recheck after the one before started.
func runClubPasses(ctx context.Context, clubs *Clubs, id string, sc MirrorSchedule, log *slog.Logger) {
	for ctx.Err() == nil {
		srv, ok := clubs.Server(id)
		if !ok {
			return
		}
		r, paired := srv.Driver().(reconciler)
		if !paired || srv.mismatch.Load() {
			return
		}
		started := sc.Now()
		pctx, cancel := context.WithTimeout(ctx, sc.PassTimeout)
		rep := r.Reconcile(pctx)
		cancel()
		if len(rep.Created) > 0 || len(rep.Removed) > 0 || len(rep.Alarms) > 0 {
			log.Info("reconciliation pass", "club", id, "created", len(rep.Created), "removed", len(rep.Removed),
				"alarms", len(rep.Alarms), "aborted", rep.Aborted, "recheck", rep.Recheck)
		}
		if sc.Passed != nil {
			sc.Passed(id, rep)
		}
		if !rep.Recheck {
			return
		}
		if wait := sc.Recheck - sc.Now().Sub(started); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-sc.After(wait):
			}
		}
	}
}
