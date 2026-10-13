package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hubGateway = `club: { id: c1, name: "Клуб" }
hub: { url: https://hub.example.test }
crm: { type: demo }
venue: { timezone: Europe/Moscow, slot_minutes: 30, hours: { mon-sun: "07:00-23:00" } }
courts: [ { id: court-1, name: "Корт 1" } ]
`

func TestHubModeClosesThePortAndNeedsNoPartners(t *testing.T) {
	s, err := Load([]byte(hubGateway), func(k string) string {
		if k == DefaultHubSecretEnv {
			return "cgs_test-secret"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Listen != "" {
		t.Fatalf("with a hub the local port must stay closed by default, got %q", s.Listen)
	}
	if s.Hub == nil || s.Hub.Polls != DefaultHubPolls || s.Hub.Secret != "cgs_test-secret" {
		t.Fatalf("hub: %+v", s.Hub)
	}
	if !strings.Contains(strings.Join(s.Secrets, " "), "cgs_test-secret") {
		t.Fatal("the hub secret is not redacted from logs")
	}
	if !s.Update.Enabled {
		t.Fatal("updates are on by default")
	}
	// explicit listen still works (LAN / debugging)
	s, err = Load([]byte(hubGateway+"listen: \"127.0.0.1:9090\"\n"), func(k string) string { return "cgs_x" })
	if err != nil || s.Listen != "127.0.0.1:9090" {
		t.Fatalf("explicit listen: %v %+v", err, s)
	}
}

func TestHubConfigErrors(t *testing.T) {
	_, err := Load([]byte(hubGateway), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_HUB_SECRET") {
		t.Fatalf("missing secret: %v", err)
	}
	bad := strings.Replace(hubGateway, "https://hub.example.test", "http://hub.example.test", 1)
	_, err = Load([]byte(bad), func(string) string { return "x" })
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("plain http hub: %v", err)
	}
	// without a hub, partners are still required
	noHub := strings.Replace(hubGateway, "hub: { url: https://hub.example.test }\n", "", 1)
	if _, err := Load([]byte(noHub), func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "partners") {
		t.Fatalf("no hub, no partners: %v", err)
	}
}

func TestDotEnv(t *testing.T) {
	vals, err := ParseDotEnv([]byte("# comment\n\nexport A=1\nB = \"two words\"\nC='x\"y'\nD=a#b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if vals["A"] != "1" || vals["B"] != "two words" || vals["C"] != `x"y` || vals["D"] != "a#b" {
		t.Fatalf("%q", vals)
	}
	_, err = ParseDotEnv([]byte("GOOD=1\nthis line holds a secret value\n"))
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "строка 2") {
		t.Fatalf("error must name the line, not echo it: %v", err)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gateway.yaml")
	_ = os.WriteFile(filepath.Join(dir, ".env"), []byte("X=from-file\nY=from-file\n"), 0o600)
	get, err := EnvWithDotEnv(cfg, func(k string) string {
		if k == "Y" {
			return "from-process"
		}
		return ""
	})
	if err != nil || get("X") != "from-file" || get("Y") != "from-process" || get("Z") != "" {
		t.Fatalf("precedence: %v %q %q", err, get("X"), get("Y"))
	}
}

func TestStatePathAgreesWithLoadFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gateway.yaml")
	_ = os.WriteFile(cfg, []byte(hubGateway), 0o644)
	get := func(k string) string { return map[string]string{DefaultHubSecretEnv: "cgs_x"}[k] }
	s, err := LoadFile(cfg, get)
	if err != nil {
		t.Fatal(err)
	}
	stateDir, listen, hub := StatePath(cfg, get)
	if stateDir != s.StateDir || stateDir != filepath.Join(dir, "state") || listen != "" || !hub {
		t.Fatalf("StatePath %q %q %v vs %q", stateDir, listen, hub, s.StateDir)
	}
	get2 := func(k string) string {
		return map[string]string{DefaultHubSecretEnv: "cgs_x", StateDirEnv: "/elsewhere"}[k]
	}
	s, _ = LoadFile(cfg, get2)
	if d, _, _ := StatePath(cfg, get2); d != "/elsewhere" || s.StateDir != "/elsewhere" {
		t.Fatalf("env override: %q %q", d, s.StateDir)
	}
}

func TestHealthStatusRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := WriteHealthStatus(dir, HealthStatus{Ready: false, Reason: "CRM_UNREACHABLE", Hub: "connected", Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	st, err := ReadHealthStatus(dir)
	if err != nil || st.Ready || st.Reason != "CRM_UNREACHABLE" || st.Hub != "connected" {
		t.Fatalf("%+v %v", st, err)
	}
}
