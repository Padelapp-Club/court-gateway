package tunnel

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackoffGrowsWithJitterAndCaps(t *testing.T) {
	c, err := NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", HubKey: testPub, Installation: "inst", Handler: http.NotFoundHandler(),
		MinBackoff: 100 * time.Millisecond, MaxBackoff: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for failures := 1; failures < 20; failures++ {
		ceil := 100 * time.Millisecond << (failures - 1)
		if ceil > 2*time.Second || ceil <= 0 {
			ceil = 2 * time.Second
		}
		for i := 0; i < 50; i++ {
			d := c.backoff(failures, false)
			if d < 50*time.Millisecond || d >= 50*time.Millisecond+ceil {
				t.Fatalf("failures=%d: %v outside [50ms, %v)", failures, d, 50*time.Millisecond+ceil)
			}
		}
	}
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[c.backoff(5, false)] = true
	}
	if len(seen) < 5 {
		t.Fatal("no jitter: reconnects of a fleet would be synchronised")
	}
	if d := c.backoff(1, true); d < 50*time.Millisecond {
		t.Fatalf("unauthorized backoff: %v", d)
	}
}

func TestExecuteBoundsTheResponse(t *testing.T) {
	big := strings.Repeat("x", MaxResponseBody+1)
	c, _ := NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", HubKey: testPub, Installation: "inst", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	})})
	r := c.Execute(context.Background(), signed(Request{Method: http.MethodGet, Path: "/v1/clubs", RequestID: "rq-1"}))
	if r.Status != http.StatusBadGateway || !strings.Contains(string(r.Body), "RESPONSE_TOO_LARGE") || len(r.Body) > 1000 {
		t.Fatalf("%d %.100s", r.Status, r.Body)
	}
	// Authorization is never forwarded into the gateway
	var sawAuth string
	c, _ = NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", HubKey: testPub, Installation: "inst", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
	})})
	c.Execute(context.Background(), signed(Request{Method: http.MethodGet, Path: "/v1/clubs", Header: map[string]string{"Authorization": "Bearer partner-key"}}))
	if sawAuth != "" {
		t.Fatal("a partner key reached the gateway")
	}
}

// The allow-list is checked by the tunnel itself, not left to the local
// handler: here the handler would serve any path, so only the allow-list
// stands between the hub and everything else.
func TestAllowListIndependentOfHandler(t *testing.T) {
	c, _ := NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", HubKey: testPub, Installation: "inst", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("served " + r.URL.Path))
	})})
	for _, p := range []string{"/hub/status", "/debug/pprof/", "/", "", "/v1/../etc", "/v1/clubs/%2e%2e", "/healthz", "/health/ready"} {
		if r := c.Execute(context.Background(), signed(Request{Method: http.MethodGet, Path: p})); r.Status != http.StatusNotFound {
			t.Fatalf("%q reached the handler: %d %s", p, r.Status, r.Body)
		}
	}
	if r := c.Execute(context.Background(), signed(Request{Method: "CONNECT", Path: "/v1/clubs"})); r.Status != http.StatusNotFound {
		t.Fatalf("CONNECT reached the handler: %d", r.Status)
	}
	// the gateway API for the hub's operator: POST only
	if r := c.Execute(context.Background(), signed(Request{Method: http.MethodGet, Path: PathDiscover})); r.Status != http.StatusNotFound {
		t.Fatalf("GET %s reached the handler: %d", PathDiscover, r.Status)
	}
	if r := c.Execute(context.Background(), signed(Request{Method: http.MethodPost, Path: PathDiscover})); r.Status != http.StatusOK {
		t.Fatalf("POST %s blocked: %d", PathDiscover, r.Status)
	}
	// never for a partner, even one the hub signed with every scope
	if r := c.Execute(context.Background(), signed(Request{Method: http.MethodPost, Path: PathDiscover, Partner: "booker", Scopes: []string{"read", "book"}})); r.Status != http.StatusNotFound {
		t.Fatalf("a partner reached %s: %d", PathDiscover, r.Status)
	}
	for _, p := range []string{"/gateway/other", "/gateway/discover/../../hub/status"} {
		if r := c.Execute(context.Background(), signed(Request{Method: http.MethodPost, Path: p})); r.Status != http.StatusNotFound {
			t.Fatalf("%q reached the handler: %d", p, r.Status)
		}
	}
	for _, p := range []string{"/v1/clubs", "/v1/clubs/a/courts"} {
		if r := c.Execute(context.Background(), signed(Request{Method: http.MethodGet, Path: p})); r.Status != http.StatusOK {
			t.Fatalf("%q blocked: %d", p, r.Status)
		}
	}
}

// The hub's timeout reaches the handler, cut to two minutes: a discovery
// with a long timeout keeps two minutes, not a default 30 seconds.
func TestExecuteClampsTheTimeout(t *testing.T) {
	var left time.Duration
	c, _ := NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", HubKey: testPub, Installation: "inst", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dl, _ := r.Context().Deadline()
		left = time.Until(dl)
	})})
	for _, tc := range []struct {
		ms       int
		min, max time.Duration
	}{
		{int((10 * time.Minute).Milliseconds()), 119 * time.Second, MaxRequestTimeout},
		{int((150 * time.Second).Milliseconds()), 119 * time.Second, MaxRequestTimeout},
		{int((45 * time.Second).Milliseconds()), 44 * time.Second, 45 * time.Second},
		{1, 900 * time.Millisecond, time.Second},
		{0, 29 * time.Second, 30 * time.Second},
	} {
		c.Execute(context.Background(), signed(Request{Method: http.MethodGet, Path: "/v1/clubs", TimeoutMS: tc.ms}))
		if left < tc.min || left > tc.max {
			t.Errorf("timeout_ms %d: the handler got %s", tc.ms, left)
		}
	}
}

// hub.ca_file pins the hub's CA: only its certificates are trusted, not
// the system roots next to them (which would let a TLS-inspecting proxy
// trusted by the machine stand in for the hub). Without it, the system
// roots are used.
func TestCAFilePinsTheHubCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "hub-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadRoots(caFile)
	if err != nil {
		t.Fatal(err)
	}
	only := x509.NewCertPool()
	only.AddCert(srv.Certificate())
	if !pool.Equal(only) {
		t.Fatal("hub.ca_file trusts more than its own certificates")
	}
	if pool, err := LoadRoots(""); pool != nil || err != nil {
		t.Fatalf("without hub.ca_file: %v %v", pool, err)
	}
}
