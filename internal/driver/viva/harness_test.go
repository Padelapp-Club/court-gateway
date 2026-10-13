package viva_test

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
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
	services string // YAML services section; empty = none
	password string // CRM_PASSWORD; empty = the stub's
	now      func() time.Time
	extraCRM string
}

func configYAML(srvURL string, o fixtureOpts) string {
	return fmt.Sprintf(`club: { id: test-club, name: "Тестовый клуб" }
listen: "127.0.0.1:8080"
crm:
  type: viva
  username: %s
  studio: %s
  auth_url: %s/token
  api_url: %s/api/v1
%svenue:
  timezone: Europe/Moscow
  slot_minutes: 30
  hours: { mon-fri: "07:00-23:00", sat-sun: "08:00-23:00" }
courts:
  - { id: court-1, name: "Корт 1", viva_room: %s, price_list: %s }
  - { id: court-2, name: "Корт 2", viva_room: %s, price_list: %s }
%s%s`, stubUser, studio, srvURL, srvURL, o.extraCRM, room1, priceList, room2, priceList, o.services, contract.PartnersYAML)
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
	env := contract.KeyEnv()
	env["CRM_PASSWORD"] = stubPass
	if o.password != "" {
		env["CRM_PASSWORD"] = o.password
	}
	b := contract.Build(t, configYAML(srv.URL, o), env, "viva", viva.New, func(c *driver.Config) {
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
		Handler:      f.built.Server.Handler(),
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

const bookingService = "services:\n  booking: { direction: 12, type: 3 }\n"

// The same driver-agnostic suite as every other driver, here with a
// service for games (exercises) and none for tournaments (breaks).
func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		return newFixture(t, fixtureOpts{services: bookingService}).env()
	})
}

// And again with no services at all: every booking becomes a break.
func TestContractBreaksOnly(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		return newFixture(t, fixtureOpts{}).env()
	})
}

// And with a CRM that does not return comments on read. VivaCRM does
// return the comment of an exercise (observed), and an exercise read
// without one is refused; what a break read returns is not observed, so
// bookings here are breaks (no service configured) and ownership falls
// back to the break title.
func TestContractNoCommentOnRead(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		f := newFixture(t, fixtureOpts{})
		f.stub.OmitCommentOnRead = true
		env := f.env()
		env.NoPartnerIsolation = "the CRM returns no comment on read, so the ownership marker cannot be checked"
		return env
	})
}

func containsAny(s string, subs []string) string {
	for _, m := range subs {
		if strings.Contains(s, m) {
			return m
		}
	}
	return ""
}
