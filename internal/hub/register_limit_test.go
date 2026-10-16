package hub_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/hub"
)

func registerFrom(t *testing.T, h http.Handler, peer, xff, code string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/tunnel/register", strings.NewReader(`{"code":"`+code+`"}`))
	r.RemoteAddr = peer
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// Failed registrations are limited per client: anonymous junk codes from
// one address do not lock out an installation registering from another.
// X-Forwarded-For names the client only when a trusted proxy sent it.
func TestRegistrationLimitIsPerClient(t *testing.T) {
	dir := t.TempDir()
	s, err := hub.Load([]byte("state_dir: "+filepath.Join(dir, "state")+"\ntrusted_proxies: [127.0.0.1, 10.0.0.0/8]\n"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	store, err := hub.OpenStore(s.StateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := hub.NewServer(s, store, hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	const junk = "0000-0000-0000-0000"
	for i := 0; i < 20; i++ {
		if c := registerFrom(t, h, "198.51.100.7:4000", "", junk); c != http.StatusForbidden {
			t.Fatalf("junk %d: %d", i, c)
		}
	}
	if c := registerFrom(t, h, "198.51.100.7:4001", "", junk); c != http.StatusTooManyRequests {
		t.Fatalf("the guessing address is not limited: %d", c)
	}
	// the guessing address cannot dodge the limit with a header of its own
	if c := registerFrom(t, h, "198.51.100.7:4002", "203.0.113.50", junk); c != http.StatusTooManyRequests {
		t.Fatalf("X-Forwarded-For from an untrusted peer was believed: %d", c)
	}
	code := func(id string) string {
		jc, err := hub.CreateJoinCode(hub.FileState(s.StateDir), id, "X", time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return jc.Code
	}
	if c := registerFrom(t, h, "203.0.113.9:5000", "", code("club-x")); c != http.StatusOK {
		t.Fatalf("a valid code from another address: %d", c)
	}
	// behind a trusted proxy the client is the forwarded address
	for i := 0; i < 20; i++ {
		registerFrom(t, h, "127.0.0.1:6000", "198.51.100.8, 10.1.2.3", junk)
	}
	if c := registerFrom(t, h, "127.0.0.1:6000", "198.51.100.8", junk); c != http.StatusTooManyRequests {
		t.Fatalf("forwarded guessing client not limited: %d", c)
	}
	if c := registerFrom(t, h, "127.0.0.1:6000", "203.0.113.10", code("club-y")); c != http.StatusOK {
		t.Fatalf("another client behind the same proxy: %d", c)
	}
}

func TestTrustedProxiesConfig(t *testing.T) {
	if _, err := hub.Load([]byte("state_dir: /tmp/x\ntrusted_proxies: [not-an-ip]\n"), func(string) string { return "" }); err == nil ||
		!strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("bad proxy accepted: %v", err)
	}
}
