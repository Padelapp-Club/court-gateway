package hub

import (
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

func TestMetricKey(t *testing.T) {
	for in, want := range map[string]string{
		"demo-club":   "installation_demo_club",
		"club-2b":     "installation_club_2b",
		"Club.Name x": "installation_club_name_x",
	} {
		if got := MetricKey("installation", in); got != want {
			t.Fatalf("MetricKey(installation, %q) = %q, want %q", in, got, want)
		}
	}
	if got := MetricKey("9lives"); got != "m_9lives" {
		t.Fatalf("a key must start with a letter: %q", got)
	}
}

// A deleted installation leaves nothing behind in the monitor: no state,
// no checks in the status files.
func TestMonitorForgetsDeletedInstallations(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, err := CreateJoinCode(FileState(dir), "gone-net", "", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Redeem(code.Code); err != nil {
		t.Fatal(err)
	}
	m := NewMonitor(st, NewBroker(time.Second, nil), MonitorConfig{OfflineAfter: time.Minute})
	m.Heartbeat("gone-net", tunnel.Heartbeat{Ready: true}, true)
	m.Tick()
	if err := st.DeleteInstallation("gone-net"); err != nil {
		t.Fatal(err)
	}
	m.Tick()
	m.mu.Lock()
	n := len(m.state)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d monitor entries left after the installation was deleted", n)
	}
}
