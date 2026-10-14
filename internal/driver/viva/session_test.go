package viva_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/remote"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// slowRT adds CRM latency to every request.
type slowRT struct{ d time.Duration }

func (s slowRT) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-time.After(s.d):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// installation is a Viva installation as serve builds it: the account,
// whose session every club's driver shares, and a syncer for the hub's
// club config.
type installation struct {
	clubs  *core.Clubs
	syncer *remote.Syncer
}

func newInstallation(t *testing.T, srvURL string, hc *http.Client, now func() time.Time) *installation {
	t.Helper()
	env := map[string]string{"CRM_PASSWORD": stubPass, core.DefaultHubSecretEnv: "test-hub-secret"}
	base, err := core.Load([]byte("hub: { url: https://hub.example.test, installation: test-net, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= }\n"+crmYAML(srvURL, "")), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	hook := func(c *driver.Config) { c.HTTPClient, c.Now = hc, now }
	hook(&base.Driver)
	if base.Driver.Account, err = viva.NewAccount(base.Driver); err != nil {
		t.Fatal(err)
	}
	clubs := core.NewClubs(core.ClubsOptions{})
	return &installation{clubs: clubs, syncer: remote.New(remote.Options{Base: base, Factories: map[string]driver.Factory{"viva": viva.New}, Clubs: clubs, DriverHook: hook})}
}

func (in *installation) apply(t *testing.T, specs ...core.ClubSpec) error {
	t.Helper()
	rc := tunnel.RemoteConfig{Version: 1}
	for _, s := range specs {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		rc.Clubs = append(rc.Clubs, raw)
	}
	return in.syncer.Apply(context.Background(), rc)
}

func studioCalls(stub *vivastub.Stub) int {
	n := 0
	for _, c := range stub.Calls() {
		if strings.HasPrefix(c, "GET /studios?") {
			n++
		}
	}
	return n
}

// An installation with many clubs behind a slow CRM logs in once for the
// account and all of them, and checks their config against one studio
// list per evaluation (applying the config, a readiness round).
func TestManyClubsShareOneSession(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	in := newInstallation(t, srv.URL, &http.Client{Transport: slowRT{40 * time.Millisecond}}, nil)
	var specs []core.ClubSpec
	for i := 0; i < 24; i++ {
		room := vivastub.NewID()
		stub.AddRoom(room, "R")
		c := testClub(fixtureOpts{})
		c.ID, c.Courts = fmt.Sprintf("club-%02d", i), []core.CourtSpec{{ID: "court-1", Name: "K", Params: map[string]any{"viva_room": room}}}
		specs = append(specs, c)
	}
	if err := in.apply(t, specs...); err != nil {
		t.Fatal(err)
	}
	if n := studioCalls(stub); n != 1 {
		t.Fatalf("%d studio list requests to apply the config, want 1", n)
	}
	if err := in.syncer.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v (%s)", err, core.ReadyReason(err))
	}
	if n := stub.LoginAttempts(); n != 1 {
		t.Fatalf("%d logins for one account, want 1", n)
	}
	// one more for the readiness round: shared by its clubs, not kept
	if n := studioCalls(stub); n != 2 {
		t.Fatalf("%d studio list requests after one readiness round, want 2", n)
	}
}

// A wrong password is tried once and then not again for ten minutes: every
// call in between fails at once with the same error, so the gateway cannot
// get the account locked by retrying.
func TestWrongPasswordIsNotRetriedForTenMinutes(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	clk := &clock{t: time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)}
	a, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": srv.URL + "/token", "api_url": srv.URL + "/api/v1"},
		Password: "wrong-password-0000", HTTPClient: srv.Client(), Now: clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	perMinute := map[int]int{}
	for i := 0; i < 60; i++ { // ten minutes, every ten seconds
		before := stub.LoginAttempts()
		var ue *driver.UpstreamError
		if err := a.CheckLogin(context.Background()); !errors.As(err, &ue) || ue.Kind != driver.UpstreamAuth {
			t.Fatalf("call %d: %v", i, err)
		}
		perMinute[i/6] += stub.LoginAttempts() - before
		clk.add(10 * time.Second)
	}
	for m, n := range perMinute {
		if n > 1 {
			t.Fatalf("%d login attempts in minute %d", n, m)
		}
	}
	if n := stub.LoginAttempts(); n != 1 {
		t.Fatalf("%d login attempts in ten minutes, want 1", n)
	}
	_ = a.CheckLogin(context.Background())
	if n := stub.LoginAttempts(); n != 2 {
		t.Fatalf("no new attempt after ten minutes: %d", n)
	}
}

