package lunda_test

import (
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
)

// Obviously fake identifiers: nothing here refers to a real club.
const (
	clubUID     = "00000000-0000-4000-8000-0000000000aa"
	clubName    = "Тестовый падел-клуб"
	court1      = "00000000-0000-4000-8000-0000000000c1"
	court2      = "00000000-0000-4000-8000-0000000000c2"
	courtOther  = "00000000-0000-4000-8000-0000000000c9"
	tariffUID   = "00000000-0000-4000-8000-0000000000d1"
	serviceUID  = "00000000-0000-4000-8000-0000000000e1"
	clubClient  = "00000000-0000-4000-8000-0000000000e2"
	stubPhone   = "+7 999 000-00-00"
	stubCountry = "7"
)

// tariffJSON: working days 2000 ₽/h 07–16 and 3000 ₽/h 16–23, weekends
// 2500 ₽/h, holidays 4000 ₽/h.
const tariffJSON = `{"uid":"` + tariffUID + `","name":"Основной","prices":[
 {"dayCategory":"REGULAR","timeFrom":"07:00","timeTo":"16:00","price":2000},
 {"dayCategory":"REGULAR","timeFrom":"16:00","timeTo":"23:00","price":"3000.00"},
 {"dayCategory":"WEEKEND","timeFrom":"07:00","timeTo":"23:00","price":2500},
 {"dayCategory":"HOLIDAY","timeFrom":"07:00","timeTo":"23:00","price":4000}]}`

// Markers planted in a club booking; none may ever leave the gateway.
var privacyMarkers = []string{
	"PRIVACY-MARKER-FIRSTNAME-91c2",
	"PRIVACY-MARKER-LASTNAME-4be0",
	"PRIVACY-MARKER-PHONE-79990000000",
	"privacy-marker-email-3d7e@example.test",
	"PRIVACY-MARKER-COMMENT-a51f",
	"PRIVACY-MARKER-UNKNOWN-FIELD-0c9d",
	"PRIVACY-MARKER-NOTE-7f3a",
}

// memStore is a driver.SessionStore in memory.
type memStore struct {
	mu    sync.Mutex
	data  []byte
	saves int
	err   error
}

func (m *memStore) LoadSession() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.data...), nil
}

func (m *memStore) SaveSession(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.data, m.saves = append([]byte(nil), b...), m.saves+1
	return nil
}

func (m *memStore) token() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return lunda.SessionToken(m.data)
}

func (m *memStore) set(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = append([]byte(nil), b...)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	stub  *lundastub.Stub
	srv   *httptest.Server
	store *memStore
	built *contract.Built
	drv   *lunda.Driver
	clk   *clock
}

type fixtureOpts struct {
	noService bool // no crm.service_client
	noSession bool // nobody logged in
	clock     driver.Clock
}

// crmYAML is the installation's crm section.
func crmYAML(srvURL string, service bool) string {
	extra := ""
	if service {
		extra = ", service_client: " + serviceUID
	}
	return fmt.Sprintf("crm: { type: lunda, api_url: %s/api%s }\n", srvURL, extra)
}

// testClub is the club the hub would configure.
func testClub() core.ClubSpec {
	return core.ClubSpec{ID: "test-club", Name: "Тестовый клуб", Timezone: "Europe/Moscow", SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "07:00-23:00"},
		Courts: []core.CourtSpec{
			{ID: "court-1", Name: "Корт 1", Params: map[string]any{lunda.CourtKey: court1, lunda.TariffKey: tariffUID}},
			{ID: "court-2", Name: "Корт 2", Params: map[string]any{lunda.CourtKey: court2, lunda.TariffKey: tariffUID}},
		}}
}

func newStub(clk *clock) *lundastub.Stub {
	stub := lundastub.New(lundastub.Club{UID: clubUID, Name: clubName})
	stub.Now = clk.now
	stub.Zone, _ = time.LoadLocation("Europe/Moscow")
	stub.AddCourt(court1, "Корт 1")
	stub.AddCourt(court2, "Корт 2")
	stub.AddCourt(courtOther, "Корт 9")
	stub.SetTariff(tariffUID, tariffJSON)
	return stub
}

func newFixture(t *testing.T, o fixtureOpts) *fixture {
	t.Helper()
	clk := &clock{t: time.Now()}
	stub := newStub(clk)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	assertOnlyAllowedCalls(t, stub)
	store := &memStore{}
	if !o.noSession {
		store.set(lunda.EncodeSession(stub.IssueSession(), clubUID, clubName))
	}
	b := contract.Build(t, crmYAML(srv.URL, !o.noService), testClub(), nil, "lunda", lunda.New, func(c *driver.Config) {
		c.HTTPClient = srv.Client()
		c.Sessions = store
		c.Now = clk.now
		c.Clock = o.clock
	})
	return &fixture{stub: stub, srv: srv, store: store, built: b, drv: b.Driver.(*lunda.Driver), clk: clk}
}

// assertOnlyAllowedCalls fails the test, at its end, if the driver called
// anything but its operations — add-ext-payment above all (REQ-394).
func assertOnlyAllowedCalls(t *testing.T, stub *lundastub.Stub) {
	t.Helper()
	allowed := lunda.Operations()
	t.Cleanup(func() {
		for _, c := range stub.Calls() {
			if strings.Contains(c.Path, "payment") || strings.Contains(c.Path, "refund") {
				t.Errorf("the driver called %s %s: no payment may ever be written to Lunda", c.Method, c.Path)
			}
			if !slices.Contains(allowed, c.Method+" "+c.Path) {
				t.Errorf("the driver called %s %s, which is not one of its operations", c.Method, c.Path)
			}
		}
	})
}

// seedClubBooking plants a booking as the club's staff would make it,
// full of personal data the gateway must not pass on.
func seedClubBooking(stub *lundastub.Stub, court string, start, end time.Time) string {
	return stub.SeedBooking(lundastub.Booking{Court: court, Begin: start, End: end, Customer: clubClient, Price: 1500,
		Notes: "PRIVACY-MARKER-COMMENT-a51f",
		Extra: map[string]any{
			"customer": map[string]any{"uid": clubClient, "firstName": "PRIVACY-MARKER-FIRSTNAME-91c2", "lastName": "PRIVACY-MARKER-LASTNAME-4be0",
				"phone": "PRIVACY-MARKER-PHONE-79990000000", "email": "privacy-marker-email-3d7e@example.test"},
			"contact":            map[string]any{"name": "PRIVACY-MARKER-FIRSTNAME-91c2", "phone": "PRIVACY-MARKER-PHONE-79990000000"},
			"unknownVendorField": map[string]any{"nested": "PRIVACY-MARKER-UNKNOWN-FIELD-0c9d"},
		}})
}

func courtUID(court string) string {
	if court == "court-2" {
		return court2
	}
	return court1
}

// priceCase is 10:00–11:30 on court-1 by the stub's tariff.
func priceCase(day time.Time) contract.PriceCase {
	s := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
	total := int64(3000) // 1.5 h x 2000
	if wd := s.Weekday(); wd == time.Saturday || wd == time.Sunday {
		total = 3750 // 1.5 h x 2500
	}
	return contract.PriceCase{Court: "court-1", Start: s, End: s.Add(90 * time.Minute), Total: total}
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
			return "l-" + seedClubBooking(f.stub, courtUID(court), start, end)
		},
		Price:          priceCase,
		Logs:           f.built.Logs.String,
		PrivacyMarkers: privacyMarkers,
		Advance:        f.clk.add,
	}
}
