package setup

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
)

const mirrorPassword = "fixture-crm-password"

// mirrorSetup is Viva on a stub as the first CRM and Lunda (a person logs
// in: fakeLogin) as the second; calls counts the join code's redemptions.
func mirrorSetup(t *testing.T) (Input, Deps, *int) {
	t.Helper()
	crm := httptest.NewServer(vivastub.New("admin@example.test", mirrorPassword, "00000000-0000-4000-8000-000000000001"))
	t.Cleanup(crm.Close)
	accounts := map[string]driver.AccountFactory{"viva": drivers.Accounts["viva"], "lunda": newFakeLogin}
	calls := 0
	deps := loginDeps()
	deps.Accounts, deps.CRMClient, deps.HubTransport = accounts, crm.Client(), countingTransport{calls: &calls}
	in := Input{ConfigPath: filepath.Join(t.TempDir(), "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA",
		HubKey: testHubKey, CRM: map[string]string{"username": "admin@example.test", "auth_url": crm.URL + "/token", "api_url": crm.URL + "/api/v1"},
		Password: mirrorPassword, Mirror: "lunda"}
	return in, deps, &calls
}

// The second CRM is set up as the first: a person logs it in, the session
// is saved in its own file (the owner's only) for the service, the service
// client chosen goes to the mirror section, and the written config loads
// with it. No session secret lands in gateway.yaml or .env.
func TestSetupWithMirror(t *testing.T) {
	in, deps, _ := mirrorSetup(t)
	res, err := Run(context.Background(), in, deps)
	if err != nil || res.Mirror != "lunda" {
		t.Fatalf("%+v %v", res, err)
	}
	dir := filepath.Dir(in.ConfigPath)
	sess := filepath.Join(dir, "state", core.MirrorSessionFile)
	if b, err := os.ReadFile(sess); err != nil || string(b) != fakeSession+":c1" {
		t.Fatalf("mirror session: %q %v", b, err)
	}
	if fi, _ := os.Stat(sess); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mirror session mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "state", core.SessionFile)); err == nil {
		t.Fatal("the second CRM's session went to the first CRM's file")
	}
	s, err := core.LoadFile(in.ConfigPath, func(k string) string {
		return map[string]string{core.PasswordEnv: mirrorPassword, core.DefaultHubSecretEnv: "x"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.CRMType != "viva" || s.Mirror == nil || s.Mirror.CRMType != "lunda" || s.Mirror.Driver.CRM["service_client"] != "client-7" {
		t.Fatalf("loaded: crm %s, mirror %+v", s.CRMType, s.Mirror)
	}
	cfg, _ := os.ReadFile(in.ConfigPath)
	env, _ := os.ReadFile(filepath.Join(dir, core.EnvFileName))
	if strings.Contains(string(cfg)+string(env), fakeSession) || strings.Contains(string(cfg)+string(env), fakeQR) {
		t.Fatal("a session secret was written to gateway.yaml or .env")
	}
}

// The second CRM's login fails: nothing is written, the join code is not
// spent, no session is left, and the failure says it is the second CRM's.
func TestSetupMirrorLoginFailsWritesNothing(t *testing.T) {
	in, deps, calls := mirrorSetup(t)
	deps.Login = func(ctx context.Context, il driver.InteractiveLogin) error {
		if err := personLogsIn(ctx, il); err != nil {
			return err
		}
		return driver.ErrLoginDeclined // the session was saved, then the login failed
	}
	res, err := Run(context.Background(), in, deps)
	if err == nil || res.Error.Code != "CRM_LOGIN_DECLINED" || !strings.Contains(res.Error.Message, "вторая CRM (Лунда)") {
		t.Fatalf("%+v %v", res.Error, err)
	}
	dir := filepath.Dir(in.ConfigPath)
	for _, f := range []string{in.ConfigPath, filepath.Join(dir, core.EnvFileName), filepath.Join(dir, "state", core.MirrorSessionFile)} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s left after a failed setup", f)
		}
	}
	if *calls != 0 {
		t.Fatalf("the join code was redeemed %d times", *calls)
	}
}

// Without a person at the terminal the second CRM cannot be logged in:
// refused before anything is written.
func TestSetupMirrorNeedsAPerson(t *testing.T) {
	in, deps, calls := mirrorSetup(t)
	deps.Login = nil
	res, err := Run(context.Background(), in, deps)
	if err == nil || res.Error.Code != "LOGIN_REQUIRED" || !strings.Contains(res.Error.Message, "вторая CRM (Лунда)") || *calls != 0 {
		t.Fatalf("%+v %v, redeemed %d", res.Error, err, *calls)
	}
	if _, err := os.Stat(in.ConfigPath); err == nil {
		t.Fatal("gateway.yaml written")
	}
}

// Only Lunda can be the second CRM, and not of a Lunda installation.
func TestSetupMirrorTypes(t *testing.T) {
	for _, c := range []struct{ crm, mirror string }{{"viva", "viva"}, {"viva", "yclients"}, {"lunda", "lunda"}} {
		in, deps, _ := mirrorSetup(t)
		in.CRMType, in.Mirror = c.crm, c.mirror
		if c.crm == "lunda" {
			in.CRM = nil
		}
		if res, err := Run(context.Background(), in, deps); err == nil || res.Error.Code != "INVALID_INPUT" || !strings.Contains(res.Error.Message, "второй CRM установки") {
			t.Fatalf("%s + %s: %+v %v", c.crm, c.mirror, res.Error, err)
		}
	}
}

