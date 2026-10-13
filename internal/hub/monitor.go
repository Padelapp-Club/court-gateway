package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Monitor is the heartbeat: every interval it looks at every registered
// gateway, asks connected ones for /health/ready through the tunnel and
// alerts when a gateway has been offline for offlineAfter or failed
// readyFailures probes in a row. It also says when things recover.
type Monitor struct {
	store         *Store
	broker        *Broker
	sinks         []Sink
	log           *slog.Logger
	now           func() time.Time
	interval      time.Duration
	offlineAfter  time.Duration
	readyFailures int
	probeTimeout  time.Duration
	started       time.Time

	mu    sync.Mutex
	state map[string]*clubHealth
}

type clubHealth struct {
	offlineSince    time.Time
	failures        int
	checked         bool
	ready           bool
	reason          string
	lastCheck       time.Time
	alertedOffline  bool
	alertedNotReady bool
}

// Health is the monitor's view of one club, for /hub/status.
type Health struct {
	Checked      bool
	Ready        bool
	Reason       string
	LastCheck    time.Time
	OfflineSince time.Time
	Failures     int
}

// MonitorConfig configures a Monitor.
type MonitorConfig struct {
	Interval      time.Duration
	OfflineAfter  time.Duration
	ReadyFailures int
	ProbeTimeout  time.Duration
	Now           func() time.Time
	Logger        *slog.Logger
}

// NewMonitor builds a heartbeat monitor.
func NewMonitor(store *Store, broker *Broker, sinks []Sink, cfg MonitorConfig) *Monitor {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 15 * time.Second
	}
	return &Monitor{
		store: store, broker: broker, sinks: sinks, log: cfg.Logger, now: cfg.Now,
		interval: cfg.Interval, offlineAfter: cfg.OfflineAfter, readyFailures: cfg.ReadyFailures,
		probeTimeout: cfg.ProbeTimeout, started: cfg.Now(), state: map[string]*clubHealth{},
	}
}

// Run ticks until ctx is done.
func (m *Monitor) Run(ctx context.Context) {
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

// Health returns the monitor's view of club.
func (m *Monitor) Health(club string) Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.state[club]
	if !ok {
		return Health{}
	}
	return Health{Checked: h.checked, Ready: h.ready, Reason: h.reason, LastCheck: h.lastCheck, OfflineSince: h.offlineSince, Failures: h.failures}
}

// Tick runs one heartbeat round over every registered gateway.
func (m *Monitor) Tick(ctx context.Context) {
	var wg sync.WaitGroup
	for _, g := range m.store.Clubs() {
		wg.Add(1)
		go func(g GatewayRecord) {
			defer wg.Done()
			m.check(ctx, g)
		}(g)
	}
	wg.Wait()
}

func (m *Monitor) health(club string) *clubHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.state[club]
	if !ok {
		// a gateway never seen since the hub started counts as offline
		// from the start, so a hub restart cannot hide a dead club
		h = &clubHealth{offlineSince: m.started}
		m.state[club] = h
	}
	return h
}

func (m *Monitor) check(ctx context.Context, g GatewayRecord) {
	h := m.health(g.Club)
	now := m.now()
	if !m.broker.Connected(g.Club) {
		m.mu.Lock()
		if h.offlineSince.IsZero() {
			h.offlineSince = now
		}
		since := h.offlineSince
		fire := !h.alertedOffline && now.Sub(since) >= m.offlineAfter
		if fire {
			h.alertedOffline = true
		}
		m.mu.Unlock()
		if fire {
			m.send(ctx, Alert{Event: EventOffline, Club: g.Club, ClubName: g.Name, Time: now,
				Text: fmt.Sprintf("court-gateway: НЕТ СВЯЗИ со шлюзом клуба «%s» (%s) с %s UTC — %s.",
					g.Name, g.Club, since.UTC().Format("02.01 15:04"), minutes(now.Sub(since)))})
		}
		return
	}

	m.mu.Lock()
	wasOffline, since := h.alertedOffline, h.offlineSince
	h.alertedOffline = false
	h.offlineSince = time.Time{}
	m.mu.Unlock()
	if wasOffline {
		m.send(ctx, Alert{Event: EventOnline, Club: g.Club, ClubName: g.Name, Time: now,
			Text: fmt.Sprintf("court-gateway: шлюз клуба «%s» (%s) снова на связи (не было связи %s).",
				g.Name, g.Club, minutes(now.Sub(since)))})
	}

	ready, reason := m.probe(ctx, g.Club)
	m.mu.Lock()
	h.checked, h.ready, h.reason, h.lastCheck = true, ready, reason, now
	var fireNotReady, fireReady bool
	if ready {
		fireReady = h.alertedNotReady
		h.failures, h.alertedNotReady = 0, false
	} else {
		h.failures++
		if h.failures >= m.readyFailures && !h.alertedNotReady {
			fireNotReady, h.alertedNotReady = true, true
		}
	}
	failures := h.failures
	m.mu.Unlock()
	switch {
	case fireNotReady:
		m.send(ctx, Alert{Event: EventNotReady, Club: g.Club, ClubName: g.Name, Time: now,
			Text: fmt.Sprintf("court-gateway: шлюз клуба «%s» (%s) на связи, но НЕ ГОТОВ: %s (%d проверки подряд).",
				g.Name, g.Club, reason, failures)})
	case fireReady:
		m.send(ctx, Alert{Event: EventReady, Club: g.Club, ClubName: g.Name, Time: now,
			Text: fmt.Sprintf("court-gateway: шлюз клуба «%s» (%s) снова готов.", g.Name, g.Club)})
	}
}

var reasonRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,39}$`)

// probe asks the gateway for /health/ready through the tunnel. The reason
// it reports is accepted only as a short upper-case code, so a gateway
// cannot put arbitrary text into an operator alert.
func (m *Monitor) probe(ctx context.Context, club string) (bool, string) {
	req := tunnel.Request{
		ID: newCallID(), RequestID: "hb-" + newCallID()[:16], Method: http.MethodGet, Path: "/health/ready",
		TimeoutMS: int(m.probeTimeout / time.Millisecond),
	}
	resp, err := m.broker.Do(ctx, club, req, m.probeTimeout, m.probeTimeout+5*time.Second)
	switch {
	case err == ErrOffline:
		return false, "TUNNEL_OFFLINE"
	case err != nil:
		return false, "TUNNEL_TIMEOUT"
	case resp.Status == http.StatusOK:
		return true, ""
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(resp.Body, &body) == nil && reasonRe.MatchString(body.Reason) {
		return false, body.Reason
	}
	return false, fmt.Sprintf("HTTP_%d", resp.Status)
}

func (m *Monitor) send(ctx context.Context, a Alert) {
	m.log.Warn("alert", "event", a.Event, "club", a.Club)
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
