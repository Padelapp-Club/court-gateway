package hub_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

func demoClub(id, studio string, courts ...string) core.ClubSpec {
	c := testkit.Club(id, "Клуб "+id, courts...)
	c.CRM = map[string]any{"studio": studio}
	return c
}

func startOperatorHub(t *testing.T, opts hub.Options) *testkit.Hub {
	t.Helper()
	return testkit.StartHub(t, "poll_wait: 1s\nrequest_timeout: 3s\n"+testkit.OperatorYAML+testkit.HubPartnersYAML, testkit.HubEnv(), opts)
}

func demoInstallation(t *testing.T, h *testkit.Hub, id string, clubs ...core.ClubSpec) *testkit.Installation {
	t.Helper()
	return testkit.StartInstallation(t, h, id, testkit.InstallationOptions{
		CRMYAML: testkit.DemoCRM, CRMType: "demo", Factory: demo.New, Account: demo.NewAccount, Clubs: clubs,
	})
}

func installation(t *testing.T, h *testkit.Hub, id string) hub.InstallationDTO {
	t.Helper()
	var dto hub.InstallationDTO
	if err := json.Unmarshal(h.MustOperator(t, http.StatusOK, http.MethodGet, "/hub/installations/"+id, nil), &dto); err != nil {
		t.Fatal(err)
	}
	return dto
}

// One installation (one CRM account) serves two clubs at once; a third is
// added on the hub and served without a restart; a removed club is gone.
func TestInstallationServesClubsFromHub(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	in := demoInstallation(t, h, "net-1", demoClub("club-north", "demo-north", "Корт 1", "Корт 2"), demoClub("club-south", "demo-south", "Корт A"))

	// both clubs, concurrently (two partners: one partner keeps a poll
	// of the two-poll test gateway free)
	var wg sync.WaitGroup
	for club, key := range map[string]string{"club-north": contract.ReadKey, "club-south": contract.BookKey} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := call(t, h, http.MethodGet, "/v1/clubs/"+club+"/courts", key, "")
			if r.status != http.StatusOK || !strings.Contains(string(r.body), "court-1") {
				t.Errorf("%s: %d %s", club, r.status, r.body)
			}
		}()
	}
	wg.Wait()
	r := call(t, h, http.MethodGet, "/v1/clubs", contract.ReadKey, "")
	if !strings.Contains(string(r.body), "club-north") || !strings.Contains(string(r.body), "club-south") || strings.Contains(string(r.body), "net-1") {
		t.Fatalf("club list: %s", r.body)
	}
	// a booking in one club does not show in the other
	day := time.Now().AddDate(0, 0, 2)
	start := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	body := `{"court":"court-1","start":"` + start.Format(time.RFC3339) + `","end":"` + start.Add(time.Hour).Format(time.RFC3339) + `","kind":"game"}`
	if r := call(t, h, http.MethodPost, "/v1/clubs/club-north/bookings", contract.BookKey, body); r.status != http.StatusCreated {
		t.Fatalf("book north: %d %s", r.status, r.body)
	}
	if r := call(t, h, http.MethodPost, "/v1/clubs/club-south/bookings", contract.BookKey, body); r.status != http.StatusCreated {
		t.Fatalf("the same slot in another club: %d %s", r.status, r.body)
	}

	// a third club: no restart, the gateway pulls it
	h.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/net-1/clubs/club-east", demoClub("club-east", "demo-south", "Корт B"))
	in.WaitServing(t, "club-east", "club-north", "club-south")
	if r := call(t, h, http.MethodGet, "/v1/clubs/club-east/courts", contract.ReadKey, ""); r.status != http.StatusOK || !strings.Contains(string(r.body), "Корт B") {
		t.Fatalf("new club: %d %s", r.status, r.body)
	}
	// changing a club is a new version too
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/net-1/clubs/club-east", demoClub("club-east", "demo-south", "Корт B", "Корт C"))
	testkit.WaitFor(t, "changed club served", 10*time.Second, func() bool {
		r := call(t, h, http.MethodGet, "/v1/clubs/club-east/courts", contract.ReadKey, "")
		return strings.Contains(string(r.body), "Корт C")
	})

	// removed: 404 for partners at once, and the gateway drops it
	h.MustOperator(t, http.StatusOK, http.MethodDelete, "/hub/installations/net-1/clubs/club-south", nil)
	if r := call(t, h, http.MethodGet, "/v1/clubs/club-south/courts", contract.ReadKey, ""); r.status != http.StatusNotFound {
		t.Fatalf("removed club: %d %s", r.status, r.body)
	}
	in.WaitServing(t, "club-east", "club-north")
	h.MustOperator(t, http.StatusNotFound, http.MethodDelete, "/hub/installations/net-1/clubs/club-south", nil)

	// the gateway reports what it runs; the operator sees it
	in.Beat(t, true)
	dto := installation(t, h, "net-1")
	if dto.ConfigVersion != 5 || len(dto.Clubs) != 2 || dto.Gateway == nil || dto.Gateway.AppliedConfigVersion != 5 ||
		strings.Join(dto.Gateway.ClubsServed, ",") != "club-east,club-north" || dto.Gateway.ConfigError != "" {
		t.Fatalf("installation: %+v gateway %+v", dto, dto.Gateway)
	}
}

