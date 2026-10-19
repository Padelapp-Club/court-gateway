package lunda_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
)

// acctFixture is an installation's account against the stub, logged in or
// not, logging into logs.
type acctFixture struct {
	stub  *lundastub.Stub
	srv   *httptest.Server
	store *memStore
	logs  *contract.LogBuffer
	clk   *clock
}

func newAcctFixture(t *testing.T) *acctFixture {
	t.Helper()
	clk := &clock{t: time.Now()}
	stub := newStub(clk)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	assertOnlyAllowedCalls(t, stub)
	return &acctFixture{stub: stub, srv: srv, store: &memStore{}, logs: &contract.LogBuffer{}, clk: clk}
}

func (f *acctFixture) config() driver.Config {
	return driver.Config{
		CRM:        driver.Params{"api_url": f.srv.URL + "/api", "service_client": serviceUID},
		HTTPClient: f.srv.Client(),
		Sessions:   f.store,
		Logger:     slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:        f.clk.now,
	}
}

func (f *acctFixture) account(t *testing.T) *lunda.Account {
	t.Helper()
	a, err := lunda.NewAccount(f.config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.EndSession(context.Background(), a) })
	return a.(*lunda.Account)
}

// driver builds a club's driver on the courts 1 and 2, sharing acct's
// session when it is given.
func (f *acctFixture) driver(t *testing.T, acct driver.Account) *lunda.Driver {
	t.Helper()
	cfg := f.config()
	cfg.Account = acct
	cfg.Timezone, _ = time.LoadLocation("Europe/Moscow")
	cfg.Courts = []driver.CourtConfig{
		{ID: "court-1", Name: "Корт 1", Params: driver.Params{lunda.CourtKey: court1}},
		{ID: "court-2", Name: "Корт 2", Params: driver.Params{lunda.CourtKey: court2}},
	}
	d, err := lunda.NewDriver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func (f *acctFixture) loginQR(t *testing.T, a *lunda.Account) driver.Challenge {
	t.Helper()
	ctx := context.Background()
	before := f.store.token()
	ch, err := a.StartLogin(ctx, driver.LoginQR, "")
	if err != nil || ch.Method != driver.LoginQR || ch.QR == "" || ch.Expires.IsZero() {
		t.Fatalf("start: %+v %v", ch, err)
	}
	if st, err := a.PollLogin(ctx); err != nil || st.Done {
		t.Fatalf("poll before the scan: %+v %v", st, err)
	}
	f.stub.ConfirmQR()
	st, err := a.PollLogin(ctx)
	if err != nil || !st.Done || len(st.Clubs) != 1 || st.Clubs[0].ID != clubUID || st.Clubs[0].Name != clubName {
		t.Fatalf("poll after the scan: %+v %v", st, err)
	}
	if f.store.token() != before {
		t.Fatalf("a login was saved before its club was chosen")
	}
	_, opened, _ := f.stub.WebSockets()
	if err := a.SelectClub(ctx, clubUID); err != nil {
		t.Fatalf("select: %v", err)
	}
	waitKeepAlive(t, f.stub, opened)
	return ch
}

// A QR login: the QR is qr/start's token, polled until the staff member
// confirms; the club is switched to (the only one too) and checked, and
// the club's session is saved. A new account takes it up without a login.
func TestQRLoginSavesTheClubSession(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	if a.HasSession() {
		t.Fatal("a session before any login")
	}
	if err := a.CheckLogin(context.Background()); !errors.Is(err, driver.ErrNoSession) || upstreamKind(err) != driver.UpstreamAuth {
		t.Fatalf("check without a session: %v", err)
	}
	f.loginQR(t, a)
	saved := f.store.token()
	if saved == "" || !slices.Contains(f.stub.Sessions(), saved) {
		t.Fatalf("the saved token is not a live club session")
	}
	if f.stub.CallsTo("/profile/switch-club") != 1 || f.stub.CallsTo("/profile/current") != 1 {
		t.Fatalf("calls: %+v", f.stub.Calls())
	}
	b := f.account(t)
	if !b.HasSession() {
		t.Fatal("the saved session is not taken up")
	}
	n := len(f.stub.Calls())
	if err := b.CheckLogin(context.Background()); err != nil {
		t.Fatalf("check with the saved session: %v", err)
	}
	// the token goes in X-Club-Auth-Token, and only there
	checked := false
	for _, c := range f.stub.Calls()[n:] {
		if c.Path == "/profile/current" {
			checked = c.Token == saved
		}
	}
	if !checked {
		t.Fatalf("profile/current did not go with the saved token: %+v", f.stub.Calls()[n:])
	}
}

// Several clubs: the login is Done with them, and nothing is usable or
// saved until one is chosen; a club outside the list is refused.
func TestQRLoginWithSeveralClubs(t *testing.T) {
	f := newAcctFixture(t)
	other := lundastub.Club{UID: "00000000-0000-4000-8000-0000000000ab", Name: "Другой клуб"}
	f.stub.Clubs = []lundastub.Club{{UID: clubUID, Name: clubName}, other}
	a := f.account(t)
	ctx := context.Background()
	if _, err := a.StartLogin(ctx, driver.LoginQR, ""); err != nil {
		t.Fatal(err)
	}
	f.stub.ConfirmQR()
	st, err := a.PollLogin(ctx)
	if err != nil || !st.Done || len(st.Clubs) != 2 {
		t.Fatalf("poll: %+v %v", st, err)
	}
	if a.HasSession() {
		t.Fatal("usable before a club is chosen")
	}
	if err := a.SelectClub(ctx, "00000000-0000-4000-8000-0000000000ff"); err == nil {
		t.Fatal("a club outside the login's list was taken")
	}
	if err := a.SelectClub(ctx, other.UID); err != nil {
		t.Fatal(err)
	}
	if f.stub.Calls()[len(f.stub.Calls())-2].Params["uid"] != other.UID {
		t.Fatalf("switched to the wrong club: %+v", f.stub.Calls())
	}
	if !a.HasSession() || f.store.token() == "" {
		t.Fatal("not saved")
	}
}

func TestQRLoginExpiredAndDeclined(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		act  func(*lundastub.Stub)
		want error
	}{
		"expired":  {(*lundastub.Stub).ExpireQR, driver.ErrLoginExpired},
		"declined": {(*lundastub.Stub).DeclineQR, driver.ErrLoginDeclined},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAcctFixture(t)
			a := f.account(t)
			if _, err := a.StartLogin(ctx, driver.LoginQR, ""); err != nil {
				t.Fatal(err)
			}
			tc.act(f.stub)
			if _, err := a.PollLogin(ctx); !errors.Is(err, tc.want) {
				t.Fatalf("poll: %v", err)
			}
			// over: polling again needs a new QR
			if _, err := a.PollLogin(ctx); err == nil {
				t.Fatal("a finished login polled again")
			}
			if a.HasSession() || f.store.saves != 0 {
				t.Fatal("a session after a failed login")
			}
		})
	}
}

