// Package remote keeps a gateway's clubs equal to its installation's club
// configuration on the hub, and answers the hub's discovery requests.
//
// One installation of the gateway is one CRM account; the account may
// hold several clubs. The club side installs once and keeps only the CRM
// login and the hub connection; the operator adds, changes and removes
// clubs on the hub. The hub announces its config version on every poll
// and heartbeat answer; when it differs from the version the gateway
// runs, the gateway pulls the config, validates every club with the
// driver, and swaps the whole set at once. A config that does not
// validate is refused as a whole: the gateway keeps the previous one and
// reports why in its heartbeats. The config lives in memory only.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// maxErrorText bounds the refusal reason sent in heartbeats.
const maxErrorText = 2000

// Syncer applies the hub's club configuration.
type Syncer struct {
	base      *core.Settings
	factories map[string]driver.Factory
	clubs     *core.Clubs
	local     []*core.Server
	fetch     func(context.Context) (tunnel.RemoteConfig, error)
	log       *slog.Logger
	hook      func(*driver.Config)
	retry     time.Duration

	notify chan int64

	mu      sync.Mutex
	applied int64
	refused int64 // version refused last; -1 for none
	errText string
}

// Options configure New.
type Options struct {
	Base      *core.Settings
	Factories map[string]driver.Factory
	Clubs     *core.Clubs
	// Local are the clubs of gateway.yaml, served next to the hub's; a
	// club of the hub with the same id replaces one of them.
	Local []*core.Server
	// Fetch pulls the config (tunnel.Client.FetchConfig).
	Fetch  func(context.Context) (tunnel.RemoteConfig, error)
	Logger *slog.Logger
	// DriverHook adjusts each club's driver config before the driver is
	// built (tests: an HTTP client for a CRM stub).
	DriverHook func(*driver.Config)
	// Retry is the wait after a failed pull (default 5s).
	Retry time.Duration
}

// New builds a Syncer and starts serving the local clubs.
func New(o Options) *Syncer {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Retry <= 0 {
		o.Retry = 5 * time.Second
	}
	s := &Syncer{base: o.Base, factories: o.Factories, clubs: o.Clubs, local: o.Local, fetch: o.Fetch,
		log: o.Logger, hook: o.DriverHook, retry: o.Retry, notify: make(chan int64, 1), refused: -1}
	s.clubs.Set(o.Local)
	return s
}

// SetFetch sets how the config is pulled (before Run).
func (s *Syncer) SetFetch(f func(context.Context) (tunnel.RemoteConfig, error)) { s.fetch = f }

// Notify hands over the hub's current version (latest wins). It never
// blocks: it is called from the tunnel's poll loop.
func (s *Syncer) Notify(v int64) {
	for {
		select {
		case s.notify <- v:
			return
		default:
		}
		select {
		case <-s.notify:
		default:
		}
	}
}

// State is what the heartbeat reports.
type State struct {
	Version int64
	Error   string
	Clubs   []string
}

// State returns the applied version, the last refusal and the clubs served.
func (s *Syncer) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return State{Version: s.applied, Error: s.errText, Clubs: s.clubs.IDs()}
}

func (s *Syncer) wanted(v int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return v != s.applied && v != s.refused
}

// Run pulls and applies configs until ctx is done.
func (s *Syncer) Run(ctx context.Context) {
	for {
		var v int64
		select {
		case <-ctx.Done():
			return
		case v = <-s.notify:
		}
		if !s.wanted(v) {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rc, err := s.fetch(fctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("club config not pulled from the hub, retrying", "version", v, "error", err)
			go func() {
				select {
				case <-ctx.Done():
				case <-time.After(s.retry):
					s.Notify(v)
				}
			}()
			continue
		}
		_ = s.Apply(rc)
	}
}

// Apply validates rc and, if every club is valid, serves exactly its clubs
// (plus the local ones it does not replace). Otherwise it keeps the
// previous set and records the reason.
func (s *Syncer) Apply(rc tunnel.RemoteConfig) error {
	servers, ids, err := s.build(rc)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		msg := err.Error()
		if len(msg) > maxErrorText {
			msg = msg[:maxErrorText] + "…"
		}
		s.refused, s.errText = rc.Version, msg
		s.log.Error("club config from the hub refused; the previous one stays", "version", rc.Version, "kept_version", s.applied, "error", msg)
		return err
	}
	s.clubs.Set(servers)
	s.applied, s.refused, s.errText = rc.Version, -1, ""
	s.log.Info("club config applied", "version", rc.Version, "clubs", strings.Join(ids, ","))
	return nil
}