// A club whose config the gateway's driver refuses is refused on its own:
// the other clubs are served, a refused new club answers 503 (not 404), a
// refused change keeps the club's previous config, and the reason reaches
// the operator per club.
func TestRefusedClubDoesNotStopTheOthers(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	in := demoInstallation(t, h, "net-2", demoClub("club-a", "demo-north", "Корт 1"))

	// valid for the hub (CRM-agnostic checks), invalid for the demo driver
	h.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/net-2/clubs/club-b", demoClub("club-b", "no-such-studio", "Корт 1"))
	h.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/net-2/clubs/club-c", demoClub("club-c", "demo-south", "Корт 1"))
	in.WaitServing(t, "club-a", "club-c")
	st := in.Syncer.State()
	if st.Version != 3 || !strings.Contains(st.ClubErrors["club-b"], "crm.studio") || len(st.ClubErrors) != 1 {
		t.Fatalf("state: %+v", st)
	}
	if r := call(t, h, http.MethodGet, "/v1/clubs/club-b/courts", contract.ReadKey, ""); r.status != http.StatusServiceUnavailable || !strings.Contains(string(r.body), "CLUB_UNAVAILABLE") {
		t.Fatalf("refused club: %d %s", r.status, r.body)
	}
	// a refused change of a served club keeps its previous config
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/net-2/clubs/club-a", demoClub("club-a", "no-such-studio", "Другой корт"))
	testkit.WaitFor(t, "change refused", 10*time.Second, func() bool { return in.Syncer.State().ClubErrors["club-a"] != "" })
	if r := call(t, h, http.MethodGet, "/v1/clubs/club-a/courts", contract.ReadKey, ""); r.status != http.StatusOK || !strings.Contains(string(r.body), "Корт 1") {
		t.Fatalf("previous config of club-a: %d %s", r.status, r.body)
	}
	in.Beat(t, true)
	dto := installation(t, h, "net-2")
	if dto.ConfigVersion != 4 || dto.Gateway.AppliedConfigVersion != 4 || !strings.Contains(dto.Gateway.ClubErrors["club-b"], "crm.studio") ||
		!strings.Contains(dto.Gateway.ClubErrors["club-a"], "crm.studio") || !strings.Contains(dto.Gateway.ConfigError, "club-b") {
		t.Fatalf("operator view: %+v", dto.Gateway)
	}
	// fixed: served, and the errors clear
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/net-2/clubs/club-a", demoClub("club-a", "demo-north", "Корт 1"))
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/net-2/clubs/club-b", demoClub("club-b", "demo-south", "Корт 1"))
	in.WaitServing(t, "club-a", "club-b", "club-c")
	testkit.WaitFor(t, "errors cleared", 10*time.Second, func() bool { st := in.Syncer.State(); return st.Version == 6 && st.Error == "" })
}

// The hub refuses a club that sets installation keys of the crm section:
// the login and the CRM endpoints belong to the gateway's own config.
func TestHubRefusesInstallationKeysInAClub(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	h.MustOperator(t, http.StatusCreated, http.MethodPost, "/hub/installations", map[string]string{"installation_id": "net-k", "name": "Сеть"})
	for _, key := range []string{"auth_url", "api_url", "username"} {
		c := demoClub("club-k", "demo-north", "Корт 1")
		c.CRM[key] = "https://crm.example.test/x"
		code, body := h.Operator(t, testkit.OperatorKey, http.MethodPut, "/hub/installations/net-k/clubs/club-k", c)
		if code != http.StatusBadRequest || !strings.Contains(string(body), "INVALID_CLUB_CONFIG") || !strings.Contains(string(body), "crm."+key) {
			t.Fatalf("crm.%s: %d %s", key, code, body)
		}
	}
}

