package hub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

// codesUnavailable is a state whose join codes cannot be taken.
type codesUnavailable struct{ State }

func (codesUnavailable) takeCode(string) (joinCodeRecord, error) {
	return joinCodeRecord{}, ErrStateUnavailable
}

// Registrations refused because the state is unavailable count against
// the client's limit as failed ones do: junk codes sent while the
// database is slow are limited all the same.
func TestUnavailableRegistrationsAreLimited(t *testing.T) {
	store, err := NewStore(codesUnavailable{FileState(t.TempDir())}, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(&Settings{PollWait: time.Second, RequestTimeout: time.Second, OfflineAfter: time.Minute}, store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	register := func() int {
		r := httptest.NewRequest(http.MethodPost, "/tunnel/register", strings.NewReader(`{"code":"0000-0000-0000-0000"}`))
		r.RemoteAddr = "198.51.100.7:4000"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w.Code
	}
	for i := range 20 {
		if c := register(); c != http.StatusServiceUnavailable {
			t.Fatalf("registration %d: %d, want 503", i, c)
		}
	}
	if c := register(); c != http.StatusTooManyRequests {
		t.Fatalf("after 20 refused registrations: %d, want 429", c)
	}
}

// No config is signed while the memory is stale, also for a request that
// passed authentication before it became so.
func TestNoConfigFromAStaleMemory(t *testing.T) {
	store, err := NewStore(FileState(t.TempDir()), ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInstallation("net-a", ""); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(&Settings{PollWait: time.Second, RequestTimeout: time.Second, OfflineAfter: time.Minute}, store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.stale = true
	store.mu.Unlock()
	w := httptest.NewRecorder()
	srv.handleConfig(w, httptest.NewRequest(http.MethodGet, "/", nil), GatewayRecord{Installation: "net-a"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("config from a stale memory: %d %s, want 503", w.Code, w.Body)
	}
}

// eachState runs f on a fresh state in files and, with
// COURT_GATEWAY_TEST_DATABASE_URL set (testkit.TestDatabaseEnv, which
// this package cannot import), in a Postgres database of its own.
func eachState(t *testing.T, f func(t *testing.T, st State)) {
	t.Run("files", func(t *testing.T) { f(t, FileState(t.TempDir())) })
	t.Run("postgres", func(t *testing.T) {
		base := os.Getenv("COURT_GATEWAY_TEST_DATABASE_URL")
		if base == "" {
			t.Skip("set COURT_GATEWAY_TEST_DATABASE_URL to run on Postgres")
		}
		ctx := context.Background()
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		name := "cgtest_" + strings.ToLower(rand.Text()[:12])
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
				t.Error(err)
			}
			admin.Close(ctx)
		})
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		st, err := openPostgres(u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		f(t, st)
	})
}

// pausedTake is a state that holds a redemption right after its code is
// taken, until released.
type pausedTake struct {
	State
	taken, release chan struct{}
}

func (p pausedTake) takeCode(hash string) (joinCodeRecord, error) {
	rec, err := p.State.takeCode(hash)
	if err == nil {
		close(p.taken)
		<-p.release
	}
	return rec, err
}

// A redemption that took its code before the operator deleted the
// installation does not bring it back; a code made after the deletion
// (by the command line) makes the installation anew.
func TestDeletionDuringRedemption(t *testing.T) {
	eachState(t, func(t *testing.T, st State) {
		paused := pausedTake{State: st, taken: make(chan struct{}), release: make(chan struct{})}
		key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
		s, err := NewStore(paused, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateInstallation("net-d", "Сеть"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.PutClub("net-d", core.ClubSpec{ID: "club-d", Name: "Клуб"}); err != nil {
			t.Fatal(err)
		}
		jc, err := s.CreateJoinCode("net-d", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := s.Redeem(jc.Code)
			done <- err
		}()
		<-paused.taken
		if err := s.DeleteInstallation("net-d"); err != nil {
			t.Fatal(err)
		}
		close(paused.release)
		if err := <-done; !errors.Is(err, ErrJoinCode) {
			t.Fatalf("a redemption begun before the deletion: %v, want ErrJoinCode", err)
		}
		if _, ok := s.Installation("net-d"); ok {
			t.Fatal("the deleted installation is back in memory")
		}
		again, err := NewStore(st, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := again.Installation("net-d"); ok {
			t.Fatal("the deleted installation is back in the state")
		}

		jc, err = CreateJoinCode(st, "net-d", "Новая сеть", time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if g, _, err := again.Redeem(jc.Code); err != nil || g.Installation != "net-d" {
			t.Fatalf("a code made after the deletion: %+v %v", g, err)
		}
		if in, ok := again.Installation("net-d"); !ok || in.Name != "Новая сеть" || len(in.Clubs) != 0 {
			t.Fatalf("the installation made anew: %+v %v", in, ok)
		}
	})
}

// No change is made from a stale memory, also by a call that found it
// fresh before another one made it stale: a reload reads the state with
// no change running.
func TestNoChangeFromAStaleMemory(t *testing.T) {
	s, err := NewStore(FileState(t.TempDir()), ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstallation("net-a", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutClub("net-a", core.ClubSpec{ID: "club-a", Name: "Клуб"}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
	for name, change := range map[string]func() error{
		"put club": func() error {
			_, _, err := s.putClub("net-a", core.ClubSpec{ID: "club-b", Name: "Клуб"})
			return err
		},
		"delete club":         func() error { _, err := s.deleteClub("net-a", "club-a"); return err },
		"create installation": func() error { _, err := s.createInstallation("net-b", ""); return err },
		"delete installation": func() error { return s.deleteInstallationLocked("net-a") },
		"register":            func() error { _, _, err := s.register(joinCodeRecord{Installation: "net-a"}, 0); return err },
	} {
		if err := change(); !errors.Is(err, ErrStateUnavailable) {
			t.Errorf("%s from a stale memory: %v, want ErrStateUnavailable", name, err)
		}
	}
	installs, gateways, err := s.st.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 1 || installs[0].Version != 1 || len(installs[0].Clubs) != 1 || len(gateways) != 0 {
		t.Fatalf("the state was changed: %+v %+v", installs, gateways)
	}
}
