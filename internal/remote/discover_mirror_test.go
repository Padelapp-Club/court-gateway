package remote_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/remote"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// discoverAccount answers a discovery with one studio and the courts
// given, counting the calls.
type discoverAccount struct {
	studio string
	courts []driver.DiscoveredCourt
	calls  int
}

func (a *discoverAccount) CheckLogin(context.Context) error { return nil }
func (a *discoverAccount) Discover(context.Context, driver.DiscoverQuery) (driver.Discovery, error) {
	a.calls++
	return driver.Discovery{Studios: []driver.Studio{{ID: a.studio, Name: a.studio}}, Studio: a.studio, ClubName: a.studio,
		Courts: a.courts}, nil
}

func discover(t *testing.T, h http.Handler, body string) (int, remote.DiscoverResponse, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tunnel.PathDiscover, strings.NewReader(body)))
	var out remote.DiscoverResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

// The operator discovers the installation's second CRM with mirror: the
// draft is the mirror's, with its courts' keys (lunda_court), read by the
// mirror's account only; without mirror the first CRM answers as before.
func TestDiscoverTheMirror(t *testing.T) {
	primary := &discoverAccount{studio: "viva-studio", courts: []driver.DiscoveredCourt{{Name: "Корт 1", Params: driver.Params{"viva_room": "room-1"}}}}
	mirror := &discoverAccount{studio: "lunda-club", courts: []driver.DiscoveredCourt{{Name: "KORT ONE", Params: driver.Params{"lunda_court": "uid-1"}}}}
	h := remote.DiscoverHandler(primary, mirror, core.NewClubs(core.ClubsOptions{}), nil)

	code, out, raw := discover(t, h, `{"mirror": true}`)
	if code != http.StatusOK || out.Draft == nil || len(out.Draft.Courts) != 1 || out.Draft.Courts[0].Params["lunda_court"] != "uid-1" {
		t.Fatalf("mirror discovery: %d %s", code, raw)
	}
	if primary.calls != 0 || mirror.calls != 1 {
		t.Fatalf("mirror discovery read primary %d, mirror %d times", primary.calls, mirror.calls)
	}
	code, out, raw = discover(t, h, `{}`)
	if code != http.StatusOK || out.Draft == nil || out.Draft.Courts[0].Params["viva_room"] != "room-1" || primary.calls != 1 {
		t.Fatalf("first CRM discovery: %d %s", code, raw)
	}
}

// An installation without a second CRM refuses a mirror discovery by name
// and reads no CRM.
func TestDiscoverTheMirrorWithoutOne(t *testing.T) {
	primary := &discoverAccount{studio: "viva-studio"}
	h := remote.DiscoverHandler(primary, nil, core.NewClubs(core.ClubsOptions{}), nil)
	code, _, raw := discover(t, h, `{"mirror": true}`)
	if code != http.StatusConflict || !strings.Contains(raw, "NO_MIRROR") || primary.calls != 0 {
		t.Fatalf("%d %s, primary read %d", code, raw, primary.calls)
	}
}
