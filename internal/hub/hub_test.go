package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

var demoSpec = testkit.Club("demo-club", "Демо-клуб", "Корт 1", "Корт 2")

const operatorKey = "test-operator-key-0123456789abcdef"

func hubEnv() map[string]string {
	env := contract.KeyEnv()
	env["HUB_OPERATOR_KEY"] = operatorKey
	return env
}

const hubYAML = "poll_wait: 1s\nrequest_timeout: 3s\noperator_key_env: HUB_OPERATOR_KEY\n" + testkit.HubPartnersYAML

type partnerResp struct {
	status int
	body   []byte
	json   map[string]any
	header http.Header
}

func call(t *testing.T, h *testkit.Hub, method, path, key string, body string) partnerResp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.URL()+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: h.Transport(), Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	out := partnerResp{status: resp.StatusCode, body: data, header: resp.Header}
	_ = json.Unmarshal(data, &out.json)
	return out
}

type world struct {
	hub   *testkit.Hub
	built *contract.Built
	drv   *demo.Driver
	gw    *testkit.Gateway
}

func newWorld(t *testing.T, opts hub.Options, extraHub string) *world {
	t.Helper()
	h := testkit.StartHub(t, hubYAML+extraHub, hubEnv(), opts)
	b := contract.Build(t, testkit.DemoCRM, demoSpec, nil, "demo", demo.New, nil)
	return &world{hub: h, built: b, drv: b.Driver.(*demo.Driver)}
}

func (w *world) connect(t *testing.T, hubURL string) {
	t.Helper()
	secret := w.hub.Register("demo-club", "Демо-клуб")
	w.connectWith(t, hubURL, secret)
}

func (w *world) connectWith(t *testing.T, hubURL, secret string) {
	t.Helper()
	w.gw = testkit.StartGateway(t, testkit.ClientConfig(hubURL, w.hub.Transport(), secret, w.built.Clubs.TunnelHandler(), nil))
	w.waitConnected(t)
}

func (w *world) waitConnected(t *testing.T) {
	t.Helper()
	testkit.WaitFor(t, "gateway connected", 10*time.Second, func() bool {
		return w.gw.Client.Connected() && w.hub.Server().Broker().Connected("demo-club")
	})
}

func TestEndToEnd(t *testing.T) {
	w := newWorld(t, hub.Options{}, "")
	w.connect(t, w.hub.URL())

	r := call(t, w.hub, http.MethodGet, "/v1/clubs/demo-club/courts", contract.ReadKey, "")
	if r.status != http.StatusOK || !strings.Contains(string(r.body), `"court-2"`) {
		t.Fatalf("courts: %d %s", r.status, r.body)
	}
	day := time.Now().In(w.built.Settings.Location).AddDate(0, 0, 2)
	start := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
	body := `{"court":"court-1","start":"` + start.Format(time.RFC3339) + `","end":"` + start.Add(time.Hour).Format(time.RFC3339) + `","kind":"game"}`
	r = call(t, w.hub, http.MethodPost, "/v1/clubs/demo-club/bookings", contract.BookKey, body)
	if r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	id, _ := r.json["id"].(string)
	r = call(t, w.hub, http.MethodGet, "/v1/clubs/demo-club/bookings/"+id, contract.ReadKey, "")
	if r.status != http.StatusOK || r.json["status"] != "active" {
		t.Fatalf("read: %d %s", r.status, r.body)
	}
	// one request id from the partner's view to the gateway's log
	r = call(t, w.hub, http.MethodGet, "/v1/clubs/demo-club/bookings/x-nothing", contract.ReadKey, "")
	rid := r.header.Get("X-Request-ID")
	if r.status != http.StatusNotFound || rid == "" || r.json["request_id"] != rid {
		t.Fatalf("request id: %d %q %s", r.status, rid, r.body)
	}
	if !strings.Contains(w.built.Logs.String(), rid) {
		t.Fatalf("the gateway did not log the hub's request id %s", rid)
	}
	// /v1/clubs on the hub lists the registered club, online
	r = call(t, w.hub, http.MethodGet, "/v1/clubs", contract.ReadKey, "")
	if r.status != http.StatusOK || !strings.Contains(string(r.body), `"online":true`) {
		t.Fatalf("clubs: %d %s", r.status, r.body)
	}
}

