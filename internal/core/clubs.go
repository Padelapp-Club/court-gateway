package core

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
)

// Clubs serves every club of an installation on one handler. The set of
// clubs is replaced as a whole (a new config from the hub) without a
// restart and without disturbing requests in flight: each request is
// served by the set that was current when it arrived.
type Clubs struct {
	log  *slog.Logger
	now  func() time.Time
	auth *auth.Authenticator
	cur  atomic.Pointer[clubTable]
	// readyNoClubs answers readiness while no club is configured (the
	// CRM login of the account); nil means ready.
	readyNoClubs func(context.Context) error
	// gatewayAPI serves /gateway/ on the tunnel handler only.
	gatewayAPI http.Handler
}

type clubEntry struct {
	srv *Server
	api http.Handler
}

type clubTable struct {
	ids  []string
	byID map[string]clubEntry
}

// ClubsOptions configure NewClubs.
type ClubsOptions struct {
	Logger *slog.Logger
	Now    func() time.Time
	// ReadyNoClubs is the readiness check while no club is configured.
	ReadyNoClubs func(context.Context) error
	// GatewayAPI serves /gateway/... requests that arrive from the hub
	// through the tunnel (e.g. CRM discovery). It is never reachable on
	// the local port.
	GatewayAPI http.Handler
}

// NewClubs builds an empty set; partner keys for the local port come from
// base.Partners.
func NewClubs(base *Settings, o ClubsOptions) (*Clubs, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &Clubs{log: o.Logger, now: o.Now, readyNoClubs: o.ReadyNoClubs, gatewayAPI: o.GatewayAPI}
	if len(base.Partners) > 0 {
		a, err := auth.New(base.Partners, auth.Options{WriteError: writeError, Now: o.Now})
		if err != nil {
			return nil, err
		}
		c.auth = a
	}
	c.cur.Store(&clubTable{byID: map[string]clubEntry{}})
	return c, nil
}

// SetGatewayAPI sets the handler of /gateway/... on the tunnel handler; it
// must be called before TunnelHandler.
func (c *Clubs) SetGatewayAPI(h http.Handler) { c.gatewayAPI = h }

// Set replaces the served clubs.
func (c *Clubs) Set(servers []*Server) {
	t := &clubTable{byID: make(map[string]clubEntry, len(servers))}
	for _, s := range servers {
		if _, dup := t.byID[s.cfg.ClubID]; dup {
			continue
		}
		t.byID[s.cfg.ClubID] = clubEntry{srv: s, api: s.APIHandler()}
		t.ids = append(t.ids, s.cfg.ClubID)
	}
	sort.Strings(t.ids)
	c.cur.Store(t)
}

// IDs returns the ids of the served clubs, sorted.
func (c *Clubs) IDs() []string { return append([]string(nil), c.cur.Load().ids...) }

// Server returns the server of one club.
func (c *Clubs) Server(id string) (*Server, bool) {
	e, ok := c.cur.Load().byID[id]
	return e.srv, ok
}

// Ready checks every club's CRM access (one CRM account, so in practice
// the first failure is everybody's); with no club it checks the account.
func (c *Clubs) Ready(ctx context.Context) error {
	t := c.cur.Load()
	if len(t.ids) == 0 {
		if c.readyNoClubs != nil {
			return c.readyNoClubs(ctx)
		}
		return nil
	}
	for _, id := range t.ids {
		if err := t.byID[id].srv.drv.Ready(ctx); err != nil {
			return err
		}
	}
	return nil
}

// APIHandler routes /v1/clubs and /v1/clubs/{club}/... to the club's own
// API. It expects the partner name in the request context.
func (c *Clubs) APIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := auth.PartnerName(r.Context()); ok {
			info(r).partner = name
		}
		t := c.cur.Load()
		p := r.URL.Path
		if p == "/v1/clubs" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				writeError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed for this endpoint")
				return
			}
			list := make([]clubRef, 0, len(t.ids))
			for _, id := range t.ids {
				list = append(list, clubRef{ID: id, Name: t.byID[id].srv.cfg.ClubName})
			}
			writeJSON(w, http.StatusOK, map[string][]clubRef{"clubs": list})
			return
		}
		rest, ok := strings.CutPrefix(p, "/v1/clubs/")
		if !ok {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
			return
		}
		club, _, _ := strings.Cut(rest, "/")
		e, ok := t.byID[club]
		if !ok {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "no such club")
			return
		}
		e.api.ServeHTTP(w, r)
	})
}

func (c *Clubs) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), ReadyTimeout)
	defer cancel()
	if err := c.Ready(ctx); err != nil {
		reason := ReadyReason(err)
		c.log.Warn("not ready", "request_id", info(r).id, "reason", reason, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (c *Clubs) health(mux *http.ServeMux) {
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	mux.HandleFunc("GET /health/ready", c.handleReady)
	mux.Handle("/health/", methodNotAllowed())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	})
}

// Handler is the local-port handler: health and, when gateway.yaml lists
// partners, the partner API behind their keys.
func (c *Clubs) Handler() http.Handler {
	mux := http.NewServeMux()
	c.health(mux)
	if c.auth != nil {
		mux.Handle("/v1/", c.auth.Wrap(c.APIHandler()))
	} else {
		mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "the partner API of this gateway is served through the hub")
		})
	}
	return Observe(c.log, c.now, mux)
}

// TunnelHandler serves requests from the hub: health, the partner API
// (the hub names the partner) and the gateway API for the operator.
func (c *Clubs) TunnelHandler() http.Handler {
	mux := http.NewServeMux()
	c.health(mux)
	api := c.APIHandler()
	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.PartnerName(r.Context()); !ok {
			writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "the hub did not name the partner")
			return
		}
		api.ServeHTTP(w, r)
	}))
	if c.gatewayAPI != nil {
		mux.Handle("/gateway/", c.gatewayAPI)
	}
	return Observe(c.log, c.now, mux)
}
