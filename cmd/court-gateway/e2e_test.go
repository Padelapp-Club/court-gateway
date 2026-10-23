package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/doctor"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/service"
	"github.com/Padelapp-Club/court-gateway/internal/setup"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/update"
)

// Obviously fake identifiers.
const (
	studio    = "00000000-0000-4000-8000-0000000000aa"
	roomA     = "00000000-0000-4000-8000-0000000000b1"
	roomB     = "00000000-0000-4000-8000-0000000000b2"
	roomIdle  = "00000000-0000-4000-8000-0000000000b3"
	priceList = "00000000-0000-4000-8000-0000000000c1"
	crmUser   = "admin@example.test"
	crmPass   = "stub-password-with spaces#and'quote"
)

const pricing = `{"basePrices": [
  {"condition": {"type": "TIME", "days": ["MONDAY","TUESDAY","WEDNESDAY","THURSDAY","FRIDAY"], "timeStart": "07:00:00", "timeEnd": "16:00:00"}, "valueFrom": 2000},
  {"condition": {"type": "TIME", "days": ["MONDAY","TUESDAY","WEDNESDAY","THURSDAY","FRIDAY"], "timeStart": "16:00:00", "timeEnd": "23:00:00"}, "valueFrom": 3000},
  {"condition": {"type": "TIME", "days": ["SATURDAY","SUNDAY"], "timeStart": "08:00:00", "timeEnd": "22:00:00"}, "valueFrom": 2500}
]}`

type env map[string]string

func (e env) get(k string) string { return e[k] }

type stack struct {
	hub  *testkit.Hub
	stub *vivastub.Stub
	crm  *httptest.Server
	dir  string
}

func newStack(t *testing.T, hubYAML ...string) *stack {
	t.Helper()
	return newStackWith(t, hub.Options{}, hubYAML...)
}

func newStackWith(t *testing.T, opts hub.Options, hubYAML ...string) *stack {
	t.Helper()
	stub := vivastub.New(crmUser, crmPass, studio)
	stub.AddRoom(roomA, "Корт 10")
	stub.AddRoom(roomB, "Корт 2")
	stub.AddRoom(roomIdle, "Корт без броней")
	stub.SetPricing(priceList, pricing)
	day := time.Now().UTC().AddDate(0, 0, 3)
	for i, room := range []string{roomA, roomB, roomB} {
		start := time.Date(day.Year(), day.Month(), day.Day(), 10+i, 0, 0, 0, time.UTC)
		stub.SeedExercise(map[string]any{
			"timeFrom": start.Format(time.RFC3339), "timeTo": start.Add(time.Hour).Format(time.RFC3339),
			"room":                 map[string]any{"id": room, "name": map[string]string{roomA: "Корт 10", roomB: "Корт 2"}[room]},
			"masterServiceBooking": map[string]any{"client": map[string]any{"firstName": "PRIVACY-MARKER-NAME", "phone": "PRIVACY-MARKER-PHONE"}},
		})
	}
	crm := httptest.NewServer(stub)
	t.Cleanup(crm.Close)
	h := testkit.StartHub(t, "poll_wait: 1s\n"+testkit.OperatorYAML+strings.Join(hubYAML, "")+testkit.HubPartnersYAML, testkit.HubEnv(), opts)
	return &stack{hub: h, stub: stub, crm: crm, dir: t.TempDir()}
}

func (s *stack) setupArgs(extra ...string) []string {
	return append([]string{"setup", "--non-interactive", "--json",
		"--config", filepath.Join(s.dir, "gateway.yaml"),
		"--hub-url", s.hub.URL(), "--hub-ca-file", s.hub.CAFile, "--hub-key-fingerprint", s.fingerprint(),
		"--crm-username", crmUser,
		"--crm-auth-url", s.crm.URL + "/token", "--crm-api-url", s.crm.URL + "/api/v1",
	}, extra...)
}

// fingerprint is the test hub's key fingerprint, as the operator gives it.
func (s *stack) fingerprint() string { return core.HubKeyFingerprint(s.hub.Server().Store().HubKey()) }

func decodeSetup(t *testing.T, out []byte) setup.Result {
	t.Helper()
	var r setup.Result
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("setup output is not JSON: %v\n%s", err, out)
	}
	return r
}

