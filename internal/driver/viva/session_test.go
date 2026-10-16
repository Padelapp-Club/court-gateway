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
	"sync/atomic"
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

// hangRT holds GET /studios until the request's context ends, while hang.
type hangRT struct{ hang *atomic.Bool }

func (h hangRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if h.hang.Load() && strings.HasSuffix(r.URL.Path, "/studios") {
		<-r.Context().Done()
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

// A rejected login (invalid_grant) is tried again only rarely: after 15
// minutes, then once an hour, however often the gateway is asked; every
// call in between fails at once with the same error (partners get
// UPSTREAM_AUTH, readiness CRM_AUTH_FAILED), so a wrong password stays far
// below any lockout threshold. A new password (set-password: the service
// re-reads .env) logs in at once.
func TestWrongPasswordIsTriedAgainOnlyRarely(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	start := time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)
	clk := &clock{t: start}
	acct, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": srv.URL + "/token", "api_url": srv.URL + "/api/v1"},
		Password: "wrong-password-0000", HTTPClient: srv.Client(), Now: clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ps := acct.(driver.PasswordSetter)
	var attempts []time.Duration // since the start
	for i := 0; i < 3*360; i++ { // three hours, every ten seconds
		before := stub.LoginAttempts()
		if err := acct.CheckLogin(context.Background()); core.ReadyReason(err) != "CRM_AUTH_FAILED" || !ps.PasswordRejected() {
			t.Fatalf("call %d: %v", i, err)
		}
		if stub.LoginAttempts() != before {
			attempts = append(attempts, clk.now().Sub(start))
		}
		clk.add(10 * time.Second)
	}
	want := []time.Duration{0, 15 * time.Minute, 75 * time.Minute, 135 * time.Minute}
	if fmt.Sprint(attempts) != fmt.Sprint(want) {
		t.Fatalf("logins at %v, want %v", attempts, want)
	}
	if next := ps.NextLogin(); !next.Equal(start.Add(195 * time.Minute)) {
		t.Fatalf("next login at %v", next)
	}
	ps.SetPassword(stubPass)
	if err := acct.CheckLogin(context.Background()); err != nil || stub.LoginAttempts() != 5 || ps.PasswordRejected() || !ps.NextLogin().IsZero() {
		t.Fatalf("after set-password: %v, %d attempts", err, stub.LoginAttempts())
	}
}

// A rejection that passes by itself (the CRM refused a login it accepts
// later: a temporary lock, a limit on sessions) ends with the rare retry,
// nobody acting; the error names Keycloak's reason, printable and short.
// A retry that meets another failure leaves the rejection as it was.
func TestTransientRejectionEndsWithTheRareRetry(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	clk := &clock{t: time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)}
	acct, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": srv.URL + "/token", "api_url": srv.URL + "/api/v1"},
		Password: stubPass, HTTPClient: srv.Client(), Now: clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ps := acct.(driver.PasswordSetter)
	stub.FailNext(http.MethodPost, "/token", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Account temporarily\u0007 disabled"}`)
	err = acct.CheckLogin(context.Background())
	if core.ReadyReason(err) != "CRM_AUTH_FAILED" || !strings.Contains(err.Error(), "invalid_grant: Account temporarily disabled)") {
		t.Fatalf("rejected: %v", err)
	}
	clk.add(15 * time.Minute)
	stub.FailNext(http.MethodPost, "/token", http.StatusServiceUnavailable, `{}`)
	if err := acct.CheckLogin(context.Background()); core.ReadyReason(err) != "CRM_AUTH_FAILED" || stub.LoginAttempts() != 2 {
		t.Fatalf("the retry met an outage: %v, %d attempts", err, stub.LoginAttempts())
	}
	clk.add(59 * time.Minute)
	if err := acct.CheckLogin(context.Background()); err == nil || stub.LoginAttempts() != 2 {
		t.Fatalf("tried again within the hour: %v, %d attempts", err, stub.LoginAttempts())
	}
	clk.add(time.Minute)
	if err := acct.CheckLogin(context.Background()); err != nil || stub.LoginAttempts() != 3 || ps.PasswordRejected() {
		t.Fatalf("an hour later, the CRM accepts: %v, %d attempts", err, stub.LoginAttempts())
	}
}

// A login that fails for another reason (the CRM answers 503) is not
// remembered: calls fail fast for a short wait, doubling with failures in
// a row but never longer than a minute, and the first call after it logs
// in to the recovered CRM.
func TestUnavailableLoginIsTriedAgainWithinAMinute(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	clk := &clock{t: time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)}
	acct, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": srv.URL + "/token", "api_url": srv.URL + "/api/v1"},
		Password: stubPass, HTTPClient: srv.Client(), Now: clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 8 { // a long outage: eight failed logins in a row
		stub.FailNext("POST", "/token", http.StatusServiceUnavailable, `{}`)
	}
	for i := range 8 {
		before := stub.LoginAttempts()
		if err := acct.CheckLogin(context.Background()); core.ReadyReason(err) != "CRM_UNREACHABLE" {
			t.Fatalf("failure %d: %v", i, err)
		}
		if err := acct.CheckLogin(context.Background()); err == nil || stub.LoginAttempts() != before+1 {
			t.Fatalf("failure %d: tried again at once (%v)", i, err)
		}
		clk.add(time.Minute)
	}
	if err := acct.CheckLogin(context.Background()); err != nil {
		t.Fatalf("a minute after the last failure, the recovered CRM: %v", err)
	}
}