func (s *Syncer) build(rc tunnel.RemoteConfig) ([]*core.Server, []string, error) {
	var problems []string
	var servers []*core.Server
	seen := map[string]bool{}
	for i, raw := range rc.Clubs {
		var spec core.ClubSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			problems = append(problems, fmt.Sprintf("clubs[%d]: %v", i, err))
			continue
		}
		if seen[spec.ID] {
			problems = append(problems, fmt.Sprintf("клуб «%s» указан дважды", spec.ID))
			continue
		}
		seen[spec.ID] = true
		srv, err := s.buildClub(spec)
		if err != nil {
			var ce *core.ConfigError
			if errors.As(err, &ce) {
				problems = append(problems, ce.Problems...)
			} else {
				problems = append(problems, err.Error())
			}
			continue
		}
		servers = append(servers, srv)
	}
	if len(problems) > 0 {
		return nil, nil, &core.ConfigError{Problems: problems}
	}
	for _, l := range s.local {
		if id := l.Settings().ClubID; !seen[id] {
			servers = append(servers, l)
		}
	}
	ids := make([]string, len(servers))
	for i, srv := range servers {
		ids[i] = srv.Settings().ClubID
	}
	return servers, ids, nil
}

func (s *Syncer) buildClub(spec core.ClubSpec) (*core.Server, error) {
	st, err := s.base.WithClub(spec)
	if err != nil {
		return nil, err
	}
	if s.hook != nil {
		s.hook(&st.Driver)
	}
	d, err := core.BuildDriver(st, s.factories)
	if err != nil {
		var ce *core.ConfigError
		if errors.As(err, &ce) {
			for i := range ce.Problems {
				ce.Problems[i] = "клуб «" + st.ClubID + "»: " + ce.Problems[i]
			}
		}
		return nil, err
	}
	return core.NewServer(st, d, st.Driver.Logger)
}

// ---- discovery ----

// DiscoverRequest is the body of a discovery request from the hub.
type DiscoverRequest struct {
	Studio      string `json:"studio,omitempty"`
	Days        int    `json:"days,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
	SlotMinutes int    `json:"slot_minutes,omitempty"`
}

// StudioDTO is one studio of the account.
type StudioDTO struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Club is the configured club that serves the studio, if any.
	Club string `json:"club_id,omitempty"`
}

// PriceListDTO names a price list.
type PriceListDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DiscoverResponse is the gateway's answer: the account's studios and, for
// the asked studio, a draft club config (not saved anywhere).
type DiscoverResponse struct {
	Studios       []StudioDTO    `json:"studios"`
	StudiosReason string         `json:"studios_reason,omitempty"`
	Studio        string         `json:"studio,omitempty"`
	Draft         *core.ClubSpec `json:"draft"`
	PriceLists    []PriceListDTO `json:"price_lists"`
	Notices       []core.Notice  `json:"notices"`
}

// DiscoverHandler serves tunnel.PathDiscover with the installation's CRM
// account. clubs tells which studios are already served.
func DiscoverHandler(acct driver.Account, clubs *core.Clubs, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tunnel.PathDiscover || r.Method != http.MethodPost {
			core.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
			return
		}
		if acct == nil {
			core.WriteError(w, r, http.StatusNotImplemented, "DISCOVERY_UNSUPPORTED", "this CRM type has no discovery")
			return
		}
		var req DiscoverRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed discovery request")
			return
		}
		if req.Days == 0 {
			req.Days = 28
		}
		if req.Days < 1 || req.Days > 90 {
			core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "days must be 1 to 90")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		d, err := acct.Discover(ctx, driver.DiscoverQuery{Studio: strings.TrimSpace(req.Studio), Days: req.Days})
		if err != nil {
			reason := core.ReadyReason(err)
			log.Warn("discovery failed", "reason", reason, "error", err)
			core.WriteError(w, r, http.StatusBadGateway, reason, "the CRM could not be read: "+reason)
			return
		}
		served := map[string]string{} // studio -> club
		for _, id := range clubs.IDs() {
			if srv, ok := clubs.Server(id); ok {
				if st, _ := srv.Settings().Driver.CRM.String("crm", "studio", false); st != "" {
					served[strings.ToLower(st)] = id
				}
			}
		}
		out := DiscoverResponse{Studios: []StudioDTO{}, StudiosReason: d.StudiosReason, Studio: d.Studio, PriceLists: []PriceListDTO{}, Notices: []core.Notice{}}
		for _, s := range d.Studios {
			club := served[strings.ToLower(s.ID)]
			out.Studios = append(out.Studios, StudioDTO{ID: s.ID, Name: s.Name, Enabled: club != "", Club: club})
		}
		for _, p := range d.PriceLists {
			out.PriceLists = append(out.PriceLists, PriceListDTO{ID: p.ID, Name: p.Name})
		}
		if d.Studio != "" {
			spec, notes := core.DraftClub(d, req.Timezone, req.SlotMinutes)
			out.Draft = &spec
			out.Notices = append(out.Notices, notes...)
		} else {
			for _, n := range d.Notices {
				out.Notices = append(out.Notices, core.Notice{Code: n.Code, Message: n.Message})
			}
		}
		core.WriteJSON(w, http.StatusOK, out)
	})
}
