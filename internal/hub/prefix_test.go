package hub_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// prefixProxy stands in for a reverse proxy that publishes the hub under a
// path prefix and strips it (nginx: location /prefix/ { proxy_pass
// http://hub:port/; }), terminating TLS itself. It records every path the
// hub received and every request that arrived without the prefix.
type prefixProxy struct {
	srv    *httptest.Server
	prefix string

	mu         sync.Mutex
	upstream   []string
	unprefixed []string
}

func newPrefixProxy(t *testing.T, prefix, hubAddr string) *prefixProxy {
	t.Helper()
	target := &url.URL{Scheme: "http", Host: hubAddr}
	p := &prefixProxy{prefix: prefix}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.SetXForwarded()
		},
		FlushInterval: -1, // proxy_buffering off
	}
	p.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, prefix+"/")
		if !ok {
			p.mu.Lock()
			p.unprefixed = append(p.unprefixed, r.Method+" "+r.URL.Path)
			p.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		r.URL.Path, r.URL.RawPath = "/"+rest, ""
		p.mu.Lock()
		p.upstream = append(p.upstream, r.Method+" "+r.URL.Path)
		p.mu.Unlock()
		rp.ServeHTTP(w, r)
	}))
	p.srv.EnableHTTP2 = true
	p.srv.StartTLS()
	t.Cleanup(p.srv.Close)
	return p
}

// URL is the hub's public address as gateways and partners see it.
func (p *prefixProxy) URL() string { return p.srv.URL + p.prefix }

func (p *prefixProxy) transport() *http.Transport {
	roots := x509.NewCertPool()
	roots.AddCert(p.srv.Certificate())
	return tunnel.NewTransport(roots)
}

func (p *prefixProxy) seen() (upstream, unprefixed []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.upstream...), append([]string(nil), p.unprefixed...)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// The hub listens on plain HTTP (no tls section) behind a TLS-terminating
// proxy that publishes it under a path prefix. A gateway configured with
// the prefixed address registers, connects, reports heartbeats and serves
// partner calls, and every request it makes carries the prefix.
func TestHubBehindPathPrefix(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	statusDir := filepath.Join(dir, "status")
	cfg := "listen: " + addr + "\n" +
		"public_url: https://hub.example.test/gateway-hub\n" +
		"state_dir: " + filepath.Join(dir, "state") + "\n" +
		"status_dir: " + statusDir + "\n" +
		"poll_wait: 1s\nrequest_timeout: 3s\noperator_key_env: HUB_OPERATOR_KEY\n" +
		"heartbeat: { interval: 1s }\n" + testkit.HubPartnersYAML
	env := hubEnv()
	settings, err := hub.Load([]byte(cfg), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if settings.TLSCertFile != "" || settings.WebhookURL != "" {
		t.Fatalf("expected a plain-HTTP hub with no alert sink: %+v", settings)
	}
	store, err := hub.OpenStore(settings.StateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := &contract.LogBuffer{}
	log := core.NewLogger(logs, slog.LevelDebug, settings.Secrets)
	srv, err := hub.NewServer(settings, store, hub.Options{Version: "1.0.0", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx, settings, srv, log) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("hub.Run: %v", err)
		}
	})
	testkit.WaitFor(t, "plain HTTP hub listening", 5*time.Second, func() bool {
		resp, err := http.Get("http://" + addr + "/health/ready")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	px := newPrefixProxy(t, "/gateway-hub", addr)
	hubURL := px.URL() + "/" // a trailing slash in hub.url is tolerated
	rt := px.transport()
	rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer rcancel()

	jc, err := hub.CreateJoinCode(settings.StateDir, "demo-club", "Демо-клуб", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tunnel.Register(rctx, rt, hubURL, jc.Code, "1.0.0")
	if err != nil {
		t.Fatalf("register through the prefix: %v", err)
	}
	if _, _, err := srv.Store().PutClub("demo-club", core.ClubSpec{ID: "demo-club", Name: "Демо-клуб"}); err != nil {
		t.Fatal(err)
	}

	b := contract.Build(t, gatewayConfig, contract.KeyEnv(), "demo", demo.New, nil)
	gw := testkit.StartGateway(t, hubURL, rt, reg.Secret, b.Clubs.TunnelHandler(), nil)
	testkit.WaitFor(t, "gateway connected through the prefix", 10*time.Second, func() bool {
		return gw.Client.Connected() && srv.Broker().Connected("demo-club")
	})
	who, err := tunnel.FetchWhoami(rctx, rt, hubURL, reg.Secret)
	if err != nil || who.Installation != "demo-club" || !who.Connected {
		t.Fatalf("whoami through the prefix: %+v %v", who, err)
	}
	if err := gw.Client.SendHeartbeat(rctx, tunnel.Heartbeat{Version: "1.0.0", Build: tunnel.BuildRelease, Ready: true, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("heartbeat through the prefix: %v", err)
	}

	// a partner calls the hub at its public, prefixed address
	req, _ := http.NewRequest(http.MethodGet, px.URL()+"/v1/clubs/demo-club/courts", nil)
	req.Header.Set("Authorization", "Bearer "+contract.ReadKey)
	resp, err := (&http.Client{Transport: rt, Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"court-2"`) {
		t.Fatalf("courts through the prefix: %d %s", resp.StatusCode, body)
	}

	// the heartbeat reached the hub: the club is ready in status.json
	testkit.WaitFor(t, "status.json with the club ready", 10*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(statusDir, hub.StatusFile))
		if err != nil {
			return false
		}
		var st struct {
			Checks []struct {
				Check  string `json:"check"`
				OK     bool   `json:"ok"`
				Detail string `json:"detail"`
			} `json:"checks"`
		}
		if json.Unmarshal(data, &st) != nil {
			return false
		}
		for _, c := range st.Checks {
			if strings.Contains(c.Check, "demo") && strings.HasSuffix(c.Check, "_ready") && strings.HasPrefix(c.Detail, "ready: CRM login") {
				return c.OK
			}
		}
		return false
	})
	if _, err := os.Stat(filepath.Join(statusDir, hub.MetricsFile)); err != nil {
		t.Fatalf("metrics: %v", err)
	}

	upstream, unprefixed := px.seen()
	if len(unprefixed) > 0 {
		t.Fatalf("requests reached the proxy without the prefix: %v", unprefixed)
	}
	want := map[string]bool{
		"POST " + tunnel.PathRegister: false, "POST " + tunnel.PathPoll: false, "POST " + tunnel.PathReply: false,
		"GET " + tunnel.PathWhoami: false, "POST " + tunnel.PathHeartbeat: false, "GET /v1/clubs/demo-club/courts": false,
	}
	for _, u := range upstream {
		if _, ok := want[u]; ok {
			want[u] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("the hub never received %s through the prefix; got %v", k, upstream)
		}
	}
}
