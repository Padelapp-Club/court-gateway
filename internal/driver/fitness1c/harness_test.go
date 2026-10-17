package fitness1c_test

import (
	"cmp"
	"fmt"
	"maps"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c/fitness1cstub"
)

// Obviously fake identifiers: nothing here refers to a real club.
const (
	club1C      = "00000000-0000-4000-8000-0000000001aa"
	room1       = "00000000-0000-4000-8000-0000000001b1"
	room2       = "00000000-0000-4000-8000-0000000001b2"
	roomOther   = "00000000-0000-4000-8000-0000000001b9"
	gridSvc     = "00000000-0000-4000-8000-0000000001c1" // «30 мин», 875 per seance, needs a payment basis
	hourSvc     = "00000000-0000-4000-8000-0000000001c2" // «60 мин», 1750
	padelSvc    = "00000000-0000-4000-8000-0000000001c3" // «PadelApp», 0, no basis
	stubUser    = "api"
	stubPass    = "stub-password-1c0e"
	stubKey     = "stub-apikey-7d21"
	clientPhone = "+79990000001"
	partner     = "booker" // the contract suite's booking partner
)

// Markers planted in a club rental; none may ever leave the gateway.
var privacyMarkers = []string{
	"PRIVACY-MARKER-FIRSTNAME-91c2",
	"PRIVACY-MARKER-PHONE-79990000000",
	"privacy-marker-email-3d7e@example.test",
	"PRIVACY-MARKER-SERVICE-NAME-62aa",
	"PRIVACY-MARKER-NOTE-7f3a",
}

var clubPrivate = map[string]any{
	"client_name":   "PRIVACY-MARKER-FIRSTNAME-91c2",
	"phone":         []string{"PRIVACY-MARKER-PHONE-79990000000"},
	"email":         []string{"privacy-marker-email-3d7e@example.test"},
	"service_title": "PRIVACY-MARKER-SERVICE-NAME-62aa",
}

var moscow = mustZone("Europe/Moscow")

func mustZone(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// newStub is the club's 1C: three rooms (one not configured), the club's
// two paid services and the free «PadelApp» one, the technical client.
func newStub() *fitness1cstub.Stub {
	s := fitness1cstub.New(stubUser, stubPass, stubKey, club1C, "Тестовый клуб", moscow)
	s.AddService(fitness1cstub.Service{ID: gridSvc, Title: "Аренда корта 30 мин", Seance: 30, Price: "875", PaymentRequired: true})
	s.AddService(fitness1cstub.Service{ID: hourSvc, Title: "Аренда корта 60 мин", Seance: 60, Price: "1750", PaymentRequired: true})
	s.AddService(fitness1cstub.Service{ID: padelSvc, Title: "Аренда корта PadelApp", Seance: 30, Price: "0", PaymentRequired: false})
	s.AddRoom(room1, "Корт 1")
	s.AddRoom(room2, "Корт 2")
	s.AddRoom(roomOther, "Корт 9")
	s.AddClient(clientPhone)
	return s
}

func crmYAML(url, extra string) string {
	return fmt.Sprintf("crm: { type: fitness1c, base_url: %s, username: %s, client_phone: %q, partner: %s%s }\n", url, stubUser, clientPhone, partner, extra)
}

var bookingService = map[string]map[string]any{"booking": {"service": padelSvc}}

type fixtureOpts struct {
	services map[string]map[string]any // nil = bookingService; empty = none
	clubCRM  map[string]any
	extraCRM string // more installation-level keys, YAML
	now      func() time.Time
	timezone string
	env      map[string]string // overrides of the installation's env
}

func testClub(o fixtureOpts) core.ClubSpec {
	services := o.services
	if services == nil {
		services = bookingService
	}
	c := core.ClubSpec{ID: "test-club", Name: "Тестовый клуб", Timezone: cmp.Or(o.timezone, "Europe/Moscow"), SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "07:00-23:00"},
		CRM:   map[string]any{"studio": club1C},
		Courts: []core.CourtSpec{
			{ID: "court-1", Name: "Корт 1", Params: map[string]any{fitness1c.RoomKey: room1}},
			{ID: "court-2", Name: "Корт 2", Params: map[string]any{fitness1c.RoomKey: room2}},
		},
		Services: services}
	maps.Copy(c.CRM, o.clubCRM)
	return c
}

type fixture struct {
	stub  *fitness1cstub.Stub
	srv   *httptest.Server
	built *contract.Built
	drv   *fitness1c.Driver
}

func installEnv(o fixtureOpts) map[string]string {
	env := map[string]string{"CRM_PASSWORD": stubPass, fitness1c.APIKeyEnv: stubKey}
	maps.Copy(env, o.env)
	return env
}

func newFixture(t *testing.T, o fixtureOpts) *fixture {
	t.Helper()
	stub := newStub()
	stub.Now = o.now
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	b := contract.Build(t, crmYAML(srv.URL, o.extraCRM), testClub(o), installEnv(o), "fitness1c", fitness1c.New, func(c *driver.Config) {
		c.HTTPClient = srv.Client()
		c.Now = o.now
	})
	return &fixture{stub: stub, srv: srv, built: b, drv: b.Driver.(*fitness1c.Driver)}
}

func roomFor(court string) string {
	if court == "court-2" {
		return room2
	}
	return room1
}

// stretchID is the id the gateway gives a busy stretch of the grid.
func stretchID(room string, start, end time.Time) string {
	return "x-" + room + "-" + start.UTC().Format("200601021504") + "-" + end.UTC().Format("200601021504")
}

// priceCase: 875 per 30 minutes is 1750 an hour, every hour of every day.
func priceCase(day time.Time) contract.PriceCase {
	s := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
	return contract.PriceCase{Court: "court-1", Start: s, End: s.Add(90 * time.Minute), Total: 2625}
}

// movableClock is a clock tests move forward, shared by the gateway and 1C.
type movableClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *movableClock { return &movableClock{now: time.Now()} }

func (c *movableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *movableClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (f *fixture) env(clock *movableClock) *contract.Env {
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
			f.stub.SeedRental(roomFor(court), start, end, false, clubPrivate)
			return stretchID(roomFor(court), start, end)
		},
		Price:          priceCase,
		Logs:           f.built.Logs.String,
		PrivacyMarkers: privacyMarkers,
		Advance:        clock.Advance,
	}
}

func stubServer(t *testing.T, s *fitness1cstub.Stub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}
