package hub

import (
	"context"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Installation states as the monitor sees them.
const (
	// StatePending: no heartbeat yet since the hub started, but not for
	// longer than offline_after either.
	StatePending  = "pending"
	StateOnline   = "online"
	StateNotReady = "not_ready"
	StateOffline  = "offline"
)

// Monitor watches the gateways' heartbeats and never calls a gateway: one
// is offline when they stop for offlineAfter, not ready after
// readyFailures self-checks in a row with ready=false (a repeated CheckedAt,
// sent for a new config, is not another). Every Tick writes the status
// files for the external checker, which alerts, and logs state changes.
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
	state map[string]*installationHealth
}

type installationHealth struct {
	lastBeat  time.Time // hub clock: when the last heartbeat arrived
	checkedAt time.Time // gateway clock, informational only
	version   string
	build     string
	ready     bool
	reason    string
	clubs     []string  // the clubs reason is about
	nextLogin time.Time // gateway clock: when a rejected CRM login is tried again
	notReady  int       // heartbeats in a row with ready=false
	beats     int
	applied   tunnel.AppliedConfig // as reported, cleaned
	refused   int64                // requests the gateway refused as unsigned

	state string // as of the last Tick

	// the gateway process that serves registration gen (see Admit)
	gen       uint64
	instance  string    // "" when none is bound
	released  string    // an instance that said it stopped
	refusedAt time.Time // hub clock: the last refusal of another copy
}

// Health is the monitor's view of one installation, for /hub/status.
type Health struct {
	State         string
	LastHeartbeat time.Time
	CheckedAt     time.Time
	Version       string
	Build         string
	// Checked is true once a heartbeat has arrived since the hub started.
	Checked     bool
	Ready       bool
	Reason      string
	ReasonClubs []string
	// NextLogin is when the gateway tries a login the CRM rejected again
	// (gateway clock); zero without a rejection.
	NextLogin    time.Time
	OfflineSince time.Time
	// DuplicateAt is when a second copy of the gateway, with the same
	// secret, was last refused; Duplicate is true while that is recent.
	Duplicate   bool
	DuplicateAt time.Time
	// RefusedRequests: requests and configs the gateway refused because
	// the hub had not signed them for it (since the gateway started).
	RefusedRequests int64
	// the gateway's report on the club config it runs
	tunnel.AppliedConfig
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
		latest: cfg.LatestVersion, version: cfg.Version, statusDir: cfg.StatusDir, started: cfg.Now(), state: map[string]*installationHealth{},
	}
}

// Run evaluates once at start (so the status files exist at once) and
// then every interval until ctx is done.
func (m *Monitor) Run(ctx context.Context) {
	m.Tick()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Tick()
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
	if versionRe.MatchString(v) {
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

// Heartbeat records a heartbeat from installation's gateway, received now (hub
// clock: the gateway's clock is not trusted for liveness); of a copy not
// bound only that it is alive, its report does not count.
func (m *Monitor) Heartbeat(installation string, hb tunnel.Heartbeat, bound bool) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.health(installation)
	if h.lastBeat = now; !bound {
		return
	}
	h.beats++
	h.ready, h.reason, h.clubs, h.nextLogin = hb.Ready, cleanReason(hb.Ready, hb.Reason), nil, time.Time{}
	if !hb.Ready {
		h.clubs = cleanClubs(hb.ReasonClubs)
		h.clubs = h.clubs[:min(len(h.clubs), 20)]
		if y := hb.NextLogin.Year(); y > 2000 && y < 3000 {
			h.nextLogin = hb.NextLogin.UTC()
		}
	}
	h.version, h.build = cleanVersion(hb.Version), cleanBuild(hb.Build)
	fresh := true // a new self-check, not the last one sent again with a new config
	if !hb.CheckedAt.IsZero() && hb.CheckedAt.Year() > 2000 && hb.CheckedAt.Year() < 3000 {
		fresh = !hb.CheckedAt.UTC().Equal(h.checkedAt)
		h.checkedAt = hb.CheckedAt.UTC()
	}
	if hb.RefusedRequests > h.refused {
		m.log.Warn("gateway refused requests not signed by the hub: something between them forges or alters them",
			"installation", installation, "refused", hb.RefusedRequests)
	}
	h.refused = max(hb.RefusedRequests, 0)
	switch {
	case hb.Ready:
		h.notReady = 0
	case fresh:
		h.notReady++
	}
	h.applied = tunnel.AppliedConfig{ConfigVersion: hb.ConfigVersion, ConfigError: cleanConfigError(hb.ConfigError, 2000), Clubs: cleanClubs(hb.Clubs)}
	for id, why := range hb.ClubErrors {
		if core.ValidID(id) && len(h.applied.ClubErrors) < 1000 {
			if h.applied.ClubErrors == nil {
				h.applied.ClubErrors = map[string]string{}
			}
			h.applied.ClubErrors[id] = cleanConfigError(why, 500)
		}
	}
}

// Admit decides whether gateway process instance may serve registration
// gen of installation. The first instance is bound to it. Another is
// refused while the bound one keeps a poll open or runs a call it picked
// up (Broker.Serving), and takes over as soon as it does neither: a
// process that crashed or lost power has its polls closed or ended, and
// its calls end with the time the gateway gives them. An instance that
// released itself (it stopped) is not bound again. No instance id
// survives a restart. The check and the bind are one step with the hold
// (Broker.hold): the admitted request counts as an open poll until its
// release, so no other copy is admitted before it reaches the broker.
// release is nil when instance is refused.
func (m *Monitor) Admit(installation string, gen uint64, instance string) (release func()) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.health(installation)
	if h.gen != gen {
		h.gen, h.instance, h.released, h.refusedAt = gen, "", "", time.Time{}
	}
	switch {
	case instance == h.instance:
	case instance == h.released:
		return nil
	case h.instance == "" || m.broker == nil || !m.broker.Serving(installation, h.instance):
		if h.instance != "" {
			m.log.Warn("another copy of a gateway took over from one that stopped polling", "installation", installation)
		}
		h.instance = instance
	default:
		if !m.duplicate(h, now) {
			m.log.Warn("second copy of a gateway refused: two copies run with one secret", "installation", installation)
		}
		h.refusedAt = now
		return nil
	}
	if m.broker == nil {
		return func() {}
	}
	return m.broker.hold(installation, gen, instance)
}

