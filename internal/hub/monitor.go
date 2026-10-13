package hub

import (
	"context"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
	"github.com/Padelapp-Club/court-gateway/internal/version"
)

// Club states as the monitor sees them.
const (
	// StatePending: no heartbeat yet since the hub started, but not for
	// longer than offline_after either.
	StatePending  = "pending"
	StateOnline   = "online"
	StateNotReady = "not_ready"
	StateOffline  = "offline"
)

// Monitor watches the heartbeats gateways send after each of their own
// self-checks. It never calls a gateway: a gateway is offline when its
// heartbeats stop for longer than offlineAfter, and not ready after
// readyFailures heartbeats in a row that report ready=false. Every Tick it
// re-evaluates every registered gateway, writes the status files for the
// external checker (which decides whether and how to alert) and logs
// state changes. The hub sends no alerts of its own.
type Monitor struct {
	store         *Store
	broker        *Broker
	log           *slog.Logger
	now           func() time.Time
	interval      time.Duration
	offlineAfter  time.Duration
	readyFailures int
	latest        string
	version       string
	statusDir     string
	started       time.Time

	mu    sync.Mutex
	state map[string]*clubHealth
}

type clubHealth struct {
	lastBeat  time.Time // hub clock: when the last heartbeat arrived
	checkedAt time.Time // gateway clock, informational only
	version   string
	build     string
	ready     bool
	reason    string
	notReady  int // heartbeats in a row with ready=false
	beats     int
	// what the gateway reported about its club config
	configVersion int64
	configError   string
	clubErrors    map[string]string
	clubsServed   []string

	state string // as of the last Tick
}

// Health is the monitor's view of one club, for /hub/status.
type Health struct {
	State         string
	LastHeartbeat time.Time
	CheckedAt     time.Time
	Version       string
	Build         string
	// Checked is true once a heartbeat has arrived since the hub started.
	Checked      bool
	Ready        bool
	Reason       string
	Failures     int
	OfflineSince time.Time
	// ConfigVersion, ConfigError and ClubsServed are the gateway's report
	// on the club config it runs.
	ConfigVersion int64
	ConfigError   string
	ClubErrors    map[string]string
	ClubsServed   []string
}

// MonitorConfig configures a Monitor.
type MonitorConfig struct {
	Interval      time.Duration
	OfflineAfter  time.Duration
	ReadyFailures int
	// LatestVersion marks older gateways as outdated in the status files.
	LatestVersion string
	// Version is the hub's own version, reported in status.json.
	Version string
	// StatusDir receives status.json and metrics every Tick; empty means
	// no files.
	StatusDir string
	Now       func() time.Time
	Logger    *slog.Logger
}

// NewMonitor builds a heartbeat monitor. broker may be nil; it only adds
// the tunnel state to the status details.
func NewMonitor(store *Store, broker *Broker, cfg MonitorConfig) *Monitor {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.ReadyFailures < 1 {
		cfg.ReadyFailures = DefaultReadyFailures
	}
	return &Monitor{
		store: store, broker: broker, log: cfg.Logger, now: cfg.Now,
		interval: cfg.Interval, offlineAfter: cfg.OfflineAfter, readyFailures: cfg.ReadyFailures,
		latest: cfg.LatestVersion, version: cfg.Version, statusDir: cfg.StatusDir, started: cfg.Now(), state: map[string]*clubHealth{},
	}
}

// Run evaluates once at start (so the status files exist at once) and
// then every interval until ctx is done.
func (m *Monitor) Run(ctx context.Context) {
	m.Tick(ctx)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Tick(ctx)
		}
	}
}

var (
	reasonRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,39}$`)
	versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,39}$`)
)

// cleanReason accepts a gateway's reason only as a short upper-case code,
// so a gateway cannot put arbitrary text into status files.
func cleanReason(ready bool, r string) string {
	switch {
	case ready:
		return ""
	case reasonRe.MatchString(r):
		return r
	default:
		return "NOT_READY"
	}
}

func cleanVersion(v string) string {
	v = strings.TrimSpace(v)
	if version.Valid(v) || versionRe.MatchString(v) {
		return v
	}
	return ""
}

func cleanBuild(b string) string {
	if b == tunnel.BuildRelease || b == tunnel.BuildSelf {
		return b
	}
	return ""
}

// Heartbeat records a heartbeat from club's gateway, received now (hub
// clock: the gateway's clock is not trusted for liveness).
func (m *Monitor) Heartbeat(club string, hb tunnel.Heartbeat) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.health(club)
	h.lastBeat, h.beats = now, h.beats+1
	h.ready, h.reason = hb.Ready, cleanReason(hb.Ready, hb.Reason)
	h.version, h.build = cleanVersion(hb.Version), cleanBuild(hb.Build)
	if !hb.CheckedAt.IsZero() && hb.CheckedAt.Year() > 2000 && hb.CheckedAt.Year() < 3000 {
		h.checkedAt = hb.CheckedAt.UTC()
	}
	if hb.Ready {
		h.notReady = 0
	} else {
		h.notReady++
	}
	h.configVersion, h.configError, h.clubsServed = hb.ConfigVersion, cleanConfigError(hb.ConfigError, 2000), cleanClubs(hb.Clubs)
	h.clubErrors = nil
	for id, why := range hb.ClubErrors {
		if ValidClubID(id) && len(h.clubErrors) < 1000 {
			if h.clubErrors == nil {
				h.clubErrors = map[string]string{}
			}
			h.clubErrors[id] = cleanConfigError(why, 500)
		}
	}
}

