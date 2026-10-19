package core

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
)

// passDriver is a club driver with a reconciliation loop whose pass is f.
type passDriver struct {
	driver.Driver
	f func(ctx context.Context) pair.Report
}

func (d passDriver) Capabilities() driver.Capabilities         { return driver.Capabilities{} }
func (d passDriver) Reconcile(ctx context.Context) pair.Report { return d.f(ctx) }
func (d passDriver) Ready(context.Context) error               { return nil }
func (d passDriver) Resource(court string) string              { return court }
func (d passDriver) Get(context.Context, string) (driver.Booking, error) {
	return driver.Booking{}, driver.ErrNotFound
}

// The clubs' passes run in parallel — a club whose pass hangs on a
// slow CRM delays no other club — each bounded by the pass timeout, and a
// new config (Clubs.Set) never waits for a running pass; the club's next
// pass runs on the new config's driver.
func TestMirrorPassesPerClubInParallel(t *testing.T) {
	release := make(chan struct{})
	slowStarted := make(chan time.Duration, 4)
	fastPasses := make(chan struct{}, 16)
	var newPasses atomic.Int32
	slow := passDriver{f: func(ctx context.Context) pair.Report {
		dl, _ := ctx.Deadline()
		slowStarted <- time.Until(dl)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return pair.Report{}
	}}
	fast := passDriver{f: func(context.Context) pair.Report { fastPasses <- struct{}{}; return pair.Report{} }}
	c := NewClubs(ClubsOptions{})
	c.Set("", []*Server{NewServer(&Settings{ClubID: "a-slow"}, slow, nil), NewServer(&Settings{ClubID: "b-fast"}, fast, nil)})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunMirrorsWith(ctx, c, MirrorSchedule{Interval: 200 * time.Millisecond, PassTimeout: 10 * time.Second}, nil)
	}()
	defer func() { cancel(); <-done }()

	select {
	case left := <-slowStarted:
		if left <= 0 || left > 10*time.Second {
			t.Fatalf("the pass is not bounded by the pass timeout: %s left", left)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the slow club's pass did not start")
	}
	for i := range 3 { // passes of the other club go on, interval after interval
		select {
		case <-fastPasses:
		case <-time.After(5 * time.Second):
			t.Fatalf("pass %d of the other club waited for the slow club's pass", i+1)
		}
	}
	// a new config while the slow pass runs: Set returns at once
	renewed := passDriver{f: func(context.Context) pair.Report { newPasses.Add(1); return pair.Report{} }}
	set := make(chan struct{})
	go func() {
		c.Set("", []*Server{NewServer(&Settings{ClubID: "a-slow"}, renewed, nil), NewServer(&Settings{ClubID: "b-fast"}, fast, nil)})
		close(set)
	}()
	select {
	case <-set:
	case <-time.After(2 * time.Second):
		t.Fatal("Clubs.Set waited for a running pass")
	}
	close(release) // the slow pass ends: the club's next pass is the new driver's
	deadline := time.Now().Add(5 * time.Second)
	for newPasses.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no pass on the new config's driver")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A pass that reports Recheck is followed by another of its club after
// Recheck, not after the whole interval; one that does not, is not.
func TestMirrorRecheckPass(t *testing.T) {
	var passes atomic.Int32
	d := passDriver{f: func(context.Context) pair.Report {
		n := passes.Add(1)
		return pair.Report{Recheck: n == 1}
	}}
	c := NewClubs(ClubsOptions{})
	c.Set("", []*Server{NewServer(&Settings{ClubID: "a"}, d, nil)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunMirrorsWith(ctx, c, MirrorSchedule{Interval: time.Hour, Recheck: 50 * time.Millisecond}, nil)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for passes.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no extra pass after a pass that asked for one")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if n := passes.Load(); n != 2 {
		t.Fatalf("%d passes: an extra one without a Recheck", n)
	}
}
