package core

import (
	"context"
	"errors"
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
	log     *slog.Logger
	cur     atomic.Pointer[clubTable]
	pending atomic.Bool // SetPending
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
	// run is the hub run (config epoch) of the config they come from.
	run string
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

// Set replaces the served clubs with those of a config of hub run run (its
// epoch; "" for clubs not from the hub): a request the hub signed in
// another run (WithHubRun) gets 503 CONFIG_PENDING. unavailable are clubs of the
// config that are not served because their config does not load: partners
// get 503 for them rather than 404.
func (c *Clubs) Set(run string, servers []*Server, unavailable ...string) {
	t := &clubTable{loaded: true, byID: make(map[string]clubEntry, len(servers)), unavailable: map[string]bool{}, run: run}
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

type hubRunKey struct{}

// WithHubRun returns a context of a request the hub signed in run (its
// config epoch): only clubs of a config of that run serve it (Set).
func WithHubRun(ctx context.Context, run string) context.Context {
	return context.WithValue(ctx, hubRunKey{}, run)
}

// SetPending says whether the hub announced a config not applied yet: a
// club not served then answers 503 CONFIG_PENDING, not 404.
func (c *Clubs) SetPending(p bool) { c.pending.Store(p) }

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
// error is a *ReadyError: the first failing club's, in id order, or, when
// the clubs have only config alerts (driver.ConfigAlert), the first alert
// with every club that has one. A club found not to match its CRM
// (driver.MisconfigError) is blocked until a later check passes.
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
	var alerts *ReadyError
	for i, err := range errs {
		var ca *driver.ConfigAlert
		switch {
		case err == nil:
		case errors.As(err, &ca):
			if alerts == nil {
				alerts = &ReadyError{Err: err}
			}
			alerts.Clubs = append(alerts.Clubs, t.ids[i])
		default:
			return &ReadyError{Clubs: []string{t.ids[i]}, Err: err}
		}
	}
	if alerts != nil {
		return alerts
	}
	return nil
}

// APIHandler routes /v1/clubs/{club}/... to the club's own API (partners
// get the club list from the hub). It expects the partner name in the
// request context.
func (c *Clubs) APIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, _ := auth.PartnerName(r.Context())
		SetPartner(r, name)
		t := c.cur.Load()
		rest, ok := strings.CutPrefix(r.URL.Path, "/v1/clubs/")
		if !ok {
			WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
			return
		}
		club, _, _ := strings.Cut(rest, "/")
		e, ok := t.byID[club]
		run, tunnelled := r.Context().Value(hubRunKey{}).(string)
		other := tunnelled && t.run != "" && run != t.run // its run's config is not applied yet
		switch {
		case ok && !other:
		case !t.loaded || other || c.pending.Load() && !t.unavailable[club]:
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
