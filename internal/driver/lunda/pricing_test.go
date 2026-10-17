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
)

// The tariff's REGULAR rules price Monday to Friday, WEEKEND Saturday and
// Sunday, in kopecks per hour, in the club's zone.
func TestPriceTableByDayCategory(t *testing.T) {
	_, d := loggedIn(t)
	pt, err := d.PriceTable(context.Background(), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	if pt.Zone != nil || len(pt.Adjustments) != 0 {
		t.Fatalf("table: %+v", pt)
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
	// a court without a tariff has no prices
	if _, err := d.PriceTable(context.Background(), "court-2"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("court-2: %v", err)
	}
}

// A holiday of the calendar ahead that prices otherwise is quoted as its
// weekday (the table is weekly) and logged, by date.
func TestHolidaysAreLogged(t *testing.T) {
	f, d := loggedIn(t)
	day := futureDay()
	f.stub.SetCalendar(day.Year(), `{"year":`+day.Format("2006")+`,"days":[{"date":"`+day.Format("2006-01-02")+`","category":"HOLIDAY"}]}`)
	if _, err := d.PriceTable(context.Background(), "court-1"); err != nil {
		t.Fatal(err)
	}
	if logs := f.logs.String(); !strings.Contains(logs, day.Format("2006-01-02")+" HOLIDAY") {
		t.Fatalf("logs: %s", logs)
	}
	// a calendar that does not read costs the note only
	f.stub.FailNext(http.MethodGet, "/club/settings/calendar/load", http.StatusInternalServerError, `{}`)
	if _, err := d.PriceTable(context.Background(), "court-1"); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedTariffIsInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"no prices":     `{"result":{"uid":"x"}}`,
		"bad price":     `{"result":{"prices":[{"dayCategory":"REGULAR","timeFrom":"07:00","timeTo":"23:00","price":"free"}]}}`,
		"bad stretch":   `{"result":{"prices":[{"dayCategory":"REGULAR","timeFrom":"23:00","timeTo":"07:00","price":1}]}}`,
		"no category":   `{"result":{"prices":[{"timeFrom":"07:00","timeTo":"23:00","price":1}]}}`,
		"negative":      `{"result":{"prices":[{"dayCategory":"REGULAR","timeFrom":"07:00","timeTo":"23:00","price":-5}]}}`,
		"not an object": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			f.stub.FailNext(http.MethodGet, "/club/settings/tariff/load", http.StatusOK, body)
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
	if out.Studio != clubUID || out.ClubName != clubName || len(out.Studios) != 1 || out.Timezone != "" || len(out.CRM) != 0 {
		t.Fatalf("discovery: %+v", out)
	}
	if len(out.Courts) != 3 || out.Courts[0].Params[lunda.CourtKey] != court1 || out.Courts[0].Params[lunda.TariffKey] != tariffUID {
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
	if !codes["TIMEZONE_UNKNOWN"] || !codes["PRICE_CALENDAR"] {
		t.Fatalf("notices: %+v", out.Notices)
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
	f.stub.AddVisitor(map[string]any{"profileUid": serviceUID, "firstName": "Kosmos", "lastName": "PRIVACY-MARKER-LASTNAME-4be0",
		"phone": "+79990001267", "email": "privacy-marker-email-3d7e@example.test"})
	f.stub.AddVisitor(map[string]any{"profileUid": clubClient, "firstName": "Иван", "lastName": "Петров", "phone": "+79990002233"})
	a := f.account(t)
	if a.ClientKey() != "service_client" {
		t.Fatalf("client key %q", a.ClientKey())
	}
	got, err := a.FindClients(context.Background(), "kosmos")
	if err != nil || len(got) != 1 || got[0].ID != serviceUID || got[0].Name != "Kosmos PRIVACY-MARKER-LASTNAME-4be0" || got[0].Hint != "телефон …67" {
		t.Fatalf("clients: %+v %v", got, err)
	}
	if f.stub.Calls()[len(f.stub.Calls())-1].Params["freeTextPattern"] != "kosmos" {
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
	var me *driver.MisconfigError
	if err := d.CheckConfig(context.Background()); !errors.As(err, &me) || !strings.Contains(me.Error(), "нет в клубе Лунды") {
		t.Fatalf("check: %v", err)
	}
}

// The setup of crm.type lunda is registered: interactive, two keys set by
// flag only.
func TestSetupRegistered(t *testing.T) {
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