// Discovery runs on the gateway through its tunnel and returns a draft
// that is a valid club once it has an id. Nothing is stored.
func TestDiscoveryReturnsDraft(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	h.MustOperator(t, http.StatusNotFound, http.MethodPost, "/hub/installations/nope/discover", map[string]any{})
	in := demoInstallation(t, h, "net-3", demoClub("club-north", "demo-north", "Корт 1"))
	_ = in

	var resp struct {
		Studios []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
			Club    string `json:"club_id"`
		} `json:"studios"`
		Draft   *core.ClubSpec `json:"draft"`
		Notices []core.Notice  `json:"notices"`
	}
	_ = json.Unmarshal(h.MustOperator(t, http.StatusOK, http.MethodPost, "/hub/installations/net-3/discover", map[string]any{}), &resp)
	if len(resp.Studios) != 2 || resp.Draft != nil || resp.Notices[0].Code != "STUDIO_REQUIRED" {
		t.Fatalf("without a studio: %+v", resp)
	}
	if !resp.Studios[0].Enabled || resp.Studios[0].Club != "club-north" || resp.Studios[1].Enabled {
		t.Fatalf("enabled studios: %+v", resp.Studios)
	}
	resp.Draft = nil
	_ = json.Unmarshal(h.MustOperator(t, http.StatusOK, http.MethodPost, "/hub/installations/net-3/discover", map[string]any{"studio": "demo-south"}), &resp)
	if resp.Draft == nil || len(resp.Draft.Courts) != 3 || resp.Draft.CRM["studio"] != "demo-south" || resp.Draft.Name != "Демо Юг" {
		t.Fatalf("draft: %+v", resp.Draft)
	}
	if v := installation(t, h, "net-3").ConfigVersion; v != 1 {
		t.Fatalf("discovery stored something: version %d", v)
	}
	// the draft, given an id, is accepted and served
	h.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/net-3/clubs/club-south", resp.Draft)
	in.WaitServing(t, "club-north", "club-south")
	// bad input and an unknown studio
	h.MustOperator(t, http.StatusBadRequest, http.MethodPost, "/hub/installations/net-3/discover", map[string]any{"bogus": 1})
	h.MustOperator(t, http.StatusBadGateway, http.MethodPost, "/hub/installations/net-3/discover", map[string]any{"studio": "nope"})
	// a gateway that is not connected
	in.Gateway.Stop()
	testkit.WaitFor(t, "gateway gone", 10*time.Second, func() bool { return !h.Server().Broker().Connected("net-3") })
	h.MustOperator(t, http.StatusServiceUnavailable, http.MethodPost, "/hub/installations/net-3/discover", map[string]any{})
}

