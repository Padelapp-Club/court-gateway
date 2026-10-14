package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

func TestKeyedLocks(t *testing.T) {
	k := keyedLocks{m: map[string]*keyedLock{}}
	unlock, err := k.lock(context.Background(), "room-1")
	if err != nil {
		t.Fatal(err)
	}
	// another key is independent
	other, err := k.lock(context.Background(), "room-2")
	if err != nil {
		t.Fatal(err)
	}
	other()
	// the same key waits, and gives up with its context
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := k.lock(ctx, "room-1"); err == nil {
		t.Fatal("a held lock was taken twice")
	}
	unlock()
	again, err := k.lock(context.Background(), "room-1")
	if err != nil {
		t.Fatal(err)
	}
	again()
	if len(k.m) != 0 {
		t.Fatalf("locks left behind: %d", len(k.m))
	}
}

// A create waits for another create of the same court at most
// createLockWait, then answers 503 BUSY.
func TestCreateLockWaitIsBounded(t *testing.T) {
	defer func(d time.Duration) { createLockWait = d }(createLockWait)
	createLockWait = 50 * time.Millisecond
	srv := NewServer(&Settings{ClubID: "club-a", Location: time.UTC, SlotMinutes: 60, Courts: []Court{{ID: "court-1", Name: "C1"}}}, slowDriver{}, nil)
	unlock, err := createLocks.lock(context.Background(), resourceKey(srv.drv, "court-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now().UTC().Truncate(time.Hour).Add(48 * time.Hour)
	body := fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
	r := httptest.NewRequest(http.MethodPost, "/v1/clubs/club-a/bookings", strings.NewReader(body))
	r = r.WithContext(auth.WithPartnerName(r.Context(), "booker"))
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { srv.APIHandler().ServeHTTP(w, r); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the create still waits for the court's lock")
	}
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"BUSY"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// writeDriver's occupancy read takes delay and then runs read; it counts
// the creates that reach it.
type writeDriver struct {
	slowDriver
	delay   time.Duration
	read    func()
	created *int
}

func (d writeDriver) Occupancy(context.Context, driver.OccupancyQuery) (driver.Occupancy, error) {
	time.Sleep(d.delay)
	d.read()
	return driver.Occupancy{Complete: true}, nil
}

func (d writeDriver) Create(context.Context, driver.CreateRequest) (driver.Booking, error) {
	*d.created++
	return driver.Booking{ID: "e-00000000-0000-4000-8000-0000000000e1"}, nil
}

// After the court's lock and the occupancy read, right before the CRM
// write, a create checks again that the start is still ahead on the hub's
// clock (400) and that the write has time to finish: at least half the
// request's time, or minWriteTime, left (503 BUSY). Neither writes.
func TestCreateRechecksBeforeTheWrite(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Hour).Add(48 * time.Hour)
	for _, c := range []struct {
		name    string
		delay   time.Duration
		passes  bool // the start passes during the read
		code    int
		created int
	}{
		{"in time", 0, false, http.StatusCreated, 1},
		{"the start passed during the read", 0, true, http.StatusBadRequest, 0},
		{"too little time left", 600 * time.Millisecond, false, http.StatusServiceUnavailable, 0},
	} {
		now, created := start.Add(-time.Hour), 0
		read := func() {
			if c.passes {
				now = start.Add(time.Second)
			}
		}
		st := &Settings{ClubID: "club-a", Location: time.UTC, SlotMinutes: 60, Courts: []Court{{ID: "court-1", Name: "C1"}},
			Driver: driver.Config{Clock: func() (time.Time, bool) { return now, true }}}
		srv := NewServer(st, writeDriver{delay: c.delay, read: read, created: &created}, nil)
		body := fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
		ctx, cancel := context.WithTimeout(auth.WithPartnerName(context.Background(), "booker"), time.Second)
		r := httptest.NewRequest(http.MethodPost, "/v1/clubs/club-a/bookings", strings.NewReader(body)).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.APIHandler().ServeHTTP(w, r)
		cancel()
		if w.Code != c.code || created != c.created {
			t.Fatalf("%s: %d %s, %d created", c.name, w.Code, w.Body, created)
		}
	}
}
