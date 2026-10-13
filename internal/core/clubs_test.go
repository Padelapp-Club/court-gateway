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
	c := NewClubs(ClubsOptions{ClubTimeout: 200 * time.Millisecond})
	var servers []*Server
	for i, d := range drivers {
		servers = append(servers, NewServer(&Settings{ClubID: string(rune('a' + i))}, d, nil))
	}
	c.Set(servers)
	return c
}

// Readiness checks the clubs in parallel, each club within its own time
// limit: twenty clubs that each take 100 ms are ready with a 200 ms limit
// per club, although together they take half a second; a club that is
// slower than its limit is not. The reported error is the first club's.
func TestClubsReadyInParallel(t *testing.T) {
	var ds []driver.Driver
	for i := 0; i < 20; i++ {
		ds = append(ds, slowDriver{delay: 100 * time.Millisecond})
	}
	if err := clubsOf(t, ds...).Ready(context.Background()); err != nil {
		t.Fatalf("twenty clubs, each within its limit, are not ready: %v", err)
	}
	if err := clubsOf(t, slowDriver{}, slowDriver{delay: time.Second}).Ready(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a club slower than its limit: %v", err)
	}
	first, second := errors.New("first"), errors.New("second")
	if err := clubsOf(t, slowDriver{err: first, delay: 20 * time.Millisecond}, slowDriver{err: second}).Ready(context.Background()); err != first {
		t.Fatalf("err = %v, want the first club's", err)
	}
}
