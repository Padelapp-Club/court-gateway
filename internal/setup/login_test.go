package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

func init() {
	driver.RegisterSetup("logintest", driver.SetupSpec{Name: "Тест-вход", Needs: "сотрудник клуба", Interactive: true,
		Fields: []driver.SetupField{{Key: "service_client", Flag: "crm-service-client"}}})
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

func loginInput(dir string) Input {
	return Input{ConfigPath: filepath.Join(dir, "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA",
		HubKey: testHubKey, CRMType: "logintest"}
}

// personLogsIn stands for the person at the terminal: scans the QR code.
func personLogsIn(ctx context.Context, il driver.InteractiveLogin) error {
	if _, err := il.StartLogin(ctx, driver.LoginQR, ""); err != nil {
		return err
	}
	for {
		st, err := il.PollLogin(ctx)
		if err != nil {
			return err
		}
		if st.Done {
			return il.SelectClub(ctx, st.Clubs[0].ID)
		}
	}
}

func loginDeps() Deps {
	return Deps{HubTransport: registerTransport, Accounts: map[string]driver.AccountFactory{"logintest": newFakeLogin},
		Login: personLogsIn,
		PickClient: func(ctx context.Context, cp driver.ClientPicker) (string, error) {
			found, err := cp.FindClients(ctx, "клуб")
			if err != nil {
				return "", err
			}
			return found[0].ID, nil
		}}
}

// Setup of a CRM a person logs in to: the session is saved, the owner's
// only, in the state directory the service reads — and the service, built
// from the written config in another process, uses it without a person.
// The service client chosen goes to gateway.yaml. No session secret lands
// in gateway.yaml, .env or the result.
func TestInteractiveSetupSavesTheSessionForTheService(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(context.Background(), loginInput(dir), loginDeps())
	if err != nil {
		t.Fatalf("%+v %v", res.Error, err)
	}
	sess := filepath.Join(dir, "state", core.SessionFile)
	b, err := os.ReadFile(sess)
	if err != nil || string(b) != fakeSession+":c1" {
		t.Fatalf("session file: %q %v", b, err)
	}
	if fi, _ := os.Stat(sess); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode %v", fi.Mode().Perm())
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "gateway.yaml"))
	env, _ := os.ReadFile(filepath.Join(dir, core.EnvFileName))
	out, _ := json.Marshal(res)
	for name, text := range map[string]string{"gateway.yaml": string(cfg), ".env": string(env), "result": string(out)} {
		if strings.Contains(text, fakeSession) || strings.Contains(text, fakeQR) {
			t.Fatalf("%s holds a login secret:\n%s", name, text)
		}
	}
	if !strings.Contains(string(cfg), `service_client: "client-7"`) || strings.Contains(string(env), core.PasswordEnv) {
		t.Fatalf("gateway.yaml:\n%s\n.env:\n%s", cfg, env)
	}
	// the service: another process, the written config
	getenv, err := core.EnvWithDotEnv(filepath.Join(dir, "gateway.yaml"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.LoadFile(filepath.Join(dir, "gateway.yaml"), getenv)
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := newFakeLogin(s.Driver)
	if err := acct.CheckLogin(context.Background()); err != nil {
		t.Fatalf("the service has no session after setup: %v", err)
	}
}

// Nobody to log the gateway in (--non-interactive): setup refuses before
// the join code is spent, and writes nothing.
func TestInteractiveSetupNeedsAPerson(t *testing.T) {
	dir := t.TempDir()
	deps := loginDeps()
	deps.Login = nil
	res, err := Run(context.Background(), loginInput(dir), deps)
	if err == nil || res.Error.Code != "LOGIN_REQUIRED" {
		t.Fatalf("%+v %v", res.Error, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files written: %v", entries)
	}
}

// No service client chosen: setup succeeds, warns that the gateway will
// not book, and writes no client key.
func TestSetupWithoutServiceClientWarns(t *testing.T) {
	dir := t.TempDir()
	deps := loginDeps()
	deps.PickClient = func(context.Context, driver.ClientPicker) (string, error) { return "", nil }
	res, err := Run(context.Background(), loginInput(dir), deps)
	if err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != "NO_SERVICE_CLIENT" {
		t.Fatalf("%+v %v", res, err)
	}
	if cfg, _ := os.ReadFile(filepath.Join(dir, "gateway.yaml")); strings.Contains(string(cfg), "service_client") {
		t.Fatalf("gateway.yaml:\n%s", cfg)
	}
}

// set-password has nothing to set for a CRM without a password.
func TestSetPasswordRefusesAnInteractiveCRM(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(context.Background(), loginInput(dir), loginDeps()); err != nil {
		t.Fatal(err)
	}
	err := SetPassword(context.Background(), filepath.Join(dir, "gateway.yaml"), "x", func(string) string { return "" }, loginDeps())
	if p, ok := err.(*Problem); !ok || p.Code != "NO_PASSWORD" {
		t.Fatalf("set-password: %v", err)
	}
}
