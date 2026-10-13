package contract

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Test keys used by harnesses. They exist only in tests.
const (
	ReadKey      = "test-read-key-0123456789abcdef"
	BookKey      = "test-book-key-0123456789abcdef"
	OtherBookKey = "test-other-key-0123456789abcdef"
)

// PartnersYAML is the partners section every harness uses.
const PartnersYAML = `partners:
  - { name: reader, key_env: TEST_READ_KEY, scopes: [read] }
  - { name: booker, key_env: TEST_BOOK_KEY, scopes: [read, book] }
  - { name: other, key_env: TEST_OTHER_KEY, scopes: [read, book] }
`

// KeyEnv returns the environment for PartnersYAML.
func KeyEnv() map[string]string {
	return map[string]string{"TEST_READ_KEY": ReadKey, "TEST_BOOK_KEY": BookKey, "TEST_OTHER_KEY": OtherBookKey}
}

// LogBuffer captures log output safely across goroutines.
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

// Built is a gateway assembled from a config text.
type Built struct {
	Settings *core.Settings
	Driver   driver.Driver
	Server   *core.Server
	Logs     *LogBuffer
}

// Build loads config text with env, builds the driver through factory and
// wraps it in a server that logs into a buffer. hook may adjust the driver
// config (e.g. inject an HTTP client) before the driver is built.
func Build(t *testing.T, configYAML string, env map[string]string, crmType string, factory driver.Factory, hook func(*driver.Config)) *Built {
	t.Helper()
	s, err := core.Load([]byte(configYAML), func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config: %v", err)
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
	srv, err := core.NewServer(s, d, log)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return &Built{Settings: s, Driver: d, Server: srv, Logs: logs}
}
