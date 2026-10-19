package pair_test

// Helpers of the T13 matrix (scenarios 6–8 on the three stub pairs).

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// eachPair runs f on a new Env of every pair. The pairs without YCLIENTS
// run in parallel with each other: YCLIENTS paces every request of the
// process (one throttle for all its clients), so its pair runs alone.
func eachPair(t *testing.T, f func(t *testing.T, env *pairtest.Env)) {
	eachPairAt(t, pairtest.Options{}, f)
}

// eachPairAt is eachPair on Envs set up with o — each with a clock of its
// own made when it is set up, where o.At is set.
func eachPairAt(t *testing.T, o pairtest.Options, f func(t *testing.T, env *pairtest.Env)) {
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			if p.name != "yclients+lunda" {
				t.Parallel()
			}
			f(t, p.setup(t, o))
		})
	}
}

// direction is one way of mirroring: a club booking in src, its mirror in dst.
type direction struct {
	name     string
	src, dst pairtest.CRM
	// markerless: the mirrors in dst carry no marker (1C: removed by the
	// second pass that finds them invalid)
	markerless bool
}

// directions are both ways of env's pair.
func directions(env *pairtest.Env) []direction {
	return []direction{
		{"primary to lunda", env.Primary, env.Mirror, false},
		{"lunda to primary", env.Mirror, env.Primary, env.MarkerlessPrimary},
	}
}

// settle runs the passes a change in d takes: two where d's mirrors carry
// no marker.
func (d direction) settle(t *testing.T, env *pairtest.Env) pair.Report {
	t.Helper()
	rep := env.Pass(t)
	if d.markerless {
		next := env.Pass(t)
		rep.Created = append(rep.Created, next.Created...)
		rep.Removed = append(rep.Removed, next.Removed...)
		rep.Alarms = append(rep.Alarms, next.Alarms...)
		rep.Aborted = rep.Aborted || next.Aborted
	}
	return rep
}

// snapshot is the raw records of handles in crm.
type snapshot map[string]string

func snap(t testing.TB, crm pairtest.CRM, handles []string) snapshot {
	out := snapshot{}
	for _, h := range handles {
		out[h] = crm.Raw(t, h)
		if out[h] == "" {
			t.Fatalf("%s: no record %s", crm.Type(), h)
		}
	}
	return out
}

// same fails t unless every record of s is still byte for byte what it was.
func (s snapshot) same(t testing.TB, crm pairtest.CRM, when string) {
	t.Helper()
	for h, before := range s {
		if now := crm.Raw(t, h); now != before {
			t.Errorf("%s: a pass changed the club's record %s (%s):\nbefore %s\nafter  %s", crm.Type(), h, when, before, now)
		}
	}
}

// handles of entries.
func handles(entries []pairtest.Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Handle)
	}
	return out
}

// mirrorsOn are the mirrors of crm on court.
func mirrorsOn(t testing.TB, crm pairtest.CRM, court string) []pairtest.Entry {
	return pairtest.Of(crm.Live(t), pairtest.Mirror, court)
}

// clubLoop is the club's reconciliation loop (core.RunMirrorsWith) on a
// clock the test moves by hand, every pass taking took of it.
type clubLoop struct {
	clk    *schedClock
	passes chan pair.Report
	last   pair.Report
	took   time.Duration
	stop   func()
}

// startLoop runs the loop of env's club and waits for its first pass.
func startLoop(t *testing.T, env *pairtest.Env, took time.Duration) *clubLoop {
	t.Helper()
	l := &clubLoop{clk: &schedClock{now: time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)}, passes: make(chan pair.Report, 16), took: took}
	sc := core.MirrorSchedule{Interval: core.MirrorInterval, Recheck: core.MirrorRecheck, Now: l.clk.Now, After: l.clk.After,
		Passed: func(_ string, rep pair.Report) { l.clk.Advance(took); l.passes <- rep }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); core.RunMirrorsWith(ctx, env.Built.Clubs, sc, nil) }()
	l.stop = func() { cancel(); <-done }
	t.Cleanup(l.stop)
	l.last = l.pass(t)
	return l
}

func (l *clubLoop) pass(t *testing.T) pair.Report {
	t.Helper()
	select {
	case rep := <-l.passes:
		return rep
	case <-time.After(60 * time.Second):
		t.Fatal("no pass")
	}
	return pair.Report{}
}

// within runs act right after the last pass read both CRMs (the worst
// moment), then the loop from timer to timer until settled holds after a
// pass, and returns the loop time from the start of that last pass.
func (l *clubLoop) within(t *testing.T, act func(), settled func() bool) (time.Duration, []pair.Report) {
	t.Helper()
	t0 := l.clk.Now().Add(-l.took)
	act()
	var reps []pair.Report
	for {
		if l.clk.Now().Sub(t0) > 5*time.Minute {
			t.Fatalf("not settled after 5 minutes of loop time: %+v", reps)
		}
		n := 1 // the interval's timer
		if l.last.Recheck {
			n = 2 // and the club's extra pass
		}
		l.clk.next(t, n)
		l.last = l.pass(t)
		reps = append(reps, l.last)
		if settled() {
			return l.clk.Now().Sub(t0), reps
		}
	}
}

// markerOf is the marker a mirror of the item handle of src carries, as
// its id is written in it ("" for a source whose id the driver derives:
// 1C's busy time).
func markerOf(src pairtest.CRM, handle string) string {
	switch src.Type() {
	case "lunda":
		return "[cg-mirror:lunda:l-" + handle + "]"
	case "viva":
		return "[cg-mirror:viva:e-" + strings.ToLower(handle) + "]"
	case "yclients":
		return "[cg-mirror:yclients:y-" + handle + "]"
	}
	return ""
}

func alarmText(rep pair.Report, code string) string {
	i := slices.IndexFunc(rep.Alarms, func(a pair.Alarm) bool { return a.Code == code })
	if i < 0 {
		return ""
	}
	return rep.Alarms[i].Problem
}
