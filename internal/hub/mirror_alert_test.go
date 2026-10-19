package hub

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

func TestMirrorAlarmReachesHub(t *testing.T) {
	e := &driver.ConfigAlert{Code: "MIRROR_REMOVAL_FUSE", Problems: []string{"Запланировано снятий: 4; mirror.max_removals=3. Проверьте план."}}
	check := tunnel.SelfCheck{Ready: func(context.Context) error {
		return errors.Join(&driver.ConfigAlert{Code: "MIRROR_DAMAGED", Problems: []string{"Повреждена метка."}}, e)
	}, Reason: core.ReadyReason, Detail: core.ReadyDetail}
	hb, ok := check.Step(context.Background())
	if !ok || hb.Ready {
		t.Fatal(hb)
	}
	m := NewMonitor(nil, nil, MonitorConfig{})
	m.Heartbeat("test", hb, true)
	h := *m.state["test"]
	if h.reason != "MIRROR_DAMAGED" || !strings.Contains(reasonText(h), "снятий: 4") {
		t.Fatal(reasonText(h))
	}
	m.Heartbeat("test", tunnel.Heartbeat{Ready: true, ReasonDetail: "old"}, true)
	if m.state["test"].reasonDetail != "" {
		t.Fatal("stale alarm detail")
	}
	if core.ReadyDetail(errors.New("private data")) != "" {
		t.Fatal("upstream error leaked")
	}
}
