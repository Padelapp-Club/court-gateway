// Package testkit assembles a hub and gateways over real TLS for tests:
// the hub on an httptest TLS server that can be restarted on the same
// address, tunnel clients that trust only the test certificate, and a TCP
// proxy that can cut the network between them. Nothing here is used by the
// program itself.
package testkit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Hub is a running test hub.
type Hub struct {
	t        *testing.T
	Settings *hub.Settings
	StateDir string
	Opts     hub.Options
	Logs     *contract.LogBuffer

	mu     sync.Mutex
	addr   string
	srv    *httptest.Server
	server *hub.Server
	// CAFile is a PEM file with the test server certificate.
	CAFile string
	Roots  *x509.CertPool
}

// StartHub starts a hub with settings parsed from hubYAML (state_dir is
// filled in) and env.
func StartHub(t *testing.T, hubYAML string, env map[string]string, opts hub.Options) *Hub {
	t.Helper()
	dir := t.TempDir()
	settings, err := hub.Load([]byte("state_dir: "+filepath.Join(dir, "state")+"\n"+hubYAML), func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("hub config: %v", err)
	}
	h := &Hub{t: t, Settings: settings, StateDir: settings.StateDir, Opts: opts, Logs: &contract.LogBuffer{}}
	if h.Opts.Logger == nil {
		h.Opts.Logger = core.NewLogger(h.Logs, slog.LevelDebug, settings.Secrets)
	}
	h.start("127.0.0.1:0")
	t.Cleanup(h.Stop)
	return h
}

func (h *Hub) start(addr string) {
	h.t.Helper()
	store, err := hub.OpenStore(h.StateDir, h.Opts.Now)
	if err != nil {
		h.t.Fatalf("store: %v", err)
	}
	server, err := hub.NewServer(h.Settings, store, h.Opts)
	if err != nil {
		h.t.Fatalf("hub: %v", err)
	}
	var ln net.Listener
	for i := 0; i < 50; i++ { // the old socket may linger for a moment
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		h.t.Fatalf("listen %s: %v", addr, err)
	}
	srv := httptest.NewUnstartedServer(server.Handler())
	srv.Listener.Close()
	srv.Listener = ln
	srv.EnableHTTP2 = true
	srv.StartTLS()
	h.mu.Lock()
	h.srv, h.server, h.addr = srv, server, ln.Addr().String()
	h.mu.Unlock()
	if h.CAFile == "" {
		cert := srv.Certificate()
		h.CAFile = filepath.Join(filepath.Dir(h.StateDir), "hub-ca.pem")
		if err := os.WriteFile(h.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
			h.t.Fatal(err)
		}
		h.Roots = x509.NewCertPool()
		h.Roots.AddCert(cert)
	}
}

// URL is the hub's base URL.
func (h *Hub) URL() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return "https://" + h.addr
}

// Addr is host:port.
func (h *Hub) Addr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addr
}

// Server returns the current hub server.
func (h *Hub) Server() *hub.Server {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.server
}

// Stop stops the hub (idempotent).
func (h *Hub) Stop() {
	h.mu.Lock()
	srv := h.srv
	h.srv = nil
	h.mu.Unlock()
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

// Restart stops the hub and starts a new process-equivalent on the same
// address from the same state directory.
func (h *Hub) Restart() {
	h.t.Helper()
	addr := h.Addr()
	h.Stop()
	h.start(addr)
}

// JoinCode creates a join code for club.
func (h *Hub) JoinCode(club, name string) string {
	h.t.Helper()
	now := time.Now()
	if h.Opts.Now != nil {
		now = h.Opts.Now()
	}
	jc, err := hub.CreateJoinCode(h.StateDir, club, name, time.Hour, now)
	if err != nil {
		h.t.Fatal(err)
	}
	return jc.Code
}

// Transport returns a transport that trusts the test hub only.
func (h *Hub) Transport() *http.Transport { return tunnel.NewTransport(h.Roots, nil) }

// Register makes an installation with the same id as club that serves
// club, redeems a join code for it and returns the gateway secret. The
// club is put on the hub only so the hub routes it; the test gateway
// serves its own copy (a contract.Built), not the hub's.
func (h *Hub) Register(club, name string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rr, err := tunnel.Register(ctx, h.Transport(), h.URL(), h.JoinCode(club, name), "test")
	if err != nil {
		h.t.Fatalf("register: %v", err)
	}
	spec := core.ClubSpec{ID: club, Name: name, Timezone: "Europe/Moscow", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "00:00-24:00"},
		Courts: []core.CourtSpec{{ID: "court-1", Name: "Court 1"}}}
	if _, _, err := h.Server().Store().PutClub(club, spec); err != nil {
		h.t.Fatalf("put club: %v", err)
	}
	return rr.Secret
}

// Gateway is a running tunnel client.
type Gateway struct {
	Client *tunnel.Client
	cancel context.CancelFunc
	done   chan struct{}
}