// A club's studio and rooms are checked against the account when the
// hub's config is applied: a club that does not match is refused with its
// reason, the others are served. A club whose time zone is not the
// studio's is served (the driver reads the studio's days) and readiness
// reports TIMEZONE_MISMATCH for it. The readiness check asks again.
func TestClubConfigMustMatchTheStudio(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.AddRoom(room1, "Room one")
	stub.AddRoom(room2, "Room two")
	stub.AddStudio(studio2, "Студия Юг", map[string]string{room21: "Корт Юг"})
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	clk := &clock{t: time.Now()}
	in := newInstallation(t, srv.URL, srv.Client(), clk.now)

	good := testClub(fixtureOpts{})
	noStudio := testClub(fixtureOpts{})
	noStudio.ID, noStudio.CRM = "no-studio", map[string]any{"studio": "00000000-0000-4000-8000-0000000000ff"}
	foreignRoom := testClub(fixtureOpts{})
	foreignRoom.ID = "foreign-room"
	foreignRoom.Courts[1].Params["viva_room"] = room21 // a room of the other studio
	otherZone := testClub(fixtureOpts{})
	otherZone.ID, otherZone.Timezone = "other-zone", "Asia/Yekaterinburg" // UTC+5, the studio is UTC+3

	err := in.apply(t, good, noStudio, foreignRoom, otherZone)
	if err == nil {
		t.Fatal("mismatching clubs were applied")
	}
	errs := in.syncer.State().ClubErrors
	for club, want := range map[string]string{"no-studio": "crm.studio", "foreign-room": "courts[1].viva_room"} {
		if !strings.Contains(errs[club], want) {
			t.Errorf("%s: %q, want %q", club, errs[club], want)
		}
	}
	if errs["other-zone"] != "" {
		t.Errorf("other-zone refused: %q", errs["other-zone"])
	}
	if ids := strings.Join(in.clubs.IDs(), ","); ids != "other-zone,test-club" {
		t.Fatalf("served: %s", ids)
	}
	if err := in.clubs.Ready(context.Background()); core.ReadyReason(err) != "TIMEZONE_MISMATCH" || fmt.Sprint(core.ReadyClubs(err)) != "[other-zone]" {
		t.Fatalf("ready: %v", err)
	}
	// the room goes away in the CRM: the next readiness check sees it
	stub.RemoveRoom(room2)
	if err := in.clubs.Ready(context.Background()); core.ReadyReason(err) != "CONFIG_MISMATCH" {
		t.Fatalf("ready after the room was removed: %v", err)
	}
}

func TestGatewayClockBehindTheCRMAtMidnight(t *testing.T) {
	msk := time.FixedZone("UTC+3", 3*3600)
	gw := time.Date(2030, 3, 4, 23, 59, 50, 0, msk)
	f := newFixture(t, fixtureOpts{now: func() time.Time { return gw }})
	f.stub.Now = func() time.Time { return gw.Add(20 * time.Second) }
	st := time.Date(2030, 3, 5, 10, 0, 0, 0, msk)
	id := f.seedClubBooking(room1, st, st.Add(time.Hour))
	occ, err := f.drv.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: st, To: st.Add(time.Hour)})
	if err != nil || !occ.Complete || len(occ.Intervals) != 1 || occ.Intervals[0].ID != "e-"+id {
		t.Fatalf("tomorrow's booking: %+v %v", occ, err)
	}
}

// A long range is asked a week at a time, and nothing is lost at the seams.
func TestLongRangesAreListedAWeekAtATime(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := futureMidnight()
	var ids []string
	for _, day := range []int{0, 6, 7, 13, 14, 29} {
		s := d.AddDate(0, 0, day).Add(10 * time.Hour)
		ids = append(ids, f.seedClubBooking(room1, s, s.Add(time.Hour)))
	}
	before := len(f.stub.Calls())
	st, _, raw := call(t, f, http.MethodGet, occupancyPath(d, d.AddDate(0, 0, 30)), contract.ReadKey, "")
	if st != http.StatusOK {
		t.Fatalf("%d %s", st, raw)
	}
	for _, id := range ids {
		if !strings.Contains(raw, "e-"+id) {
			t.Fatalf("booking %s lost: %s", id, raw)
		}
	}
	lists := 0
	for _, c := range f.stub.Calls()[before:] {
		path, query, _ := strings.Cut(c, "?")
		if path != "GET /exercises" {
			continue
		}
		lists++
		v, _ := url.ParseQuery(query)
		from, _ := time.Parse("2006-01-02", v.Get("dateFrom"))
		to, _ := time.Parse("2006-01-02", v.Get("dateTo"))
		if days := int(to.Sub(from).Hours()/24) + 1; days > 7 {
			t.Fatalf("one request asks for %d days: %s", days, c)
		}
	}
	if lists < 5 {
		t.Fatalf("%d exercise list requests for 32 days", lists)
	}
}
