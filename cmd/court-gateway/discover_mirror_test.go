package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/remote"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// mirrorAccount stands for the Lunda account of a second CRM: one club,
// one court.
type mirrorAccount struct{}

func (mirrorAccount) CheckLogin(context.Context) error { return nil }
func (mirrorAccount) Discover(context.Context, driver.DiscoverQuery) (driver.Discovery, error) {
	return driver.Discovery{Studios: []driver.Studio{{ID: "lunda-club", Name: "Клуб"}}, Studio: "lunda-club", ClubName: "Клуб",
		Courts: []driver.DiscoveredCourt{{Name: "KORT ONE", Params: driver.Params{"lunda_court": "uid-1"}}}}, nil
}

// The installation's gateway answers a discovery of its second CRM with
// that CRM's account, as the hub forwards it.
func TestServeDiscoversTheMirror(t *testing.T) {
	old := drivers.Accounts["lunda"]
	drivers.Accounts["lunda"] = func(driver.Config) (driver.Account, error) { return mirrorAccount{}, nil }
	t.Cleanup(func() { drivers.Accounts["lunda"] = old })
	cfg := "hub:\n  url: https://hub.example.test\n  installation: inst-1\n  key: " + core.EncodeHubKey(make([]byte, 32)) +
		"\nstate_dir: " + filepath.Join(t.TempDir(), "state") + "\ncrm:\n  type: demo\nmirror:\n  type: lunda\n"
	s, err := core.Load([]byte(cfg), func(k string) string { return map[string]string{core.DefaultHubSecretEnv: "secret"}[k] })
	if err != nil {
		t.Fatal(err)
	}
	clubs, _, err := buildInstallation(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	clubs.TunnelHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tunnel.PathDiscover, strings.NewReader(`{"mirror": true}`)))
	var out remote.DiscoverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK || out.Draft == nil ||
		len(out.Draft.Courts) != 1 || out.Draft.Courts[0].Params["lunda_court"] != "uid-1" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
