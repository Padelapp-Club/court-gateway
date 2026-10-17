package yclients_test

import (
	"fmt"
	"maps"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// Obviously fake identifiers and secrets: nothing here is a real branch.
const (
	company      = "9900001"
	staff1       = "9910001"
	staff2       = "9910002"
	staffCoach   = "9910009"
	partnerToken = "stub-partner-token-6b1f0c2d"
	userToken    = "stub-user-token-91aa3e70"
	stubLogin    = "manager@example.test"
	stubPassword = "stub-password-2c9e"
	clientPhone  = "79000000000"
	zoneName     = "Asia/Yekaterinburg"
)

var privacyMarkers = []string{
	"PRIVACY-MARKER-NAME-91c2",
	"PRIVACY-MARKER-PHONE-79990000001",
	"privacy-marker-email-3d7e@example.test",
	"PRIVACY-MARKER-COMMENT-a51f",
	"PRIVACY-MARKER-UNKNOWN-FIELD-0c9d",
	"PRIVACY-MARKER-NOTE-7f3a",
}

// keySource is whose YCLIENTS keys the installation holds; the driver
// cannot tell, and the suite runs on each.
type keySource struct {
	name string
	env  map[string]string // the installation's .env
	crm  string            // more installation crm keys, YAML
}

var keySources = []keySource{
	// PadelApp's marketplace keys: one partner and one user token.
	{name: "marketplace keys", env: map[string]string{yclients.PartnerTokenEnv: partnerToken, yclients.UserTokenEnv: userToken}},
	// a club's own: its partner token and its staff member's login.
	{name: "club's own keys, login", env: map[string]string{yclients.PartnerTokenEnv: partnerToken, "CRM_PASSWORD": stubPassword}, crm: ", login: " + stubLogin},
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	stub  *yclientsstub.Stub
	srv   *httptest.Server
	built *contract.Built
	drv   *yclients.Driver
	clk   *clock
}

type opts struct {
	keys    keySource
	clubCRM map[string]any
	courts  []core.CourtSpec
	stub    func(*yclientsstub.Stub)
	clock   driver.Clock
}

// services of every court: 30 min, 1 h, 1.5 h and 2 h, one price per hour.
func courtServices(base int64) []yclientsstub.Service {
	return []yclientsstub.Service{
		{ID: base + 1, Title: "Аренда 30 мин", SeanceLength: 1800, Price: 1000},
		{ID: base + 2, Title: "Аренда 1 ч", SeanceLength: 3600, Price: 2000},
		{ID: base + 3, Title: "Аренда 1,5 ч", SeanceLength: 5400, Price: 3000},
		{ID: base + 4, Title: "Аренда 2 ч", SeanceLength: 7200, Price: 4000},
	}
}

func newFixture(t *testing.T, o opts) *fixture {
	t.Helper()
	if o.keys.name == "" {
		o.keys = keySources[0]
	}
	zone, err := time.LoadLocation(zoneName)
	if err != nil {
		t.Fatal(err)
	}
	stub := yclientsstub.New(partnerToken, userToken, stubLogin, stubPassword, company, zone)
	// the suite expects an empty day to read empty: courts work round the
	// clock unless a test closes them (closed time is occupancy)
	stub.DefaultDay = yclientsstub.Day{Working: true, Slots: [][2]string{{"00:00", "00:00"}}}
	stub.AddStaff(yclientsstub.Staff{ID: staff1, Name: "Корт 1", Bookable: true, Prepaid: "allowed", Services: courtServices(31000)})
	stub.AddStaff(yclientsstub.Staff{ID: staff2, Name: "Корт 2", Bookable: true, Prepaid: "allowed", Services: courtServices(32000)})
	stub.AddStaff(yclientsstub.Staff{ID: staffCoach, Name: "Тренер", Bookable: true, Prepaid: "allowed"})
	if o.stub != nil {
		o.stub(stub)
	}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	club := core.ClubSpec{ID: "test-club", Name: "Тестовый клуб", Timezone: zoneName, SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "07:00-24:00"},
		CRM:   map[string]any{"company_id": company, "client_phone": "+7 900 000-00-00"},
		Courts: []core.CourtSpec{
			{ID: "court-1", Name: "Корт 1", Params: map[string]any{"staff_id": staff1}},
			{ID: "court-2", Name: "Корт 2", Params: map[string]any{"staff_id": staff2}},
		}}
	if o.courts != nil {
		club.Courts = o.courts
	}
	maps.Copy(club.CRM, o.clubCRM)
	crm := fmt.Sprintf("crm: { type: yclients, api_url: %s/api/v1%s }\n", srv.URL, o.keys.crm)
	clk := &clock{t: time.Now()}
	b := contract.Build(t, crm, club, o.keys.env, "yclients", yclients.New, func(c *driver.Config) {
		c.HTTPClient = srv.Client()
		c.Now = clk.now
		c.Clock = o.clock
	})
	return &fixture{stub: stub, srv: srv, built: b, drv: b.Driver.(*yclients.Driver), clk: clk}
}

func staffOf(court string) string {
	if court == "court-2" {
		return staff2
	}
	return staff1
}

// seedClubBooking plants a record as the club's administrator makes one:
// a real client, full of personal data the gateway must not pass on.
func (f *fixture) seedClubBooking(staff string, start, end time.Time) int64 {
	return f.stub.Seed(yclientsstub.Record{
		StaffID: staff, Start: start, Length: int(end.Sub(start) / time.Second),
		Comment: "PRIVACY-MARKER-COMMENT-a51f", ClientName: "PRIVACY-MARKER-NAME-91c2",
		ClientPhone: "79990000001", ClientEmail: "privacy-marker-email-3d7e@example.test",
		Extra: map[string]any{"unknown_vendor_field": map[string]any{"nested": "PRIVACY-MARKER-UNKNOWN-FIELD-0c9d"},
			"client_phone_raw": "PRIVACY-MARKER-PHONE-79990000001"},
	})
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
			return fmt.Sprintf("y-%d", f.seedClubBooking(staffOf(court), start, end))
		},
		Price: func(day time.Time) contract.PriceCase {
			s := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
			return contract.PriceCase{Court: "court-1", Start: s, End: s.Add(90 * time.Minute)}
		},
		Logs:           f.built.Logs.String,
		PrivacyMarkers: privacyMarkers,
		Advance:        f.clk.add,
	}
}

// The driver-agnostic suite, on both sources of keys.
func TestContract(t *testing.T) {
	for _, ks := range keySources {
		t.Run(ks.name, func(t *testing.T) {
			contract.Run(t, func(t *testing.T) *contract.Env { return newFixture(t, opts{keys: ks}).env() })
		})
	}
}
