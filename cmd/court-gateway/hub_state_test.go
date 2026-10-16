package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// The hub's commands work on either state, the key file made once by
// keygen: hub key, join-code create, gateway revoke.
func TestHubCommandsOnEitherState(t *testing.T) {
	for _, backend := range []string{"files", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			key := filepath.Join(dir, "hub-signing.key")
			cfg := "signing_key_file: " + key + "\n"
			if backend == "files" {
				cfg += "state_dir: " + filepath.Join(dir, "state") + "\n"
			} else {
				url := testkit.TestDatabase(t)
				if url == "" {
					t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
				}
				cfg += "database_url_env: HUB_DATABASE_URL\n"
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("HUB_DATABASE_URL="+url+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			hubCfg := filepath.Join(dir, "hub.yaml")
			if err := os.WriteFile(hubCfg, []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := func(args ...string) (int, string, string) {
				var out, errOut bytes.Buffer
				rc := run(append([]string{"hub"}, args...), &out, &errOut, env{}.get)
				return rc, out.String(), errOut.String()
			}

			if rc, _, errOut := cmd("key", "-config", hubCfg); rc == 0 || !strings.Contains(errOut, "hub keygen -out "+key) {
				t.Fatalf("hub key without the key file: rc %d %s", rc, errOut)
			}
			if rc, _, errOut := cmd("-config", hubCfg); rc != 2 || !strings.Contains(errOut, "signing_key_file") {
				t.Fatalf("the hub started without its key file: rc %d %s", rc, errOut)
			}
			rc, made, _ := cmd("keygen", "-out", key)
			if rc != 0 || !strings.HasPrefix(made, "key: ") {
				t.Fatalf("keygen: rc %d %q", rc, made)
			}
			if rc, _, _ := cmd("keygen", "-out", key); rc == 0 {
				t.Fatal("keygen replaced an existing key")
			}
			if rc, shown, _ := cmd("key", "-config", hubCfg); rc != 0 || shown != made {
				t.Fatalf("hub key: rc %d %q, keygen printed %q", rc, shown, made)
			}
			if runtime.GOOS != "windows" {
				if err := os.Chmod(key, 0o644); err != nil {
					t.Fatal(err)
				}
				if rc, _, errOut := cmd("-config", hubCfg); rc != 2 || !strings.Contains(errOut, "chmod 600") {
					t.Fatalf("the hub started with a key file others may read: rc %d %s", rc, errOut)
				}
				if err := os.Chmod(key, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			rc, out, errOut := cmd("join-code", "create", "-config", hubCfg, "--installation", "net-c", "--name", "Сеть C")
			code := regexp.MustCompile(`установки net-c: (\S+)`).FindStringSubmatch(out)
			if rc != 0 || code == nil {
				t.Fatalf("join-code create: rc %d %s %s", rc, out, errOut)
			}
			s, err := loadHub(hubCfg, env{}.get)
			if err != nil {
				t.Fatal(err)
			}
			store, err := hub.Open(s, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			_, secret, err := store.Redeem(code[1])
			if err != nil {
				t.Fatal(err)
			}
			if rc, _, errOut := cmd("gateway", "revoke", "-config", hubCfg, "--installation", "net-c"); rc != 0 {
				t.Fatalf("revoke: rc %d %s", rc, errOut)
			}
			if _, err := store.Authenticate(secret); !errors.Is(err, hub.ErrNotRegistered) {
				t.Fatalf("a revoked gateway: %v", err)
			}
			if rc, _, _ := cmd("gateway", "revoke", "-config", hubCfg, "--installation", "net-c"); rc != 1 {
				t.Fatalf("a second revoke: rc %d", rc)
			}
		})
	}
}

func TestHubKeygenNeedsOut(t *testing.T) {
	if rc := run([]string{"hub", "keygen"}, io.Discard, io.Discard, env{}.get); rc != 2 {
		t.Fatalf("rc %d", rc)
	}
}
