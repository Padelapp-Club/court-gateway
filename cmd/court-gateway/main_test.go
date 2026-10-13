package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
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
	if err := os.WriteFile(p, []byte("hub: { url: https://hub.example.test }\n"), 0o600); err != nil {
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
// than the hub's offline threshold, or a club config refused.
func TestReadinessIncludesTheHub(t *testing.T) {
	ok := hubState{connected: true}
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
		{"hub gone briefly", true, hubState{silentFor: time.Minute}, true, ""},
		{"hub gone for long", true, hubState{silentFor: hubOfflineAfter + time.Second}, false, "HUB_UNREACHABLE"},
		{"config refused", true, hubState{connected: true, configRefused: true}, false, "CONFIG_REFUSED"},
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
