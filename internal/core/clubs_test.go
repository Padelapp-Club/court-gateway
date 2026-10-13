package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	c, err := NewClubs(&Settings{}, ClubsOptions{})
	if err != nil {
		t.Fatal(err)
	}
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

// /health/ready reports a reason beyond the CRM (the hub) when there is one.
func TestHealthReadyIncludesTheHub(t *testing.T) {
	c := clubsOf(t, slowDriver{})
	get := func() (int, string) {
		w := httptest.NewRecorder()
		c.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
		var m map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return w.Code, m["reason"]
	}
	if code, _ := get(); code != http.StatusOK {
		t.Fatalf("ready: %d", code)
	}
	reason := "HUB_UNREACHABLE"
	c.SetNotReady(func() string { return reason })
	if code, got := get(); code != http.StatusServiceUnavailable || got != "HUB_UNREACHABLE" {
		t.Fatalf("hub down: %d %s", code, got)
	}
	reason = ""
	if code, _ := get(); code != http.StatusOK {
		t.Fatalf("hub back: %d", code)
	}
}