func TestSetupNonInteractive(t *testing.T) {
	s := newStack(t)
	code := s.hub.JoinCode("club-one", "Клуб Один")
	var out, errb bytes.Buffer

	// a wrong password fails before the join code is spent
	rc := run(s.setupArgs(), &out, &errb, env{"CRM_PASSWORD": "wrong", "GATEWAY_JOIN_CODE": code}.get)
	if r := decodeSetup(t, out.Bytes()); rc != 1 || r.OK || r.Error.Code != "CRM_AUTH_FAILED" {
		t.Fatalf("wrong password: rc=%d %s", rc, out.String())
	}
	// missing inputs are named, nothing is prompted
	out.Reset()
	rc = run(s.setupArgs(), &out, &errb, env{}.get)
	if r := decodeSetup(t, out.Bytes()); rc != 1 || r.Error.Code != "MISSING_INPUT" || !strings.Contains(r.Error.Message, "CRM_PASSWORD") {
		t.Fatalf("missing: rc=%d %s", rc, out.String())
	}

	out.Reset()
	rc = run(s.setupArgs(), &out, &errb, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": code}.get)
	r := decodeSetup(t, out.Bytes())
	if rc != 0 || !r.OK {
		t.Fatalf("setup: rc=%d %s %s", rc, out.String(), errb.String())
	}
	if r.Installation.ID != "club-one" || r.Installation.Name != "Клуб Один" || len(r.Warnings) != 0 {
		t.Fatalf("installation: %+v %+v", r.Installation, r.Warnings)
	}
	// no secret on stdout or stderr
	for _, secret := range []string{crmPass, code, "cgs_"} {
		if strings.Contains(out.String()+errb.String(), secret) {
			t.Fatalf("setup printed a secret")
		}
	}
	// the files
	// the club side keeps only the CRM login and the hub: no club, no courts
	cfg, _ := os.ReadFile(r.Config)
	if strings.Contains(string(cfg), crmPass) || strings.Contains(string(cfg), "cgs_") {
		t.Fatalf("gateway.yaml:\n%s", cfg)
	}
	for _, w := range []string{"club:", "venue:", "courts:", "studio", studio} {
		if strings.Contains(string(cfg), w) {
			t.Fatalf("gateway.yaml holds %q:\n%s", w, cfg)
		}
	}
	if strings.Contains(string(cfg), "listen") {
		t.Fatalf("setup opened a local port:\n%s", cfg)
	}
	info, err := os.Stat(r.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode %v", info.Mode().Perm())
	}
	getenv, err := core.EnvWithDotEnv(r.Config, env{}.get)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := core.LoadFile(r.Config, getenv)
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	if loaded.ClubID != "" || loaded.Driver.Password != crmPass || loaded.Hub == nil || !strings.HasPrefix(loaded.Hub.Secret, "cgs_") {
		t.Fatalf("loaded: password ok=%v hub=%v", loaded.Driver.Password == crmPass, loaded.Hub != nil)
	}

	// the same code twice, and an existing config
	out.Reset()
	rc = run(s.setupArgs("--force"), &out, &errb, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": code}.get)
	if r := decodeSetup(t, out.Bytes()); rc != 1 || r.Error.Code != "JOIN_CODE_REJECTED" {
		t.Fatalf("reused code: rc=%d %s", rc, out.String())
	}
	out.Reset()
	rc = run(s.setupArgs(), &out, &errb, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "x")}.get)
	if r := decodeSetup(t, out.Bytes()); rc != 1 || r.Error.Code != "CONFIG_EXISTS" {
		t.Fatalf("existing config: rc=%d %s", rc, out.String())
	}
}

// setup --install-service installs the service only after a successful
// setup, with the config setup wrote.
func TestSetupThenInstallService(t *testing.T) {
	s := newStack(t)
	var calls [][]string
	installService = func(args []string, _, _ io.Writer, _ string, uninstall bool) int {
		calls = append(calls, append([]string{fmt.Sprint(uninstall)}, args...))
		return 0
	}
	defer func() { installService = cmdInstallService }()
	var out bytes.Buffer
	if rc := run(s.setupArgs("--install-service"), &out, io.Discard, env{"CRM_PASSWORD": "wrong", "GATEWAY_JOIN_CODE": "x"}.get); rc != 1 || len(calls) != 0 {
		t.Fatalf("failed setup: rc=%d, service installs %v", rc, calls)
	}
	out.Reset()
	code := s.hub.JoinCode("club-one", "Клуб Один")
	if rc := run(s.setupArgs("--install-service"), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": code}.get); rc != 0 {
		t.Fatalf("rc=%d %s", rc, out.String())
	}
	r := decodeSetup(t, out.Bytes()) // stdout stays pure JSON
	if len(calls) != 1 || strings.Join(calls[0], " ") != "false -config "+r.Config {
		t.Fatalf("service installs: %v", calls)
	}
}

// A release knows its hub and that hub's key, so the human gives the agent
// only the Viva login, the password and the join code; a self-built binary
// needs --hub-url and the key's fingerprint. A hub answering with another
// key than the embedded one is refused.
func TestSetupUsesTheEmbeddedHub(t *testing.T) {
	s := newStack(t)
	args := []string{"setup", "--non-interactive", "--json", "--config", filepath.Join(s.dir, "gateway.yaml"), "--hub-ca-file", s.hub.CAFile,
		"--crm-username", crmUser, "--crm-auth-url", s.crm.URL + "/token", "--crm-api-url", s.crm.URL + "/api/v1"}
	getenv := env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб")}.get
	var out bytes.Buffer
	if rc := run(args, &out, io.Discard, getenv); rc != 1 || !strings.Contains(decodeSetup(t, out.Bytes()).Error.Message, "--hub-url") {
		t.Fatalf("self-built, no --hub-url: rc=%d %s", rc, out.String())
	}
	defaultHubURL, defaultHubKey = s.hub.URL(), core.EncodeHubKey(make([]byte, 32))
	defer func() { defaultHubURL, defaultHubKey = "", "" }()
	out.Reset()
	if rc := run(args, &out, io.Discard, getenv); rc != 1 || decodeSetup(t, out.Bytes()).Error.Code != "HUB_KEY_MISMATCH" {
		t.Fatalf("release whose hub answers with another key: rc=%d %s", rc, out.String())
	}
	defaultHubKey = core.EncodeHubKey(s.hub.Server().Store().HubKey())
	getenv = env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб")}.get
	out.Reset()
	if rc := run(args, &out, io.Discard, getenv); rc != 0 || decodeSetup(t, out.Bytes()).Hub != s.hub.URL() {
		t.Fatalf("release with an embedded hub: rc=%d %s", rc, out.String())
	}
}

// The join code and the password come from the environment; neither may
// appear in the usage text that --help or a mistyped flag prints.
func TestSetupUsageNeverPrintsSecrets(t *testing.T) {
	const code, pass, user = "SECRET-JOIN-CODE-SENTINEL", "SECRET-PASSWORD-SENTINEL", "login-sentinel@example.test"
	getenv := env{"GATEWAY_JOIN_CODE": code, "CRM_PASSWORD": pass, "CRM_USERNAME": user, "GATEWAY_HUB_URL": "https://hub.example.test"}.get
	for _, arg := range []string{"--help", "--no-such-flag"} {
		var out, errb bytes.Buffer
		if rc := run([]string{"setup", arg}, &out, &errb, getenv); rc != 2 && rc != 0 {
			t.Fatalf("%s: rc=%d", arg, rc)
		}
		all := out.String() + errb.String()
		if !strings.Contains(all, "-join-code") {
			t.Fatalf("%s: no usage printed: %q", arg, all)
		}
		for _, secret := range []string{code, pass, user} {
			if strings.Contains(all, secret) {
				t.Errorf("%s: usage output contains an environment value", arg)
			}
		}
	}
}

