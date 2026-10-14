package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/remote"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// OperatorKey is the operator key of hubs started with OperatorYAML.
const OperatorKey = "test-operator-key-0123456789abcdef"

// HubPartnersYAML gives the contract suite's three keys to a hub, with
// access to every club.
const HubPartnersYAML = `partners:
  - { name: reader, key_env: TEST_READ_KEY, scopes: [read], clubs: ["*"] }
  - { name: booker, key_env: TEST_BOOK_KEY, scopes: [read, book], clubs: ["*"] }
  - { name: other, key_env: TEST_OTHER_KEY, scopes: [read, book], clubs: ["*"] }
`

// OperatorYAML enables the operator API with OperatorKey (put
// HUB_OPERATOR_KEY into the hub env, see HubEnv).
const OperatorYAML = "operator_key_env: HUB_OPERATOR_KEY\n"

// HubEnv is the contract keys plus the operator key.
func HubEnv() map[string]string {
	env := contract.KeyEnv()
	env["HUB_OPERATOR_KEY"] = OperatorKey
	return env
}

// Operator calls the hub's operator API with key; body is marshalled to
// JSON unless it is a string or nil.
func (h *Hub) Operator(t *testing.T, key, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	resp, data := h.Do(t, 90*time.Second, method, path, key, rd)
	return resp.StatusCode, data
}

// Do sends a request to the hub, with key as a Bearer token (if any) and
// body as JSON (if any), and returns the answer with its body read.
func (h *Hub) Do(t *testing.T, timeout time.Duration, method, path, key string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, h.URL()+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: h.Transport(), Timeout: timeout}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// MustOperator is Operator that fails the test unless the status is want.
func (h *Hub) MustOperator(t *testing.T, want int, method, path string, body any) []byte {
	t.Helper()
	code, data := h.Operator(t, OperatorKey, method, path, body)
	if code != want {
		t.Fatalf("%s %s: %d, want %d: %s", method, path, code, want, data)
	}
	return data
}

// Installation is a gateway that serves the clubs configured on the hub
// for its installation, as `serve` runs it.
type Installation struct {
	Clubs   *core.Clubs
	Syncer  *remote.Syncer
	Gateway *Gateway
	Logs    *contract.LogBuffer
}

// InstallationOptions configure StartInstallation.
type InstallationOptions struct {
	// CRMYAML is the crm section of gateway.yaml (no club).
	CRMYAML string
	// Env is the gateway's environment (CRM_PASSWORD...).
	Env     map[string]string
	CRMType string
	Factory driver.Factory
	Account driver.AccountFactory
	Hook    func(*driver.Config)
	Clubs   []core.ClubSpec
}

// StartInstallation creates installation id on h through the operator API,
// stores o.Clubs, redeems a join code and connects a gateway that keeps
// its clubs in sync with the hub. It waits until every club is served.
func StartInstallation(t *testing.T, h *Hub, id string, o InstallationOptions) *Installation {
	t.Helper()
	h.MustOperator(t, http.StatusCreated, http.MethodPost, "/hub/installations", map[string]string{"installation_id": id, "name": "Сеть " + id})
	for _, c := range o.Clubs {
		h.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/"+id+"/clubs/"+c.ID, c)
	}
	var jc struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(h.MustOperator(t, http.StatusCreated, http.MethodPost, "/hub/installations/"+id+"/join-codes", map[string]string{}), &jc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rr, err := tunnel.Register(ctx, h.Transport(), h.URL(), jc.Code, "test")
	if err != nil || rr.Installation != id {
		t.Fatalf("register: %+v %v", rr, err)
	}
	env := map[string]string{}
	for k, v := range o.Env {
		env[k] = v
	}
	env[core.DefaultHubSecretEnv] = rr.Secret
	base, err := core.Load([]byte("hub: { url: "+h.URL()+", installation: "+id+", key: "+rr.HubKey+" }\n"+o.CRMYAML), func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("installation config: %v", err)
	}
	logs := &contract.LogBuffer{}
	log := core.NewLogger(logs, slog.LevelDebug, base.Secrets)
	base.Driver.Logger = log
	if o.Hook != nil {
		o.Hook(&base.Driver)
	}
	var acct driver.Account
	if o.Account != nil {
		if acct, err = o.Account(base.Driver); err != nil {
			t.Fatalf("account: %v", err)
		}
		base.Driver.Account = acct
	}
	co := core.ClubsOptions{Logger: log}
	if acct != nil {
		co.ReadyNoClubs = acct.CheckLogin
	}
	clubs := core.NewClubs(co)
	clubs.SetGatewayAPI(remote.DiscoverHandler(acct, clubs, log))
	syncer := remote.New(remote.Options{Base: base, Factories: map[string]driver.Factory{o.CRMType: o.Factory}, Clubs: clubs,
		Logger: log, DriverHook: o.Hook, Retry: 100 * time.Millisecond})
	in := &Installation{Clubs: clubs, Syncer: syncer, Logs: logs}
	cfg := h.ClientConfig(id, rr.Secret, clubs.TunnelHandler(), log)
	cfg.OnConfigVersion = syncer.Notify
	in.Gateway = StartGateway(t, cfg)
	syncer.SetFetch(in.Gateway.Client.FetchConfig)
	sctx, scancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); syncer.Run(sctx) }()
	t.Cleanup(func() { scancel(); <-done })
	WaitFor(t, "installation connected", 10*time.Second, func() bool {
		who, err := FetchWhoami(context.Background(), h.Transport(), h.URL(), rr.Secret)
		return in.Gateway.Client.Connected() && err == nil && who.Connected
	})
	in.WaitServing(t, core.SortedClubIDs(o.Clubs)...)
	return in
}

// WaitServing waits until the gateway serves exactly ids.
func (in *Installation) WaitServing(t *testing.T, ids ...string) {
	t.Helper()
	want := strings.Join(ids, ",")
	WaitFor(t, "gateway serves "+want, 10*time.Second, func() bool {
		return strings.Join(in.Clubs.IDs(), ",") == want
	})
}

// Beat sends one heartbeat with the syncer's state, as serve does.
func (in *Installation) Beat(t *testing.T, ready bool) {
	t.Helper()
	hb := tunnel.Heartbeat{Version: "1.0.0", Build: tunnel.BuildRelease, Ready: ready, CheckedAt: time.Now(), AppliedConfig: in.Syncer.State()}
	if !ready {
		hb.Reason = "CRM_UNREACHABLE"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.Gateway.Client.SendHeartbeat(ctx, hb); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

// Forwarder returns a handler that sends every request over real HTTPS to
// the hub, for running the contract suite against the hub.
func (h *Hub) Forwarder(t *testing.T) http.Handler {
	client := &http.Client{Transport: h.Transport(), Timeout: 60 * time.Second}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := http.NewRequest(r.Method, h.URL()+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			t.Errorf("forward: %v", err)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("forward: %v", err)
			w.WriteHeader(599)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

// DemoCRM is the crm section of a demo installation.
const DemoCRM = "crm: { type: demo }\n"

// Club is a club spec with courts that carry no driver keys, open
// 07:00-23:00 every day.
func Club(id, name string, courts ...string) core.ClubSpec {
	c := core.ClubSpec{ID: id, Name: name, Timezone: "Europe/Moscow", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "07:00-23:00"}}
	for i, n := range courts {
		c.Courts = append(c.Courts, core.CourtSpec{ID: fmt.Sprintf("court-%d", i+1), Name: n})
	}
	return c
}
