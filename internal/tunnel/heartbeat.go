package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Reason is a short upper-case code when Ready is false.
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	// ConfigVersion is the club configuration version the gateway runs;
	// ConfigError says why the hub's current version was refused (the
	// gateway then keeps running the previous one); Clubs are the clubs it
	// serves.
	ConfigVersion int64    `json:"config_version"`
	ConfigError   string   `json:"config_error,omitempty"`
	Clubs         []string `json:"clubs,omitempty"`
}

// MaxHeartbeatBody bounds an encoded heartbeat.
const MaxHeartbeatBody = 64 << 10

// SelfCheck runs the gateway's readiness check (CRM login and a narrow
// occupancy request) at most once per Interval. It is the only scheduled
// CRM call of a gateway: its result feeds both the local health status
// file and the heartbeat to the hub. Step must be called from one
// goroutine.
type SelfCheck struct {
	// Ready is the driver's readiness check.
	Ready func(context.Context) error
	// Reason maps a readiness error to a short code with no secrets.
	Reason func(error) string
	// Interval between checks (default 60s).
	Interval time.Duration
	// Timeout bounds one check (default 5s).
	Timeout time.Duration
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
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
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
	c.noteConfigVersion(resp.Header)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		c.authFail.Store(true)
		return ErrUnauthorized
	default:
		return fmt.Errorf("hub answered HTTP %d to a heartbeat", resp.StatusCode)
	}
}
