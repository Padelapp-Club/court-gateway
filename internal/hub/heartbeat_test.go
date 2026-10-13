package hub_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// fakeClock is shared by the hub and the gateway's self-check in a test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// countingDriver stands in for the CRM: it records, on the fake clock,
// every readiness check (CRM login plus narrow occupancy read) it serves,
// whoever asked for it.
type countingDriver struct {
	driver.Driver
	clock *fakeClock
	mu    sync.Mutex
	calls []time.Time
	err   error
}

func (d *countingDriver) Ready(ctx context.Context) error {
	d.mu.Lock()
	d.calls = append(d.calls, d.clock.Now())
	err := d.err
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return d.Driver.Ready(ctx)
}

func (d *countingDriver) setErr(err error) { d.mu.Lock(); d.err = err; d.mu.Unlock() }

func (d *countingDriver) Calls() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.calls...)
}

type recorded struct {
	path string
	body string
}

// fakeWebhook records what the optional webhook sink posts.
func fakeWebhook(t *testing.T) (*httptest.Server, func() []recorded) {
	var mu sync.Mutex
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, recorded{r.URL.Path, string(b)})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), got...)
	}
}

// beatWorld is a hub on a fake clock with one registered gateway whose
// tunnel client is connected and whose self-check runs on the same clock.
type beatWorld struct {
	clock  *fakeClock
	hub    *testkit.Hub
	crm    *countingDriver
	gw     *testkit.Gateway
	check  *tunnel.SelfCheck
	secret string
	status string // status_dir
}

func newBeatWorld(t *testing.T, extraYAML string, env map[string]string, opts hub.Options) *beatWorld {
	t.Helper()
	w := &beatWorld{clock: newFakeClock(), status: filepath.Join(t.TempDir(), "status")}
	opts.Now = w.clock.Now
	if env == nil {
		env = hubEnv()
	}
	w.hub = testkit.StartHub(t, hubYAML+"status_dir: "+w.status+"\n"+extraYAML, env, opts)
	w.crm = &countingDriver{clock: w.clock}
	factory := func(cfg driver.Config) (driver.Driver, error) {
		d, err := demo.New(cfg)
		w.crm.Driver = d
		return w.crm, err
	}
	b := contract.Build(t, gatewayConfig, contract.KeyEnv(), "demo", factory, nil)
	w.secret = w.hub.Register("demo-club", "Демо-клуб")
	w.gw = testkit.StartGateway(t, w.hub.URL(), w.hub.Transport(), w.secret, b.Clubs.TunnelHandler(), nil)
	testkit.WaitFor(t, "connected", 10*time.Second, func() bool { return w.hub.Server().Broker().Connected("demo-club") })
	w.check = &tunnel.SelfCheck{Ready: w.crm.Ready, Reason: core.ReadyReason, Interval: time.Minute,
		Now: w.clock.Now, Version: "1.0.0", Build: tunnel.BuildRelease}
	return w
}