func TestClubOfflineAndUnknown(t *testing.T) {
	w := newWorld(t, hub.Options{}, "")
	w.hub.Register("demo-club", "Демо-клуб") // registered, never connected
	r := call(t, w.hub, http.MethodGet, "/v1/clubs/demo-club/courts", contract.ReadKey, "")
	if r.status != http.StatusServiceUnavailable || r.json["code"] != "CLUB_OFFLINE" || r.json["request_id"] == "" {
		t.Fatalf("offline: %d %s", r.status, r.body)
	}
	r = call(t, w.hub, http.MethodGet, "/v1/clubs/no-such-club/courts", contract.ReadKey, "")
	if r.status != http.StatusNotFound || r.json["code"] != "NOT_FOUND" {
		t.Fatalf("unknown: %d %s", r.status, r.body)
	}
	// auth is checked before anything about the club is revealed
	r = call(t, w.hub, http.MethodGet, "/v1/clubs/demo-club/courts", "", "")
	if r.status != http.StatusUnauthorized {
		t.Fatalf("no key: %d %s", r.status, r.body)
	}
}

func TestPartnerLimitedToItsClubs(t *testing.T) {
	extra := `  - { name: narrow, key_env: NARROW_KEY, scopes: [read], clubs: [other-club] }
`
	env := hubEnv()
	env["NARROW_KEY"] = "narrow-key-0123456789abcdef0123"
	h := testkit.StartHub(t, hubYAML+extra, env, hub.Options{})
	b := contract.Build(t, testkit.DemoCRM, demoSpec, nil, "demo", demo.New, nil)
	gw := testkit.StartGateway(t, testkit.ClientConfig(h.URL(), h.Transport(), h.Register("demo-club", "Демо"), b.Clubs.TunnelHandler(), nil))
	testkit.WaitFor(t, "connected", 10*time.Second, gw.Client.Connected)
	r := call(t, h, http.MethodGet, "/v1/clubs/demo-club/courts", "narrow-key-0123456789abcdef0123", "")
	if r.status != http.StatusNotFound {
		t.Fatalf("a partner reached a club it has no access to: %d %s", r.status, r.body)
	}
	r = call(t, h, http.MethodGet, "/v1/clubs", "narrow-key-0123456789abcdef0123", "")
	if strings.Contains(string(r.body), "demo-club") {
		t.Fatalf("club list shows a club the partner has no access to: %s", r.body)
	}
}

func TestRequestTooLarge(t *testing.T) {
	w := newWorld(t, hub.Options{}, "")
	w.connect(t, w.hub.URL())
	big := `{"note":"` + strings.Repeat("x", tunnel.MaxRequestBody+10) + `"}`
	r := call(t, w.hub, http.MethodPost, "/v1/clubs/demo-club/bookings", contract.BookKey, big)
	if r.status != http.StatusRequestEntityTooLarge || r.json["code"] != "REQUEST_TOO_LARGE" {
		t.Fatalf("big body: %d %.200s", r.status, r.body)
	}
}

