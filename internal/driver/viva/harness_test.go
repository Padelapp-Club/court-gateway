package viva_test

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

// Obviously fake identifiers: nothing here refers to a real studio.
const (
	studio      = "00000000-0000-4000-8000-0000000000aa"
	room1       = "00000000-0000-4000-8000-0000000000b1"
	room2       = "00000000-0000-4000-8000-0000000000b2"
	roomOther   = "00000000-0000-4000-8000-0000000000b9"
	priceList   = "00000000-0000-4000-8000-0000000000c1"
	stubUser    = "admin@example.test"
	stubPass    = "stub-password-5d1c"
	allDaysJSON = `["MONDAY","TUESDAY","WEDNESDAY","THURSDAY","FRIDAY","SATURDAY","SUNDAY"]`
)

// pricingJSON: 2000/h 07-16, 3000/h 16-24 (end written as 00:00:00),
// weekends 07-12 discounted by 500/h.
var pricingJSON = `{
  "id": "` + priceList + `", "name": "Test price list",
  "basePrices": [
    {"condition": {"type": "TIME", "days": ` + allDaysJSON + `, "timeStart": "07:00:00", "timeEnd": "16:00:00"}, "valueFrom": 2000},
    {"condition": {"type": "TIME", "days": ` + allDaysJSON + `, "timeStart": "16:00:00", "timeEnd": "00:00:00"}, "valueFrom": "3000.00"}
  ],
  "priceImpacts": {"TIME": [
    {"impactType": "FIXED", "impactDirection": "DISCOUNT", "value": 500,
     "condition": {"type": "TIME", "days": ["SATURDAY", "SUNDAY"], "timeStart": "07:00:00", "timeEnd": "12:00:00"}}
  ]}
}`

// Markers planted in a club booking; none may ever leave the gateway.
var privacyMarkers = []string{
	"PRIVACY-MARKER-FIRSTNAME-91c2",
	"PRIVACY-MARKER-LASTNAME-4be0",
	"PRIVACY-MARKER-PHONE-79990000000",
	"privacy-marker-email-3d7e@example.test",
	"PRIVACY-MARKER-COMMENT-a51f",
	"PRIVACY-MARKER-UNKNOWN-FIELD-0c9d",
	"PRIVACY-MARKER-SERVICE-NAME-62aa",
	"PRIVACY-MARKER-ROOM-NAME-e3f1",
	"PRIVACY-MARKER-NOTE-7f3a",
}

type fixture struct {
	stub  *vivastub.Stub
	srv   *httptest.Server
	built *contract.Built
	drv   *viva.Driver
}

type fixtureOpts struct {
	services   map[string]map[string]any // nil = none
	password   string                    // CRM_PASSWORD; empty = the stub's
	now        func() time.Time
	installCRM string         // more installation-level crm keys, YAML
	clubCRM    map[string]any // more club-level crm keys
}

// crmYAML is the installation's crm section.
func crmYAML(srvURL, extra string) string {
	return fmt.Sprintf("crm: { type: viva, username: %s, auth_url: %s/token, api_url: %s/api/v1%s }\n", stubUser, srvURL, srvURL, extra)
}

// testClub is the club the hub would configure.
func testClub(o fixtureOpts) core.ClubSpec {
	c := core.ClubSpec{ID: "test-club", Name: "Тестовый клуб", Timezone: "Europe/Moscow", SlotMinutes: 30,
		Hours: map[string]string{"mon-fri": "07:00-23:00", "sat-sun": "08:00-23:00"},
		CRM:   map[string]any{"studio": studio},
		Courts: []core.CourtSpec{
			{ID: "court-1", Name: "Корт 1", Params: map[string]any{"viva_room": room1, "price_list": priceList}},
			{ID: "court-2", Name: "Корт 2", Params: map[string]any{"viva_room": room2, "price_list": priceList}},
		},
		Services: o.services}
	maps.Copy(c.CRM, o.clubCRM)
	return c
}

func newFixture(t *testing.T, o fixtureOpts) *fixture {
	t.Helper()
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.AddRoom(room1, "Room one")
	stub.AddRoom(room2, "Room two")
	stub.AddRoom(roomOther, "Room elsewhere")
	stub.SetPricing(priceList, pricingJSON)
	if o.now != nil {
		stub.Now = o.now
	}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	env := map[string]string{"CRM_PASSWORD": cmp.Or(o.password, stubPass)}
	b := contract.Build(t, crmYAML(srv.URL, o.installCRM), testClub(o), env, "viva", viva.New, func(c *driver.Config) {
		c.HTTPClient = srv.Client()
		if o.now != nil {
			c.Now = o.now
		}
	})
	return &fixture{stub: stub, srv: srv, built: b, drv: b.Driver.(*viva.Driver)}
}

