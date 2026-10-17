package core

import (
	"bytes"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// The secrets a crm.type declares reach its driver by name and never a
// log line; an undeclared variable reaches neither.
func TestDeclaredCRMSecretsReachTheDriverAndStayOutOfLogs(t *testing.T) {
	driver.RegisterSetup("secrettest", driver.SetupSpec{Secrets: []driver.SetupSecret{{Env: "SECRETTEST_TOKEN"}, {Env: PasswordEnv}}})
	data := []byte("hub: { url: https://hub.example.test, installation: test-net, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= }\ncrm:\n  type: secrettest\n")
	s, err := Load(data, env(map[string]string{DefaultHubSecretEnv: "cgs_test", "SECRETTEST_TOKEN": "tok-0123456789", PasswordEnv: "pw-secret", "OTHER_SECRET": "not-mine"}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SECRETTEST_TOKEN": "tok-0123456789", PasswordEnv: "pw-secret"}
	if !maps.Equal(s.Driver.Secrets, want) || s.Driver.Password != "pw-secret" {
		t.Fatalf("driver secrets = %v, password %q", s.Driver.Secrets, s.Driver.Password)
	}
	if !slices.Contains(s.Secrets, "tok-0123456789") || slices.Contains(s.Secrets, "not-mine") {
		t.Fatalf("redacted = %v", s.Secrets)
	}
	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelDebug, s.Secrets).Info("login", "token", "tok-0123456789", "detail", "Bearer tok-0123456789")
	if strings.Contains(buf.String(), "tok-0123456789") {
		t.Fatalf("the declared secret reached a log line: %s", buf.String())
	}
}