func TestUnauthorizedGatewayRejected(t *testing.T) {
	w := newWorld(t, hub.Options{}, "")
	w.hub.Register("demo-club", "Демо-клуб")
	req, _ := http.NewRequest(http.MethodPost, w.hub.URL()+tunnel.PathPoll, nil)
	req.Header.Set("Authorization", "Bearer cgs_not-a-real-secret")
	resp, err := (&http.Client{Transport: w.hub.Transport()}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("poll with a wrong secret: %d", resp.StatusCode)
	}
	gw := testkit.StartGateway(t, testkit.ClientConfig(w.hub.URL(), w.hub.Transport(), "cgs_wrong", w.built.Clubs.TunnelHandler(), nil))
	testkit.WaitFor(t, "secret rejected", 5*time.Second, gw.Client.Unauthorized)
	if gw.Client.Connected() || w.hub.Server().Broker().Connected("demo-club") {
		t.Fatal("a gateway with a wrong secret counts as connected")
	}
	// a revoked gateway loses access at its next request
	secret := w.hub.Register("demo-club", "Демо-клуб")
	if err := hub.RevokeGateway(w.hub.StateDir, "demo-club"); err != nil {
		t.Fatal(err)
	}
	if _, err := tunnel.FetchWhoami(context.Background(), w.hub.Transport(), w.hub.URL(), secret); !errors.Is(err, tunnel.ErrUnauthorized) {
		t.Fatalf("revoked secret: %v", err)
	}
}

func TestJoinCodeSingleUseAndExpiry(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	w := newWorld(t, hub.Options{Now: clock}, "")
	ctx := context.Background()

	code := w.hub.JoinCode("demo-club", "Демо-клуб")
	// typed by hand: lower case, no dashes, O for 0
	typed := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(code, "-", ""), "0", "O"))
	rr, err := tunnel.Register(ctx, w.hub.Transport(), w.hub.URL(), typed, "1.0.0")
	if err != nil || rr.Installation != "demo-club" || rr.Name != "Демо-клуб" || !strings.HasPrefix(rr.Secret, "cgs_") {
		t.Fatalf("first use: %+v %v", rr, err)
	}
	if _, err := tunnel.Register(ctx, w.hub.Transport(), w.hub.URL(), code, "1.0.0"); !errors.Is(err, tunnel.ErrJoinCodeRejected) {
		t.Fatalf("second use: %v", err)
	}
	// concurrent redemption of one code: exactly one winner
	code = w.hub.JoinCode("demo-club", "Демо-клуб")
	var wins, wg = 0, sync.WaitGroup{}
	var wmu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tunnel.Register(ctx, w.hub.Transport(), w.hub.URL(), code, ""); err == nil {
				wmu.Lock()
				wins++
				wmu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("one code redeemed %d times", wins)
	}
	// expiry
	jc, err := hub.CreateJoinCode(w.hub.StateDir, "demo-club", "Демо-клуб", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	code = jc.Code
	mu.Lock()
	now = now.Add(time.Hour + time.Second)
	mu.Unlock()
	if _, err := tunnel.Register(ctx, w.hub.Transport(), w.hub.URL(), code, ""); !errors.Is(err, tunnel.ErrJoinCodeRejected) {
		t.Fatalf("expired code: %v", err)
	}
	// re-registration revokes the previous secret
	secret1 := w.hub.Register("demo-club", "Демо-клуб")
	secret2 := w.hub.Register("demo-club", "Демо-клуб")
	if _, err := tunnel.FetchWhoami(ctx, w.hub.Transport(), w.hub.URL(), secret1); !errors.Is(err, tunnel.ErrUnauthorized) {
		t.Fatalf("old secret still works: %v", err)
	}
	if who, err := tunnel.FetchWhoami(ctx, w.hub.Transport(), w.hub.URL(), secret2); err != nil || who.Installation != "demo-club" {
		t.Fatalf("new secret: %+v %v", who, err)
	}
	// nothing secret is stored in plain text
	assertNoPlaintext(t, w.hub.StateDir, []string{secret2, hub.NormalizeJoinCode(code)})
}

func TestReconnectAfterHubRestart(t *testing.T) {
	w := newWorld(t, hub.Options{}, "")
	w.connect(t, w.hub.URL())
	w.hub.Restart()
	// the gateway notices and comes back by itself; the secret survived
	// the restart because the hub keeps it on disk
	testkit.WaitFor(t, "reconnected after hub restart", 15*time.Second, func() bool {
		return w.hub.Server().Broker().Connected("demo-club")
	})
	r := call(t, w.hub, http.MethodGet, "/v1/clubs/demo-club/courts", contract.ReadKey, "")
	if r.status != http.StatusOK {
		t.Fatalf("after restart: %d %s", r.status, r.body)
	}
}

