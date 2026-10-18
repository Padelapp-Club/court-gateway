package lunda_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
)

// The tariff of the court's category: REGULAR prices Monday to Friday,
// WEEKEND Saturday and Sunday, in kopecks per hour, joined over hours of
// one price, in the club's zone; dates are priced from yesterday to today
// plus the club's booking depth. It is asked as the admin asks it.
func TestPriceTableByDayCategory(t *testing.T) {
	f, d := loggedIn(t)
	pt, err := d.PriceTable(context.Background(), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	if pt.Zone != nil || len(pt.Adjustments) != 0 || len(pt.Dates) != 0 {
		t.Fatalf("table: %+v", pt)
	}
	today := localToday(f.clk.now())
	if pt.DatesFrom != today.AddDate(0, 0, -1).Format("2006-01-02") || pt.DatesTo != today.AddDate(0, 0, f.stub.BookingDepth).Format("2006-01-02") {
		t.Fatalf("range %s..%s", pt.DatesFrom, pt.DatesTo)
	}
	want := map[int][]driver.PriceRule{}
	for iso := 0; iso < 7; iso++ {
		var days driver.Weekdays
		days[iso] = true
		if iso < 5 {
			want[iso] = []driver.PriceRule{{Days: days, From: 420, To: 960, PerHour: 200000}, {Days: days, From: 960, To: 1380, PerHour: 300000}}
		} else {
			want[iso] = []driver.PriceRule{{Days: days, From: 420, To: 1380, PerHour: 250000}}
		}
	}
	got := map[int][]driver.PriceRule{}
	for _, r := range pt.Base {
		for iso, on := range r.Days {
			if on {
				got[iso] = append(got[iso], r)
			}
		}
	}
	for iso := 0; iso < 7; iso++ {
		if len(got[iso]) != len(want[iso]) {
			t.Fatalf("day %d: %+v, want %+v", iso, got[iso], want[iso])
		}
		for i := range want[iso] {
			if got[iso][i] != want[iso][i] {
				t.Fatalf("day %d: %+v, want %+v", iso, got[iso], want[iso])
			}
		}
	}
	var list, load *lundastub.Call
	for _, c := range f.stub.Calls() {
		switch c.Path {
		case "/club/settings/tariff/load-list":
			list = &c
		case "/club/settings/tariff/load":
			load = &c
		}
	}
	if list == nil || list.Query != "courtCategoryId="+f.stub.Category || load == nil || load.Query != "beginDate=2020-01-01&courtCategoryId="+f.stub.Category {
		t.Fatalf("tariff calls: %+v %+v", list, load)
	}
	// a court whose category has no tariff has no prices
	f.stub.AddCourtIn("00000000-0000-4000-8000-0000000000c5", "Корт 5", "00000000-0000-4000-8000-00000000ca72")
	cfg := f.config()
	cfg.Courts = []driver.CourtConfig{{ID: "court-5", Params: driver.Params{lunda.CourtKey: "00000000-0000-4000-8000-0000000000c5"}}}
	other, err := lunda.NewDriver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	if _, err := other.PriceTable(context.Background(), "court-5"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("court-5: %v", err)
	}
}

func localToday(now time.Time) time.Time {
	msk, _ := time.LoadLocation("Europe/Moscow")
	n := now.In(msk)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, msk)
}

