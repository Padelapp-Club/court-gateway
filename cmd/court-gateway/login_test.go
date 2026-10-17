package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
)

func init() {
	driver.RegisterSetup("logintest", driver.SetupSpec{Name: "Тест-вход", Interactive: true})
}

// fakeLogin is an account a person logs in to: QR (confirmed on the
// second poll) or code 123456, one club, one service client.
type fakeLogin struct {
	cfg     driver.Config
	session []byte
	polls   int
}

func newFakeLogin(cfg driver.Config) (driver.Account, error) {
	a := &fakeLogin{cfg: cfg}
	if cfg.Sessions != nil {
		a.session, _ = cfg.Sessions.LoadSession()
	}
	return a, nil
}

const (
	fakeQR      = "qr-payload-SECRET-1"
	fakeSession = "session-token-SECRET"
)

func (a *fakeLogin) save(b []byte) error {
	a.session = b
	if a.cfg.Sessions == nil {
		return nil
	}
	return a.cfg.Sessions.SaveSession(b)
}
func (a *fakeLogin) LoginMethods() []driver.LoginMethod {
	return []driver.LoginMethod{driver.LoginQR, driver.LoginCode}
}
func (a *fakeLogin) StartLogin(_ context.Context, m driver.LoginMethod, _ string) (driver.Challenge, error) {
	return driver.Challenge{Method: m, QR: fakeQR, Sent: "в Telegram"}, nil
}
func (a *fakeLogin) PollLogin(context.Context) (driver.LoginState, error) {
	if a.polls++; a.polls < 2 {
		return driver.LoginState{}, nil
	}
	return driver.LoginState{Done: true, Clubs: []driver.Studio{{ID: "c1", Name: "Клуб"}}}, a.save([]byte(fakeSession))
}
func (a *fakeLogin) ConfirmLogin(_ context.Context, code string) (driver.LoginState, error) {
	if code != "123456" {
		return driver.LoginState{}, driver.ErrLoginDeclined
	}
	return driver.LoginState{Done: true}, a.save([]byte(fakeSession))
}
func (a *fakeLogin) SelectClub(_ context.Context, id string) error {
	return a.save(append(append([]byte(nil), a.session...), ":"+id...))
}
func (a *fakeLogin) HasSession() bool { return a.session != nil }
func (a *fakeLogin) CheckLogin(context.Context) error {
	if a.session == nil {
		return driver.Upstream(driver.UpstreamAuth, driver.ErrNoSession.Error())
	}
	return nil
}
func (a *fakeLogin) Discover(context.Context, driver.DiscoverQuery) (driver.Discovery, error) {
	return driver.Discovery{}, nil
}
func (a *fakeLogin) ClientKey() string { return "service_client" }
func (a *fakeLogin) FindClients(context.Context, string) ([]driver.Client, error) {
	return []driver.Client{{ID: "client-7", Name: "Клуб (служебный)"}}, nil
}

// loginInstallation writes an installation of the CRM a person logs in to.
func loginInstallation(t *testing.T) string {
	t.Helper()
	drivers.Accounts["logintest"] = newFakeLogin
	t.Cleanup(func() { delete(drivers.Accounts, "logintest") })
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(cfg, []byte("hub: { url: https://hub.example.test, installation: inst-1, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= }\ncrm:\n  type: logintest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, core.EnvFileName), []byte(core.DefaultHubSecretEnv+"=cgs_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// court-gateway login: the person scans the QR code drawn in the terminal;
// the session lands in the state directory, the owner's only, and the QR
// picture is gone after. Neither the QR's text nor the session is printed.
func TestLoginByQRSavesTheSession(t *testing.T) {
	cfg := loginInstallation(t)
	defer func(d time.Duration) { loginPoll = d }(loginPoll)
	loginPoll = time.Millisecond
	defer func(r io.Reader) { stdin = r }(stdin)
	stdin = strings.NewReader("1\n")
	var out, errOut bytes.Buffer
	if code := run([]string{"login", "-config", cfg}, &out, &errOut, func(string) string { return "" }); code != 0 {
		t.Fatalf("login: %d\n%s%s", code, out.String(), errOut.String())
	}
	all := out.String() + errOut.String()
	if !strings.Contains(errOut.String(), "▄") || strings.Contains(all, fakeQR) || strings.Contains(all, fakeSession) {
		t.Fatalf("output:\n%s", all)
	}
	state := filepath.Join(filepath.Dir(cfg), "state")
	b, err := os.ReadFile(filepath.Join(state, core.SessionFile))
	if err != nil || string(b) != fakeSession+":c1" {
		t.Fatalf("session: %q %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(state, core.SessionFile)); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(state, "login-qr.png")); !os.IsNotExist(err) {
		t.Fatalf("the QR picture is left behind: %v", err)
	}
}

// A wrong code may be typed again; the right one logs in.
func TestLoginByCodeRetries(t *testing.T) {
	cfg := loginInstallation(t)
	defer func(r io.Reader) { stdin = r }(stdin)
	stdin = strings.NewReader("000000\n123456\n")
	var out, errOut bytes.Buffer
	if code := run([]string{"login", "-config", cfg, "-method", "code", "-phone", "+79990000000"}, &out, &errOut, func(string) string { return "" }); code != 0 {
		t.Fatalf("login: %d\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "Код не подошёл") || strings.Contains(out.String()+errOut.String(), "+79990000000") {
		t.Fatalf("output:\n%s%s", out.String(), errOut.String())
	}
}

// login is for a CRM a person logs in to; a password CRM is pointed to
// set-password.
func TestLoginRefusesAPasswordCRM(t *testing.T) {
	cfg := loginInstallation(t)
	if err := os.WriteFile(cfg, []byte("hub: { url: https://hub.example.test, installation: inst-1, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= }\ncrm:\n  type: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"login", "-config", cfg}, &out, &errOut, func(string) string { return "" }); code == 0 || !strings.Contains(errOut.String(), "set-password") {
		t.Fatalf("login: %d %s", code, errOut.String())
	}
}