func TestReconnectAfterNetworkFailure(t *testing.T) {
	w := newWorld(t, hub.Options{}, "")
	proxy := testkit.NewProxy(t, w.hub.Addr())
	w.connect(t, proxy.URL())

	proxy.Break() // the club's internet goes down
	testkit.WaitFor(t, "hub sees the gateway gone", 10*time.Second, func() bool {
		return !w.hub.Server().Broker().Connected("demo-club")
	})
	r := call(t, w.hub, http.MethodGet, "/v1/clubs/demo-club/courts", contract.ReadKey, "")
	if r.status != http.StatusServiceUnavailable || r.json["code"] != "CLUB_OFFLINE" {
		t.Fatalf("while offline: %d %s", r.status, r.body)
	}
	time.Sleep(500 * time.Millisecond) // several failed reconnects
	proxy.Heal()
	testkit.WaitFor(t, "reconnected after network failure", 15*time.Second, func() bool {
		return w.hub.Server().Broker().Connected("demo-club") && w.gw.Client.Connected()
	})
	r = call(t, w.hub, http.MethodGet, "/v1/clubs/demo-club/courts", contract.ReadKey, "")
	if r.status != http.StatusOK {
		t.Fatalf("after recovery: %d %s", r.status, r.body)
	}
}

func TestTunnelReachesOnlyAPIAndHealth(t *testing.T) {
	b := contract.Build(t, testkit.DemoCRM, demoSpec, nil, "demo", demo.New, nil)
	c, err := tunnel.NewClient(tunnel.ClientConfig{HubURL: "https://hub.invalid", Secret: "x", Handler: b.Clubs.TunnelHandler()})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/hub/status", "/v1/../hub/status", "/debug/pprof/", "", "/v1/clubs/%2e%2e"} {
		r := c.Execute(context.Background(), tunnel.Request{ID: "1", Method: http.MethodGet, Path: p, Partner: "reader"})
		if r.Status != http.StatusNotFound {
			t.Fatalf("%q: %d", p, r.Status)
		}
	}
	r := c.Execute(context.Background(), tunnel.Request{ID: "1", Method: http.MethodGet, Path: "/v1/clubs/demo-club/courts"})
	if r.Status != http.StatusUnauthorized {
		t.Fatalf("a request without a partner name: %d", r.Status)
	}
	r = c.Execute(context.Background(), tunnel.Request{ID: "1", Method: http.MethodGet, Path: "/v1/clubs/demo-club/courts", Partner: "reader"})
	if r.Status != http.StatusOK {
		t.Fatalf("partner request: %d %s", r.Status, r.Body)
	}
}

func TestUpdateHint(t *testing.T) {
	var mu sync.Mutex
	var hint string
	h := testkit.StartHub(t, hubYAML+"latest_version: 1.2.0\n", hubEnv(), hub.Options{})
	b := contract.Build(t, testkit.DemoCRM, demoSpec, nil, "demo", demo.New, nil)
	c, err := tunnel.NewClient(tunnel.ClientConfig{
		HubURL: h.URL(), Secret: h.Register("demo-club", "Демо"), Handler: b.Clubs.TunnelHandler(),
		Version: "1.0.0", Build: tunnel.BuildRelease, Transport: h.Transport(), PollTimeout: 5 * time.Second,
		OnHint: func(v string) { mu.Lock(); hint = v; mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	testkit.WaitFor(t, "hint", 10*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return hint == "1.2.0" })
}

func assertNoPlaintext(t *testing.T, dir string, secrets []string) {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n++
		for _, s := range secrets {
			if bytes.Contains(data, []byte(s)) {
				t.Fatalf("%s holds a secret in plain text", p)
			}
		}
		if info, _ := d.Info(); info != nil && info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is readable by others: %v", p, info.Mode())
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("walk: %v (%d files)", err, n)
	}
}
