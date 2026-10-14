package doctor

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// hubKey signs the club config slowHub serves; gatewayYAML pins it.
var (
	hubKey      = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	gatewayYAML = "hub: { url: https://hub.example.test, installation: test-net, key: " +
		core.EncodeHubKey(hubKey.Public().(ed25519.PublicKey)) + " }\ncrm: { type: demo }\n"
)

// slowHub answers the hub endpoints after a delay.
type slowHub time.Duration

func (d slowHub) RoundTrip(r *http.Request) (*http.Response, error) {
	time.Sleep(time.Duration(d))
	body := ""
	switch r.URL.Path {
	case "/health/live":
		body = `{"status":"live"}`
	case "/tunnel/whoami":
		body = `{"installation_id":"inst-1","connected":true,"config_version":1,"clubs":["club-a"],"applied_config_version":1,"clubs_served":["club-a"]}`
	case "/tunnel/config":
		rc := tunnel.RemoteConfig{Version: 1, Clubs: []json.RawMessage{[]byte(`{"id":"club-a","name":"A","timezone":"Europe/Moscow","slot_minutes":30,` +
			`"hours":{"mon-sun":"07:00-23:00"},"crm":{"studio":"demo-north"},"courts":[{"id":"court-1","name":"C1"}]}`)}}
		rc.Sign(hubKey, "test-net", tunnel.RegistrationOf(tunnel.SecretSHA256("cgs_x")), time.Now())
		b, _ := json.Marshal(rc)
		body = string(b)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// slowAccount logs in after a delay.
type slowAccount struct {
	driver.Account
	delay time.Duration
}

func (a slowAccount) CheckLogin(ctx context.Context) error {
	select {
	case <-time.After(a.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ctxDriver fails a read whose context has already ended, as a real CRM
// client does.
type ctxDriver struct{ driver.Driver }

func (d ctxDriver) Occupancy(ctx context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	if err := ctx.Err(); err != nil {
		return driver.Occupancy{}, err
	}
	return d.Driver.Occupancy(ctx, q)
}

// A slow login followed by slow hub checks does not use up the occupancy
// check's time: each step has its own timeout.
func TestDoctorStepsHaveTheirOwnTimeouts(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(cfg, []byte(gatewayYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := Run(context.Background(), Options{
		ConfigPath: cfg, Getenv: func(k string) string { return map[string]string{"GATEWAY_HUB_SECRET": "cgs_x"}[k] },
		Factories: map[string]driver.Factory{"demo": func(c driver.Config) (driver.Driver, error) {
			d, err := demo.New(c)
			return ctxDriver{d}, err
		}},
		Accounts: map[string]driver.AccountFactory{"demo": func(c driver.Config) (driver.Account, error) {
			a, err := demo.NewAccount(c)
			return slowAccount{a, 400 * time.Millisecond}, err
		}},
		Version: "1.0.0", HubTransport: slowHub(300 * time.Millisecond), Timeout: time.Second,
	})
	for _, c := range rep.Checks {
		if c.Status == Fail {
			t.Errorf("%s failed: %s", c.Code, c.Message)
		}
		if c.Code == CodeOccupancy && c.Status != OK {
			t.Errorf("occupancy: %s %s", c.Status, c.Message)
		}
	}
}

// downHub is a hub that cannot be reached.
type downHub struct{}

func (downHub) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

// While the hub is unreachable, the occupancy check says so, not that the
// operator has not connected clubs yet.
func TestDoctorOccupancyWhileTheHubIsUnreachable(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(cfg, []byte(gatewayYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := Run(context.Background(), Options{
		ConfigPath: cfg, Getenv: func(k string) string { return map[string]string{"GATEWAY_HUB_SECRET": "cgs_x"}[k] },
		Factories: map[string]driver.Factory{"demo": demo.New}, Accounts: map[string]driver.AccountFactory{"demo": demo.NewAccount},
		Version: "1.0.0", HubTransport: downHub{}, Timeout: time.Second,
	})
	for _, c := range rep.Checks {
		if c.Code == CodeOccupancy && (c.Status != Skip || !strings.Contains(c.Message, "хаб недоступен")) {
			t.Errorf("occupancy: %s %s", c.Status, c.Message)
		}
	}
}

// doctor reports the version the service runs, not the installed file's.
func TestDoctorReportsTheRunningVersion(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(cfg, []byte(gatewayYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return map[string]string{"GATEWAY_HUB_SECRET": "cgs_x"}[k] }
	if err := core.WriteHealthStatus(core.StateDir(cfg, getenv), core.HealthStatus{Version: "1.3.0"}); err != nil {
		t.Fatal(err)
	}
	rep := Run(context.Background(), Options{
		ConfigPath: cfg, Getenv: getenv,
		Factories: map[string]driver.Factory{"demo": demo.New}, Accounts: map[string]driver.AccountFactory{"demo": demo.NewAccount},
		Version: "1.0.0", HubTransport: slowHub(0), Timeout: time.Second,
	})
	if rep.Version != "1.3.0" {
		t.Fatalf("report version %s, want the running 1.3.0", rep.Version)
	}
}
