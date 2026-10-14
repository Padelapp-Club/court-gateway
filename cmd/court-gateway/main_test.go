package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/update"
)

func noEnv(string) string { return "" }

func TestVersionAndUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb, noEnv); code != 0 || !strings.HasPrefix(out.String(), "court-gateway ") {
		t.Fatalf("version: %d %q", code, out.String())
	}
	if code := run([]string{"frobnicate"}, &out, &errb, noEnv); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	// the service runs a newer version than this file: version says so
	cfg := filepath.Join(t.TempDir(), "gateway.yaml")
	getenv := func(k string) string { return map[string]string{"GATEWAY_CONFIG": cfg}[k] }
	if err := core.WriteHealthStatus(core.StateDir(cfg, getenv), core.HealthStatus{Version: "9.9.9"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"version"}, &out, &errb, getenv); code != 0 || !strings.Contains(out.String(), "служба работает на версии 9.9.9") {
		t.Fatalf("version with a running update: %d %q", code, out.String())
	}
}

func TestServeRefusesBadConfigInRussian(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(p, []byte("club: { id: x }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"serve", "-config", p}, &out, &errb, noEnv); code != 2 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errb.String(), "неизвестный параметр «club»") {
		t.Fatalf("stderr: %s", errb.String())
	}
	errb.Reset()
	if code := run([]string{"serve", "-config", filepath.Join(dir, "missing.yaml")}, &out, &errb, noEnv); code != 2 ||
		!strings.Contains(errb.String(), "не удалось прочитать файл конфигурации") {
		t.Fatalf("missing file: %d %s", code, errb.String())
	}
}

// healthcheck reads the status file the service writes next to its
// config; it needs no secret and opens no connection.
func TestHealthcheck(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(p, []byte("hub: { url: https://hub.example.test, installation: test-net, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"healthcheck", "-config", p}, &out, &errb, noEnv); code != 1 {
		t.Fatalf("no status file: %d", code)
	}
	for _, c := range []struct {
		st   core.HealthStatus
		code int
	}{
		{core.HealthStatus{Ready: true, Time: time.Now()}, 0},
		{core.HealthStatus{Ready: false, Reason: "CRM_UNREACHABLE", Time: time.Now()}, 1},
		{core.HealthStatus{Ready: true, Time: time.Now().Add(-4 * statusInterval)}, 1}, // stale: the service hangs
	} {
		if err := core.WriteHealthStatus(filepath.Join(dir, "state"), c.st); err != nil {
			t.Fatal(err)
		}
		if code := run([]string{"healthcheck", "-config", p}, &out, &errb, noEnv); code != c.code {
			t.Fatalf("%+v: %d, want %d", c.st, code, c.code)
		}
	}
}

// A state directory set only in the .env is the one serve uses: the
// healthcheck reads the status there, not a fresh one in the default.
func TestHealthcheckReadsTheStateDirOfTheDotEnv(t *testing.T) {
	dir := t.TempDir()
	cfg, state := filepath.Join(dir, "gateway.yaml"), filepath.Join(dir, "custom-state")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(core.StateDirEnv+"="+state+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(dir, "state"), state} {
		if err := core.WriteHealthStatus(d, core.HealthStatus{Ready: d != state, Reason: "CRM_UNREACHABLE", Time: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"healthcheck", "-config", cfg}, &out, &errb, noEnv); code != 1 || !strings.Contains(out.String(), "ready=false") {
		t.Fatalf("the status of the default directory was read: %d %s", code, out.String())
	}
}

// The example shipped in the repository must stay a valid config.
func TestExampleConfigIsValid(t *testing.T) {
	env := map[string]string{"CRM_PASSWORD": "example-password", "GATEWAY_HUB_SECRET": "cgs_example"}
	s, err := core.LoadFile("../../gateway.example.yaml", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drivers.Accounts[s.CRMType](s.Driver); err != nil {
		t.Fatal(err)
	}
	if s.ClubID != "" {
		t.Fatalf("the example must show an installation with no club: club=%q", s.ClubID)
	}
}

// The hub example must stay a valid hub config.
func TestHubExampleConfigIsValid(t *testing.T) {
	env := map[string]string{"HUB_OPERATOR_KEY": strings.Repeat("o", 32), "PARTNER_KEY_1": strings.Repeat("k", 32)}
	if _, err := hub.LoadFile("../../hub.example.yaml", func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
}

// Readiness is the CRM and the hub: not accepted, unreachable for longer
// than the hub's offline threshold, a club config refused, or a clock too
// far off the hub's to be corrected.
func TestReadinessIncludesTheHub(t *testing.T) {
	ok := hubState{connected: true, clockKnown: true}
	cases := []struct {
		name     string
		crmReady bool
		hub      hubState
		ready    bool
		reason   string
	}{
		{"all well", true, ok, true, ""},
		{"crm down wins", false, hubState{authFailed: true}, false, "CRM_UNREACHABLE"},
		{"secret rejected", true, hubState{authFailed: true}, false, "HUB_AUTH_FAILED"},
		{"another copy serves", true, hubState{duplicate: true}, false, "DUPLICATE_GATEWAY"},
		{"hub gone briefly", true, hubState{silentFor: time.Minute}, true, ""},
		{"hub or config not there since the start", true, hubState{connected: true, clockKnown: true, starting: true}, false, "STARTING"},
		{"hub gone for long", true, hubState{silentFor: hubOfflineAfter + time.Second}, false, "HUB_UNREACHABLE"},
		{"config refused", true, hubState{connected: true, clockKnown: true, configRefused: true}, false, "CONFIG_REFUSED"},
		{"config not pulled briefly", true, hubState{connected: true, clockKnown: true, configBehind: time.Minute}, true, ""},
		{"config not pulled for long", true, hubState{connected: true, clockKnown: true, configBehind: configPendingAfter + time.Second}, false, "CONFIG_PENDING"},
		{"clock a little off", true, hubState{connected: true, clockKnown: true, clockOffset: -10 * time.Minute}, true, ""},
		{"clock far off", true, hubState{connected: true, clockOffset: -2 * time.Hour}, false, "CLOCK_SKEW"},
		{"no signed hub time", true, hubState{connected: true}, false, "CLOCK_UNKNOWN"},
		{"hub signs with another key", true, hubState{connected: true, keyMismatch: true}, false, "HUB_KEY_MISMATCH"},
		{"signatures refused lately", true, hubState{connected: true, clockKnown: true, refusedGrew: true}, false, "SIGNATURE_REFUSED"},
		{"state directory not writable", true, hubState{connected: true, clockKnown: true, stateUnwritable: true}, false, "STATE_UNWRITABLE"},
	}
	for _, c := range cases {
		reason := ""
		if !c.crmReady {
			reason = "CRM_UNREACHABLE"
		}
		if ready, got := readiness(c.crmReady, reason, c.hub); ready != c.ready || got != c.reason {
			t.Errorf("%s: %v %q, want %v %q", c.name, ready, got, c.ready, c.reason)
		}
	}
}

// The version the installed binary hands over to runs serve with the same
// config and the same log file: under the Windows service manager the log
// file is not among the process's own arguments, and without it the
// updated version would log nowhere.
func TestHandoverKeepsTheLogFile(t *testing.T) {
	got := strings.Join(serveArgs(`C:\court-gateway\gateway.yaml`, `C:\court-gateway\court-gateway.log`), " ")
	if got != `serve -config C:\court-gateway\gateway.yaml -log-file C:\court-gateway\court-gateway.log` {
		t.Fatalf("handover arguments: %s", got)
	}
	if got := strings.Join(serveArgs("gateway.yaml", ""), " "); got != "serve -config gateway.yaml" {
		t.Fatalf("without a log file: %s", got)
	}
}

// openTracker is a log file that knows whether this process holds it open.
type openTracker struct {
	*core.RotatingFile
	open bool
}

func (f *openTracker) Write(p []byte) (int, error) { f.open = true; return f.RotatingFile.Write(p) }

func (f *openTracker) Close() error { f.open = false; return f.RotatingFile.Close() }

// While the version the installed binary started runs, the installed
// binary holds no handle on the log file — every start, also after a
// version exits to be restarted — so the version can rotate it (Windows
// cannot rename a file another process keeps open). The log is opened
// again for the lines after.
func TestLauncherClosesTheLogBeforeEveryStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "court-gateway.log")
	f := &openTracker{RotatingFile: &core.RotatingFile{Path: path, Max: 1 << 20}}
	defer f.Close()
	log := slog.New(slog.NewTextHandler(f, nil))
	versions := []string{"v2", "v2", ""}
	var started []string
	start := func(_ context.Context, p string) (int, error) {
		if f.open {
			t.Fatalf("starting %s with the log file open", p)
		}
		started = append(started, p)
		if len(started) == 1 {
			return exitRestart, nil
		}
		return 0, nil
	}
	next := func() string { v := versions[0]; versions = versions[1:]; return v }
	code, ran := launch(context.Background(), next, start, log, func() { _ = f.Close() })
	if !ran || code != 0 || len(started) != 2 {
		t.Fatalf("ran=%v code=%d started=%v", ran, code, started)
	}
	if data, _ := os.ReadFile(path); strings.Count(string(data), "starting the newest verified version") != 2 {
		t.Fatalf("log:\n%s", data)
	}
}

// After set-password the service picks the password up by itself: no
// restart. A container must be created again: it keeps the environment,
// and the old password, it was created with.
func TestPasswordHintNamesTheInstalledService(t *testing.T) {
	const recreate = "docker compose up -d --force-recreate court-gateway"
	inDocker := passwordSetHint(func(p string) bool { return p == "/.dockerenv" })
	if !strings.Contains(inDocker, recreate) || strings.Contains(inDocker, "не нужен") {
		t.Fatalf("in Docker: %q", inDocker)
	}
	if h := passwordSetHint(func(string) bool { return false }); !strings.Contains(h, "перезапуск не нужен") || strings.Contains(h, "systemctl") {
		t.Fatalf("a service: %q", h)
	}
}

// setup asks for the CRM login and the join code only: clubs, courts and
// hours come from the hub.
func TestUsageOfSetup(t *testing.T) {
	for _, line := range strings.Split(usage, "\n") {
		if strings.HasPrefix(line, "  setup ") {
			if !strings.Contains(line, "код подключения") || strings.Contains(line, "корты") || strings.Contains(line, "часы") {
				t.Fatalf("usage: %s", line)
			}
			return
		}
	}
	t.Fatal("usage has no setup line")
}

// After an update, a CRM that still rejects the password at the deadline
// is update.ErrNotRun (the update stays pending); any other failure is
// not.
func TestWaitHealthyRejectedPasswordIsUndecided(t *testing.T) {
	up := func() bool { return true }
	rejected := func(context.Context) error { return driver.Upstream(driver.UpstreamAuth, "rejected") }
	if err := waitHealthy(context.Background(), rejected, up, 50*time.Millisecond); !errors.Is(err, update.ErrNotRun) {
		t.Fatalf("rejected password: %v", err)
	}
	down := func(context.Context) error { return driver.Upstream(driver.UpstreamUnavailable, "down") }
	if err := waitHealthy(context.Background(), down, up, 50*time.Millisecond); err == nil || errors.Is(err, update.ErrNotRun) {
		t.Fatalf("CRM down: %v", err)
	}
}
