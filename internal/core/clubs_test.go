package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// slowDriver answers Ready after a delay.
type slowDriver struct {
	driver.Driver
	delay time.Duration
	err   error
}

func (d slowDriver) Capabilities() driver.Capabilities { return driver.Capabilities{} }

func (d slowDriver) Ready(ctx context.Context) error {
	select {
	case <-time.After(d.delay):
		return d.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func clubsOf(t *testing.T, drivers ...driver.Driver) *Clubs {
	t.Helper()
	c := NewClubs(ClubsOptions{})
	var servers []*Server
	for i, d := range drivers {
		servers = append(servers, NewServer(&Settings{ClubID: string(rune('a' + i))}, d, nil))
	}
	c.Set(servers)
	return c
}

// Readiness checks the clubs in parallel: six clubs that each take 150 ms
// fit in a 400 ms budget (one by one: 900 ms), and the reported error is
// the first club's.
func TestClubsReadyInParallel(t *testing.T) {
	var ds []driver.Driver
	for i := 0; i < 6; i++ {
		ds = append(ds, slowDriver{delay: 150 * time.Millisecond})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := clubsOf(t, ds...).Ready(ctx); err != nil {
		t.Fatalf("six clubs did not fit in the budget: %v", err)
	}
	first, second := errors.New("first"), errors.New("second")
	if err := clubsOf(t, slowDriver{err: first, delay: 20 * time.Millisecond}, slowDriver{err: second}).Ready(context.Background()); err != first {
		t.Fatalf("err = %v, want the first club's", err)
	}
}
