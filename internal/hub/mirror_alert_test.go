package hub_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// The text of a reconciliation alarm — what it counted and what the
// operator is to do — travels with the heartbeat (reason_detail) and is
// shown by the hub (status files, /hub/status), not the code alone. Only
// the alarm's Detail goes; the rest of the alert (Problems) and any other
// error's words stay on the gateway; a ready heartbeat clears the text,
// and a reason that is not a pair's alarm never carries one.
func TestMirrorAlarmReachesTheHub(t *testing.T) {
	w := newBeatWorld(t, "", nil, hub.Options{})
	mon := w.hub.Server().Monitor()
	text := "сверка CRM: к снятию 4 отражений за один проход, а порог — 3; ничего не снято. Проверьте отмены в CRM; если они верны, поднимите mirror.max_removals клуба на хабе до 4 или выше, держите порог поднятым, пока тревога не уйдёт, затем верните"
	alert := &driver.ConfigAlert{Code: "MIRROR_FUSE", Problems: []string{text, "Лунда: PRIVACY-MARKER-PROBLEM-only-in-logs"}, Detail: []string{text + "\x07"}}
	var readyErr error = &core.ReadyError{Clubs: []string{"club-north"}, Err: alert}
	check := &tunnel.SelfCheck{Ready: func(context.Context) error { return readyErr }, Reason: core.ReadyReason, Clubs: core.ReadyClubs,
		Detail: core.ReadyDetail, Interval: time.Minute, Now: w.clock.Now, Version: "1.0.0", Build: tunnel.BuildRelease}
	send := func() {
		t.Helper()
		w.clock.Advance(time.Minute)
		hb, ok := check.Step(context.Background())
		if !ok {
			t.Fatal("no self-check was due")
		}
		if err := w.gw.Client.SendHeartbeat(context.Background(), hb); err != nil {
			t.Fatal(err)
		}
	}
	status := func() hub.GatewayStatus {
		t.Helper()
		r := call(t, w.hub, http.MethodGet, "/hub/status", testkit.OperatorKey, "")
		var st struct {
			Gateways []hub.GatewayStatus `json:"gateways"`
		}
		if err := json.Unmarshal(r.body, &st); err != nil || len(st.Gateways) != 1 {
			t.Fatalf("status: %s", r.body)
		}
		if strings.Contains(string(r.body), "PRIVACY-MARKER") {
			t.Fatalf("status shows an alert's problems beyond its detail: %s", r.body)
		}
		return st.Gateways[0]
	}
	for range 3 {
		send()
	}
	mon.Tick()
	if g := status(); g.ReadyReason != "MIRROR_FUSE" || g.ReadyReasonDetail != text {
		t.Fatalf("status: reason %q, detail %q", g.ReadyReason, g.ReadyReasonDetail)
	}
	checks, _ := validateStatusFiles(t, w.status)
	if d, _ := checks["installation_demo_club_ready"]["detail"].(string); !strings.Contains(d, "MIRROR_FUSE (clubs: club-north): ") || !strings.Contains(d, "к снятию 4") {
		t.Fatalf("ready detail in the status files: %q", d)
	}
	readyErr = nil
	send()
	mon.Tick()
	if g := status(); g.ReadyReasonDetail != "" {
		t.Fatalf("a ready heartbeat left the alarm's text: %q", g.ReadyReasonDetail)
	}
	// a reason that is not a pair's alarm: its text, if any, is not shown
	hb := tunnel.Heartbeat{Version: "1.0.0", Build: tunnel.BuildRelease, Reason: "TIMEZONE_MISMATCH", ReasonDetail: "PRIVACY-MARKER-not-a-mirror", CheckedAt: w.clock.Now()}
	if err := w.gw.Client.SendHeartbeat(context.Background(), hb); err != nil {
		t.Fatal(err)
	}
	status()
}