// A version of the tariff that begins within the range prices from its
// first day on (Dates); the one of today stays the weekly table.
func TestTariffVersionWithinTheRange(t *testing.T) {
	f, d := loggedIn(t)
	begin := localToday(f.clk.now()).AddDate(0, 0, 5)
	var cheaper []lundastub.TariffInfo
	for h := 7; h < 23; h++ {
		cheaper = append(cheaper, lundastub.TariffInfo{Category: "REGULAR", Hour: h, Price: 1000}, lundastub.TariffInfo{Category: "WEEKEND", Hour: h, Price: 1000})
	}
	f.stub.SetTariff(f.stub.Category, begin.Format("2006-01-02"), cheaper)
	pt, err := d.PriceTable(context.Background(), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pt.Dates[begin.AddDate(0, 0, -1).Format("2006-01-02")]; ok {
		t.Fatalf("the day before the version is priced apart: %v", pt.Dates)
	}
	for day := begin; day.Format("2006-01-02") <= pt.DatesTo; day = day.AddDate(0, 0, 1) {
		rules := pt.Dates[day.Format("2006-01-02")]
		if len(rules) != 1 || rules[0].PerHour != 100000 || rules[0].From != 420 || rules[0].To != 1380 {
			t.Fatalf("%s: %+v", day.Format("2006-01-02"), rules)
		}
	}
}

func TestMalformedTariffIsInvalid(t *testing.T) {
	info := func(s string) string { return `{"result":{"view":{"infos":[` + s + `]}}}` }
	for name, body := range map[string]string{
		"no view":          `{"result":{}}`,
		"no infos":         `{"result":{"view":{}}}`,
		"old shape":        `{"result":{"prices":[{"dayCategory":"REGULAR","timeFrom":"07:00","timeTo":"23:00","price":1}]}}`,
		"bad price":        info(`{"tariffCategory":"REGULAR","hour":7,"price":"free"}`),
		"bad hour":         info(`{"tariffCategory":"REGULAR","hour":24,"price":1}`),
		"no hour":          info(`{"tariffCategory":"REGULAR","price":1}`),
		"no category":      info(`{"hour":7,"price":1}`),
		"negative":         info(`{"tariffCategory":"REGULAR","hour":7,"price":-5}`),
		"two for one hour": info(`{"tariffCategory":"REGULAR","hour":7,"price":1},{"tariffCategory":"REGULAR","hour":7,"price":2}`),
		"not an object":    `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			f.stub.FailNext(http.MethodGet, "/club/settings/tariff/load", http.StatusOK, body)
			if _, err := d.PriceTable(context.Background(), "court-1"); upstreamKind(err) != driver.UpstreamInvalid {
				t.Fatalf("price table: %v", err)
			}
		})
	}
	for name, body := range map[string]string{"old list shape": `{"result":[{"uid":"x","name":"y"}]}`, "bad beginDate": `{"result":{"infos":[{"beginDate":"soon"}]}}`} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			f.stub.FailNext(http.MethodGet, "/club/settings/tariff/load-list", http.StatusOK, body)
			if _, err := d.PriceTable(context.Background(), "court-1"); upstreamKind(err) != driver.UpstreamInvalid {
				t.Fatalf("price table: %v", err)
			}
		})
	}
}

// Discovery drafts the session's club: its courts with their Lunda ids,
// the one tariff, hours from the schedule, and no zone; nothing about
// clients.
func TestDiscover(t *testing.T) {
	f := newAcctFixture(t)
	f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName))
	seedClubBooking(f.stub, court1, futureDay().Add(10*time.Hour), futureDay().Add(11*time.Hour))
	a := f.account(t)
	out, err := a.Discover(context.Background(), driver.DiscoverQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Studio != clubUID || out.ClubName != clubName || len(out.Studios) != 1 || out.Timezone != "Europe/Moscow" || len(out.CRM) != 0 {
		t.Fatalf("discovery: %+v", out)
	}
	if len(out.Courts) != 3 || out.Courts[0].Params[lunda.CourtKey] != court1 || len(out.Courts[0].Params) != 1 {
		t.Fatalf("courts: %+v", out.Courts)
	}
	for wd, h := range out.Hours {
		if h == nil || h[0] != 7*60 || h[1] != 23*60 {
			t.Fatalf("hours of day %d: %v", wd, h)
		}
	}
	codes := map[string]bool{}
	for _, n := range out.Notices {
		codes[n.Code] = true
	}
	if codes["TIMEZONE_UNKNOWN"] {
		t.Fatalf("notices: %+v", out.Notices)
	}
	f.stub.TimeZoneID = ""
	if out, err := a.Discover(context.Background(), driver.DiscoverQuery{}); err != nil || out.Timezone != "" || len(out.Notices) == 0 || out.Notices[0].Code != "TIMEZONE_UNKNOWN" {
		t.Fatalf("without a zone: %+v %v", out, err)
	}
	if _, err := a.Discover(context.Background(), driver.DiscoverQuery{Studio: "00000000-0000-4000-8000-0000000000ff"}); upstreamKind(err) != driver.UpstreamRejected {
		t.Fatalf("another club: %v", err)
	}
	for _, m := range privacyMarkers {
		if strings.Contains(f.logs.String(), m) {
			t.Fatalf("logs leak %s", m)
		}
	}
}

// The service client is chosen among the club's visitors: id, name and a
// phone masked to its last two digits; nothing of them is logged.
func TestFindClients(t *testing.T) {
	f := newAcctFixture(t)
	f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName))
	f.stub.AddVisitor(map[string]any{"profileUid": serviceUID, "name": "Kosmos PRIVACY-MARKER-LASTNAME-4be0",
		"phone": "+79990001267", "email": "privacy-marker-email-3d7e@example.test"})
	f.stub.AddVisitor(map[string]any{"profileUid": clubClient, "name": "Иван Петров", "phone": "+79990002233"})
	a := f.account(t)
	if a.ClientKey() != "service_client" {
		t.Fatalf("client key %q", a.ClientKey())
	}
	got, err := a.FindClients(context.Background(), "kosmos")
	if err != nil || len(got) != 1 || got[0].ID != serviceUID || got[0].Name != "Kosmos PRIVACY-MARKER-LASTNAME-4be0" || got[0].Hint != "телефон …67" {
		t.Fatalf("clients: %+v %v", got, err)
	}
	if f.stub.Calls()[findCall(f, "/club/visitor/list")].Params["freeTextPattern"] != "kosmos" {
		t.Fatalf("search: %+v", f.stub.Calls())
	}
	if logs := f.logs.String(); strings.Contains(logs, "PRIVACY-MARKER") || strings.Contains(logs, "1267") || strings.Contains(logs, "kosmos") {
		t.Fatalf("logs: %s", logs)
	}
}

// The config is checked as setup and the hub write it.
func TestConfigValidation(t *testing.T) {
	f := newAcctFixture(t)
	base := f.config()
	base.Courts = []driver.CourtConfig{{ID: "court-1", Params: driver.Params{lunda.CourtKey: court1}}}
	for name, tc := range map[string]struct {
		crm    driver.Params
		courts []driver.CourtConfig
		svc    map[string]driver.Params
		want   string
	}{
		"unknown key":      {crm: driver.Params{"studio": "x"}, want: "неизвестные параметры"},
		"plain http":       {crm: driver.Params{"api_url": "http://club.lundapadel.ru/api"}, want: "https://"},
		"bad client":       {crm: driver.Params{"service_client": "a b"}, want: "service_client"},
		"no court":         {courts: []driver.CourtConfig{}, want: "хотя бы один корт"},
		"court without id": {courts: []driver.CourtConfig{{ID: "c", Params: driver.Params{}}}, want: "lunda_court"},
		"court twice": {courts: []driver.CourtConfig{{ID: "a", Params: driver.Params{lunda.CourtKey: court1}}, {ID: "b", Params: driver.Params{lunda.CourtKey: court1}}},
			want: "уже привязан"},
		"services": {svc: map[string]driver.Params{"booking": {"direction": 1}}, want: "services"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.CRM = driver.Params{"api_url": f.srv.URL + "/api"}
			for k, v := range tc.crm {
				cfg.CRM[k] = v
			}
			if tc.courts != nil {
				cfg.Courts = tc.courts
			}
			cfg.Services = tc.svc
			if _, err := lunda.NewDriver(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error: %v", err)
			}
		})
	}
	// a court of another club is a mismatch of the config, in Russian
	cfg := base
	cfg.Courts = []driver.CourtConfig{{ID: "court-1", Params: driver.Params{lunda.CourtKey: "00000000-0000-4000-8000-0000000000cf"}}}
	f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName))
	d, err := lunda.NewDriver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close) // its keep-alive must not outlive the fixture
	var me *driver.MisconfigError
	if err := d.CheckConfig(context.Background()); !errors.As(err, &me) || !strings.Contains(me.Error(), "нет в клубе Лунды") {
		t.Fatalf("check: %v", err)
	}
}

// crm.type lunda is registered: the driver, its court key, and an
// interactive setup with two keys set by flag only.
func TestSetupRegistered(t *testing.T) {
	c, ok := driver.Registered()["lunda"]
	if !ok || c.Driver == nil || c.Account == nil || len(c.ClubCRMKeys) != 0 || len(c.CourtKeys) != 1 || c.CourtKeys[0] != "lunda_court" {
		t.Fatalf("registration: %+v", c)
	}
	s, ok := driver.SetupFor("lunda")
	if !ok || !s.Interactive || s.Name != "Лунда" || len(s.Secrets) != 0 || len(s.Fields) != 2 {
		t.Fatalf("setup: %+v", s)
	}
	for _, f := range s.Fields {
		if f.Prompt != "" || f.Required {
			t.Fatalf("field %+v", f)
		}
	}
}
