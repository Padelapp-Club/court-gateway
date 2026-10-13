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
// /hub/status and the status files, for as long as the first keeps its
// polls open. The other takes over as soon as the first stops polling
// (a crash closes its connections), or at once when the first says it
// stopped (Release).
func TestSecondCopyIsRefused(t *testing.T) {
	w := newBeatWorld(t, "", nil, hub.Options{})
	w.beat(t, true, "")
	var served atomic.Int32
	copyOf := func(instance string) *testkit.Gateway {
		cfg := w.hub.ClientConfig("demo-club", w.secret, http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
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
	w.hub.Server().Monitor().Tick()
	metrics, _ := os.ReadFile(filepath.Join(w.status, hub.MetricsFile))
	status, _ := os.ReadFile(filepath.Join(w.status, hub.StatusFile))
	if !strings.Contains(string(metrics), "check_installation_demo_club_single=0\n") || !strings.Contains(string(status), "a second copy with the same secret") {
		t.Fatalf("status files:\n%s\n%s", metrics, status)
	}

	// while the first copy polls — across several of its poll cycles — the
	// second stays refused, whatever the clock says
	w.clock.Advance(time.Hour)
	time.Sleep(2500 * time.Millisecond)
	if !second.Client.Duplicate() || second.Client.Connected() {
		t.Fatal("the second copy took over from one that still polls")
	}

	// the first copy stops without a word (killed: no release): its polls
	// close, and the second takes over within seconds
	w.gw.Stop()
	stopped := time.Now()
	testkit.WaitFor(t, "the second copy takes over", 10*time.Second, func() bool { return second.Client.Connected() && !second.Client.Duplicate() })
	if took := time.Since(stopped); took > 5*time.Second {
		t.Fatalf("took over after %s", took)
	}
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