// Only Keycloak's invalid_grant is a rejected password. Any other refusal
// of the login (a 400, 401 or 403 that does not say invalid_grant: a
// proxy, a firewall, a changed client) is retried like an outage, with
// readiness CRM_AUTH_ERROR, and does not stop the logins.
func TestOnlyInvalidGrantStopsLogins(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	clk := &clock{t: time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)}
	acct, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": srv.URL + "/token", "api_url": srv.URL + "/api/v1"},
		Password: stubPass, HTTPClient: srv.Client(), Now: clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ps := acct.(driver.PasswordSetter)
	for _, refusal := range []struct {
		status int
		body   string
	}{{http.StatusUnauthorized, `{"error":"unauthorized_client"}`}, {http.StatusForbidden, `<html>blocked</html>`}, {http.StatusBadRequest, `{"error":"invalid_request"}`}} {
		stub.FailNext("POST", "/token", refusal.status, refusal.body)
		before := stub.LoginAttempts()
		if err := acct.CheckLogin(context.Background()); core.ReadyReason(err) != "CRM_AUTH_ERROR" || ps.PasswordRejected() {
			t.Fatalf("HTTP %d %s: %s, rejected %v", refusal.status, refusal.body, core.ReadyReason(err), ps.PasswordRejected())
		}
		clk.add(time.Minute)
		if err := acct.CheckLogin(context.Background()); err != nil || stub.LoginAttempts() != before+2 {
			t.Fatalf("HTTP %d: not logged in again after the wait: %v", refusal.status, err)
		}
		if err := driver.EndSession(context.Background(), acct); err != nil { // the next call logs in
			t.Fatal(err)
		}
	}
	ps.SetPassword("wrong-password-0000") // the stub answers invalid_grant
	if err := acct.CheckLogin(context.Background()); core.ReadyReason(err) != "CRM_AUTH_FAILED" || !ps.PasswordRejected() {
		t.Fatalf("invalid_grant: %v, rejected %v", err, ps.PasswordRejected())
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

// The studio list the clubs of a readiness round share is read within the
// time limit of the club that reads it (core.ReadyTimeout), not only that
// of the whole round: a CRM that hangs on it does not hold the round.
func TestSharedStudioReadIsBounded(t *testing.T) {
	t.Parallel() // it waits out a ReadyTimeout
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.AddRoom(room1, "1")
	stub.AddRoom(room2, "2")
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	var hang atomic.Bool
	in := newInstallation(t, srv.URL, &http.Client{Transport: hangRT{&hang}}, nil)
	if err := in.apply(t, testClub(fixtureOpts{})); err != nil {
		t.Fatal(err)
	}
	hang.Store(true)
	done := make(chan error, 1)
	go func() { done <- in.syncer.Ready(context.Background()) }() // a round with no limit of its own
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ready while the CRM hangs on the studio list: %v", err)
		}
	case <-time.After(core.ReadyTimeout + 3*time.Second):
		t.Fatal("the readiness round waits for a hanging studio list")
	}
}

