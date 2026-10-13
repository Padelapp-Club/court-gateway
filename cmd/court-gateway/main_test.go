package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
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
	if !strings.Contains(errb.String(), "club.name: параметр обязателен") {
		t.Fatalf("stderr: %s", errb.String())
	}
	errb.Reset()
	if code := run([]string{"serve", "-config", filepath.Join(dir, "missing.yaml")}, &out, &errb, noEnv); code != 2 ||
		!strings.Contains(errb.String(), "не удалось прочитать файл конфигурации") {
		t.Fatalf("missing file: %d %s", code, errb.String())
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health/ready" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"x"}`))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	var out, errb bytes.Buffer
	if code := run([]string{"healthcheck", "-addr", addr}, &out, &errb, noEnv); code != 0 {
		t.Fatalf("healthy: %d %s %s", code, out.String(), errb.String())
	}
	status = http.StatusServiceUnavailable
	if code := run([]string{"healthcheck", "-addr", addr}, &out, &errb, noEnv); code != 1 {
		t.Fatalf("unhealthy: %d", code)
	}
	// listen is read from the config; an unspecified host means loopback
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.yaml")
	_, port, _ := strings.Cut(addr, ":")
	if err := os.WriteFile(p, []byte("listen: \"0.0.0.0:"+port+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status = http.StatusOK
	if code := run([]string{"healthcheck", "-config", p}, &out, &errb, noEnv); code != 0 {
		t.Fatalf("from config: %d %s", code, errb.String())
	}
	srv.Close()
	if code := run([]string{"healthcheck", "-addr", addr}, &out, &errb, noEnv); code != 1 {
		t.Fatalf("down: %d", code)
	}
}

// The example shipped in the repository must stay a valid config.
func TestExampleConfigIsValid(t *testing.T) {
	env := map[string]string{"CRM_PASSWORD": "example-password", "GATEWAY_HUB_SECRET": "cgs_example"}
	s, err := core.LoadFile("../../gateway.example.yaml", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts[s.CRMType](s.Driver); err != nil {
		t.Fatal(err)
	}
	if s.Listen != "" || s.Hub == nil || s.ClubID != "" {
		t.Fatalf("the example must show a hub installation with no local port and no club: listen=%q club=%q", s.Listen, s.ClubID)
	}
}

// The hub example must stay a valid hub config.
func TestHubExampleConfigIsValid(t *testing.T) {
	env := map[string]string{"HUB_OPERATOR_KEY": strings.Repeat("o", 32), "PARTNER_KEY_1": strings.Repeat("k", 32)}
	if _, err := hub.LoadFile("../../hub.example.yaml", func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
}
