package hub

import (
	"context"
	"fmt"
	"log/slog"
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
// self-checks. It never calls a gateway: a club is offline when its
// heartbeats stop for longer than offlineAfter, and not ready after
// readyFailures heartbeats in a row that report ready=false. Every Tick it
// re-evaluates every registered club, writes the status files for the
// external checker and, when sinks are configured, sends transitions to
// them.
type Monitor struct {
	store         *Store
	broker        *Broker
	sinks         []Sink
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

	state      string // as of the last Tick
	stateSince time.Time
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
func NewMonitor(store *Store, broker *Broker, sinks []Sink, cfg MonitorConfig) *Monitor {
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
		store: store, broker: broker, sinks: sinks, log: cfg.Logger, now: cfg.Now,
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
// so a gateway cannot put arbitrary text into status files or alerts.
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

// Tick evaluates every registered club, writes the status files and sends
// state transitions to the sinks.
func (m *Monitor) Tick(ctx context.Context) {
	now := m.now()
	var views []clubView
	var alerts []Alert
	clubs := m.store.Clubs()
	connected := map[string]bool{}
	if m.broker != nil {
		for _, g := range clubs {
			connected[g.Club] = m.broker.Connected(g.Club)
		}
	}
	m.mu.Lock()
	for _, g := range clubs {
		h := m.health(g.Club)
		st := m.evaluate(h, g.RegisteredAt, now)
		prev, prevSince := h.state, h.stateSince
		if st != prev {
			h.state, h.stateSince = st, now
			if a, ok := m.transition(g, *h, prev, prevSince, now); ok {
				alerts = append(alerts, a)
			}
		}
		views = append(views, clubView{GatewayRecord: g, clubHealth: *h, connected: connected[g.Club]})
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
	for _, a := range alerts {
		m.send(ctx, a)
	}
}

// transition builds the alert for a state change. The first evaluation of
// a club (prev empty) alerts only when it is already bad; pending never
// alerts.
func (m *Monitor) transition(g GatewayRecord, h clubHealth, prev string, prevSince, now time.Time) (Alert, bool) {
	a := Alert{Club: g.Club, ClubName: g.Name, Time: now}
	switch h.state {
	case StateOffline:
		since := m.silentSince(&h, g.RegisteredAt)
		a.Event = EventOffline
		a.Text = fmt.Sprintf("court-gateway: НЕТ СВЯЗИ со шлюзом клуба «%s» (%s): последний сигнал %s UTC, %s назад.",
			g.Name, g.Club, since.UTC().Format("02.01 15:04"), minutes(now.Sub(since)))
		if h.lastBeat.IsZero() {
			a.Text = fmt.Sprintf("court-gateway: НЕТ СВЯЗИ со шлюзом клуба «%s» (%s): ни одного сигнала за %s.",
				g.Name, g.Club, minutes(now.Sub(since)))
		}
		return a, true
	case StateNotReady:
		a.Event = EventNotReady
		a.Text = fmt.Sprintf("court-gateway: шлюз клуба «%s» (%s) на связи, но НЕ ГОТОВ: %s (%d проверки подряд).",
			g.Name, g.Club, h.reason, h.notReady)
		return a, true
	case StateOnline:
		switch prev {
		case StateOffline:
			a.Event = EventOnline
			a.Text = fmt.Sprintf("court-gateway: шлюз клуба «%s» (%s) снова на связи (не было связи %s).",
				g.Name, g.Club, minutes(now.Sub(prevSince)))
			return a, true
		case StateNotReady:
			a.Event = EventReady
			a.Text = fmt.Sprintf("court-gateway: шлюз клуба «%s» (%s) снова готов.", g.Name, g.Club)
			return a, true
		}
	}
	return Alert{}, false
}

func (m *Monitor) send(ctx context.Context, a Alert) {
	m.log.Warn("club state changed", "event", a.Event, "club", a.Club)
	for _, s := range m.sinks {
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.Send(sctx, a)
		cancel()
		if err != nil {
			m.log.Error("alert delivery failed", "sink", s.Name(), "event", a.Event, "club", a.Club, "error", err)
		}
	}
}

func minutes(d time.Duration) string {
	n := int(d.Round(time.Minute) / time.Minute)
	return fmt.Sprintf("%d мин", n)
}
