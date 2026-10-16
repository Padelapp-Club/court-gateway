package hub

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
