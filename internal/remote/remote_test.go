package remote_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/remote"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

const (
	studio = "00000000-0000-4000-8000-0000000000aa"
	room   = "00000000-0000-4000-8000-0000000000b1"
)

func installation(t *testing.T, gatewayYAML string, env map[string]string) (*core.Clubs, *remote.Syncer) {
	t.Helper()
	s, err := core.Load([]byte(gatewayYAML), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	clubs := core.NewClubs(core.ClubsOptions{})
	return clubs, remote.New(remote.Options{Base: s, Factories: drivers.Factories, Clubs: clubs})
}

func config(t *testing.T, version int64, clubs ...map[string]any) tunnel.RemoteConfig {
	rc := tunnel.RemoteConfig{Version: version}
	for _, c := range clubs {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		rc.Clubs = append(rc.Clubs, raw)
	}
	return rc
}

func club(id string, crm map[string]any) map[string]any {
	return map[string]any{
		"id": id, "name": id, "timezone": "Europe/Moscow", "slot_minutes": 30,
		"hours": map[string]string{"mon-sun": "07:00-23:00"}, "crm": crm,
		"courts": []map[string]any{{"id": "court-1", "name": "C1", "viva_room": room}},
	}
}

// A club pushed by the hub must not redirect the installation's CRM login:
// a club that sets crm.auth_url (or any installation key) is refused, the
// reason is reported, and the local password never reaches that address.
func TestHubClubCannotRedirectTheCRMLogin(t *testing.T) {
	var hits atomic.Int32
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer capture.Close()
	gw := "hub:\n  url: https://hub.example.test\ncrm:\n  type: viva\n  username: admin@club.test\n"
	clubs, syn := installation(t, gw, map[string]string{"CRM_PASSWORD": "local-only-password-42", "GATEWAY_HUB_SECRET": "cgs_x"})
	for _, key := range []string{"auth_url", "api_url", "username"} {
		err := syn.Apply(context.Background(), config(t, 1, club("club-a", map[string]any{"studio": studio, key: capture.URL + "/token"})))
		if err == nil || !strings.Contains(err.Error(), "crm."+key) {
			t.Fatalf("crm.%s: applied: %v", key, err)
		}
		if st := syn.State(); !strings.Contains(st.ClubErrors["club-a"], "crm."+key) || len(st.Clubs) != 0 {
			t.Fatalf("crm.%s: state %+v", key, st)
		}
		_ = clubs.Ready(context.Background())
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the hub-chosen address received %d requests", n)
	}
}

// Until the first config from the hub is applied, a club request is "come
// back later", not "no such club"; a club refused by the driver is 503
// too, while the other clubs are served.
func TestClubsAnswer503UntilConfigured(t *testing.T) {
	gw := "hub:\n  url: https://hub.example.test\ncrm:\n  type: demo\n"
	clubs, syn := installation(t, gw, map[string]string{"GATEWAY_HUB_SECRET": "cgs_x"})
	get := func(club string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "/v1/clubs/"+club+"/courts", nil)
		r = r.WithContext(auth.WithPartnerName(r.Context(), "reader"))
		w := httptest.NewRecorder()
		clubs.TunnelHandler().ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := get("club-a"); code != http.StatusServiceUnavailable || !strings.Contains(body, "CONFIG_PENDING") {
		t.Fatalf("before the first config: %d %s", code, body)
	}
	demo := func(id, studio string) map[string]any {
		c := club(id, map[string]any{"studio": studio})
		c["courts"] = []map[string]any{{"id": "court-1", "name": "C1"}}
		return c
	}
	if err := syn.Apply(context.Background(), config(t, 1, demo("club-a", "demo-north"), demo("club-b", "no-such-studio"))); err == nil {
		t.Fatal("a refused club was not reported")
	}
	if code, body := get("club-a"); code != http.StatusOK {
		t.Fatalf("valid club: %d %s", code, body)
	}
	if code, body := get("club-b"); code != http.StatusServiceUnavailable || !strings.Contains(body, "CLUB_UNAVAILABLE") {
		t.Fatalf("refused club: %d %s", code, body)
	}
	if code, _ := get("club-z"); code != http.StatusNotFound {
		t.Fatalf("unknown club: %d", code)
	}
}
