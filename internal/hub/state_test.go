package hub_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// forEachState runs f on the state in files and, with
// testkit.TestDatabaseEnv set, in a Postgres database of its own; cfg
// names the fresh state the way hub.yaml does.
func forEachState(t *testing.T, f func(t *testing.T, cfg *hub.Settings)) {
	t.Run("files", func(t *testing.T) {
		f(t, &hub.Settings{StateDir: filepath.Join(t.TempDir(), "state")})
	})
	t.Run("postgres", func(t *testing.T) {
		url := testkit.TestDatabase(t)
		if url == "" {
			t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
		}
		f(t, postgresSettings(t, url))
	})
}

func postgresSettings(t *testing.T, url string) *hub.Settings {
	t.Helper()
	key := filepath.Join(t.TempDir(), "hub-signing.key")
	if _, err := hub.WriteSigningKey(key); err != nil {
		t.Fatal(err)
	}
	return &hub.Settings{DatabaseURL: url, SigningKeyFile: key}
}

func openStore(t *testing.T, cfg *hub.Settings) *hub.Store {
	t.Helper()
	s, err := hub.Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// openState opens the state as the command line does.
func openState(t *testing.T, cfg *hub.Settings) hub.State {
	t.Helper()
	st, err := hub.OpenState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// One code redeemed at once many times over registers exactly one
// gateway: by one hub, and on Postgres by several hubs on one database.
// (A state_dir serves one hub: two removals of one file may both succeed,
// as on macOS APFS.)
func TestStateRedeemsACodeOnce(t *testing.T) {
	forEachState(t, func(t *testing.T, cfg *hub.Settings) {
		hubs := 1
		if cfg.DatabaseURL != "" {
			hubs = 4
		}
		var stores []*hub.Store
		for range hubs {
			stores = append(stores, openStore(t, cfg))
		}
		cli := openState(t, cfg)
		for round := range 25 {
			jc, err := hub.CreateJoinCode(cli, "race-net", "", time.Hour, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var wins, mu, wg = 0, sync.Mutex{}, sync.WaitGroup{}
			start := make(chan struct{})
			for i := range 16 {
				wg.Go(func() {
					<-start
					_, _, err := stores[i%len(stores)].Redeem(jc.Code)
					switch {
					case err == nil:
						mu.Lock()
						wins++
						mu.Unlock()
					case !errors.Is(err, hub.ErrJoinCode):
						t.Errorf("redeem: %v", err)
					}
				})
			}
			close(start)
			wg.Wait()
			if wins != 1 {
				t.Fatalf("round %d: one code redeemed %d times", round, wins)
			}
		}
	})
}

// The config version of an installation only grows, also across a
// restart of the hub, and the clubs, routes and registrations stay.
func TestStateSurvivesRestart(t *testing.T) {
	forEachState(t, func(t *testing.T, cfg *hub.Settings) {
		s := openStore(t, cfg)
		if _, err := s.CreateInstallation("net-a", "Сеть А"); err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{"club-1", "club-2"} {
			if _, _, err := s.PutClub("net-a", demoClub(c, "studio-"+c, "Корт 1")); err != nil {
				t.Fatal(err)
			}
		}
		if v, err := s.DeleteClub("net-a", "club-2"); err != nil || v != 3 {
			t.Fatalf("delete club: %d %v", v, err)
		}
		jc, err := hub.CreateJoinCode(openState(t, cfg), "net-a", "", time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		g, secret, err := s.Redeem(jc.Code)
		if err != nil || g.Installation != "net-a" || g.Name != "Сеть А" {
			t.Fatalf("redeem: %+v %v", g, err)
		}
		pending, err := s.CreateJoinCode("net-a", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		key := s.HubKey()
		s.Close()

		again := openStore(t, cfg)
		if v := again.ConfigVersion("net-a"); v != 3 {
			t.Fatalf("version after a restart: %d, want 3", v)
		}
		if g, err := again.Authenticate(secret); err != nil || g.Installation != "net-a" {
			t.Fatalf("a gateway registered before the restart: %+v %v", g, err)
		}
		if inst, ok := again.Route("club-1"); !ok || inst != "net-a" {
			t.Fatalf("route: %q %v", inst, ok)
		}
		if _, ok := again.Route("club-2"); ok {
			t.Fatal("a deleted club is routed after a restart")
		}
		if refs := again.ClubRefs(); len(refs) != 1 || refs[0].ID != "club-1" || refs[0].Name != "Клуб club-1" || refs[0].Installation != "net-a" {
			t.Fatalf("club refs: %+v", refs)
		}
		if !again.HubKey().Equal(key) {
			t.Fatal("the signing key changed across a restart")
		}
		if _, err := again.CreateInstallation("net-a", ""); !errors.Is(err, hub.ErrInstallationExists) {
			t.Fatalf("create again: %v", err)
		}
		if _, v, err := again.PutClub("net-a", demoClub("club-1", "studio-club-1", "Корт 2")); err != nil || v != 4 {
			t.Fatalf("a change after the restart: %d %v, want 4", v, err)
		}
		// what other installations serve is known after the restart too
		if _, err := again.CreateInstallation("net-b", ""); err != nil {
			t.Fatal(err)
		}
		if _, _, err := again.PutClub("net-b", demoClub("club-x", "STUDIO-club-1")); !errors.Is(err, hub.ErrStudioTaken) {
			t.Fatalf("a taken studio: %v", err)
		}
		if _, _, err := again.PutClub("net-b", demoClub("club-1", "studio-b")); !errors.Is(err, hub.ErrClubTaken) {
			t.Fatalf("a taken club: %v", err)
		}

		// a new code replaces the gateway's secret; the old one is out
		_, secret2, err := again.Redeem(pending.Code)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := again.Authenticate(secret); !errors.Is(err, hub.ErrNotRegistered) {
			t.Fatalf("the replaced secret: %v", err)
		}
		// a revocation from the command line is seen by the running hub
		if err := hub.RevokeGateway(openState(t, cfg), "net-a"); err != nil {
			t.Fatal(err)
		}
		if err := hub.RevokeGateway(openState(t, cfg), "net-a"); !errors.Is(err, hub.ErrNotRegistered) {
			t.Fatalf("a second revocation: %v", err)
		}
		if _, err := again.Authenticate(secret2); !errors.Is(err, hub.ErrNotRegistered) {
			t.Fatalf("a revoked secret: %v", err)
		}
		if gs := again.Gateways(); len(gs) != 0 {
			t.Fatalf("gateways after the revocation: %+v", gs)
		}

		// deleting the installation takes its clubs and pending codes
		code, err := again.CreateJoinCode("net-a", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := again.DeleteInstallation("net-a"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := again.Redeem(code.Code); !errors.Is(err, hub.ErrJoinCode) {
			t.Fatalf("a code of a deleted installation: %v", err)
		}
		third := openStore(t, cfg)
		if _, ok := third.Installation("net-a"); ok {
			t.Fatal("a deleted installation is back after a restart")
		}
		if _, ok := third.Route("club-1"); ok {
			t.Fatal("a deleted installation's club is routed after a restart")
		}
	})
}

// A code made by the command line creates its installation at
// redemption; an expired one does not.
func TestStateCodeOfTheCommandLine(t *testing.T) {
	forEachState(t, func(t *testing.T, cfg *hub.Settings) {
		now := time.Now()
		s, err := hub.Open(cfg, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		cli := openState(t, cfg)
		old, err := hub.CreateJoinCode(cli, "net-old", "", time.Minute, now.Add(-2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Redeem(old.Code); !errors.Is(err, hub.ErrJoinCode) {
			t.Fatalf("an expired code: %v", err)
		}
		jc, err := hub.CreateJoinCode(cli, "net-cli", "Сеть из консоли", time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Redeem(strings.ToLower(strings.ReplaceAll(jc.Code, "-", ""))); err != nil {
			t.Fatal(err)
		}
		if in, ok := s.Installation("net-cli"); !ok || in.Name != "Сеть из консоли" || in.Version != 0 {
			t.Fatalf("installation: %+v %v", in, ok)
		}
		if _, ok := s.Installation("net-old"); ok {
			t.Fatal("an expired code created its installation")
		}
	})
}

// The signing key file is only read: a missing one or one others may
// read stops the hub, before it touches the state.
func TestSigningKeyFile(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "hub-signing.key")
	cfg := &hub.Settings{StateDir: filepath.Join(dir, "state"), SigningKeyFile: key}
	if _, err := hub.Open(cfg, nil); err == nil || !strings.Contains(err.Error(), "hub keygen") {
		t.Fatalf("a missing key file: %v", err)
	}
	pub, err := hub.WriteSigningKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.WriteSigningKey(key); err == nil {
		t.Fatal("keygen replaced an existing key")
	}
	s, err := hub.Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if !s.HubKey().Equal(pub) {
		t.Fatal("the hub signs with another key than the file's")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "hub-signing.key")); !os.IsNotExist(err) {
		t.Fatalf("a key was made in state_dir besides the key file: %v", err)
	}
	if runtime.GOOS != "windows" {
		for _, mode := range []os.FileMode{0o640, 0o604} {
			if err := os.Chmod(key, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := hub.Open(cfg, nil); err == nil || !strings.Contains(err.Error(), "chmod 600") {
				t.Fatalf("a key file of mode %v: %v", mode, err)
			}
		}
	}
}

func TestDatabaseConfig(t *testing.T) {
	env := map[string]string{"HUB_DATABASE_URL": "postgres://hub:secret-pw@db.example.test/hub"}
	getenv := func(k string) string { return env[k] }
	s, err := hub.Load([]byte("database_url_env: HUB_DATABASE_URL\nsigning_key_file: /etc/hub/signing.key\n"), getenv)
	if err != nil || s.DatabaseURL != env["HUB_DATABASE_URL"] || s.StateDir != "" || s.SigningKeyFile != "/etc/hub/signing.key" {
		t.Fatalf("%+v %v", s, err)
	}
	// the password alone is hidden too, in every form a connection string
	// may give it: an error may quote it without the rest of the string
	for _, dsn := range []string{
		"postgres://hub:secret-pw@db.example.test/hub",
		"host=db.example.test user=hub password=secret-pw dbname=hub",
		"postgres://db.example.test/hub?user=hub&password=secret-pw",
	} {
		env["HUB_DATABASE_URL"] = dsn
		s, err := hub.Load([]byte("database_url_env: HUB_DATABASE_URL\nsigning_key_file: /etc/hub/signing.key\n"), getenv)
		if err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		core.NewLogger(&logs, slog.LevelInfo, s.Secrets).Error("database", "error", "password authentication failed: secret-pw")
		if strings.Contains(logs.String(), "secret-pw") {
			t.Errorf("%s: the password is in the log: %s", dsn, logs.String())
		}
	}
	for yaml, want := range map[string]string{
		"database_url_env: HUB_DATABASE_URL\n":                                      "signing_key_file",
		"database_url_env: HUB_DATABASE_URL\nstate_dir: /x\nsigning_key_file: /k\n": "что-то одно",
		"database_url_env: NOT_SET\nsigning_key_file: /k\n":                         "NOT_SET",
		"listen: \":8443\"\n": "database_url_env",
	} {
		if _, err := hub.Load([]byte(yaml), getenv); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want an error naming %s", yaml, err, want)
		}
	}
}

// A database that stops answering is not a deregistration and loses no
// write: gateways and the operator get 503 and readiness fails while it
// is away, and the hub goes on where it was once it is back.
func TestDatabaseOutage(t *testing.T) {
	base := testkit.TestDatabase(t)
	if base == "" {
		t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	proxy := testkit.NewProxy(t, u.Host)
	u.Host = strings.TrimPrefix(proxy.URL(), "https://")
	cfg := postgresSettings(t, u.String())
	s := openStore(t, cfg)
	if _, err := s.CreateInstallation("net-o", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutClub("net-o", demoClub("club-o", "studio-o", "Корт 1")); err != nil {
		t.Fatal(err)
	}
	jc, err := s.CreateJoinCode("net-o", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := s.Redeem(jc.Code)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := hub.NewServer(&hub.Settings{PollWait: time.Second, RequestTimeout: time.Second, OfflineAfter: time.Minute}, s, hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	proxy.Break()
	if _, err := s.Authenticate(secret); !errors.Is(err, hub.ErrStateUnavailable) {
		t.Fatalf("Authenticate = %v, want ErrStateUnavailable", err)
	}
	if code := get(tunnel.PathWhoami); code != http.StatusServiceUnavailable {
		t.Fatalf("whoami: %d, want 503", code)
	}
	if code := get("/health/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("ready: %d, want 503", code)
	}
	if _, _, err := s.PutClub("net-o", demoClub("club-p", "studio-p", "Корт 1")); !errors.Is(err, hub.ErrStateUnavailable) {
		t.Fatalf("PutClub = %v, want ErrStateUnavailable", err)
	}
	if _, err := s.CreateJoinCode("net-o", time.Hour); !errors.Is(err, hub.ErrStateUnavailable) {
		t.Fatalf("CreateJoinCode = %v, want ErrStateUnavailable", err)
	}
	if v := s.ConfigVersion("net-o"); v != 1 {
		t.Fatalf("a failed write changed the version: %d", v)
	}
	if _, ok := s.Route("club-p"); ok {
		t.Fatal("a failed write is served")
	}
	if gs := s.Gateways(); len(gs) != 1 {
		t.Fatalf("gateways while the database is away: %+v", gs)
	}

	proxy.Heal()
	if g, err := s.Authenticate(secret); err != nil || g.Installation != "net-o" {
		t.Fatalf("after the database is back: %+v %v", g, err)
	}
	if code := get("/health/ready"); code != http.StatusOK {
		t.Fatalf("ready: %d", code)
	}
	if _, v, err := s.PutClub("net-o", demoClub("club-p", "studio-p", "Корт 1")); err != nil || v != 2 {
		t.Fatalf("PutClub after the database is back: %d %v", v, err)
	}
	s.Close()
	again := openStore(t, cfg)
	if v := again.ConfigVersion("net-o"); v != 2 {
		t.Fatalf("version after a restart: %d", v)
	}
}