// ClientConfig is the config of a test gateway of installation that
// registered at h with secret: two polls, fast reconnects.
func (h *Hub) ClientConfig(installation, secret string, handler http.Handler, log *slog.Logger) tunnel.ClientConfig {
	return ClientConfig(h.URL(), h.Transport(), h.Server().Store().HubKey(), installation, secret, handler, log)
}

// ClientConfig is the tunnel client config test gateways use: two polls,
// fast reconnects; key is the hub's public key.
func ClientConfig(hubURL string, rt http.RoundTripper, key ed25519.PublicKey, installation, secret string, handler http.Handler, log *slog.Logger) tunnel.ClientConfig {
	return tunnel.ClientConfig{
		HubURL: hubURL, Secret: secret, HubKey: key, Installation: installation, Polls: 2, Handler: handler, Version: "1.0.0", Build: tunnel.BuildRelease,
		Platform: "test", Logger: log, Transport: rt, MinBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
		PollTimeout: 5 * time.Second,
	}
}

// StartGateway runs a tunnel client with cfg until the test ends.
func StartGateway(t *testing.T, cfg tunnel.ClientConfig) *Gateway {
	t.Helper()
	c, err := tunnel.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &Gateway{Client: c, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(g.done)
		c.Run(ctx)
	}()
	t.Cleanup(g.Stop)
	return g
}

// Stop disconnects the gateway (idempotent).
func (g *Gateway) Stop() {
	g.cancel()
	<-g.done
}

// Signing is a hub key and a gateway registration, for tests that hand
// requests to a tunnel client directly, without a hub.
type Signing struct {
	Key          ed25519.PrivateKey
	Installation string
	Secret       string
}

// NewSigning makes a key and a registration of installation.
func NewSigning(installation string) Signing {
	_, key, _ := ed25519.GenerateKey(nil)
	return Signing{Key: key, Installation: installation, Secret: "cgs_test-secret-" + installation}
}

// Client is a tunnel client that trusts s.
func (s Signing) Client(t *testing.T, handler http.Handler) *tunnel.Client {
	t.Helper()
	pub := s.Key.Public().(ed25519.PublicKey)
	v, err := tunnel.NewVerifier(pub, s.Installation, s.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	// the hub's time, as the client's first poll answer brings it
	reg := tunnel.RegistrationOf(tunnel.SecretSHA256(s.Secret))
	if err := v.Time("sync", tunnel.SignTime(s.Key, s.Installation, reg, "sync", "0", time.Now()), "0", time.Now()); err != nil {
		t.Fatal(err)
	}
	c, err := tunnel.NewClient(tunnel.ClientConfig{HubURL: "https://hub.invalid", Secret: s.Secret, Handler: handler,
		HubKey: pub, Installation: s.Installation, Verifier: v})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Sign signs rq as the hub does, with a fresh id.
func (s Signing) Sign(rq tunnel.Request) tunnel.Request {
	var id [8]byte
	_, _ = rand.Read(id[:])
	rq.ID = hex.EncodeToString(id[:])
	rq.Sign(s.Key, s.Installation, tunnel.RegistrationOf(tunnel.SecretSHA256(s.Secret)), time.Now())
	return rq
}

// WaitFor polls cond until it is true or the timeout passes.
func WaitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Proxy is a TCP relay whose network can be cut and restored, standing in
// for a club's internet connection.
type Proxy struct {
	ln     net.Listener
	target string

	mu     sync.Mutex
	broken bool
	conns  map[net.Conn]bool
}

// NewProxy relays 127.0.0.1:<random> to target.
func NewProxy(t *testing.T, target string) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{ln: ln, target: target, conns: map[net.Conn]bool{}}
	go p.serve()
	t.Cleanup(func() { ln.Close(); p.Break() })
	return p
}

// URL is the https URL through the proxy.
func (p *Proxy) URL() string { return "https://" + p.ln.Addr().String() }

func (p *Proxy) track(c net.Conn, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if on {
		p.conns[c] = true
	} else {
		delete(p.conns, c)
	}
}

func (p *Proxy) serve() {
	for {
		in, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		broken := p.broken
		p.mu.Unlock()
		if broken {
			in.Close()
			continue
		}
		out, err := net.Dial("tcp", p.target)
		if err != nil {
			in.Close()
			continue
		}
		p.track(in, true)
		p.track(out, true)
		go func() {
			_, _ = io.Copy(out, in)
			out.Close()
			in.Close()
			p.track(in, false)
			p.track(out, false)
		}()
		go func() {
			_, _ = io.Copy(in, out)
			in.Close()
			out.Close()
		}()
	}
}

// Break drops every connection and refuses new ones.
func (p *Proxy) Break() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.broken = true
	for c := range p.conns {
		c.Close()
	}
	p.conns = map[net.Conn]bool{}
}

// Heal accepts connections again.
func (p *Proxy) Heal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.broken = false
}