// seedClubBooking plants an exercise as the club's admin would create it,
// full of personal data and fields the gateway must not pass on.
func (f *fixture) seedClubBooking(room string, start, end time.Time) string {
	return f.stub.SeedExercise(map[string]any{
		"timeFrom":     start.UTC().Format(time.RFC3339),
		"timeTo":       end.UTC().Format(time.RFC3339),
		"room":         map[string]any{"id": room, "name": "PRIVACY-MARKER-ROOM-NAME-e3f1"},
		"studio":       map[string]any{"id": studio, "name": "Studio"},
		"type":         map[string]any{"id": 3, "name": "PRIVACY-MARKER-SERVICE-NAME-62aa"},
		"comment":      "PRIVACY-MARKER-COMMENT-a51f",
		"clientsCount": 1,
		"canceled":     false,
		"masterServiceBooking": map[string]any{
			"id": vivastub.NewID(),
			"client": map[string]any{
				"firstName": "PRIVACY-MARKER-FIRSTNAME-91c2",
				"lastName":  "PRIVACY-MARKER-LASTNAME-4be0",
				"phone":     "PRIVACY-MARKER-PHONE-79990000000",
				"email":     "privacy-marker-email-3d7e@example.test",
			},
		},
		"unknownVendorField": map[string]any{"nested": "PRIVACY-MARKER-UNKNOWN-FIELD-0c9d"},
	})
}

func roomFor(court string) string {
	if court == "court-2" {
		return room2
	}
	return room1
}

func (f *fixture) env() *contract.Env {
	return &contract.Env{
		Handler:      f.built.Handler(),
		Club:         "test-club",
		ReadKey:      contract.ReadKey,
		BookKey:      contract.BookKey,
		OtherBookKey: contract.OtherBookKey,
		Courts:       []string{"court-1", "court-2"},
		Location:     f.built.Settings.Location,
		SlotMinutes:  30,
		SeedForeign: func(t *testing.T, court string, start, end time.Time) string {
			return "e-" + f.seedClubBooking(roomFor(court), start, end)
		},
		Price: func(day time.Time) contract.PriceCase {
			s := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
			total := int64(3000) // 1.5 h x 2000
			if wd := s.Weekday(); wd == time.Saturday || wd == time.Sunday {
				total = 2250 // 1.5 h x (2000 - 500)
			}
			return contract.PriceCase{Court: "court-1", Start: s, End: s.Add(90 * time.Minute), Total: total}
		},
		Logs:           f.built.Logs.String,
		PrivacyMarkers: privacyMarkers,
	}
}

var bookingService = map[string]map[string]any{"booking": {"direction": 12, "type": 3}}

// The same driver-agnostic suite as every other driver, here with a
// service for games (exercises) and none for tournaments (breaks).
func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		clk := &clock{t: time.Now()}
		env := newFixture(t, fixtureOpts{services: bookingService, now: clk.now}).env()
		env.Advance = clk.add
		return env
	})
}

// And again with no services at all: every booking becomes a break.
func TestContractBreaksOnly(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		clk := &clock{t: time.Now()}
		env := newFixture(t, fixtureOpts{now: clk.now}).env()
		env.Advance = clk.add
		return env
	})
}

// A CRM that answers a read without the comment gives no evidence of
// ownership: nothing is cancelled, not even the caller's own booking, and
// the record stays in the CRM.
func TestCancelRefusedWithoutTheComment(t *testing.T) {
	for _, kind := range []string{"game", "tournament"} {
		f := newFixture(t, fixtureOpts{services: bookingService}) // game: exercise, tournament: break
		start := futureDay().Add(12 * time.Hour)
		st, m, raw := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, createBody(start, kind, ""))
		if st != http.StatusCreated {
			t.Fatalf("%s: create: %d %s", kind, st, raw)
		}
		id := m["id"].(string)
		f.stub.OmitCommentOnRead = true
		for _, key := range []string{contract.BookKey, contract.OtherBookKey} {
			if st, m, raw := call(t, f, http.MethodDelete, "/v1/clubs/test-club/bookings/"+id, key, ""); st != http.StatusForbidden || m["code"] != "NOT_OWNED" {
				t.Fatalf("%s: cancel without a comment on read: %d %s", kind, st, raw)
			}
		}
		rec := f.stub.Break(id[2:])
		if id[:2] == "e-" {
			rec = f.stub.Exercise(id[2:])
		}
		if rec["canceled"] != false {
			t.Fatalf("%s: the record was cancelled: %v", kind, rec)
		}
	}
	// another partner's break, read back with the shared title or none
	for _, title := range []string{"", viva.DefaultBreakTitle} {
		f := newFixture(t, fixtureOpts{})
		f.stub.OmitCommentOnRead = true
		id := f.stub.SeedBreak(map[string]any{"room": map[string]any{"id": room1}, "title": title, "comment": "[cg:other]", "canceled": false,
			"timeFrom": futureDay().Add(9 * time.Hour).Format(time.RFC3339), "timeTo": futureDay().Add(10 * time.Hour).Format(time.RFC3339)})
		if st, _, raw := call(t, f, http.MethodDelete, "/v1/clubs/test-club/bookings/b-"+id, contract.BookKey, ""); st != http.StatusForbidden {
			t.Fatalf("title %q: %d %s", title, st, raw)
		}
		if f.stub.Break(id)["canceled"] != false {
			t.Fatalf("title %q: another partner's break was cancelled", title)
		}
	}
}

func containsAny(s string, subs []string) string {
	for _, m := range subs {
		if strings.Contains(s, m) {
			return m
		}
	}
	return ""
}
