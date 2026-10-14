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
