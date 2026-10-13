package doctor_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/doctor"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

const (
	studio      = "00000000-0000-4000-8000-000000000001"
	otherStudio = "00000000-0000-4000-8000-000000000002"
	room        = "00000000-0000-4000-8000-000000000011"
)

// doctorAgainst runs doctor for a Viva installation whose hub serves cfg
// (one club, club-a) and reports who about the running service.
func doctorAgainst(t *testing.T, clubStudio string, who tunnel.Whoami) doctor.Report {
	t.Helper()
	stub := vivastub.New("review@example.test", "fixture-password", studio)
	stub.AddRoom(room, "Court")
	crm := httptest.NewServer(stub)
	t.Cleanup(crm.Close)
	spec := core.ClubSpec{ID: "club-a", Name: "Club A", Timezone: "Etc/GMT-3", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "00:00-24:00"},
		CRM: map[string]any{"studio": clubStudio}, Courts: []core.CourtSpec{{ID: "court-1", Name: "Court", Params: map[string]any{"viva_room": room}}}}
	raw, _ := json.Marshal(spec)
	who.Installation, who.Connected, who.ConfigVersion, who.Clubs = "inst-a", true, 1, []string{"club-a"}
	pub, key, _ := ed25519.GenerateKey(nil)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/live":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "live"})
		case tunnel.PathWhoami:
			_ = json.NewEncoder(w).Encode(who)
		case tunnel.PathConfig:
			rc := tunnel.RemoteConfig{Version: 1, Clubs: []json.RawMessage{raw}}
			rc.Sign(key, "inst-a", tunnel.RegistrationOf(tunnel.SecretSHA256("fixture-hub-secret")), time.Now())
			_ = json.NewEncoder(w).Encode(rc)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(hub.Close)
	p := filepath.Join(t.TempDir(), "gateway.yaml")
	yaml := fmt.Sprintf("hub: {url: %s, installation: inst-a, key: %s}\ncrm: {type: viva, username: review@example.test, auth_url: %s/token, api_url: %s/api/v1}\n",
		hub.URL, core.EncodeHubKey(pub), crm.URL, crm.URL)
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CRM_PASSWORD": "fixture-password", "GATEWAY_HUB_SECRET": "fixture-hub-secret"}
	return doctor.Run(context.Background(), doctor.Options{ConfigPath: p, Getenv: func(k string) string { return env[k] },
		Factories: drivers.Factories, Accounts: drivers.Accounts, Version: "1.0.0", Timeout: 10 * time.Second})
}

func hubConfig(rep doctor.Report) doctor.Check {
	for _, c := range rep.Checks {
		if c.Code == doctor.CodeHubConfig {
			return c
		}
	}
	return doctor.Check{}
}

// doctor does not call a club config fine when the service refused it or
// does not serve it, whatever the versions say, nor when the CRM does not
// match it.
func TestDoctorReportsAConfigTheServiceDoesNotRun(t *testing.T) {
	served := func(ids ...string) tunnel.Whoami { return tunnel.Whoami{AppliedConfigVersion: 1, ClubsServed: ids} }
	refused := served("club-a") // the previous config of the club kept
	refused.ConfigError = "клуб «club-a»: комната не из студии"
	for name, tc := range map[string]struct {
		studio string
		who    tunnel.Whoami
		want   doctor.Status
	}{
		"fine":                        {studio, served("club-a"), doctor.OK},
		"refused at the same version": {studio, refused, doctor.Fail},
		"not served":                  {studio, served(), doctor.Fail},
		"studio not in the account":   {otherStudio, served("club-a"), doctor.Fail},
	} {
		t.Run(name, func(t *testing.T) {
			rep := doctorAgainst(t, tc.studio, tc.who)
			if c := hubConfig(rep); c.Status != tc.want || rep.OK != (tc.want == doctor.OK) {
				t.Fatalf("HUB_CONFIG %s (%s), ok=%v; want %s", c.Status, c.Message, rep.OK, tc.want)
			}
		})
	}
}
