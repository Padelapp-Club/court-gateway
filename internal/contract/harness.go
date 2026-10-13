package contract

import (
	"bytes"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Test keys used by harnesses. They exist only in tests.
const (
	ReadKey      = "test-read-key-0123456789abcdef"
	BookKey      = "test-book-key-0123456789abcdef"
	OtherBookKey = "test-other-key-0123456789abcdef"
)

// KeyEnv returns the environment holding the three test keys.
func KeyEnv() map[string]string {
	return map[string]string{"TEST_READ_KEY": ReadKey, "TEST_BOOK_KEY": BookKey, "TEST_OTHER_KEY": OtherBookKey}
}

// LogBuffer captures log output safely across goroutines (every test
// harness logs into one).
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Built is one club served the way a gateway serves a club the hub
// configured, without the hub in front.
type Built struct {
	Settings *core.Settings
	Driver   driver.Driver
	Server   *core.Server
	Clubs    *core.Clubs
	Logs     *LogBuffer
}

// Handler is the club's API behind the three test keys, checked by the same
// middleware the hub puts in front of it: a test harness for the API
// itself, not a way partners reach a gateway.
func (b *Built) Handler() http.Handler {
	var partners []auth.Partner
	for name, key := range map[string]string{"reader": ReadKey, "booker": BookKey, "other": OtherBookKey} {
		p := auth.Partner{Name: name, Key: key, Scopes: []auth.Scope{auth.ScopeRead, auth.ScopeBook}, WritesPerMinute: core.DefaultWritesPerMinute}
		if name == "reader" {
			p.Scopes = p.Scopes[:1]
		}
		partners = append(partners, p)
	}
	a, err := auth.New(partners, auth.Options{WriteError: core.WriteError})
	if err != nil {
		panic(err)
	}
	api := b.Clubs.TunnelHandler()
	return core.Observe(slog.New(slog.DiscardHandler), nil, a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/clubs" { // the hub lists the clubs; here, the built ones
			type club struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			list := []club{}
			for _, id := range b.Clubs.IDs() {
				srv, _ := b.Clubs.Server(id)
				list = append(list, club{ID: id, Name: srv.Settings().ClubName})
			}
			core.WriteJSON(w, http.StatusOK, map[string][]club{"clubs": list})
			return
		}
		api.ServeHTTP(w, r.WithContext(core.WithRequestID(r.Context(), core.RequestID(r))))
	})))
}

// Build serves spec with an installation whose crm section (no hub) is
// crmYAML, the driver built through factory, logging into a buffer. hook
// may adjust the driver config (e.g. inject an HTTP client).
func Build(t *testing.T, crmYAML string, spec core.ClubSpec, env map[string]string, crmType string, factory driver.Factory, hook func(*driver.Config)) *Built {
	t.Helper()
	getenv := func(k string) string {
		if k == core.DefaultHubSecretEnv {
			return "test-hub-secret"
		}
		return env[k]
	}
	base, err := core.Load([]byte("hub: { url: https://hub.example.test }\n"+crmYAML), getenv)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s, err := base.WithClub(spec, slices.Collect(maps.Keys(spec.CRM)))
	if err != nil {
		t.Fatalf("club: %v", err)
	}
	logs := &LogBuffer{}
	log := core.NewLogger(logs, slog.LevelDebug, s.Secrets)
	s.Driver.Logger = log
	if hook != nil {
		hook(&s.Driver)
	}
	d, err := core.BuildDriver(s, map[string]driver.Factory{crmType: factory})
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	srv := core.NewServer(s, d, log)
	clubs := core.NewClubs(core.ClubsOptions{Logger: log, Now: s.Driver.Now})
	clubs.Set([]*core.Server{srv})
	return &Built{Settings: s, Driver: d, Server: srv, Clubs: clubs, Logs: logs}
}