func TestSetupInteractiveAndPasswordStdin(t *testing.T) {
	s := newStack(t)
	old := stdin
	defer func() { stdin = old }()
	isTerminal = func() bool { return false }

	// password, join code: nothing about clubs; the CRM type only when
	// several are offered (Enter takes the default, Viva), then whether
	// there is a second CRM (Enter: no)
	answers := strings.Join([]string{crmPass, s.hub.JoinCode("club-two", "Клуб Два")}, "\n") + "\n"
	answers = "\n" + answers
	if len(setupTypes(env{}.get)) > 1 {
		answers = "\n" + answers
	}
	stdin = strings.NewReader(answers)
	var out, errb bytes.Buffer
	args := []string{"setup", "--config", filepath.Join(s.dir, "gateway.yaml"), "--hub-url", s.hub.URL(), "--hub-ca-file", s.hub.CAFile,
		"--hub-key-fingerprint", s.fingerprint(),
		"--crm-username", crmUser, "--crm-auth-url", s.crm.URL + "/token", "--crm-api-url", s.crm.URL + "/api/v1"}
	if rc := run(args, &out, &errb, env{}.get); rc != 0 {
		t.Fatalf("interactive: %d\n%s\n%s", rc, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "Код подключения") || !strings.Contains(errb.String(), "Пароль администратора Viva") || strings.Contains(errb.String(), "Тип CRM") {
		t.Fatalf("prompts: %s", errb.String())
	}
	for _, q := range []string{"Часовой пояс", "студии", "клуба"} {
		if strings.Contains(errb.String(), q+":") || strings.Contains(errb.String(), q+" [") {
			t.Fatalf("setup asked about %q: %s", q, errb.String())
		}
	}
	if strings.Contains(out.String()+errb.String(), crmPass) {
		t.Fatal("interactive setup echoed the password")
	}
	if !strings.Contains(out.String(), "Готово") {
		t.Fatalf("summary: %s", out.String())
	}

	// --password-stdin for agents
	dir2 := t.TempDir()
	stdin = strings.NewReader(crmPass + "\n")
	out.Reset()
	args = []string{"setup", "--non-interactive", "--json", "--password-stdin", "--config", filepath.Join(dir2, "gateway.yaml"),
		"--hub-url", s.hub.URL(), "--hub-ca-file", s.hub.CAFile, "--crm-username", crmUser,
		"--crm-auth-url", s.crm.URL + "/token", "--crm-api-url", s.crm.URL + "/api/v1"}
	if rc := run(args, &out, &errb, env{"GATEWAY_JOIN_CODE": s.hub.JoinCode("club-three", "Клуб Три"), "GATEWAY_HUB_KEY_FINGERPRINT": s.fingerprint()}.get); rc != 0 {
		t.Fatalf("password-stdin: %d %s", rc, out.String())
	}
}

// Until the service has reached the hub and applied its club config it is
// not ready (STARTING), however short the hub's absence: the grace for a
// lost hub is for later disconnects only.
func TestNotReadyUntilTheHubAndAConfig(t *testing.T) {
	s := newStack(t)
	statusInterval = 200 * time.Millisecond
	defer func() { statusInterval = 60 * time.Second }()
	var out bytes.Buffer
	if rc := run(s.setupArgs(), &out, &out, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	cfg := filepath.Join(s.dir, "gateway.yaml")
	s.hub.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, cfg, "", &contract.LogBuffer{}, env{}.get) }()
	defer func() { cancel(); <-done }()
	health := func(want string) func() bool {
		return func() bool {
			var b bytes.Buffer
			run([]string{"healthcheck", "-config", cfg}, &b, io.Discard, env{}.get)
			return strings.Contains(b.String(), want)
		}
	}
	testkit.WaitFor(t, "not ready before the hub is reached", 10*time.Second, health("ready=false reason=STARTING"))
	s.hub.Restart()
	testkit.WaitFor(t, "ready once connected, with a config", 30*time.Second, health("ready=true"))
}