// Release unbinds instance, which says it has stopped, so the copy
// started after it is admitted at once.
func (m *Monitor) Release(installation string, gen uint64, instance string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.health(installation); h.gen == gen && h.instance == instance {
		h.instance, h.released = "", instance
	}
}

// duplicate reports whether a second copy was refused within offlineAfter.
func (m *Monitor) duplicate(h *installationHealth, now time.Time) bool {
	return !h.refusedAt.IsZero() && now.Sub(h.refusedAt) <= m.offlineAfter
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
		if core.ValidID(id) && len(out) < 1000 {
			out = append(out, id)
		}
	}
	return out
}

// health returns installation's record; m.mu must be held.
func (m *Monitor) health(installation string) *installationHealth {
	h, ok := m.state[installation]
	if !ok {
		h = &installationHealth{}
		m.state[installation] = h
	}
	return h
}

// silentSince is when the installation was last known alive: its last
// heartbeat, before the first the later of the hub's start and its
// registration (a restart hides no dead gateway, a new one gets time).
func (m *Monitor) silentSince(h *installationHealth, registered time.Time) time.Time {
	if !h.lastBeat.IsZero() {
		return h.lastBeat
	}
	if registered.After(m.started) {
		return registered
	}
	return m.started
}

// evaluate computes an installation's state at now; m.mu must be held.
func (m *Monitor) evaluate(h *installationHealth, registered, now time.Time) string {
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

// Health returns the monitor's view of installation.
func (m *Monitor) Health(installation string) Health {
	now := m.now()
	g, _ := m.store.Gateway(installation)
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.state[installation]
	if !ok {
		h = &installationHealth{}
	}
	out := Health{
		State: m.evaluate(h, g.RegisteredAt, now), LastHeartbeat: h.lastBeat, CheckedAt: h.checkedAt, Version: h.version, Build: h.build,
		Checked: h.beats > 0, Ready: h.ready, Reason: h.reason, ReasonClubs: append([]string(nil), h.clubs...), NextLogin: h.nextLogin, Duplicate: m.duplicate(h, now), DuplicateAt: h.refusedAt, RefusedRequests: h.refused,
		AppliedConfig: tunnel.AppliedConfig{ConfigVersion: h.applied.ConfigVersion, ConfigError: h.applied.ConfigError,
			ClubErrors: maps.Clone(h.applied.ClubErrors), Clubs: append([]string(nil), h.applied.Clubs...)},
	}
	if out.State == StateOffline {
		out.OfflineSince = m.silentSince(h, g.RegisteredAt)
	}
	return out
}

// installationView is one installation's evaluated state, for the status files.
type installationView struct {
	GatewayRecord
	installationHealth
	connected bool
	duplicate bool
}

// Tick evaluates every registered gateway, writes the status files and
// logs state changes.
func (m *Monitor) Tick() {
	now := m.now()
	var views []installationView
	gateways := m.store.Gateways()
	connected := map[string]bool{}
	if m.broker != nil {
		for _, g := range gateways {
			connected[g.Installation] = m.broker.Connected(g.Installation)
		}
	}
	type change struct{ installation, event string }
	var changes []change
	registered := map[string]bool{}
	for _, g := range gateways {
		registered[g.Installation] = true
	}
	m.mu.Lock()
	for id := range m.state {
		if !registered[id] {
			delete(m.state, id) // deleted installation or revoked gateway
		}
	}
	for _, g := range gateways {
		h := m.health(g.Installation)
		st := m.evaluate(h, g.RegisteredAt, now)
		if prev := h.state; st != prev {
			h.state = st
			if ev := event(prev, st); ev != "" {
				changes = append(changes, change{g.Installation, ev})
			}
		}
		views = append(views, installationView{GatewayRecord: g, installationHealth: *h, connected: connected[g.Installation], duplicate: m.duplicate(h, now)})
	}
	m.mu.Unlock()

	if m.statusDir != "" {
		o := statusOptions{version: m.version, latest: m.latest, offlineAfter: m.offlineAfter, readyFailures: m.readyFailures}
		if err := writeStatus(m.statusDir, now, o, views); err != nil {
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

// event names a state change worth a log line; a first evaluation only
// when already bad, pending never.
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
