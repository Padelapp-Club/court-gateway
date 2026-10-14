package viva_test

import (
	"crypto/ed25519"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Every decision on "now" is made on the hub's time, which the hub signs
// on its poll answers, carried on by the monotonic clock, not on the club
// computer's clock. Here that clock is 55 minutes fast when the hub's time
// arrives and is then set right, so the old offset would make it 23:35 of
// the day before; it is half past midnight by the hub and the CRM. An
// exercise that began yesterday is left out of the CRM's lists by now, so
// the first hours of today are incomplete, a booking that has started
// cannot be cancelled, and before the hub's time is known nothing is
// booked or cancelled at all.
func TestDecisionsUseTheHubsClock(t *testing.T) {
	msk := time.FixedZone("UTC+3", 3*3600) // the studio's own offset
	today := time.Date(2030, 1, 8, 0, 0, 0, 0, msk)
	hubNow := today.Add(30 * time.Minute)
	var local atomic.Int64 // this computer's clock, unix ns
	local.Store(hubNow.Add(55 * time.Minute).UnixNano())
	now := func() time.Time { return time.Unix(0, local.Load()) }
	pub, key, _ := ed25519.GenerateKey(nil)
	v, err := tunnel.NewVerifier(pub, "inst", "fixture-secret", now)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, fixtureOpts{services: bookingService, now: now,
		crmNow: func() time.Time { return hubNow }, clock: v.Now})
	// a club booking from 23:00 yesterday to 02:00, which Viva no longer lists
	f.seedClubBooking(room1, today.Add(-time.Hour), today.Add(2*time.Hour))
	// a booking of ours that began ten minutes ago, by the hub's clock
	started := f.stub.SeedBreak(map[string]any{"room": map[string]any{"id": room1}, "comment": "[cg:booker]", "canceled": false,
		"timeFrom": hubNow.Add(-10 * time.Minute).Format(time.RFC3339), "timeTo": hubNow.Add(20 * time.Minute).Format(time.RFC3339)})
	occupancy := func() map[string]any {
		t.Helper()
		st, m, raw := call(t, f, http.MethodGet, occupancyPath(today, today.Add(time.Hour))+"&court=court-1", contract.ReadKey, "")
		if st != http.StatusOK {
			t.Fatalf("occupancy: %d %s", st, raw)
		}
		return m
	}
	create := func() (int, string) {
		t.Helper()
		st, m, _ := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, createBody(today.Add(time.Hour), "game", ""))
		code, _ := m["code"].(string)
		return st, code
	}
	cancel := func() (int, string) {
		t.Helper()
		st, m, _ := call(t, f, http.MethodDelete, "/v1/clubs/test-club/bookings/b-"+started, contract.BookKey, "")
		code, _ := m["code"].(string)
		return st, code
	}

	// no signed hub time yet: nothing depends on this computer's clock
	if m := occupancy(); m["complete"] != false {
		t.Fatalf("occupancy before the hub's time is known: %v", m)
	}
	if st, code := create(); st != http.StatusServiceUnavailable || code != "CLOCK_UNKNOWN" {
		t.Fatalf("create before the hub's time is known: %d %s", st, code)
	}
	if st, code := cancel(); st != http.StatusServiceUnavailable || code != "CLOCK_UNKNOWN" {
		t.Fatalf("cancel before the hub's time is known: %d %s", st, code)
	}

	reg := tunnel.RegistrationOf(tunnel.SecretSHA256("fixture-secret"))
	if err := v.Time("n", tunnel.SignTime(key, "inst", reg, "n", "1", hubNow), "1", now(), v.Ticket()); err != nil {
		t.Fatal(err)
	}
	local.Store(hubNow.UnixNano()) // the computer's clock is set right
	if m := occupancy(); m["complete"] != false {
		t.Fatalf("the first hour of the CRM's today counts as complete: %v", m)
	}
	if st, code := create(); st != http.StatusConflict || code != "OCCUPANCY_UNKNOWN" {
		t.Fatalf("create over a booking the CRM no longer lists: %d %s", st, code)
	}
	if st, code := cancel(); st != http.StatusConflict || code != "BOOKING_STARTED" {
		t.Fatalf("cancel of a started booking: %d %s", st, code)
	}
	if f.stub.Break(started)["canceled"] != false {
		t.Fatal("a started booking was cancelled")
	}
	for _, c := range f.stub.Calls() {
		if strings.HasPrefix(c, "POST ") || strings.HasPrefix(c, "DELETE ") {
			t.Fatalf("the CRM got a write: %v", f.stub.Calls())
		}
	}
}