func TestOperatorAPIAuthAndValidation(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	paths := []struct{ method, path string }{
		{http.MethodGet, "/hub/status"},
		{http.MethodPost, "/hub/installations"},
		{http.MethodGet, "/hub/installations/x1"},
		{http.MethodDelete, "/hub/installations/x1"},
		{http.MethodPost, "/hub/installations/x1/join-codes"},
		{http.MethodPut, "/hub/installations/x1/clubs/c1"},
		{http.MethodDelete, "/hub/installations/x1/clubs/c1"},
		{http.MethodPost, "/hub/installations/x1/discover"},
	}
	for _, p := range paths {
		for _, key := range []string{"", "wrong-key-wrong-key-wrong-key", contract.ReadKey} {
			if code, body := h.Operator(t, key, p.method, p.path, "{}"); code != http.StatusUnauthorized {
				t.Fatalf("%s %s with key %q: %d %s", p.method, p.path, key, code, body)
			}
		}
	}
	// a hub without an operator key refuses everything
	h2 := testkit.StartHub(t, "poll_wait: 1s\n"+testkit.HubPartnersYAML, contract.KeyEnv(), hub.Options{})
	if code, _ := h2.Operator(t, testkit.OperatorKey, http.MethodPost, "/hub/installations", map[string]string{"installation_id": "x1"}); code != http.StatusUnauthorized {
		t.Fatalf("no operator key configured: %d", code)
	}

	bad := func(want int, code string, method, path string, body any) {
		t.Helper()
		got, data := h.Operator(t, testkit.OperatorKey, method, path, body)
		if got != want || !strings.Contains(string(data), `"`+code+`"`) {
			t.Fatalf("%s %s %v: %d %s, want %d %s", method, path, body, got, data, want, code)
		}
	}
	for _, id := range []string{"Bad Id", "a", "-x", strings.Repeat("x", 64), "x_y"} {
		bad(400, "INVALID_ID", http.MethodPost, "/hub/installations", map[string]string{"installation_id": id})
	}
	bad(400, "INVALID_NAME", http.MethodPost, "/hub/installations", map[string]string{"installation_id": "x1", "name": "a\x00b"})
	bad(400, "INVALID_REQUEST", http.MethodPost, "/hub/installations", `{"installation_id":"x1","club_id":"y"}`)
	bad(400, "INVALID_REQUEST", http.MethodPost, "/hub/installations", `not json`)
	h.MustOperator(t, http.StatusCreated, http.MethodPost, "/hub/installations", map[string]string{"installation_id": "x1", "name": "Сеть"})
	bad(409, "INSTALLATION_EXISTS", http.MethodPost, "/hub/installations", map[string]string{"installation_id": "x1"})
	bad(404, "NOT_FOUND", http.MethodGet, "/hub/installations/x2", nil)
	bad(404, "NOT_FOUND", http.MethodPost, "/hub/installations/x2/join-codes", map[string]string{})
	for _, ttl := range []string{"-1h", "0s", "721h", "soon"} {
		bad(400, "INVALID_TTL", http.MethodPost, "/hub/installations/x1/join-codes", map[string]string{"ttl": ttl})
	}
	// club configs
	good := demoClub("c1", "demo-north", "Корт 1")
	bad(404, "NOT_FOUND", http.MethodPut, "/hub/installations/x2/clubs/c1", good)
	broken := good
	broken.Timezone, broken.SlotMinutes = "Mars/Base", 7
	got, data := h.Operator(t, testkit.OperatorKey, http.MethodPut, "/hub/installations/x1/clubs/c1", broken)
	if got != 400 || !strings.Contains(string(data), "INVALID_CLUB_CONFIG") || !strings.Contains(string(data), "timezone: неизвестный") || !strings.Contains(string(data), "slot_minutes: 7") {
		t.Fatalf("invalid club: %d %s", got, data)
	}
	bad(400, "INVALID_REQUEST", http.MethodPut, "/hub/installations/x1/clubs/c1", `{"name":"x","venue":{}}`)
	other := good
	other.ID = "c9"
	bad(400, "INVALID_REQUEST", http.MethodPut, "/hub/installations/x1/clubs/c1", other)
	bad(400, "INVALID_ID", http.MethodPut, "/hub/installations/x1/clubs/C1", demoClub("", "demo-north", "Корт 1"))
	h.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/x1/clubs/c1", good)
	h.MustOperator(t, http.StatusCreated, http.MethodPost, "/hub/installations", map[string]string{"installation_id": "x2"})
	bad(409, "CLUB_TAKEN", http.MethodPut, "/hub/installations/x2/clubs/c1", good)
	// one CRM studio, two installations: two gateways would serve it
	bad(409, "STUDIO_TAKEN", http.MethodPut, "/hub/installations/x2/clubs/c2", demoClub("c2", "DEMO-NORTH", "Корт 1"))
	h.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/x2/clubs/c2", demoClub("c2", "demo-south", "Корт 1"))
	bad(404, "NOT_FOUND", http.MethodDelete, "/hub/installations/x1/clubs/c3", nil)
	// the operator's view
	dto := installation(t, h, "x1")
	if dto.Name != "Сеть" || dto.ConfigVersion != 1 || len(dto.Clubs) != 1 || dto.Clubs[0].ID != "c1" || dto.Gateway != nil {
		t.Fatalf("installation: %+v", dto)
	}
}

