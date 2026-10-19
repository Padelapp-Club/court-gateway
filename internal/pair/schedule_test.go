package pair_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// schedClock is a schedule's clock (core.MirrorSchedule) a test moves by
// hand: nothing fires until it is moved.
type schedClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []schedWaiter
}

type schedWaiter struct {
	at time.Time
	ch chan time.Time
}

func (c *schedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *schedClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, schedWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock on by d, firing what falls due.
func (c *schedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set(c.now.Add(d))
}

func (c *schedClock) set(t time.Time) {
	c.now = t
	keep := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(t) {
			keep = append(keep, w)
			continue
		}
		w.ch <- t
	}
	c.waiters = keep
}

// next waits (in real time) for n waiters, then moves the clock to the
// earliest of them.
func (c *schedClock) next(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		if len(c.waiters) >= n {
			at := c.waiters[0].at
			for _, w := range c.waiters {
				if w.at.Before(at) {
					at = w.at
				}
			}
			c.set(at)
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("the schedule is not waiting on %d timers", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// D11: a 1C mirror carries no marker, so it goes on the second pass in a
// row that finds its Lunda source gone — and that must still be within 2
// minutes of the club's cancel (scenario 7). The schedule runs the club's
// extra pass core.MirrorRecheck after the pass that first found it invalid
// started, not a whole interval later. The cancel lands at the worst
// moment, right after a pass read Lunda; passes take no time, or 25 s each
// (the clock moved on by the pass): the mirror is gone within 2 minutes
// either way. (Without the extra pass: 2 min 25 s with passes of 25 s.)
func TestMarkerlessRemovalWithinTwoMinutes(t *testing.T) {
	for _, took := range []time.Duration{0, 25 * time.Second} {
		t.Run(fmt.Sprintf("passes of %s", took), func(t *testing.T) {
			env := pairtest.Fitness1CLunda(t, pairtest.Options{})
			start := env.Day(3, 10, 0)
			src := env.Mirror.Book(t, "court-1", start, start.Add(time.Hour))
			clk := &schedClock{now: time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)}
			passes := make(chan pair.Report, 16)
			sc := core.MirrorSchedule{Interval: core.MirrorInterval, Recheck: core.MirrorRecheck, Now: clk.Now, After: clk.After,
				Passed: func(_ string, rep pair.Report) { clk.Advance(took); passes <- rep }}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			t0 := clk.Now()
			go func() { defer close(done); core.RunMirrorsWith(ctx, env.Built.Clubs, sc, nil) }()
			defer func() { cancel(); <-done }()
			pass := func() pair.Report {
				t.Helper()
				select {
				case rep := <-passes:
					return rep
				case <-time.After(20 * time.Second):
					t.Fatal("no pass")
				}
				return pair.Report{}
			}
			if rep := pass(); len(rep.Created) != 1 {
				t.Fatalf("first pass: %+v", rep)
			}
			env.Mirror.Cancel(t, src) // just after the first pass read Lunda, at t0
			rep := pair.Report{}
			for len(rep.Removed) == 0 {
				if clk.Now().Sub(t0) > 5*time.Minute {
					t.Fatal("the mirror was not removed")
				}
				n := 1 // the interval's timer
				if rep.Recheck {
					n = 2 // and the club's extra pass
				}
				clk.next(t, n)
				rep = pass()
			}
			after := clk.Now().Sub(t0)
			if after > 2*time.Minute {
				t.Fatalf("the mirror went %s after the cancel", after)
			}
			t.Logf("the mirror went %s after the cancel", after)
			if n := len(pairtest.Of(env.Primary.Live(t), pairtest.Mirror, "")); n != 0 {
				t.Fatalf("1C mirrors left: %d", n)
			}
		})
	}
}