// An expired access token is renewed within the session (the refresh
// grant), not with a new login: the installation keeps one session. A
// renewal the CRM refuses (the session expired or was ended) is no word on
// the password: what is left of the session is ended and one password
// login opens the next, still one session. Only a refused password login
// stops logins, with the rare retry.
func TestExpiredTokenIsRenewedInTheSession(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	clk := &clock{t: time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)}
	acct, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": srv.URL + "/token", "api_url": srv.URL + "/api/v1"},
		Password: stubPass, HTTPClient: srv.Client(), Now: clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ps := acct.(driver.PasswordSetter)
	ctx := context.Background()
	for range 10 { // fifty minutes: the token expires ten times
		if err := acct.CheckLogin(ctx); err != nil {
			t.Fatal(err)
		}
		clk.add(5 * time.Minute)
	}
	if stub.LoginAttempts() != 1 || stub.Refreshes() != 9 || stub.OpenSessions() != 1 {
		t.Fatalf("%d logins, %d renewals, %d sessions open", stub.LoginAttempts(), stub.Refreshes(), stub.OpenSessions())
	}

	// the session is gone (Keycloak's idle time): one login, one session
	stub.ExpireSessions()
	if err := acct.CheckLogin(ctx); err != nil || stub.LoginAttempts() != 2 || stub.OpenSessions() != 1 || ps.PasswordRejected() {
		t.Fatalf("after the session expired: %v, %d logins, %d sessions open", err, stub.LoginAttempts(), stub.OpenSessions())
	}
	// the renewal refused while the session is still open: it is ended first
	clk.add(5 * time.Minute)
	stub.FailNext(http.MethodPost, "/token", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Token is not active"}`)
	if err := acct.CheckLogin(ctx); err != nil || stub.LoginAttempts() != 3 || stub.Logouts() != 1 || stub.OpenSessions() != 1 || ps.PasswordRejected() {
		t.Fatalf("after a refused renewal: %v, %d logins, %d logouts, %d sessions open", err, stub.LoginAttempts(), stub.Logouts(), stub.OpenSessions())
	}
	// a renewal that does not reach the CRM keeps the session for later
	clk.add(5 * time.Minute)
	stub.FailNext(http.MethodPost, "/token", http.StatusServiceUnavailable, `{}`)
	if err := acct.CheckLogin(ctx); core.ReadyReason(err) != "CRM_UNREACHABLE" || stub.LoginAttempts() != 3 {
		t.Fatalf("an outage at renewal: %v, %d logins", err, stub.LoginAttempts())
	}
	clk.add(time.Minute)
	if err := acct.CheckLogin(ctx); err != nil || stub.LoginAttempts() != 3 || stub.OpenSessions() != 1 {
		t.Fatalf("renewed after the outage: %v, %d logins, %d sessions open", err, stub.LoginAttempts(), stub.OpenSessions())
	}
	// renewal refused and then the password: the rare retry, as ever
	clk.add(5 * time.Minute)
	stub.FailNext(http.MethodPost, "/token", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Session not active"}`)
	stub.FailNext(http.MethodPost, "/token", http.StatusUnauthorized, `{"error":"invalid_grant","error_description":"Invalid user credentials"}`)
	if err := acct.CheckLogin(ctx); core.ReadyReason(err) != "CRM_AUTH_FAILED" || !ps.PasswordRejected() || !ps.NextLogin().Equal(clk.now().Add(15*time.Minute)) {
		t.Fatalf("a refused password after a refused renewal: %v, next login %v", err, ps.NextLogin())
	}
}