// cleanConfigError bounds a gateway's config refusal text; it is shown to
// the operator only (API), never written to status files.
func cleanConfigError(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= ' ' {
			return r
		}
		return -1
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func cleanClubs(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		if ValidClubID(id) && len(out) < 1000 {
			out = append(out, id)
		}
	}
	return out
}

// health returns club's record; m.mu must be held.
func (m *Monitor) health(club string) *clubHealth {
	h, ok := m.state[club]
	if !ok {
		h = &clubHealth{}
		m.state[club] = h
	}
	return h
}

// silentSince is when the club was last known alive: its last heartbeat,
// or, before the first one, the later of the hub's start and the club's
// registration. So a hub restart cannot hide a dead club, and a club that
// has just registered gets offline_after to send its first heartbeat.
func (m *Monitor) silentSince(h *clubHealth, registered time.Time) time.Time {
	if !h.lastBeat.IsZero() {
		return h.lastBeat
	}
	if registered.After(m.started) {
		return registered
	}
	return m.started
}

// evaluate computes club's state at now; m.mu must be held.
func (m *Monitor) evaluate(h *clubHealth, registered, now time.Time) string {
	seen := m.silentSince(h, registered)
	switch {
	case now.Sub(seen) > m.offlineAfter:
		return StateOffline
	case h.beats == 0:
		return StatePending
	case h.notReady >= m.readyFailures:
		return StateNotReady
	default:
		return StateOnline
	}
}

// Health returns the monitor's view of club.
func (m *Monitor) Health(club string) Health {
	now := m.now()
	g, _ := m.store.Get(club)
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.state[club]
	if !ok {
		h = &clubHealth{}
	}
	out := Health{
		State: m.evaluate(h, g.RegisteredAt, now), LastHeartbeat: h.lastBeat, CheckedAt: h.checkedAt, Version: h.version, Build: h.build,
		Checked: h.beats > 0, Ready: h.ready, Reason: h.reason, Failures: h.notReady,
		ConfigVersion: h.configVersion, ConfigError: h.configError, ClubErrors: maps.Clone(h.clubErrors), ClubsServed: append([]string(nil), h.clubsServed...),
	}
	if out.State == StateOffline {
		out.OfflineSince = m.silentSince(h, g.RegisteredAt)
	}
	return out
}

// clubView is one club's evaluated state, for the status files.
type clubView struct {
	GatewayRecord
	clubHealth
	connected bool
}

// Tick evaluates every registered gateway, writes the status files and
// logs state changes.
func (m *Monitor) Tick(context.Context) {
	now := m.now()
	var views []clubView
	gateways := m.store.Gateways()
	connected := map[string]bool{}
	if m.broker != nil {
		for _, g := range gateways {
			connected[g.Installation] = m.broker.Connected(g.Installation)
		}
	}
	type change struct{ installation, event string }
	var changes []change
	m.mu.Lock()
	for _, g := range gateways {
		h := m.health(g.Installation)
		st := m.evaluate(h, g.RegisteredAt, now)
		if prev := h.state; st != prev {
			h.state = st
			if ev := event(prev, st); ev != "" {
				changes = append(changes, change{g.Installation, ev})
			}
		}
		views = append(views, clubView{GatewayRecord: g, clubHealth: *h, connected: connected[g.Installation]})
	}
	m.mu.Unlock()

	if m.statusDir != "" {
		o := statusOptions{version: m.version, latest: m.latest, offlineAfter: m.offlineAfter, readyFailures: m.readyFailures}
		dropped, err := writeStatus(m.statusDir, now, o, views)
		if len(dropped) > 0 {
			m.log.Warn("status keys dropped: malformed or repeated", "keys", strings.Join(dropped, ","))
		}
		if err != nil {
			m.log.Error("cannot write the status files", "dir", m.statusDir, "error", err)
		}
	}
	for _, c := range changes {
		m.log.Warn("gateway state changed", "event", c.event, "installation", c.installation)
	}
}

// Events of state changes, as logged.
const (
	EventOffline  = "offline"
	EventOnline   = "online"
	EventNotReady = "not_ready"
	EventReady    = "ready"
)

// event names a state change worth a log line. The first evaluation of a
// gateway (prev empty) counts only when it is already bad; pending never
// does.
func event(prev, next string) string {
	switch {
	case next == StateOffline:
		return EventOffline
	case next == StateNotReady:
		return EventNotReady
	case next == StateOnline && prev == StateOffline:
		return EventOnline
	case next == StateOnline && prev == StateNotReady:
		return EventReady
	}
	return ""
}
