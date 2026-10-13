package viva_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

const (
	studio2 = "00000000-0000-4000-8000-0000000000a2"
	room21  = "00000000-0000-4000-8000-0000000000d1"
)

func newAccount(t *testing.T, stub *vivastub.Stub, password string) driver.Account {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	a, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": srv.URL + "/token", "api_url": srv.URL + "/api/v1"},
		Password: password, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Discovery reads GET /studios: every studio of the account, every room of
// the asked studio as a court (bookings or not), hours and zone from the
// studio's workTime and offset; price lists from GET /pricing.
func TestAccountDiscoverStudios(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.StudioName = "Студия Север"
	stub.WorkTime = map[string][2]string{
		"MONDAY": {"07:00+03:00", "23:00+03:00"}, "TUESDAY": {"07:00+03:00", "23:00+03:00"}, "WEDNESDAY": {"07:00+03:00", "23:00+03:00"},
		"THURSDAY": {"07:00+03:00", "23:00+03:00"}, "FRIDAY": {"07:00+03:00", "23:00+03:00"},
		"SATURDAY": {"00:00+03:00", "00:00+03:00"}, // around the clock
	}
	stub.AddRoom(room1, "Корт 10")
	stub.AddRoom(room2, "Корт 2")
	stub.SetPricing(priceList, pricingJSON)
	stub.AddStudio(studio2, "Студия Юг", map[string]string{room21: "Корт Юг"})
	a := newAccount(t, stub, stubPass)
	ctx := context.Background()
	if err := a.CheckLogin(ctx); err != nil {
		t.Fatal(err)
	}

	d, err := a.Discover(ctx, driver.DiscoverQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Studios) != 2 || d.Studio != "" || len(d.Courts) != 0 || d.Notices[0].Code != "STUDIO_REQUIRED" {
		t.Fatalf("without a studio: %+v", d)
	}

	d, err = a.Discover(ctx, driver.DiscoverQuery{Studio: strings.ToUpper(studio)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Studio != studio || d.ClubName != "Студия Север" || d.Timezone != "Etc/GMT-3" || d.CRM["studio"] != studio {
		t.Fatalf("studio: %+v", d)
	}
	// both rooms, in natural order, with no bookings at all in the stub
	if len(d.Courts) != 2 || d.Courts[0].Name != "Корт 2" || d.Courts[0].Params["viva_room"] != room2 || d.Courts[0].Params["price_list"] != priceList {
		t.Fatalf("courts: %+v", d.Courts)
	}
	if h := d.Hours[0]; h == nil || h[0] != 7*60 || h[1] != 23*60 {
		t.Fatalf("monday: %v", h)
	}
	if h := d.Hours[5]; h == nil || h[0] != 0 || h[1] != 1440 {
		t.Fatalf("saturday (00:00-00:00 is 24h): %v", h)
	}
	if d.Hours[6] != nil {
		t.Fatalf("sunday not in workTime must stay unknown: %v", d.Hours[6])
	}
	// the draft built from it is a valid club for this very driver
	spec, _ := core.DraftClub(d, "", 0)
	spec.ID = "club-north"
	base, err := core.Load([]byte("hub: { url: https://hub.example.test }\ncrm: { type: viva, username: "+stubUser+" }\n"),
		func(k string) string {
			return map[string]string{"GATEWAY_HUB_SECRET": "s", "CRM_PASSWORD": stubPass}[k]
		})
	if err != nil {
		t.Fatal(err)
	}
	st, err := base.WithClub(spec, viva.ClubCRMKeys)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if _, err := viva.New(st.Driver); err != nil {
		t.Fatalf("draft does not build a driver: %v", err)
	}

	// an unknown studio and a wrong password
	if _, err := a.Discover(ctx, driver.DiscoverQuery{Studio: "00000000-0000-4000-8000-0000000000ff"}); err == nil {
		t.Fatal("unknown studio accepted")
	}
	var ue *driver.UpstreamError
	if err := newAccount(t, stub, "wrong").CheckLogin(ctx); !errors.As(err, &ue) || ue.Kind != driver.UpstreamAuth {
		t.Fatalf("wrong password: %v", err)
	}
}

// A studio without workTime: hours come from the price lists, and the
// only studio is drafted without being named.
func TestAccountDiscoverHoursFromPricing(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.AddRoom(room1, "Корт 1")
	stub.SetPricing(priceList, pricingJSON)
	d, err := newAccount(t, stub, stubPass).Discover(context.Background(), driver.DiscoverQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Studio != studio || len(d.Courts) != 1 {
		t.Fatalf("only studio: %+v", d)
	}
	if h := d.Hours[0]; h == nil || h[0] != 7*60 || h[1] != 1440 {
		t.Fatalf("hours from pricing: %v", h)
	}
	found := false
	for _, n := range d.Notices {
		found = found || n.Code == "HOURS_FROM_PRICE_LISTS"
	}
	if !found {
		t.Fatalf("notices: %+v", d.Notices)
	}
}

// The no-clubs readiness check asks the CRM every time, not only the
// token cache: the API being down is noticed with a valid token.
func TestCheckLoginCallsTheAPI(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	a := newAccount(t, stub, stubPass)
	ctx := context.Background()
	if err := a.CheckLogin(ctx); err != nil {
		t.Fatal(err)
	}
	stub.FailNext("GET", "/studios", 503, `{}`)
	if err := a.CheckLogin(ctx); err == nil {
		t.Fatal("a failing CRM API passed the check on a cached token")
	}
	if err := a.CheckLogin(ctx); err != nil || stub.Logins() != 1 {
		t.Fatalf("recovered: %v, logins %d", err, stub.Logins())
	}
}

// With a proxy configured, the CRM is reached through it.
func TestCRMThroughTheProxy(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	crm := httptest.NewServer(stub)
	defer crm.Close()
	target, _ := url.Parse(crm.URL)
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	a, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": crm.URL + "/token", "api_url": crm.URL + "/api/v1"},
		Password: stubPass, Proxy: http.ProxyURL(pu),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CheckLogin(context.Background()); err != nil || hits.Load() == 0 {
		t.Fatalf("login %v, %d requests through the proxy", err, hits.Load())
	}
}
