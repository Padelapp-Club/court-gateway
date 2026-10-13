package hub_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Two running copies of one gateway (one secret): the hub serves the
// first and refuses the other with 409 DUPLICATE_GATEWAY — it gets no
// partner request and its heartbeats do not count — and reports it in
// /hub/status and the status files. The other takes over only after the
// first has been silent for longer than offline_after, or at once when
// the first says it stopped (Release).
func TestSecondCopyIsRefused(t *testing.T) {
	w := newBeatWorld(t, "", nil, hub.Options{})
	w.beat(t, true, "")
	var served atomic.Int32
	copyOf := func(instance string) *testkit.Gateway {
		cfg := testkit.ClientConfig(w.hub.URL(), w.hub.Transport(), w.secret, http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			served.Add(1)
			rw.WriteHeader(http.StatusNoContent)
		}), nil)
		cfg.Instance = instance
		return testkit.StartGateway(t, cfg)
	}
	second := copyOf("second-copy")
	testkit.WaitFor(t, "the second copy refused", 10*time.Second, second.Client.Duplicate)
	if second.Client.Connected() {
		t.Fatal("the refused copy counts itself connected")
	}
	if err := second.Client.SendHeartbeat(context.Background(), tunnel.Heartbeat{Ready: false, Reason: "CRM_AUTH_FAILED"}); !errors.Is(err, tunnel.ErrDuplicate) {
		t.Fatalf("heartbeat of the refused copy: %v", err)
	}
	if h := w.hub.Server().Monitor().Health("demo-club"); !h.Ready || !h.Duplicate {
		t.Fatalf("health: %+v", h)
	}
	var st struct {
		Gateways []hub.GatewayStatus `json:"gateways"`
	}
	if err := json.Unmarshal(w.hub.MustOperator(t, http.StatusOK, http.MethodGet, "/hub/status", nil), &st); err != nil || len(st.Gateways) != 1 ||
		!st.Gateways[0].Duplicate || st.Gateways[0].DuplicateSeenAt == "" {
		t.Fatalf("/hub/status: %+v %v", st, err)
	}
	w.hub.Server().Monitor().Tick(context.Background())
	metrics, _ := os.ReadFile(filepath.Join(w.status, hub.MetricsFile))
	status, _ := os.ReadFile(filepath.Join(w.status, hub.StatusFile))
	if !strings.Contains(string(metrics), "check_installation_demo_club_single=0\n") || !strings.Contains(string(status), "a second copy with the same secret") {
		t.Fatalf("status files:\n%s\n%s", metrics, status)
	}

	// the first copy stops without a word (power cut): the second takes
	// over once it has been silent for longer than offline_after
	w.gw.Stop()
	w.clock.Advance(2 * time.Minute)
	time.Sleep(300 * time.Millisecond) // a few more polls of the second copy
	if !second.Client.Duplicate() {
		t.Fatal("took over before offline_after")
	}
	w.clock.Advance(time.Minute + time.Second)
	testkit.WaitFor(t, "the second copy takes over", 10*time.Second, func() bool { return second.Client.Connected() && !second.Client.Duplicate() })
	if served.Load() != 0 {
		t.Fatal("a refused copy served a request")
	}

	// a copy that stops properly releases at once, and is not bound again
	second.Stop()
	if err := second.Client.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a request of the stopped copy that arrives late does not bind it
	if err := second.Client.SendHeartbeat(context.Background(), tunnel.Heartbeat{Ready: true}); !errors.Is(err, tunnel.ErrDuplicate) {
		t.Fatalf("a released copy is bound again: %v", err)
	}
	third := copyOf("third-copy")
	testkit.WaitFor(t, "the next copy is served at once", 10*time.Second, third.Client.Connected)
}
