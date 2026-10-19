package tunnel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Heartbeat is what a gateway reports to the hub after each self-check. The
// hub treats a gateway as offline when heartbeats stop (a dead-man switch),
// so it never has to call the club's CRM through the tunnel itself.
type Heartbeat struct {
	Version string `json:"version"`
	// Build is BuildRelease or BuildSelf.
	Build string `json:"build"`
	Ready bool   `json:"ready"`
	// Reason is a short upper-case code when Ready is false; ReasonClubs
	// are the clubs it is about (none for the account or the hub).
	ReasonDetail string    `json:"reason_detail,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	ReasonClubs  []string  `json:"reason_clubs,omitempty"`
	CheckedAt    time.Time `json:"checked_at"`
	// NextLogin is when the gateway tries a login the CRM rejected again
	// (the club's set-password: at once); zero without a rejection.
	NextLogin time.Time `json:"next_login,omitzero"`
	// RefusedRequests counts, since the gateway started, the requests and
	// configs it refused because the hub had not signed them for it.
	RefusedRequests int64 `json:"refused_requests,omitempty"`
	AppliedConfig
}

// AppliedConfig is the club configuration a gateway runs: its version,
// why the config of a club was refused (the club keeps its previous
// config, if any), those reasons summed up, and the clubs it serves.
type AppliedConfig struct {
	ConfigVersion int64             `json:"config_version"`
	ConfigError   string            `json:"config_error,omitempty"`
	ClubErrors    map[string]string `json:"club_errors,omitempty"`
	Clubs         []string          `json:"clubs,omitempty"`
}

// selfCheckTimeout bounds one self-check: every club's readiness, each
// within its own limit.
const selfCheckTimeout = time.Minute

// MaxHeartbeatBody bounds an encoded heartbeat.
const MaxHeartbeatBody = 64 << 10

// SelfCheck runs the gateway's readiness check (CRM login and a narrow
// occupancy request) at most once per Interval. Its result feeds both the local health status
// file and the heartbeat to the hub. Step must be called from one
// goroutine.
type SelfCheck struct {
	// Ready is the driver's readiness check.
	Ready func(context.Context) error
	// Reason maps a readiness error to a short code with no secrets,
	// Clubs to the clubs it is about.
	Reason func(error) string
	// Detail returns only operator-safe text, never an arbitrary upstream error.
	Detail func(error) string
	Clubs  func(error) []string
	// Interval between checks (default 60s); each is bounded by
	// selfCheckTimeout.
	Interval time.Duration
	// Now is the clock (default time.Now).
	Now            func() time.Time
	Version, Build string

	last time.Time
}

// Step runs the check when it is due and reports its result as a heartbeat.
// It returns false when no check was due (or ctx is done).
func (s *SelfCheck) Step(ctx context.Context) (Heartbeat, bool) {
	now := s.clock()
	interval := s.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if !s.last.IsZero() && now.Sub(s.last) < interval {
		return Heartbeat{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, selfCheckTimeout)
	err := s.Ready(cctx)
	cancel()
	if ctx.Err() != nil {
		return Heartbeat{}, false
	}
	s.last = now
	hb := Heartbeat{Version: s.Version, Build: s.Build, Ready: err == nil, CheckedAt: s.clock().UTC()}
	if err != nil {
		hb.Reason = "NOT_READY"
		if s.Reason != nil {
			hb.Reason = s.Reason(err)
		}
		if s.Clubs != nil {
			hb.ReasonClubs = s.Clubs(err)
		}
		if s.Detail != nil {
			hb.ReasonDetail = s.Detail(err)
		}
	}
	return hb, true
}

func (s *SelfCheck) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// SendHeartbeat posts hb to the hub over the gateway's own connection. It
// does not retry: the hub tolerates missed heartbeats up to its offline
// threshold, and the next one follows after the next self-check.
func (c *Client) SendHeartbeat(ctx context.Context, hb Heartbeat) error {
	hb.RefusedRequests = c.refused.Load()
	body, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, PathHeartbeat, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return cleanError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	}
	return c.hubError(resp.StatusCode, "a heartbeat")
}