// beat sends one heartbeat over HTTP, as a gateway does.
func (w *beatWorld) beat(t *testing.T, ready bool, reason string) {
	t.Helper()
	hb := tunnel.Heartbeat{Version: "1.0.0", Build: tunnel.BuildRelease, Ready: ready, Reason: reason, CheckedAt: w.clock.Now()}
	if err := w.gw.Client.SendHeartbeat(context.Background(), hb); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func (w *beatWorld) state(t *testing.T) string {
	t.Helper()
	return w.hub.Server().Monitor().Health("demo-club").State
}

func TestHeartbeatOfflineNotReadyAndRecovery(t *testing.T) {
	hook, alerts := fakeWebhook(t)
	env := hubEnv()
	env["HOOK_URL"] = hook.URL + "/hook?token=webhook-secret-value"
	w := newBeatWorld(t, "alerts: { webhook_url_env: HOOK_URL }\n", env, hub.Options{SinkClient: hook.Client()})
	mon := w.hub.Server().Monitor()
	ctx := context.Background()

	mon.Tick(ctx)
	if st := w.state(t); st != hub.StatePending {
		t.Fatalf("before the first heartbeat: %s", st)
	}
	w.beat(t, true, "")
	mon.Tick(ctx)
	if hl := mon.Health("demo-club"); hl.State != hub.StateOnline || !hl.Ready || hl.Version != "1.0.0" {
		t.Fatalf("after a heartbeat: %+v", hl)
	}

	// heartbeats stop: offline only after MORE than offline_after (3m)
	w.clock.Advance(3 * time.Minute)
	mon.Tick(ctx)
	if st := w.state(t); st != hub.StateOnline || len(alerts()) != 0 {
		t.Fatalf("at exactly 3m: %s %+v", st, alerts())
	}
	w.clock.Advance(time.Second)
	mon.Tick(ctx)
	if st := w.state(t); st != hub.StateOffline {
		t.Fatalf("after 3m1s: %s", st)
	}
	if got := alerts(); len(got) != 1 || !strings.Contains(got[0].body, "НЕТ СВЯЗИ") || !strings.Contains(got[0].body, `"event":"offline"`) {
		t.Fatalf("offline alert: %+v", got)
	}
	w.clock.Advance(time.Minute)
	mon.Tick(ctx)
	if n := len(alerts()); n != 1 {
		t.Fatalf("offline alert repeated: %d", n)
	}
	// the tunnel alone does not make a club online: only heartbeats do
	if !w.hub.Server().Broker().Connected("demo-club") || w.state(t) != hub.StateOffline {
		t.Fatal("a connected tunnel without heartbeats must still be offline")
	}

	// recovery
	w.beat(t, true, "")
	mon.Tick(ctx)
	if got := alerts(); w.state(t) != hub.StateOnline || len(got) != 2 || !strings.Contains(got[1].body, "снова на связи") {
		t.Fatalf("recovery: %s %+v", w.state(t), got)
	}

	// three not-ready heartbeats in a row -> not_ready
	for i := 0; i < 2; i++ {
		w.clock.Advance(time.Minute)
		w.beat(t, false, "CRM_AUTH_FAILED")
		mon.Tick(ctx)
	}
	if st := w.state(t); st != hub.StateOnline || len(alerts()) != 2 {
		t.Fatalf("after 2 not-ready heartbeats: %s %d", st, len(alerts()))
	}
	w.clock.Advance(time.Minute)
	w.beat(t, false, "CRM_AUTH_FAILED")
	mon.Tick(ctx)
	if got := alerts(); w.state(t) != hub.StateNotReady || len(got) != 3 || !strings.Contains(got[2].body, "НЕ ГОТОВ: CRM_AUTH_FAILED") {
		t.Fatalf("not ready: %s %+v", w.state(t), got)
	}
	// one good heartbeat resets the streak
	w.clock.Advance(time.Minute)
	w.beat(t, true, "")
	mon.Tick(ctx)
	if got := alerts(); w.state(t) != hub.StateOnline || len(got) != 4 || !strings.Contains(got[3].body, "снова готов") {
		t.Fatalf("ready again: %s %+v", w.state(t), got)
	}
	w.beat(t, false, "CRM_UNREACHABLE")
	w.beat(t, false, "CRM_UNREACHABLE")
	w.beat(t, true, "")
	w.beat(t, false, "CRM_UNREACHABLE")
	mon.Tick(ctx)
	if st := w.state(t); st != hub.StateOnline {
		t.Fatalf("an interrupted streak counted as not ready: %s", st)
	}

	// nothing secret in alerts or the hub log
	all := w.hub.Logs.String()
	for _, a := range alerts() {
		all += a.body
	}
	for _, s := range []string{"webhook-secret-value", w.secret, operatorKey, contract.ReadKey, contract.BookKey} {
		if strings.Contains(all, s) {
			t.Fatalf("secret %q leaked into alerts or logs", s[:8])
		}
	}
}

// A club that has just registered gets offline_after to send its first
// heartbeat, counted from its registration, not from the hub's start; a
// club never heard from since the hub started counts from the start.
func TestHeartbeatSilenceCountsFromStartOrRegistration(t *testing.T) {
	w := newBeatWorld(t, "", nil, hub.Options{})
	mon := w.hub.Server().Monitor()
	w.clock.Advance(10 * time.Minute)
	w.hub.Register("late-club", "Поздний клуб")
	mon.Tick(context.Background())
	if st := mon.Health("late-club").State; st != hub.StatePending {
		t.Fatalf("a club registered a moment ago: %s", st)
	}
	if st := w.state(t); st != hub.StateOffline {
		t.Fatalf("a club silent since the hub started 10m ago: %s", st)
	}
	w.clock.Advance(3*time.Minute + time.Second)
	if st := mon.Health("late-club").State; st != hub.StateOffline {
		t.Fatalf("registered 3m1s ago without a heartbeat: %s", st)
	}
}

// The regression for the old design, where the hub probed /health/ready
// through the tunnel on top of the gateway's own check: over ten simulated
// minutes the CRM sees at most one readiness check per minute per gateway,
// all of them from the gateway's own self-check.
func TestAtMostOneCRMCheckPerMinute(t *testing.T) {
	w := newBeatWorld(t, "", nil, hub.Options{})
	mon := w.hub.Server().Monitor()
	ctx := context.Background()
	start := w.clock.Now()
	beats := 0
	for step := 1; step <= 300; step++ { // the gateway's 2-second loop
		w.clock.Advance(2 * time.Second)
		if hb, due := w.check.Step(ctx); due {
			if err := w.gw.Client.SendHeartbeat(ctx, hb); err != nil {
				t.Fatalf("heartbeat: %v", err)
			}
			beats++
		}
		if step%30 == 0 { // the hub's 60-second evaluation
			mon.Tick(ctx)
		}
	}
	calls := w.crm.Calls()
	perMinute := map[int]int{}
	for _, c := range calls {
		perMinute[int(c.Sub(start)/time.Minute)]++
	}
	for m, n := range perMinute {
		if n > 1 {
			t.Fatalf("minute %d: the CRM saw %d checks (want at most 1); all: %d over 10 minutes", m, n, len(calls))
		}
	}
	if len(calls) > 10 || len(calls) < 9 || beats != len(calls) {
		t.Fatalf("CRM checks over 10 minutes: %d, heartbeats sent: %d", len(calls), beats)
	}
	if st := w.state(t); st != hub.StateOnline {
		t.Fatalf("state after 10 minutes of heartbeats: %s", st)
	}

	// a failing CRM is reported by the same single check
	w.crm.setErr(driver.Upstream(driver.UpstreamAuth, "test", nil))
	for i := 0; i < 3; i++ {
		w.clock.Advance(time.Minute)
		hb, due := w.check.Step(ctx)
		if !due || hb.Ready || hb.Reason != "CRM_AUTH_FAILED" {
			t.Fatalf("self-check: due=%v %+v", due, hb)
		}
		if err := w.gw.Client.SendHeartbeat(ctx, hb); err != nil {
			t.Fatal(err)
		}
	}
	mon.Tick(ctx)
	if hl := mon.Health("demo-club"); hl.State != hub.StateNotReady || hl.Reason != "CRM_AUTH_FAILED" {
		t.Fatalf("health: %+v", hl)
	}
}

func TestHeartbeatEndpoint(t *testing.T) {
	w := newBeatWorld(t, "", nil, hub.Options{})
	post := func(secret, body string) int {
		req, _ := http.NewRequest(http.MethodPost, w.hub.URL()+tunnel.PathHeartbeat, strings.NewReader(body))
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, err := (&http.Client{Transport: w.hub.Transport()}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("", `{"ready":true}`); code != http.StatusUnauthorized {
		t.Fatalf("without a secret: %d", code)
	}
	if code := post("cgs_wrong", `{"ready":true}`); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", code)
	}
	if code := post(w.secret, `not json`); code != http.StatusBadRequest {
		t.Fatalf("malformed: %d", code)
	}
	if code := post(w.secret, `{"ready":false,"reason":"`+strings.Repeat("A", 70000)+`"}`); code != http.StatusBadRequest {
		t.Fatalf("oversized: %d", code)
	}
	if st := w.state(t); st != hub.StatePending {
		t.Fatalf("rejected heartbeats changed the state: %s", st)
	}
	// a gateway cannot put free text into what the operator sees
	evil := `{"ready":false,"reason":"call +7 999 <b>now</b>","version":"1.0.0\n<script>","build":"evil"}`
	if code := post(w.secret, evil); code != http.StatusNoContent {
		t.Fatalf("heartbeat: %d", code)
	}
	if hl := w.hub.Server().Monitor().Health("demo-club"); hl.Reason != "NOT_READY" || hl.Version != "" || hl.Build != "" {
		t.Fatalf("unsanitised heartbeat: %+v", hl)
	}
	w.hub.Server().Monitor().Tick(context.Background())
	for _, f := range []string{hub.StatusFile, hub.MetricsFile} {
		data, err := os.ReadFile(filepath.Join(w.status, f))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("script")) || bytes.Contains(data, []byte("999")) || bytes.Contains(data, []byte("evil")) {
			t.Fatalf("%s carries gateway-supplied text: %s", f, data)
		}
	}
}

// ---- status files ----

var (
	stdKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	stdValueRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
)

// validateStatusFiles checks dir against the monitoring standard on its
// own, without the writer's code: status.json holds a "checks" array of
// {"check","ok","detail"}, metrics holds key=number lines with keys
// [a-z][a-z0-9_]*, plain decimals of at most 15 digits and no repeats.
func validateStatusFiles(t *testing.T, dir string) (map[string]map[string]any, map[string]string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, hub.StatusFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("status.json is not JSON: %v\n%s", err, data)
	}
	var top struct {
		TS     string `json:"ts"`
		OK     *bool  `json:"ok"`
		Checks []map[string]any
	}
	_ = json.Unmarshal(data, &top)
	if _, err := time.Parse(time.RFC3339, top.TS); err != nil || top.OK == nil || doc["checks"] == nil {
		t.Fatalf("status.json top level: %s", data)
	}
	checks := map[string]map[string]any{}
	allOK := true
	for _, c := range top.Checks {
		if len(c) != 3 {
			t.Fatalf("a check must have exactly check, ok, detail: %v", c)
		}
		name, ok1 := c["check"].(string)
		okv, ok2 := c["ok"].(bool)
		_, ok3 := c["detail"].(string)
		if !ok1 || !ok2 || !ok3 || !stdKeyRe.MatchString(name) {
			t.Fatalf("malformed check: %v", c)
		}
		if checks[name] != nil {
			t.Fatalf("check %s repeats", name)
		}
		checks[name] = c
		allOK = allOK && okv
	}
	if *top.OK != allOK {
		t.Fatalf("top-level ok=%v but checks say %v", *top.OK, allOK)
	}
	f, err := os.Open(filepath.Join(dir, hub.MetricsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	metrics := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		digits := strings.NewReplacer("-", "", ".", "").Replace(v)
		if !ok || !stdKeyRe.MatchString(k) || !stdValueRe.MatchString(v) || len(digits) > 15 {
			t.Fatalf("malformed metric line %q", sc.Text())
		}
		if _, dup := metrics[k]; dup {
			t.Fatalf("metric %s repeats", k)
		}
		metrics[k] = v
	}
	return checks, metrics
}

func TestStatusFilesFormat(t *testing.T) {
	w := newBeatWorld(t, "latest_version: 1.1.0\n", nil, hub.Options{Version: "1.1.0"})
	mon := w.hub.Server().Monitor()
	w.hub.Register("club-2b", "Второй клуб") // never sends a heartbeat
	w.beat(t, true, "")
	mon.Tick(context.Background())

	checks, metrics := validateStatusFiles(t, w.status)
	for _, name := range []string{"club_demo_club_online", "club_demo_club_ready", "club_club_2b_online", "club_club_2b_ready"} {
		if checks[name] == nil {
			t.Fatalf("missing check %s; have %v", name, checks)
		}
	}
	if d := checks["club_demo_club_online"]["detail"].(string); !strings.Contains(d, "Демо-клуб") || !strings.Contains(d, "version 1.0.0") ||
		!strings.Contains(d, "last heartbeat") {
		t.Fatalf("online detail: %s", d)
	}
	want := map[string]string{
		"clubs_total": "2", "clubs_online": "2", "clubs_ready": "1",
		"club_demo_club_last_seen_age_seconds": "0", "club_demo_club_outdated": "1", "club_club_2b_outdated": "0",
		"check_club_demo_club_online": "1", "status_ok": "1",
	}
	for k, v := range want {
		if metrics[k] != v {
			t.Fatalf("metric %s = %q, want %q; all: %v", k, metrics[k], v, metrics)
		}
	}
	if _, ok := metrics["club_club_2b_last_seen_age_seconds"]; ok {
		t.Fatal("an age for a club never seen")
	}

	// demo-club goes silent, club-2b stays silent: both offline, ok=false
	w.clock.Advance(4 * time.Minute)
	mon.Tick(context.Background())
	checks, metrics = validateStatusFiles(t, w.status)
	if checks["club_demo_club_online"]["ok"] != false || metrics["clubs_online"] != "0" || metrics["status_ok"] != "0" ||
		metrics["club_demo_club_last_seen_age_seconds"] != "240" {
		t.Fatalf("after 4 silent minutes: %v %v", checks, metrics)
	}
	// readiness of an offline club is not a second alarm
	if checks["club_demo_club_ready"]["ok"] != true || !strings.Contains(checks["club_demo_club_ready"]["detail"].(string), "unknown") {
		t.Fatalf("ready while offline: %v", checks["club_demo_club_ready"])
	}

	// nothing secret in either file, and no leftover temporary files
	entries, _ := os.ReadDir(w.status)
	if len(entries) != 2 {
		t.Fatalf("status_dir holds %d entries: %v", len(entries), entries)
	}
	for _, f := range []string{hub.StatusFile, hub.MetricsFile} {
		data, _ := os.ReadFile(filepath.Join(w.status, f))
		for _, s := range []string{w.secret, "cgs_", operatorKey, contract.ReadKey, contract.BookKey} {
			if bytes.Contains(data, []byte(s)) {
				t.Fatalf("%s holds a secret", f)
			}
		}
	}
}

// Readers never see a partial file: every rewrite is a rename.
func TestStatusFilesAtomic(t *testing.T) {
	w := newBeatWorld(t, "", nil, hub.Options{})
	for i := 0; i < 30; i++ {
		w.hub.Register("club-"+string(rune('a'+i%26))+string(rune('a'+i/26)), strings.Repeat("Клуб ", 20))
	}
	mon := w.hub.Server().Monitor()
	mon.Tick(context.Background())
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				w.clock.Advance(time.Second)
				mon.Tick(context.Background())
			}
		}
	}()
	for i := 0; i < 300; i++ {
		data, err := os.ReadFile(filepath.Join(w.status, hub.StatusFile))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(data, &v); err != nil {
			close(stop)
			<-done
			t.Fatalf("read a partial status.json (%d bytes): %v", len(data), err)
		}
		m, _ := os.ReadFile(filepath.Join(w.status, hub.MetricsFile))
		if !bytes.HasSuffix(m, []byte("\n")) || !bytes.Contains(m, []byte("run_last_utc_epoch=")) {
			close(stop)
			<-done
			t.Fatalf("read a partial metrics file: %q", m)
		}
	}
	close(stop)
	<-done
	entries, _ := os.ReadDir(w.status)
	if len(entries) != 2 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestStatusAPI(t *testing.T) {
	w := newBeatWorld(t, "latest_version: 1.1.0\n", nil, hub.Options{})
	w.beat(t, true, "")
	w.hub.Server().Monitor().Tick(context.Background())
	if r := call(t, w.hub, http.MethodGet, "/hub/status", contract.ReadKey, ""); r.status != http.StatusUnauthorized {
		t.Fatalf("status with a partner key: %d", r.status)
	}
	r := call(t, w.hub, http.MethodGet, "/hub/status", operatorKey, "")
	if r.status != http.StatusOK {
		t.Fatalf("status: %d %s", r.status, r.body)
	}
	var st struct {
		Gateways []hub.GatewayStatus `json:"gateways"`
	}
	if err := json.Unmarshal(r.body, &st); err != nil || len(st.Gateways) != 1 {
		t.Fatalf("status body: %s", r.body)
	}
	c := st.Gateways[0]
	if c.Installation != "demo-club" || !c.Connected || c.State != hub.StateOnline || c.Version != "1.0.0" || c.Build != "release" ||
		!c.Outdated || c.Ready == nil || !*c.Ready || c.LastSeen == "" || c.LastHeartbeat == "" {
		t.Fatalf("row: %+v", c)
	}
	if bytes.Contains(r.body, []byte("cgs_")) || bytes.Contains(r.body, []byte("secret")) {
		t.Fatalf("status leaks a secret: %s", r.body)
	}
}

