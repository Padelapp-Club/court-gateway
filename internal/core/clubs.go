package core

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Clubs serves every club of an installation on one handler. The set of
// clubs is replaced as a whole (a new config from the hub) without a
// restart and without disturbing requests in flight: each request is
// served by the set that was current when it arrived.
type Clubs struct {
	log *slog.Logger
	cur atomic.Pointer[clubTable]
	// readyNoClubs answers readiness while no club is configured (the
	// CRM login of the account); nil means ready.
	readyNoClubs func(context.Context) error
	// gatewayAPI serves /gateway/ (the operator's requests).
	gatewayAPI http.Handler
}

type clubEntry struct {
	srv *Server
	api http.Handler
}

type clubTable struct {
	// loaded is false until the first Set: a gateway that has not yet
	// received its config answers 503, not "no such club".
	loaded bool
	ids    []string
	byID   map[string]clubEntry
	// unavailable are configured clubs whose config does not load.
	unavailable map[string]bool
}

// ClubsOptions configure NewClubs.
type ClubsOptions struct {
	Logger *slog.Logger
	// ReadyNoClubs is the readiness check while no club is configured.
	ReadyNoClubs func(context.Context) error
}

// NewClubs builds an empty set.
func NewClubs(o ClubsOptions) *Clubs {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	c := &Clubs{log: o.Logger, readyNoClubs: o.ReadyNoClubs}
	c.cur.Store(&clubTable{byID: map[string]clubEntry{}})
	return c
}

// SetGatewayAPI sets the handler of /gateway/... (the operator's requests,
// e.g. CRM discovery); it must be called before TunnelHandler.
func (c *Clubs) SetGatewayAPI(h http.Handler) { c.gatewayAPI = h }

// Set replaces the served clubs. unavailable are clubs of the config that
// are not served because their config does not load: partners get 503
// for them rather than 404.
func (c *Clubs) Set(servers []*Server, unavailable ...string) {
	t := &clubTable{loaded: true, byID: make(map[string]clubEntry, len(servers)), unavailable: map[string]bool{}}
	for _, id := range unavailable {
		t.unavailable[id] = true
	}
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

// readyParallel bounds how many clubs are checked at once.
const readyParallel = 4

// Ready checks every club's CRM access, a few clubs at a time, each within
// its own ReadyTimeout (and ctx); with no club it checks the account. The
// error is the first club's, in id order. A club found not to match its
// CRM (driver.MisconfigError) is blocked until a later check passes.
func (c *Clubs) Ready(ctx context.Context) error {
	ctx = driver.WithEvaluation(ctx)
	t := c.cur.Load()
	if len(t.ids) == 0 {
		if c.readyNoClubs == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, ReadyTimeout)
		defer cancel()
		return c.readyNoClubs(ctx)
	}
	errs := make([]error, len(t.ids))
	sem := make(chan struct{}, readyParallel)
	var wg sync.WaitGroup
	for i, id := range t.ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			ctx, cancel := context.WithTimeout(ctx, ReadyTimeout)
			defer cancel()
			srv := t.byID[id].srv
			errs[i] = srv.drv.Ready(ctx)
			srv.noteCheck(errs[i])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// APIHandler routes /v1/clubs/{club}/... to the club's own API (partners
// get the club list from the hub). It expects the partner name in the
// request context.
func (c *Clubs) APIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := auth.PartnerName(r.Context()); ok {
			info(r).partner = name
		}
		t := c.cur.Load()
		rest, ok := strings.CutPrefix(r.URL.Path, "/v1/clubs/")
		if !ok {
			WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
			return
		}
		club, _, _ := strings.Cut(rest, "/")
		e, ok := t.byID[club]
		switch {
		case ok:
		case !t.loaded:
			w.Header().Set("Retry-After", "10")
			WriteError(w, r, http.StatusServiceUnavailable, "CONFIG_PENDING", "the gateway has not received its club configuration yet")
			return
		case t.unavailable[club]:
			w.Header().Set("Retry-After", "60")
			WriteError(w, r, http.StatusServiceUnavailable, "CLUB_UNAVAILABLE", "the club's configuration does not load on its gateway")
			return
		default:
			WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such club")
			return
		}
		if e.srv.mismatch.Load() {
			w.Header().Set("Retry-After", "60")
			WriteError(w, r, http.StatusServiceUnavailable, "CLUB_UNAVAILABLE", "the club's configuration does not match its CRM")
			return
		}
		e.api.ServeHTTP(w, r)
	})
}

// TunnelHandler serves what the hub sends through the tunnel: the partner
// API (the hub names the partner) and the gateway API for the operator.
func (c *Clubs) TunnelHandler() http.Handler {
	mux := http.NewServeMux()
	api := c.APIHandler()
	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.PartnerName(r.Context()); !ok {
			WriteError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "the hub did not name the partner")
			return
		}
		api.ServeHTTP(w, r)
	}))
	if c.gatewayAPI != nil {
		mux.Handle("/gateway/", c.gatewayAPI)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	})
	return Observe(c.log, time.Now, mux)
}