// The whole path: the club runs setup once (CRM login + join code) and the
// service; the operator configures the club on the hub; the gateway pulls
// it -> doctor -> partner call through the hub -> healthcheck; then the
// CRM goes away.
func TestServeDoctorHealthcheck(t *testing.T) {
	s := newStack(t)
	statusInterval = 200 * time.Millisecond
	defer func() { statusInterval = 60 * time.Second }()
	var out, errb bytes.Buffer
	if rc := run(s.setupArgs(), &out, &errb, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	cfg := filepath.Join(s.dir, "gateway.yaml")
	// the operator adds the club to the installation (what an admin panel does)
	club := core.ClubSpec{ID: "club-one", Name: "Клуб Один", Timezone: "Europe/Moscow", SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "07:00-23:00"}, CRM: map[string]any{"studio": studio},
		Courts: []core.CourtSpec{
			{ID: "court-1", Name: "Корт 10", Params: map[string]any{"viva_room": roomA, "price_list": priceList}},
			{ID: "court-2", Name: "Корт 2", Params: map[string]any{"viva_room": roomB, "price_list": priceList}},
		}}
	s.hub.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/club-one/clubs/club-one", club)

	// doctor before the service runs: the tunnel check fails, exit 1
	out.Reset()
	if rc := run([]string{"doctor", "--json", "-config", cfg}, &out, &errb, env{}.get); rc != 1 {
		t.Fatalf("doctor without service: rc=%d %s", rc, out.String())
	}
	if rep := decodeDoctor(t, out.Bytes()); status(rep, doctor.CodeTunnel) != doctor.Fail || status(rep, doctor.CodeCRMLogin) != doctor.OK {
		t.Fatalf("doctor without service: %s", out.String())
	}
	// setup and doctor each logged in once and ended that session
	if s.stub.Logins() != 2 || s.stub.OpenSessions() != 0 {
		t.Fatalf("before the service: %d logins, %d sessions open", s.stub.Logins(), s.stub.OpenSessions())
	}

	ctx, cancel := context.WithCancel(context.Background())
	logs := &contract.LogBuffer{}
	done := make(chan int, 1)
	go func() { done <- serve(ctx, cfg, "", logs, env{}.get) }()
	defer func() {
		cancel()
		if rc := <-done; rc != 0 {
			t.Errorf("serve exit %d", rc)
		}
	}()
	mon := s.hub.Server().Monitor()
	testkit.WaitFor(t, "the service connected and applied the club config", 30*time.Second, func() bool {
		h := mon.Health("club-one")
		return h.ConfigVersion == 1 && strings.Join(h.Clubs, ",") == "club-one"
	})

	out.Reset()
	logins := s.stub.LoginAttempts()
	rc := run([]string{"doctor", "--json", "-config", cfg}, &out, &errb, env{}.get)
	rep := decodeDoctor(t, out.Bytes())
	if rc != 0 || !rep.OK {
		t.Fatalf("doctor: rc=%d %s", rc, out.String())
	}
	// with the service running doctor reports its checks and logs in not
	// at all: one session open, the service's
	if s.stub.LoginAttempts() != logins || s.stub.OpenSessions() != 1 {
		t.Fatalf("doctor beside the service: %d logins, %d sessions open", s.stub.LoginAttempts()-logins, s.stub.OpenSessions())
	}
	if rep.Installation != "club-one" || rep.ConfigVersion != 1 || rep.AppliedConfigVersion != 1 || strings.Join(rep.ClubsServed, ",") != "club-one" {
		t.Fatalf("doctor report: %s", out.String())
	}
	for _, c := range []string{doctor.CodeConfig, doctor.CodeCRMLogin, doctor.CodeOccupancy, doctor.CodeHubReach, doctor.CodeHubAuth, doctor.CodeTunnel, doctor.CodeHubConfig} {
		if status(rep, c) != doctor.OK {
			t.Fatalf("%s is %s: %s", c, status(rep, c), out.String())
		}
	}
	if status(rep, doctor.CodeVersion) != doctor.Warn { // a test binary is self-built
		t.Fatalf("version check: %s", out.String())
	}
	out.Reset()
	if rc := run([]string{"doctor", "-config", cfg}, &out, &errb, env{}.get); rc != 0 || !strings.Contains(out.String(), "Всё в порядке") || !strings.Contains(out.String(), "обслуживает: club-one") {
		t.Fatalf("doctor text: %d %s", rc, out.String())
	}

	// a partner call goes partner -> hub -> tunnel -> gateway -> CRM
	req, _ := http.NewRequest(http.MethodGet, s.hub.URL()+"/v1/clubs/club-one/courts", nil)
	req.Header.Set("Authorization", "Bearer "+contract.ReadKey)
	resp, err := (&http.Client{Transport: s.hub.Transport()}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Корт 10") {
		t.Fatalf("partner call: %d %s", resp.StatusCode, body)
	}

	// healthcheck reads the status file: healthy
	testkit.WaitFor(t, "healthy and connected", 10*time.Second, func() bool {
		var b bytes.Buffer
		return run([]string{"healthcheck", "-config", cfg}, &b, io.Discard, env{}.get) == 0 && strings.Contains(b.String(), "hub=connected")
	})
	// the same self-check reaches the hub as a heartbeat
	testkit.WaitFor(t, "hub got a ready heartbeat", 10*time.Second, func() bool {
		h := mon.Health("club-one")
		return h.Checked && h.Ready && h.Version != ""
	})
	// the CRM goes away: unhealthy, and doctor fails with a code
	s.crm.Close()
	s.stub.RevokeTokens()
	testkit.WaitFor(t, "unhealthy because of the CRM", 10*time.Second, func() bool {
		var b bytes.Buffer
		return run([]string{"healthcheck", "-config", cfg}, &b, io.Discard, env{}.get) == 1 && strings.Contains(b.String(), "reason=CRM_UNREACHABLE")
	})
	testkit.WaitFor(t, "hub got a not-ready heartbeat", 10*time.Second, func() bool {
		h := mon.Health("club-one")
		return !h.Ready && h.Reason == "CRM_UNREACHABLE"
	})
	out.Reset()
	if rc := run([]string{"healthcheck", "-config", cfg}, &out, &errb, env{}.get); rc != 1 || !strings.Contains(out.String(), "CRM_UNREACHABLE") {
		t.Fatalf("healthcheck: %d %s", rc, out.String())
	}

	// nothing secret in what the service logged or doctor printed
	all := logs.String() + out.String() + s.hub.Logs.String()
	for _, secret := range []string{crmPass, "cgs_", "PRIVACY-MARKER"} {
		if strings.Contains(all, secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
}

// A club the gateway refuses makes the installation not ready while its
// other clubs are served: the heartbeat of the running service says
// CONFIG_REFUSED, and after three of them the hub's status files show the
// installation not ready, until the operator fixes the club. An update
// installed meanwhile is still confirmed: that needs the CRM login and the
// hub, not every club.
func TestRefusedClubMakesTheInstallationNotReady(t *testing.T) {
	statusDir := filepath.Join(t.TempDir(), "status")
	s := newStack(t, "status_dir: "+statusDir+"\n")
	statusInterval = 200 * time.Millisecond
	defer func() { statusInterval = 60 * time.Second }()
	var out bytes.Buffer
	if rc := run(s.setupArgs(), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("net-one", "Сеть")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	cfg := filepath.Join(s.dir, "gateway.yaml")
	club := func(id, room string) core.ClubSpec {
		return core.ClubSpec{ID: id, Name: id, Timezone: "Europe/Moscow", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "07:00-23:00"},
			CRM: map[string]any{"studio": studio}, Courts: []core.CourtSpec{{ID: "court-1", Name: "Корт", Params: map[string]any{"viva_room": room}}}}
	}
	s.hub.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/net-one/clubs/club-one", club("club-one", roomA))
	s.hub.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/net-one/clubs/club-two", club("club-two", "not-a-room-id"))
	// this binary was just installed by an update and must confirm itself
	pending := filepath.Join(s.dir, "state", "update-pending.json")
	_ = os.MkdirAll(filepath.Dir(pending), 0o700)
	if err := os.WriteFile(pending, []byte(`{"version":"`+version+`","from":"0.0.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, cfg, "", &contract.LogBuffer{}, env{}.get) }()
	defer func() { cancel(); <-done }()
	mon := s.hub.Server().Monitor()
	readyCheck := func() (bool, string) {
		mon.Tick()
		var doc struct {
			Checks []struct {
				Check string `json:"check"`
				OK    bool   `json:"ok"`
			} `json:"checks"`
		}
		data, _ := os.ReadFile(filepath.Join(statusDir, hub.StatusFile))
		_ = json.Unmarshal(data, &doc)
		metrics, _ := os.ReadFile(filepath.Join(statusDir, hub.MetricsFile))
		for _, c := range doc.Checks {
			if c.Check == "installation_net_one_ready" {
				return c.OK, string(metrics)
			}
		}
		return false, ""
	}
	testkit.WaitFor(t, "the refused club reported", 15*time.Second, func() bool {
		h := mon.Health("net-one")
		return strings.Join(h.Clubs, ",") == "club-one" && h.ClubErrors["club-two"] != ""
	})
	testkit.WaitFor(t, "not ready in the status files", 15*time.Second, func() bool {
		ok, metrics := readyCheck()
		return !ok && strings.Contains(metrics, "check_installation_net_one_ready=0\n")
	})
	if h := mon.Health("net-one"); h.State != hub.StateNotReady || h.Reason != "CONFIG_REFUSED" {
		t.Fatalf("health: %+v", h)
	}
	// the served club still answers partners
	req, _ := http.NewRequest(http.MethodGet, s.hub.URL()+"/v1/clubs/club-one/courts", nil)
	req.Header.Set("Authorization", "Bearer "+contract.ReadKey)
	resp, err := (&http.Client{Transport: s.hub.Transport()}).Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("served club: %v %v", resp, err)
	}
	resp.Body.Close()
	testkit.WaitFor(t, "the update confirmed", 15*time.Second, func() bool {
		_, err := os.Stat(pending)
		return os.IsNotExist(err)
	})

	// the operator fixes the club
	s.hub.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/net-one/clubs/club-two", club("club-two", roomB))
	testkit.WaitFor(t, "ready again in the status files", 15*time.Second, func() bool {
		ok, metrics := readyCheck()
		return ok && strings.Contains(metrics, "check_installation_net_one_ready=1\n")
	})
}

func decodeDoctor(t *testing.T, b []byte) doctor.Report {
	t.Helper()
	var r doctor.Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("doctor output: %v\n%s", err, b)
	}
	return r
}

func status(r doctor.Report, code string) doctor.Status {
	for _, c := range r.Checks {
		if c.Code == code {
			return c.Status
		}
	}
	return ""
}

func TestDoctorFailsOnBadConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gateway.yaml")
	_ = os.WriteFile(cfg, []byte("club: { id: x }\n"), 0o644)
	var out bytes.Buffer
	if rc := run([]string{"doctor", "--json", "-config", cfg}, &out, io.Discard, env{}.get); rc != 1 {
		t.Fatalf("rc %d", rc)
	}
	if rep := decodeDoctor(t, out.Bytes()); status(rep, doctor.CodeConfig) != doctor.Fail || status(rep, doctor.CodeTunnel) != doctor.Skip {
		t.Fatalf("%s", out.String())
	}
}

func TestInstallServicePrint(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gateway.yaml")
	_ = os.WriteFile(cfg, []byte("x: 1\n"), 0o644)
	for goos, want := range map[string][]string{
		"linux":   {"[Service]", "Restart=always", "serve -config", "WantedBy=multi-user.target", "systemctl enable --now court-gateway"},
		"darwin":  {"<key>KeepAlive</key><true/>", "<string>serve</string>", "launchctl bootstrap"},
		"windows": {"sc.exe create court-gateway binPath=", "service -config", "sc.exe failure court-gateway", "restart/5000"},
	} {
		var out bytes.Buffer
		if rc := run([]string{"install-service", "--print", "--os", goos, "-config", cfg}, &out, io.Discard, env{}.get); rc != 0 {
			t.Fatalf("%s: rc %d", goos, rc)
		}
		for _, w := range want {
			if !strings.Contains(out.String(), w) {
				t.Fatalf("%s: missing %q in\n%s", goos, w, out.String())
			}
		}
	}
}

func TestHubJoinCodeCommand(t *testing.T) {
	dir := t.TempDir()
	hubCfg := filepath.Join(dir, "hub.yaml")
	_ = os.WriteFile(hubCfg, []byte("state_dir: "+filepath.Join(dir, "state")+"\npublic_url: https://hub.example.test\n"), 0o644)
	var out bytes.Buffer
	if rc := run([]string{"hub", "join-code", "create", "-config", hubCfg, "--installation", "club-x", "--name", "Клуб X"}, &out, io.Discard, env{}.get); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	printed := out.String()
	sc := bufio.NewScanner(strings.NewReader(printed))
	sc.Scan()
	line := sc.Text()
	code := strings.TrimSpace(line[strings.LastIndex(line, ":")+1:])
	store, err := hub.OpenStore(filepath.Join(dir, "state"), nil)
	if err != nil {
		t.Fatal(err)
	}
	g, secret, err := store.Redeem(code)
	if err != nil || g.Installation != "club-x" || g.Name != "Клуб X" || secret == "" {
		t.Fatalf("redeem %q: %+v %v", code, g, err)
	}
	if !strings.Contains(printed, "--hub-url https://hub.example.test") {
		t.Fatalf("output: %s", printed)
	}
	if rc := run([]string{"hub", "join-code", "create", "-config", hubCfg, "--club", "Bad Id"}, io.Discard, io.Discard, env{}.get); rc != 2 {
		t.Fatalf("bad club id accepted: %d", rc)
	}
}

// A stopped service tells the hub, so the copy started after it — a
// restart, an update — is served at once instead of being refused as a
// second copy until offline_after passes.
func TestRestartedServiceIsServedAtOnce(t *testing.T) {
	s := newStack(t)
	var out, errb bytes.Buffer
	if rc := run(s.setupArgs(), &out, &errb, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	cfg := filepath.Join(s.dir, "gateway.yaml")
	getenv, err := core.EnvWithDotEnv(cfg, env{}.get)
	if err != nil {
		t.Fatal(err)
	}
	connected := func() bool { // as the hub sees the gateway
		who, err := testkit.FetchWhoami(context.Background(), s.hub.Transport(), s.hub.URL(), getenv(core.DefaultHubSecretEnv))
		return err == nil && who.Connected
	}
	for i := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() { done <- serve(ctx, cfg, "", &contract.LogBuffer{}, env{}.get) }()
		testkit.WaitFor(t, fmt.Sprintf("start %d connected", i+1), 10*time.Second, connected)
		cancel()
		if rc := <-done; rc != 0 {
			t.Fatalf("serve exit %d", rc)
		}
		testkit.WaitFor(t, "disconnected", 10*time.Second, func() bool { return !connected() })
	}
}

// A config the service applies reaches the hub at once, not with the next
// minute's self-check: doctor half a minute after setup sees it applied.
func TestAppliedConfigIsReportedAtOnce(t *testing.T) {
	s := newStack(t)
	var out bytes.Buffer
	if rc := run(s.setupArgs(), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, filepath.Join(s.dir, "gateway.yaml"), "", &contract.LogBuffer{}, env{}.get) }()
	defer func() { cancel(); <-done }()
	mon := s.hub.Server().Monitor()
	testkit.WaitFor(t, "the first heartbeat", 15*time.Second, func() bool { return mon.Health("club-one").Checked })
	s.hub.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/club-one/clubs/club-one", core.ClubSpec{
		ID: "club-one", Name: "Клуб Один", Timezone: "Europe/Moscow", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "07:00-23:00"},
		CRM: map[string]any{"studio": studio}, Courts: []core.CourtSpec{{ID: "court-1", Name: "Корт 10", Params: map[string]any{"viva_room": roomA}}}})
	testkit.WaitFor(t, "the hub hears of the applied config", 10*time.Second, func() bool {
		h := mon.Health("club-one")
		return h.ConfigVersion == 1 && strings.Join(h.Clubs, ",") == "club-one"
	})
}

// setup takes only Viva: the demo CRM is for tests and demonstrations,
// behind GATEWAY_DEMO_CRM=1, and is refused before anything is sent.
func TestSetupTakesOnlyViva(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gateway.yaml")
	var out bytes.Buffer
	rc := run([]string{"setup", "--non-interactive", "--json", "--crm", "demo", "--config", cfg, "--hub-url", "https://hub.invalid",
		"--hub-key-fingerprint", "AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AA", "--join-code", "AAAA-AAAA-AAAA-AAAA"}, &out, io.Discard, env{}.get)
	if rc != 1 || !strings.Contains(out.String(), `"INVALID_INPUT"`) || !strings.Contains(out.String(), "demo") {
		t.Fatalf("setup --crm demo: %d %s", rc, out.String())
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatalf("a config was written: %v", err)
	}
}

// A state directory the service cannot write is its own reason at the
// hub, STATE_UNWRITABLE: nothing else is wrong.
func TestUnwritableStateIsReported(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX permissions, not root")
	}
	statusInterval = 200 * time.Millisecond
	defer func() { statusInterval = 60 * time.Second }()
	s := newStack(t)
	var out bytes.Buffer
	if rc := run(s.setupArgs(), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	cfg := filepath.Join(s.dir, "gateway.yaml")
	state := core.StateDir(cfg, env{}.get)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, cfg, "", &contract.LogBuffer{}, env{}.get) }()
	defer func() { cancel(); <-done }()
	mon := s.hub.Server().Monitor()
	testkit.WaitFor(t, "a heartbeat with STATE_UNWRITABLE", 20*time.Second, func() bool { return mon.Health("club-one").Reason == "STATE_UNWRITABLE" })
}

// The running service decides "now" on the hub's signed time, not on this
// computer's clock: with the hub 50 minutes ahead, a booking that starts
// in half an hour by this computer has started by the hub, and is refused.
func TestServeDecidesOnTheHubsClock(t *testing.T) {
	const ahead = 50 * time.Minute
	s := newStackWith(t, hub.Options{Now: func() time.Time { return time.Now().Add(ahead) }})
	var out bytes.Buffer
	if rc := run(s.setupArgs(), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	s.hub.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/club-one/clubs/club-one", core.ClubSpec{
		ID: "club-one", Name: "Клуб Один", Timezone: "Europe/Moscow", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "00:00-24:00"},
		CRM: map[string]any{"studio": studio}, Courts: []core.CourtSpec{{ID: "court-1", Name: "Корт 10", Params: map[string]any{"viva_room": roomA}}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, filepath.Join(s.dir, "gateway.yaml"), "", &contract.LogBuffer{}, env{}.get) }()
	defer func() { cancel(); <-done }()
	mon := s.hub.Server().Monitor()
	testkit.WaitFor(t, "the club config applied", 15*time.Second, func() bool { return mon.Health("club-one").ConfigVersion == 1 })
	start := time.Now().Add(10 * time.Minute).Truncate(30 * time.Minute).Add(30 * time.Minute) // in 20-40 min
	body := fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
	resp, b := s.hub.Do(t, 10*time.Second, http.MethodPost, "/v1/clubs/club-one/bookings", contract.BookKey, strings.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "start must be in the future") {
		t.Fatalf("a booking that started %s ago by the hub: %d %s", time.Now().Add(ahead).Sub(start).Round(time.Minute), resp.StatusCode, b)
	}
}

// The self-check an update runs with the new binary does not judge it
// when the CRM rejects the password: that is not the binary's fault. It
// logs in once and exits "not run", which the updater neither counts
// against the release nor retries.
func TestSelfcheckWithARejectedPasswordIsNotRun(t *testing.T) {
	s := newStack(t)
	var out bytes.Buffer
	if rc := run(s.setupArgs(), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	cfg := filepath.Join(s.dir, "gateway.yaml")
	before := s.stub.LoginAttempts()
	var errb bytes.Buffer
	s.stub.FailNext(http.MethodPost, vivastub.TokenPath, http.StatusUnauthorized, `{"error":"invalid_grant"}`) // the password changed in the CRM
	if rc := run([]string{"selfcheck", "-config", cfg}, io.Discard, &errb, env{}.get); rc != update.SelfCheckNotRunExit || s.stub.LoginAttempts() != before+1 {
		t.Fatalf("selfcheck with a rejected password: rc=%d %s, %d logins", rc, errb.String(), s.stub.LoginAttempts()-before)
	}
	// with the password of .env it logs in and reaches the hub
	if rc := run([]string{"selfcheck", "-config", cfg}, &out, &errb, env{}.get); rc != 0 || !strings.HasSuffix(out.String(), "ok\n") {
		t.Fatalf("selfcheck: rc=%d %s %s", rc, out.String(), errb.String())
	}
	// and ends its session, as setup did
	if s.stub.OpenSessions() != 0 || s.stub.Logouts() != 2 {
		t.Fatalf("after selfcheck: %d sessions open, %d logouts", s.stub.OpenSessions(), s.stub.Logouts())
	}
}

// setup --force while the service runs (a new join code: a new secret)
// restarts the service, which then runs with the new connection: nobody
// has to restart it by hand.
func TestServiceRestartsWhenSetupRewritesTheHubConnection(t *testing.T) {
	s := newStack(t)
	var out bytes.Buffer
	if rc := run(s.setupArgs(), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	cfg := filepath.Join(s.dir, "gateway.yaml")
	restarted := make(chan []string, 1)
	restartProcess = func(_ string, args []string) error { restarted <- args; return update.ErrRestartRequired }
	defer func() { restartProcess = update.Restart }()
	done := make(chan int, 1)
	go func() { done <- serve(context.Background(), cfg, "", &contract.LogBuffer{}, env{}.get) }()
	getenv, err := core.EnvWithDotEnv(cfg, env{}.get)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WaitFor(t, "the service connected", 15*time.Second, func() bool {
		who, err := testkit.FetchWhoami(context.Background(), s.hub.Transport(), s.hub.URL(), getenv(core.DefaultHubSecretEnv))
		return err == nil && who.Connected
	})
	out.Reset()
	if rc := run(s.setupArgs("--force"), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup --force: %s", out.String())
	}
	select {
	case rc := <-done:
		if rc != exitRestart || len(restarted) != 1 {
			t.Fatalf("serve returned %d, restarts %d", rc, len(restarted))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the service kept running with the replaced secret")
	}
}

// The CRM refused the service's login (not the password: it is the one
// setup checked). The service does not retry a rejected login by itself;
// doctor reports the service's state, without a login of its own beside
// it; set-password with the same password, which it checks, makes the
// service log in again, once.
func TestServiceRefusedThenSetPasswordWithTheSamePassword(t *testing.T) {
	s := newStack(t)
	statusInterval = 200 * time.Millisecond
	defer func() { statusInterval = 60 * time.Second }()
	var out, errb bytes.Buffer
	if rc := run(s.setupArgs(), &out, &errb, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	cfg := filepath.Join(s.dir, "gateway.yaml")
	s.stub.FailNext(http.MethodPost, vivastub.TokenPath, http.StatusBadRequest, `{"error":"invalid_grant"}`)
	ctx, cancel := context.WithCancel(context.Background())
	logs := &contract.LogBuffer{}
	done := make(chan int, 1)
	go func() { done <- serve(ctx, cfg, "", logs, env{}.get) }()
	defer func() { cancel(); <-done }()
	health := func(rc int, want string) func() bool {
		return func() bool {
			var b bytes.Buffer
			return run([]string{"healthcheck", "-config", cfg}, &b, io.Discard, env{}.get) == rc && strings.Contains(b.String(), want)
		}
	}
	testkit.WaitFor(t, "the service refused", 10*time.Second, health(1, "reason=CRM_AUTH_FAILED"))
	// it tries again by itself, but only in a quarter of an hour; the
	// status file and the hub say when
	st, err := core.ReadHealthStatus(filepath.Join(s.dir, "state"))
	if wait := time.Until(st.CRMNextLogin); err != nil || wait < 14*time.Minute || wait > 15*time.Minute {
		t.Fatalf("next login in %s (%v)", wait, err)
	}
	testkit.WaitFor(t, "the hub knows the next login", 10*time.Second, func() bool {
		return s.hub.Server().Monitor().Health("club-one").NextLogin.Equal(st.CRMNextLogin)
	})
	// the rejection is kept for a restart, and no password with it
	kept, err := core.RejectionStore(filepath.Join(s.dir, "state")).LoadRejections()
	if err != nil || kept.Count != 1 || kept.Key == "" {
		t.Fatalf("rejections kept: %+v (%v)", kept, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(s.dir, "state", core.RejectionsFile)); strings.Contains(string(raw), crmPass) {
		t.Fatal("the password is in the state directory")
	}

	logins := s.stub.LoginAttempts()
	out.Reset()
	if rc := run([]string{"doctor", "--json", "-config", cfg}, &out, &errb, env{}.get); rc != 1 {
		t.Fatalf("doctor: rc=%d %s", rc, out.String())
	}
	var login doctor.Check
	for _, c := range decodeDoctor(t, out.Bytes()).Checks {
		if c.Code == doctor.CodeCRMLogin {
			login = c
		}
	}
	if login.Status != doctor.Fail || !strings.Contains(login.Message, "по данным службы") || !strings.Contains(login.Message, "CRM_AUTH_FAILED") ||
		!strings.Contains(login.Message, "следующая попытка в "+st.CRMNextLogin.Local().Format("15:04")) ||
		!strings.Contains(login.Hint, "set-password") || s.stub.LoginAttempts() != logins {
		t.Fatalf("doctor beside a refused service: %+v, %d logins of its own", login, s.stub.LoginAttempts()-logins)
	}

	if rc := run([]string{"set-password", "-config", cfg}, &out, &errb, env{"CRM_PASSWORD": crmPass}.get); rc != 0 {
		t.Fatalf("set-password: %s", errb.String())
	}
	testkit.WaitFor(t, "the service logged in again", 10*time.Second, health(0, "ready=true"))
	if n := s.stub.LoginAttempts() - logins; n != 2 || s.stub.OpenSessions() != 1 {
		t.Fatalf("set-password and the service: %d logins, %d sessions open", n, s.stub.OpenSessions())
	}
	if st, _ := core.ReadHealthStatus(filepath.Join(s.dir, "state")); !st.CRMNextLogin.IsZero() || st.CRM != "ok" {
		t.Fatalf("status after the login: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "state", core.RejectionsFile)); !os.IsNotExist(err) {
		t.Fatalf("rejections still kept after the login: %v", err)
	}
	if strings.Contains(logs.String(), crmPass) || !strings.Contains(logs.String(), "invalid_grant") {
		t.Fatalf("the log names the rejection, not the password:\n%s", logs.String())
	}
}

// A running service whose CRM check failed is never a doctor success,
// whatever its status file says: a status ahead of the clock, a CRM check
// too old under a fresh file, a status of a version before the crm fields
// (its reason is the failure) and an unfinished check. doctor logs in to
// the CRM in none of them.
func TestDoctorTrustsNoUnusableStatus(t *testing.T) {
	s := newStack(t)
	old := statusInterval
	statusInterval = time.Hour // the service does not rewrite the file meanwhile
	defer func() { statusInterval = old }()
	var out bytes.Buffer
	if rc := run(s.setupArgs(), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	cfg := filepath.Join(s.dir, "gateway.yaml")
	s.stub.FailNext(http.MethodPost, vivastub.TokenPath, http.StatusBadRequest, `{"error":"invalid_grant"}`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, cfg, "", &contract.LogBuffer{}, env{}.get) }()
	defer func() { cancel(); <-done }()
	stateDir := filepath.Join(s.dir, "state")
	testkit.WaitFor(t, "the service connected, its login rejected", 10*time.Second, func() bool {
		st, err := core.ReadHealthStatus(stateDir)
		return err == nil && st.CRM == "CRM_AUTH_FAILED" && st.Hub == "connected"
	})
	for name, tc := range map[string]struct {
		st   core.HealthStatus
		want doctor.Status
	}{
		"ahead of the clock":        {core.HealthStatus{Time: time.Now().Add(24 * time.Hour), CRM: "ok", CRMCheckedAt: time.Now(), Ready: true, Hub: "connected"}, doctor.Warn},
		"old CRM check, fresh file": {core.HealthStatus{Time: time.Now(), CRM: "ok", CRMCheckedAt: time.Now().Add(-24 * time.Hour), Ready: true, Hub: "connected"}, doctor.Warn},
		"legacy, CRM failed":        {core.HealthStatus{Time: time.Now(), Version: "1.0.0", Ready: false, Reason: "CRM_AUTH_FAILED", Hub: "connected"}, doctor.Fail},
		"unfinished CRM check":      {core.HealthStatus{Time: time.Now(), Ready: false, Reason: "STARTING", Hub: "connected"}, doctor.Warn},
	} {
		t.Run(name, func(t *testing.T) {
			if err := core.WriteHealthStatus(stateDir, tc.st); err != nil {
				t.Fatal(err)
			}
			before := s.stub.LoginAttempts()
			rep := doctor.Run(context.Background(), doctor.Options{ConfigPath: cfg, Getenv: env{}.get, Factories: drivers.Factories, Accounts: drivers.Accounts,
				Version: version, Timeout: 50 * time.Millisecond})
			if rep.OK || status(rep, doctor.CodeCRMLogin) != tc.want || s.stub.LoginAttempts() != before {
				t.Fatalf("ok=%v CRM_LOGIN %s, %d logins of doctor's", rep.OK, status(rep, doctor.CodeCRMLogin), s.stub.LoginAttempts()-before)
			}
		})
	}
}

// In tests no gateway service is installed, whatever this computer has:
// doctor without a status file checks itself.
func init() { serviceState = func() service.State { return service.Stopped } }
