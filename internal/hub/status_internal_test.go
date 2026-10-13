package hub

import (
	"strings"
	"testing"
	"time"
)

func TestMetricKey(t *testing.T) {
	for in, want := range map[string]string{
		"demo-club":   "club_demo_club",
		"club-2b":     "club_club_2b",
		"Club.Name x": "club_club_name_x",
	} {
		if got := MetricKey("club", in); got != want {
			t.Fatalf("MetricKey(club, %q) = %q, want %q", in, got, want)
		}
	}
	if got := MetricKey("9lives"); got != "m_9lives" {
		t.Fatalf("a key must start with a letter: %q", got)
	}
}

func TestMetricSetRejectsMalformedAndRepeated(t *testing.T) {
	var s metricSet
	s.add("ok_key", "12")
	s.add("ok_key", "13")                 // repeat
	s.add("Bad-Key", "1")                 // charset
	s.add("_lead", "1")                   // must start with a letter
	s.add("too_long", "1234567890123456") // 16 digits
	s.add("not_number", "1e3")
	s.add("neg", "-1.5")
	s.add("max_digits", "123456789012345")
	got := string(s.bytes())
	if got != "ok_key=12\nneg=-1.5\nmax_digits=123456789012345\n" {
		t.Fatalf("metrics: %q", got)
	}
	if strings.Join(s.dropped, ",") != "ok_key,Bad-Key,_lead,too_long,not_number" {
		t.Fatalf("dropped: %v", s.dropped)
	}
}

// Two ids that sanitise to the same key cannot both be written: the check
// and its metrics appear once, and the collision is reported.
func TestStatusDropsCollidingKeys(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	views := []clubView{
		{GatewayRecord: GatewayRecord{Installation: "a-b", Name: "x"}, clubHealth: clubHealth{state: StatePending}},
		{GatewayRecord: GatewayRecord{Installation: "a_b", Name: "y"}, clubHealth: clubHealth{state: StatePending}},
	}
	status, metrics, dropped := buildStatus(now, statusOptions{offlineAfter: time.Minute, readyFailures: 3}, views)
	if strings.Count(string(status), `"club_a_b_online"`) != 1 || strings.Count(string(metrics), "club_a_b_outdated=") != 1 {
		t.Fatalf("collision written twice:\n%s\n%s", status, metrics)
	}
	if len(dropped) == 0 {
		t.Fatal("collision not reported")
	}
}