func TestWebhookErrorsDoNotLeakURL(t *testing.T) {
	sink := &hub.WebhookSink{URL: "http://127.0.0.1:1/hook?token=webhook-secret-value", Client: &http.Client{Timeout: time.Second}}
	err := sink.Send(context.Background(), hub.Alert{Text: "x"})
	if err == nil || strings.Contains(err.Error(), "webhook-secret-value") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error: %v", err)
	}
}

func TestHubConfigAlerts(t *testing.T) {
	env := func(k string) string { return hubEnv()[k] }
	// no sink unless a webhook is configured
	s, err := hub.Load([]byte("state_dir: /tmp/x\n"), env)
	if err != nil || s.WebhookURL != "" || len(hub.SinksFromSettings(s, nil)) != 0 {
		t.Fatalf("default sinks: %v %v", s, err)
	}
	// there is no built-in chat sender: any such section is an unknown key
	if _, err := hub.Load([]byte("state_dir: /tmp/x\nalerts:\n  chat_bot: { token_env: X, chat_env: Y }\n"), env); err == nil {
		t.Fatal("a removed alert section was accepted silently")
	}
	if _, err := hub.Load([]byte("state_dir: /tmp/x\nstatus_dir: /tmp/x/\n"), env); err == nil {
		t.Fatal("status_dir equal to state_dir was accepted")
	}
}