// Join codes from the operator API: single use, expiry, stored hashed;
// a second code replaces the secret (machine replacement); deleting the
// installation revokes its gateway and its pending codes.
func TestOperatorJoinCodes(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h := startOperatorHub(t, hub.Options{Now: clock})
	ctx := context.Background()
	h.MustOperator(t, http.StatusCreated, http.MethodPost, "/hub/installations", map[string]string{"installation_id": "inst-1", "name": "Сеть Один"})
	newCode := func(ttl string) hub.JoinCodeDTO {
		var jc hub.JoinCodeDTO
		body := map[string]string{}
		if ttl != "" {
			body["ttl"] = ttl
		}
		if err := json.Unmarshal(h.MustOperator(t, http.StatusCreated, http.MethodPost, "/hub/installations/inst-1/join-codes", body), &jc); err != nil {
			t.Fatal(err)
		}
		return jc
	}
	jc := newCode("")
	exp, err := time.Parse(time.RFC3339, jc.ExpiresAt)
	if err != nil || jc.Installation != "inst-1" || len(hub.NormalizeJoinCode(jc.Code)) != 16 || exp.Sub(now) < 23*time.Hour || exp.Sub(now) > 24*time.Hour+time.Second {
		t.Fatalf("code: %+v %v", jc, err)
	}
	rr, err := tunnel.Register(ctx, h.Transport(), h.URL(), jc.Code, "1.0.0")
	if err != nil || rr.Installation != "inst-1" || rr.Name != "Сеть Один" {
		t.Fatalf("register: %+v %v", rr, err)
	}
	if _, err := tunnel.Register(ctx, h.Transport(), h.URL(), jc.Code, ""); !errors.Is(err, tunnel.ErrJoinCodeRejected) {
		t.Fatalf("second use: %v", err)
	}
	// expiry
	short := newCode("1h")
	mu.Lock()
	now = now.Add(time.Hour + time.Second)
	mu.Unlock()
	if _, err := tunnel.Register(ctx, h.Transport(), h.URL(), short.Code, ""); !errors.Is(err, tunnel.ErrJoinCodeRejected) {
		t.Fatalf("expired code: %v", err)
	}
	// a new code for an installation that has a gateway: the new
	// registration replaces the secret, the old gateway gets 401
	rr2, err := tunnel.Register(ctx, h.Transport(), h.URL(), newCode("").Code, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tunnel.FetchWhoami(ctx, h.Transport(), h.URL(), rr.Secret); !errors.Is(err, tunnel.ErrUnauthorized) {
		t.Fatalf("old secret still works: %v", err)
	}
	who, err := tunnel.FetchWhoami(ctx, h.Transport(), h.URL(), rr2.Secret)
	if err != nil || who.Installation != "inst-1" {
		t.Fatalf("new secret: %+v %v", who, err)
	}
	assertNoPlaintext(t, h.StateDir, []string{rr2.Secret, hub.NormalizeJoinCode(jc.Code)})

	// delete: the gateway loses access, pending codes die, GET is 404
	pending := newCode("")
	h.MustOperator(t, http.StatusNoContent, http.MethodDelete, "/hub/installations/inst-1", nil)
	if _, err := tunnel.FetchWhoami(ctx, h.Transport(), h.URL(), rr2.Secret); !errors.Is(err, tunnel.ErrUnauthorized) {
		t.Fatalf("deleted installation's secret: %v", err)
	}
	if _, err := tunnel.Register(ctx, h.Transport(), h.URL(), pending.Code, ""); !errors.Is(err, tunnel.ErrJoinCodeRejected) {
		t.Fatalf("pending code of a deleted installation: %v", err)
	}
	h.MustOperator(t, http.StatusNotFound, http.MethodGet, "/hub/installations/inst-1", nil)
	h.MustOperator(t, http.StatusNotFound, http.MethodDelete, "/hub/installations/inst-1", nil)
}

// The installation and its clubs survive a hub restart.
func TestInstallationsSurviveRestart(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	in := demoInstallation(t, h, "net-r", demoClub("club-r", "demo-north", "Корт 1"))
	h.Restart()
	testkit.WaitFor(t, "reconnected", 15*time.Second, func() bool { return h.Server().Broker().Connected("net-r") })
	if r := call(t, h, http.MethodGet, "/v1/clubs/club-r/courts", contract.ReadKey, ""); r.status != http.StatusOK {
		t.Fatalf("after restart: %d %s", r.status, r.body)
	}
	if st := in.Syncer.State(); st.Version != 1 {
		t.Fatalf("state: %+v", st)
	}
}
