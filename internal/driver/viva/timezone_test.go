package viva_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

// A club zone that leaves the studio's offset later in the horizon
// (Helsinki on a UTC+3 studio) keeps occupancy right and raises an alert.
func TestClubZoneOtherThanTheStudioIsServedAndAlerted(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) }
	stub := vivastub.New(stubUser, stubPass, studio) // the studio is UTC+3
	stub.Now = now
	stub.AddRoom(room1, "Room one")
	stub.AddRoom(room2, "Room two")
	start := ts(t, "2026-10-26T23:00:00+02:00") // the studio's 27 October, 00:00
	end := start.Add(30 * time.Minute)
	stub.SeedExercise(map[string]any{"timeFrom": start.Format(time.RFC3339), "timeTo": end.Format(time.RFC3339),
		"room": map[string]any{"id": room1}, "canceled": false})
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	in := newInstallation(t, srv.URL, srv.Client(), now)

	helsinki := testClub(fixtureOpts{})
	helsinki.ID, helsinki.Timezone = "helsinki", "Europe/Helsinki"
	if err := in.apply(t, helsinki); err != nil {
		t.Fatalf("the club was refused: %v", err)
	}
	srvClub, ok := in.clubs.Server("helsinki")
	if !ok {
		t.Fatal("the club is not served")
	}
	var alert *driver.ConfigAlert
	if err := srvClub.Driver().(driver.ConfigChecker).CheckConfig(context.Background()); !errors.As(err, &alert) || alert.Code != "TIMEZONE_MISMATCH" {
		t.Fatalf("CheckConfig = %v, want a TIMEZONE_MISMATCH alert", err)
	}

	api := in.clubs.APIHandler()
	code, occ := serve(api, "GET", fmt.Sprintf("/v1/clubs/helsinki/occupancy?court=court-1&from=%s&to=%s", q(start), q(end)), "")
	if code != http.StatusOK || occ["complete"] != true || len(occ["intervals"].([]any)) != 1 {
		t.Fatalf("occupancy over the booking: %d %v", code, occ)
	}
	code, out := serve(api, "POST", "/v1/clubs/helsinki/bookings", fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`,
		start.Format(time.RFC3339), end.Format(time.RFC3339)))
	if code != http.StatusConflict || out["code"] != "SLOT_UNAVAILABLE" {
		t.Fatalf("booking over it: %d %v", code, out)
	}
	if err := in.syncer.Ready(context.Background()); core.ReadyReason(err) != "TIMEZONE_MISMATCH" || fmt.Sprint(core.ReadyClubs(err)) != "[helsinki]" {
		t.Fatalf("readiness: %v (%s %v)", err, core.ReadyReason(err), core.ReadyClubs(err))
	}

	moscow := testClub(fixtureOpts{}) // Europe/Moscow: UTC+3 all year
	if err := in.apply(t, moscow); err != nil {
		t.Fatal(err)
	}
	if err := in.syncer.Ready(context.Background()); err != nil {
		t.Fatalf("a club in the studio's zone: %v", err)
	}
}

// Studio prices are projected into club time.
func TestPriceRulesAreTheStudios(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) }
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.Now = now
	stub.AddRoom(room1, "Room one")
	stub.AddRoom(room2, "Room two")
	stub.SetPricing(priceList, pricingJSON)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	in := newInstallation(t, srv.URL, srv.Client(), now)
	helsinki := testClub(fixtureOpts{})
	helsinki.ID, helsinki.Timezone = "helsinki", "Europe/Helsinki"
	if err := in.apply(t, helsinki); err != nil {
		t.Fatal(err)
	}
	api := in.clubs.APIHandler()
	get := func(path string) map[string]any {
		code, out := serve(api, "GET", path, "")
		if code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, code, out)
		}
		return out
	}
	start := ts(t, "2026-11-04T15:00:00+02:00") // 16:00 in the studio
	got := get(fmt.Sprintf("/v1/clubs/helsinki/quote?court=court-1&start=%s&end=%s", q(start), q(start.Add(time.Hour))))
	if got["total"] != float64(3000) {
		t.Fatalf("quote 15:00-16:00 Helsinki: %v, want the studio's evening price 3000", got)
	}
	prices := get("/v1/clubs/helsinki/prices?court=court-1&date=2026-11-04")
	if s := fmt.Sprint(prices["periods"]); !strings.Contains(s, "start:06:00") || !strings.Contains(s, "end:15:00") || !strings.Contains(s, "end:23:00") {
		t.Fatalf("prices of the day in the club's clock: %s", s)
	}
}

// serve runs one partner request against h.
func serve(h http.Handler, method, path, body string) (int, map[string]any) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(auth.WithPartnerName(r.Context(), "booker")))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
