package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/version"
)

// Status files for an external checker. The monitor rewrites both every
// Tick, each atomically (temporary file and rename), so a reader never sees
// a partial file:
//
//	status.json  {"ts":"…","ok":true|false,"version":"…","checks":[{"check":"<name>","ok":true|false,"detail":"<text>"}, …]}
//	metrics      key=number lines
//
// A metric key matches [a-z][a-z0-9_]* and no key repeats: keys are built
// by MetricKey from installation ids, which cannot collide, and values are
// integers. The checker decides how and where to alert; the hub only
// reports. Nothing secret is written: details are built from the
// installation id and name, times, the gateway's version and build and
// fixed reason codes.
const (
	StatusFile  = "status.json"
	MetricsFile = "metrics"
)

var keyBadChars = regexp.MustCompile(`[^a-z0-9_]`)

// MetricKey turns parts into a metric or check name: lower case, every
// character outside [a-z0-9_] becomes "_". Installation ids hold only
// [a-z0-9-], so "installation_" + id is injective over valid ids.
func MetricKey(parts ...string) string {
	k := keyBadChars.ReplaceAllString(strings.ToLower(strings.Join(parts, "_")), "_")
	if k == "" || k[0] < 'a' || k[0] > 'z' {
		k = "m_" + k
	}
	return k
}

type statusCheck struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type statusDoc struct {
	TS      string        `json:"ts"`
	OK      bool          `json:"ok"`
	Version string        `json:"version"`
	Checks  []statusCheck `json:"checks"`
}

// metricSet collects metric lines in order.
type metricSet struct{ lines []string }

func (s *metricSet) addInt(key string, v int64) {
	s.lines = append(s.lines, key+"="+strconv.FormatInt(v, 10))
}

func (s *metricSet) addBool(key string, v bool) {
	if v {
		s.addInt(key, 1)
	} else {
		s.addInt(key, 0)
	}
}

func (s *metricSet) bytes() []byte {
	if len(s.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(s.lines, "\n") + "\n")
}

// buildStatus renders both files' contents.
func buildStatus(now time.Time, o statusOptions, views []installationView) ([]byte, []byte) {
	doc := statusDoc{TS: now.UTC().Format(time.RFC3339), OK: true, Version: o.version, Checks: []statusCheck{}}
	var ms metricSet
	check := func(name string, ok bool, detail string) {
		doc.Checks = append(doc.Checks, statusCheck{Check: name, OK: ok, Detail: detail})
		if !ok {
			doc.OK = false
		}
		ms.addBool(MetricKey("check", name), ok)
	}
	var online, ready int64
	for _, v := range views {
		st := v.state
		id := MetricKey("installation", v.Installation)
		isOnline := st != StateOffline
		check(id+"_online", isOnline, onlineDetail(v, now, o.offlineAfter))
		check(id+"_ready", st != StateNotReady, readyDetail(v, o.readyFailures))
		check(id+"_single", !v.duplicate, singleDetail(v))
		if isOnline {
			online++
		}
		if st == StateOnline {
			ready++
		}
		if !v.lastBeat.IsZero() {
			ms.addInt(id+"_last_seen_age_seconds", int64(now.Sub(v.lastBeat)/time.Second))
		}
		ms.addInt(id+"_refused_requests", v.refused)
		ms.addBool(id+"_outdated", o.latest != "" && v.version != "" && version.Compare(v.version, o.latest) < 0)
	}
	ms.addInt("installations_total", int64(len(views)))
	ms.addInt("installations_online", online)
	ms.addInt("installations_ready", ready)
	ms.addBool("status_ok", doc.OK)
	ms.addInt("run_last_utc_epoch", now.Unix())
	data, _ := json.Marshal(doc)
	return append(data, '\n'), ms.bytes()
}

func onlineDetail(v installationView, now time.Time, offlineAfter time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "«%s»: ", v.Name)
	if v.lastBeat.IsZero() {
		fmt.Fprintf(&b, "no heartbeat since the hub started or the club registered (limit %s)", offlineAfter)
	} else {
		fmt.Fprintf(&b, "last heartbeat %s ago at %s (limit %s)", now.Sub(v.lastBeat).Round(time.Second),
			v.lastBeat.UTC().Format(time.RFC3339), offlineAfter)
	}
	if v.version != "" {
		fmt.Fprintf(&b, ", version %s", v.version)
		if v.build != "" {
			fmt.Fprintf(&b, " (%s)", v.build)
		}
	}
	if v.connected {
		b.WriteString(", tunnel connected")
	} else {
		b.WriteString(", tunnel not connected")
	}
	return b.String()
}

func readyDetail(v installationView, readyFailures int) string {
	switch {
	case v.state == StatePending:
		return "unknown: waiting for the first heartbeat"
	case v.state == StateOffline && v.beats == 0:
		return "unknown: no heartbeat"
	case v.state == StateOffline:
		return "unknown while offline; last report: " + readyWord(v.installationHealth)
	case v.state == StateNotReady:
		return fmt.Sprintf("not ready: %s, %d heartbeats in a row (limit %d)", reasonText(v.installationHealth), v.notReady, readyFailures)
	case !v.ready:
		return fmt.Sprintf("ready; last report not ready: %s, %d of %d heartbeats in a row before it counts", reasonText(v.installationHealth), v.notReady, readyFailures)
	default:
		return "ready: CRM login and occupancy check passed"
	}
}

func singleDetail(v installationView) string {
	if !v.duplicate {
		return "one copy of the gateway"
	}
	return fmt.Sprintf("«%s»: a second copy with the same secret was refused at %s; only one copy may run", v.Name, v.refusedAt.UTC().Format(time.RFC3339))
}

func readyWord(h installationHealth) string {
	if h.ready {
		return "ready"
	}
	return "not ready " + reasonText(h)
}

// reasonText is the reason code with the clubs it is about, if any.
func reasonText(h installationHealth) string {
	if len(h.clubs) == 0 {
		return h.reason
	}
	return h.reason + " (clubs: " + strings.Join(h.clubs, ", ") + ")"
}

type statusOptions struct {
	version       string
	latest        string
	offlineAfter  time.Duration
	readyFailures int
}

// writeStatus writes both files into dir, each atomically.
func writeStatus(dir string, now time.Time, o statusOptions, views []installationView) error {
	status, metrics := buildStatus(now, o, views)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := core.WriteFileAtomic(filepath.Join(dir, MetricsFile), metrics, 0o644); err != nil {
		return err
	}
	return core.WriteFileAtomic(filepath.Join(dir, StatusFile), status, 0o644)
}