// A phone login: send-push with the country code apart, then the code; a
// wrong code is declined and may be typed again; the right one logs in.
func TestCodeLogin(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	ctx := context.Background()
	if _, err := a.StartLogin(ctx, driver.LoginCode, "89990000000"); err == nil {
		t.Fatal("a phone without a country code was taken")
	}
	ch, err := a.StartLogin(ctx, driver.LoginCode, stubPhone)
	if err != nil || ch.Method != driver.LoginCode || ch.Sent == "" || ch.QR != "" {
		t.Fatalf("start: %+v %v", ch, err)
	}
	push := f.stub.Calls()[len(f.stub.Calls())-1]
	if push.Path != "/profile/send-push" || push.Params["countryCode"] != stubCountry || push.Params["phone"] != "9990000000" {
		t.Fatalf("send-push: %+v", push)
	}
	if _, err := a.ConfirmLogin(ctx, "000000"); !errors.Is(err, driver.ErrLoginDeclined) {
		t.Fatalf("wrong code: %v", err)
	}
	st, err := a.ConfirmLogin(ctx, f.stub.Code)
	if err != nil || !st.Done || len(st.Clubs) != 1 {
		t.Fatalf("right code: %+v %v", st, err)
	}
	if err := a.SelectClub(ctx, st.Clubs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckLogin(ctx); err != nil {
		t.Fatal(err)
	}
}

// A code past its two minutes is expired without asking Lunda.
func TestCodeLoginExpires(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	ctx := context.Background()
	if _, err := a.StartLogin(ctx, driver.LoginCode, stubPhone); err != nil {
		t.Fatal(err)
	}
	f.clk.add(3 * time.Minute)
	before := f.stub.CallsTo("/profile/auth")
	if _, err := a.ConfirmLogin(ctx, f.stub.Code); !errors.Is(err, driver.ErrLoginExpired) || f.stub.CallsTo("/profile/auth") != before {
		t.Fatalf("expired code: %v", err)
	}
}

// The token arrives encrypted with the key of that very exchange: an
// answer encrypted for another key is refused, and nothing is saved.
func TestTokenOfAnotherKeyIsRefused(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	ctx := context.Background()
	if _, err := a.StartLogin(ctx, driver.LoginQR, ""); err != nil {
		t.Fatal(err)
	}
	// the answer of another exchange: chunks of another key
	other := newAcctFixture(t)
	b := other.account(t)
	if _, err := b.StartLogin(ctx, driver.LoginQR, ""); err != nil {
		t.Fatal(err)
	}
	other.stub.ConfirmQR()
	var stolen []byte
	other.srv.Config.Handler = recorder(other.stub, &stolen)
	if _, err := b.PollLogin(ctx); err != nil {
		t.Fatal(err)
	}
	f.stub.FailNext("POST", "/auth/qr/status", 200, string(stolen))
	if _, err := a.PollLogin(ctx); upstreamKind(err) != driver.UpstreamInvalid {
		t.Fatalf("a token of another key: %v", err)
	}
	if a.HasSession() || f.store.saves != 0 {
		t.Fatal("saved")
	}
}

func upstreamKind(err error) driver.UpstreamKind {
	var ue *driver.UpstreamError
	if errors.As(err, &ue) {
		return ue.Kind
	}
	return -1
}

// browserUA is the User-Agent the gateway must send, byte for byte, on
// every call and the WebSocket: Lunda binds a session to the User-Agent of
// its login, so it may never change between versions (seen live
// 2026-10-17: the same token with another one is 401 "missing auth
// token"). It carries no marker of the gateway.
const browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

// Every call, from the login on, and the WebSocket go with the one fixed
// browser User-Agent; the WebSocket also with the admin's Origin. The
// stub binds the session to the login's User-Agent, as Lunda does.
func TestUserAgentIsTheFixedBrowserOne(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	f.loginQR(t, a)
	d := f.driver(t, a)
	if err := d.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}
	ws := 0
	for _, c := range f.stub.Calls() {
		if c.UserAgent != browserUA {
			t.Fatalf("%s %s went with User-Agent %q", c.Method, c.Path, c.UserAgent)
		}
		if c.Method == "WS" {
			ws++
			if c.Origin != "https://club.lundapadel.ru" {
				t.Fatalf("WebSocket Origin %q", c.Origin)
			}
		}
	}
	if ws == 0 {
		t.Fatal("no WebSocket opened")
	}
}