// 1C:Fitness asks for the client of the mirrors only with a second CRM,
// and then requires it.
func TestSetupMirror1CNeedsTheMirrorClient(t *testing.T) {
	in := Input{HubURL: "https://hub.example.test", JoinCode: "x", HubKey: testHubKey, CRMType: "fitness1c", Password: "p",
		Secrets: map[string]string{"FITNESS1C_APIKEY": "k"}, CRM: map[string]string{"base_url": "https://1c.example.test", "username": "u"}}
	if slices.Contains(in.Missing(), "--crm-mirror-client-phone") {
		t.Fatalf("asked without a second CRM: %v", in.Missing())
	}
	in.Mirror = "lunda"
	if !slices.Contains(in.Missing(), "--crm-mirror-client-phone") {
		t.Fatalf("not asked with a second CRM: %v", in.Missing())
	}
}

// No service client chosen in the second CRM: set up, with a warning, and
// the mirror section has no client.
func TestSetupMirrorWithoutServiceClientWarns(t *testing.T) {
	in, deps, _ := mirrorSetup(t)
	deps.PickClient = func(context.Context, driver.ClientPicker) (string, error) { return "", nil }
	res, err := Run(context.Background(), in, deps)
	if err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != "NO_MIRROR_SERVICE_CLIENT" {
		t.Fatalf("%+v %v", res, err)
	}
	if cfg, _ := os.ReadFile(in.ConfigPath); !strings.Contains(string(cfg), "mirror:") || strings.Contains(string(cfg), "service_client") {
		t.Fatalf("gateway.yaml:\n%s", cfg)
	}
}

// The config about to be written must load with exactly this computer's
// keys of the second CRM, as of the first.
func TestValidateChecksTheMirror(t *testing.T) {
	d := &draft{HubURL: "https://hub.example.test", HubKey: placeholderKey, CRMType: "viva", CRM: [][2]string{{"username", "a"}},
		Mirror: "lunda", MirrorCRM: [][2]string{{"service_client", "c1"}}}
	crm := driver.Params{"username": "a"}
	if _, p := validate(d, crm, driver.Params{"service_client": "c1"}, "inst-1", "", "pw", "secret"); p != nil {
		t.Fatal(p)
	}
	if _, p := validate(d, crm, driver.Params{"service_client": "c2"}, "inst-1", "", "pw", "secret"); p == nil || p.Code != "INTERNAL" {
		t.Fatalf("a mirror key not of the inputs passed: %v", p)
	}
}

// A setup --force of a running installation that fails leaves each CRM's
// session as it was: the service keeps serving with it.
func TestSetupForceThatFailsKeepsTheLiveSessions(t *testing.T) {
	for _, c := range []struct {
		name, file string
		set        func(*Input, *Deps)
	}{
		{"second CRM", core.MirrorSessionFile, func(in *Input, d *Deps) {}},
		{"first CRM", core.SessionFile, func(in *Input, d *Deps) {
			in.CRMType, in.CRM, in.Password, in.Mirror = "logintest", nil, "", ""
			d.Accounts["logintest"] = newFakeLogin
		}},
	} {
		in, deps, calls := mirrorSetup(t)
		c.set(&in, &deps)
		dir := filepath.Dir(in.ConfigPath)
		if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
			t.Fatal(err)
		}
		sess := filepath.Join(dir, "state", c.file)
		if err := os.WriteFile(sess, []byte("LIVE-SESSION"), 0o600); err != nil {
			t.Fatal(err)
		}
		in.Force = true
		deps.Login = func(ctx context.Context, il driver.InteractiveLogin) error {
			if err := personLogsIn(ctx, il); err != nil {
				return err
			}
			return driver.ErrLoginExpired // a new session was saved, then the login failed
		}
		if res, err := Run(context.Background(), in, deps); err == nil || *calls != 0 {
			t.Fatalf("%s: %+v %v, redeemed %d", c.name, res.Error, err, *calls)
		}
		if b, err := os.ReadFile(sess); err != nil || string(b) != "LIVE-SESSION" {
			t.Fatalf("%s: the live session after a failed setup --force: %q %v", c.name, b, err)
		}
	}
}

// Choosing the second CRM's service client fails: setup fails, named as
// the second CRM's, and writes nothing.
func TestSetupMirrorClientChoiceFails(t *testing.T) {
	in, deps, calls := mirrorSetup(t)
	deps.PickClient = func(context.Context, driver.ClientPicker) (string, error) { return "", errors.New("search failed") }
	res, err := Run(context.Background(), in, deps)
	if err == nil || res.Error.Code != "CLIENT_NOT_CHOSEN" || !strings.Contains(res.Error.Message, "вторая CRM (Лунда)") || *calls != 0 {
		t.Fatalf("%+v %v, redeemed %d", res.Error, err, *calls)
	}
	if _, err := os.Stat(in.ConfigPath); err == nil {
		t.Fatal("gateway.yaml written")
	}
}
