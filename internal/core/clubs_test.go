package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
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
	c.clubTimeout = 200 * time.Millisecond
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

// strayDriver reads a booking on a court the club's config does not have.
type strayDriver struct{ slowDriver }

func (strayDriver) Get(context.Context, string) (driver.Booking, error) {
	return driver.Booking{ID: "e-00000000-0000-4000-8000-0000000000e1", Court: "removed-court", Start: time.Now(), End: time.Now().Add(time.Hour)}, nil
}

// A booking the driver places on a court the config does not have (one
// removed from it) is 409 COURT_NOT_CONFIGURED, not exposed.
func TestGetOfABookingOnAnUnknownCourt(t *testing.T) {
	srv := NewServer(&Settings{ClubID: "club-a", Location: time.UTC, Courts: []Court{{ID: "court-1", Name: "C1"}}}, strayDriver{}, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/clubs/club-a/bookings/e-00000000-0000-4000-8000-0000000000e1", nil)
	r = r.WithContext(auth.WithPartnerName(r.Context(), "reader"))
	w := httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "COURT_NOT_CONFIGURED") || strings.Contains(w.Body.String(), "removed-court") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
