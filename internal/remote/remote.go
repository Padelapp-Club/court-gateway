// Package remote keeps a gateway's clubs equal to its installation's club
// configuration on the hub (pulled when the hub announces a new version,
// kept in memory only) and answers the hub's discovery requests. Each club
// is accepted or refused on its own; see Syncer.Apply.
package remote

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// maxErrorText bounds the refusal reason sent in heartbeats.
const maxErrorText = 2000

// Syncer applies the hub's club configuration.
type Syncer struct {
	base      *core.Settings
	factories map[string]driver.Factory
	clubs     *core.Clubs
	fetch     func(context.Context) (tunnel.RemoteConfig, error)
	log       *slog.Logger
	hook      func(*driver.Config)
	retry     time.Duration
	now       func() time.Time

	notify chan int64

	op sync.Mutex // one Apply or recheck at a time

	mu         sync.Mutex
	applied    int64
	hub        int64                       // the hub's version, as last announced
	epoch      string                      // of the applied version
	hubEpoch   string                      // of the hub's
	behind     time.Time                   // since when applied differs from hub
	configured map[string]bool             // clubs of the applied config
	waiting    map[string]*core.Server     // built, not yet confirmed by the CRM
	courts     map[string][]core.CourtSpec // of the clubs whose config does not build
	refused    map[string]string           // club -> why its config is not served
	ignored    map[string]bool             // unknown club fields already logged
}

// Options configure New.
type Options struct {
	Base      *core.Settings
	Factories map[string]driver.Factory
	Clubs     *core.Clubs
	Logger    *slog.Logger
	// DriverHook adjusts each club's driver config before the driver is
	// built (tests: an HTTP client for a CRM stub).
	DriverHook func(*driver.Config)
	// Retry is the wait after a failed pull (default 5s), doubling with
	// failures in a row up to maxRetry.
	Retry time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// New builds a Syncer.
func New(o Options) *Syncer {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Retry <= 0 {
		o.Retry = 5 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Syncer{base: o.Base, factories: o.Factories, clubs: o.Clubs, now: o.Now,
		log: o.Logger, hook: o.DriverHook, retry: o.Retry, notify: make(chan int64, 1), applied: -1, refused: map[string]string{}, ignored: map[string]bool{}}
}

// SetFetch sets how the config is pulled (tunnel.Client.FetchConfig),
// before Run.
func (s *Syncer) SetFetch(f func(context.Context) (tunnel.RemoteConfig, error)) { s.fetch = f }

// Notify hands over the hub's current version and epoch (latest wins).
// It never blocks: it is called from the tunnel's poll loop.
func (s *Syncer) Notify(v int64, epoch string) {
	s.mu.Lock()
	s.hub, s.hubEpoch = v, epoch
	s.track()
	s.mu.Unlock()
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

// State returns the applied version, the refused clubs and the clubs served.
func (s *Syncer) State() tunnel.AppliedConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := tunnel.AppliedConfig{ConfigVersion: max(s.applied, 0), Clubs: s.clubs.IDs()}
	if len(s.refused) > 0 {
		st.ClubErrors = maps.Clone(s.refused)
		var lines []string
		for _, id := range slices.Sorted(maps.Keys(s.refused)) {
			lines = append(lines, s.refused[id])
		}
		st.ConfigError = strings.Join(lines, "\n")
		if len(st.ConfigError) > maxErrorText {
			st.ConfigError = st.ConfigError[:maxErrorText] + "…"
		}
	}
	return st
}

// track notes since when the applied version differs from the hub's and
// whether the hub's is newer, so a club not served may be in it
// (core.Clubs.SetPending); s.mu must be held.
func (s *Syncer) track() {
	switch {
	case s.applied == s.hub && s.epoch == s.hubEpoch:
		s.behind = time.Time{}
	case s.behind.IsZero():
		s.behind = s.now()
	}
	s.clubs.SetPending(s.hub > s.applied || s.epoch != s.hubEpoch)
}

// Applied reports whether a config from the hub has been applied.
func (s *Syncer) Applied() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied >= 0
}

// Behind is how long the applied config has differed from the hub's
// version (0 when it does not): a config that cannot be pulled.
func (s *Syncer) Behind() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.behind.IsZero() {
		return 0
	}
	return s.now().Sub(s.behind)
}

func (s *Syncer) wanted(v int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return v != s.applied || s.epoch != s.hubEpoch
}

// maxRetry bounds the wait between failed pulls.
const maxRetry = time.Minute

// Run pulls and applies configs until ctx is done. A pull that crossed a
// newer version (tunnel.ErrStaleConfig) is repeated at once; after a
// failed one it waits Retry, doubling up to maxRetry whatever is announced
// meanwhile, then pulls the newest. A row's first failure is a warning.
func (s *Syncer) Run(ctx context.Context) {
	failures := 0
	again := false // pull again at once
	var v int64
	for {
		switch {
		case again:
		case failures == 0:
			select {
			case <-ctx.Done():
				return
			case v = <-s.notify:
			}
		default:
			select {
			case v = <-s.notify: // announced during the wait
			default:
			}
		}
		if !s.wanted(v) {
			failures, again = 0, false
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rc, err := s.fetch(fctx)
		cancel()
		if err == nil {
			failures, again = 0, false
			_ = s.Apply(ctx, rc)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if again = !again && errors.Is(err, tunnel.ErrStaleConfig); again {
			continue
		}
		failures++
		wait := min(s.retry<<min(failures-1, 16), maxRetry)
		lvl := slog.LevelDebug
		if failures == 1 {
			lvl = slog.LevelWarn
		}
		s.log.Log(ctx, lvl, "club config not pulled from the hub, retrying", "version", v, "retry_in", wait.String(), "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// configCheckTimeout bounds the check of one club against the CRM when a
// config is applied; the readiness round repeats it within ReadyTimeout.
const configCheckTimeout = 20 * time.Second

// Apply serves the clubs of rc that load and that the CRM confirms
// (driver.ConfigChecker). Until then a club keeps its previous config, if
// no other club's config now books one of its rooms, or answers 503; Ready
// asks the CRM again every round. It returns the problems found.
func (s *Syncer) Apply(ctx context.Context, rc tunnel.RemoteConfig) error {
	s.op.Lock()
	defer s.op.Unlock()
	servers, refused, ignored := Build(s.base, s.factories, s.hook, rc)
	s.mu.Lock()
	var fresh []string
	for _, f := range ignored {
		if !s.ignored[f] {
			s.ignored[f] = true
			fresh = append(fresh, f)
		}
	}
	if len(fresh) > 0 {
		s.log.Warn("club config from the hub has fields this version does not know; they are ignored", "fields", strings.Join(fresh, ","))
	}
	s.applied, s.epoch = rc.Version, rc.Epoch
	s.track()
	s.configured, s.waiting, s.courts, s.refused = map[string]bool{}, map[string]*core.Server{}, map[string][]core.CourtSpec{}, map[string]string{}
	var problems []string
	for _, r := range refused {
		problems = append(problems, r.Problems...)
		why := strings.Join(r.Problems, "; ")
		if prev := s.refused[r.Club]; prev != "" {
			why = prev + "; " + why
		}
		s.refused[r.Club] = why
		if r.Club != "" {
			s.configured[r.Club] = true
			s.courts[r.Club] = r.Courts
		}
	}
	for _, srv := range servers {
		s.configured[srv.Settings().ClubID] = true
		s.waiting[srv.Settings().ClubID] = srv
	}
	s.mu.Unlock()
	// the whole check is bounded too: a readiness round must not wait long
	ectx, cancel := context.WithTimeout(ctx, configCheckTimeout)
	defer cancel()
	problems = append(problems, s.verify(driver.WithEvaluation(ectx), configCheckTimeout)...)
	if len(problems) > 0 {
		s.log.Error("club config from the hub applied without the clubs that are not confirmed", "version", rc.Version,
			"clubs", strings.Join(s.clubs.IDs(), ","), "error", strings.Join(problems, "; "))
		return &core.ConfigError{Problems: problems}
	}
	s.log.Info("club config applied", "version", rc.Version, "clubs", strings.Join(s.clubs.IDs(), ","))
	return nil
}

// Ready is the installation's readiness round: it serves the waiting clubs
// the CRM now confirms, then checks every served club (core.Clubs.Ready),
// sharing the account's reads. It does not wait for a running Apply.
func (s *Syncer) Ready(ctx context.Context) error {
	ctx = driver.WithEvaluation(ctx)
	if s.op.TryLock() {
		s.mu.Lock()
		n := len(s.waiting)
		s.mu.Unlock()
		if n > 0 {
			if problems := s.verify(ctx, core.ReadyTimeout); len(problems) == 0 {
				s.log.Info("club config confirmed by the CRM", "clubs", strings.Join(s.clubs.IDs(), ","))
			}
		}
		s.op.Unlock()
	}
	return s.clubs.Ready(ctx)
}

// verify asks the CRM about every waiting club and serves the configured
// set, confirmed clubs with their new config, others with their previous
// one; it returns the waiting clubs' problems. The caller holds s.op.
func (s *Syncer) verify(ctx context.Context, timeout time.Duration) []string {
	s.mu.Lock()
	ids := slices.Sorted(maps.Keys(s.waiting))
	waiting := maps.Clone(s.waiting)
	s.mu.Unlock()
	confirmed := map[string]*core.Server{}
	failed := map[string]string{}
	for _, id := range ids {
		var err error
		if cc, ok := waiting[id].Driver().(driver.ConfigChecker); ok {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			err = cc.CheckConfig(cctx)
			cancel()
		}
		var me *driver.MisconfigError
		var ca *driver.ConfigAlert
		switch {
		case err == nil, errors.As(err, &ca): // an alert is reported by readiness, not refused
			confirmed[id] = waiting[id]
		case errors.As(err, &me):
			var ps []string
			for _, p := range me.Problems {
				ps = append(ps, "клуб «"+id+"»: "+p)
			}
			failed[id] = strings.Join(ps, "; ")
		default:
			failed[id] = fmt.Sprintf("клуб «%s»: конфигурацию не удалось сверить с CRM (%s), сверка повторится", id, core.ReadyReason(err))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var servers []*core.Server
	var unavailable, problems []string
	claims := map[string]string{} // CRM resource -> club whose current config books it
	claim := func(club string, srv *core.Server) {
		for _, r := range resources(srv) {
			claims[r] = club
		}
	}
	retained := map[string]*core.Server{}
	for id := range s.configured {
		if srv, ok := confirmed[id]; ok {
			delete(s.waiting, id)
			delete(s.refused, id)
			servers = append(servers, srv)
			claim(id, srv)
			continue
		}
		if f, ok := failed[id]; ok {
			s.refused[id] = f
			problems = append(problems, f)
		}
		if w, ok := s.waiting[id]; ok {
			claim(id, w)
		}
		old, ok := s.clubs.Server(id)
		switch {
		case !ok:
			unavailable = append(unavailable, id)
		case s.waiting[id] != nil || s.refused[id] != "": // its previous config
			if srv, err := s.without(old, s.gone(id, old)); err == nil {
				retained[id] = srv
			} else {
				why := fmt.Sprintf("клуб «%s»: прежняя конфигурация не обслуживается: %v", id, err)
				s.refused[id] = strings.TrimPrefix(s.refused[id]+"; "+why, "; ")
				problems = append(problems, why)
				unavailable = append(unavailable, id)
			}
		default:
			servers = append(servers, old)
			claim(id, old)
		}
	}
	// a previous config is not served once another club's config books
	// one of its rooms: two clubs never book one room
	for id, old := range retained {
		other := ""
		for _, r := range resources(old) {
			if c := claims[r]; c != id {
				other = cmp.Or(other, c)
			}
		}
		if other != "" {
			why := fmt.Sprintf("клуб «%s»: прежняя конфигурация не обслуживается — её комнату теперь бронирует клуб «%s» (ROOM_CONFLICT)", id, other)
			s.refused[id] = strings.TrimPrefix(s.refused[id]+"; "+why, "; ")
			problems = append(problems, why)
			unavailable = append(unavailable, id)
			continue
		}
		servers = append(servers, old)
	}
	s.clubs.Set(s.epoch, servers, unavailable...)
	slices.Sort(problems)
	return problems
}

// gone tells the courts of old, the previous config of club id, that its
// newer config not served removed or moved to another CRM resource. When
// that config does not build, the resources it names are not known: a
// court whose entry it changed counts as moved. s.mu must be held.
func (s *Syncer) gone(id string, old *core.Server) func(court string) bool {
	if next := s.waiting[id]; next != nil {
		return func(c string) bool {
			_, ok := next.Settings().CourtByID(c)
			return !ok || resource(old, c) != resource(next, c)
		}
	}
	params := map[string]map[string]any{}
	for _, c := range s.courts[id] {
		params[strings.TrimSpace(c.ID)] = c.Params
	}
	return func(c string) bool {
		i := slices.IndexFunc(old.Settings().Driver.Courts, func(d driver.CourtConfig) bool { return d.ID == c })
		p, ok := params[c]
		return !ok || i < 0 || !reflect.DeepEqual(p, map[string]any(old.Settings().Driver.Courts[i].Params))
	}
}

// without is the previous config old less the courts gone (old itself
// when none are). Its driver is built for the courts left, so it neither
// books, reads nor cancels on the others, and it stays blocked while old
// is (core.Server.Narrowed); an error when it cannot be built (no court
// left).
func (s *Syncer) without(old *core.Server, gone func(court string) bool) (*core.Server, error) {
	st := *old.Settings()
	st.Courts = slices.DeleteFunc(slices.Clone(st.Courts), func(c core.Court) bool { return gone(c.ID) })
	if len(st.Courts) == len(old.Settings().Courts) {
		return old, nil
	}
	st.Driver.Courts = slices.DeleteFunc(slices.Clone(st.Driver.Courts), func(c driver.CourtConfig) bool { return gone(c.ID) })
	d, err := core.BuildDriver(&st, s.factories)
	if err != nil {
		return nil, err
	}
	return old.Narrowed(&st, d), nil
}

// resource is the CRM resource court of srv books ("" when the driver
// does not name it).
func resource(srv *core.Server, court string) string {
	if r, ok := srv.Driver().(driver.Resourcer); ok {
		return r.Resource(court)
	}
	return ""
}

// resources are the CRM resources the courts of srv book
// (driver.Resourcer; none when the driver does not name them).
func resources(srv *core.Server) []string {
	r, ok := srv.Driver().(driver.Resourcer)
	if !ok {
		return nil
	}
	var out []string
	for _, c := range srv.Settings().Courts {
		out = append(out, r.Resource(c.ID))
	}
	return out
}

// Refusal is a club of a hub config that does not load. Club is empty
// when the entry could not even be read.
type Refusal struct {
	Club     string
	Problems []string
	Courts   []core.CourtSpec // of the club's entry, when Club is known
}

// Build builds the server of every club in rc that loads, a refusal (in
// Russian) for every other, and the club fields this version ignored (a
// newer hub). hook adjusts each driver config (tests).
func Build(base *core.Settings, factories map[string]driver.Factory, hook func(*driver.Config), rc tunnel.RemoteConfig) ([]*core.Server, []Refusal, []string) {
	var refused []Refusal
	var servers []*core.Server
	var ignored []string
	seen := map[string]bool{}
	for i, raw := range rc.Clubs {
		spec, unknown, err := core.DecodeClubTolerant(raw)
		for _, f := range unknown {
			if !slices.Contains(ignored, f) {
				ignored = append(ignored, f)
			}
		}
		if err != nil {
			refused = append(refused, Refusal{Problems: []string{fmt.Sprintf("clubs[%d]: %v", i, err)}})
			continue
		}
		if seen[spec.ID] {
			refused = append(refused, Refusal{Problems: []string{fmt.Sprintf("клуб «%s» указан дважды", spec.ID)}})
			continue
		}
		seen[spec.ID] = true
		srv, err := buildClub(base, factories, hook, spec)
		if err != nil {
			r := Refusal{Club: spec.ID, Problems: []string{err.Error()}, Courts: spec.Courts}
			var ce *core.ConfigError
			if errors.As(err, &ce) {
				r.Problems = ce.Problems
			}
			refused = append(refused, r)
			continue
		}
		servers = append(servers, srv)
	}
	slices.Sort(ignored)
	return servers, refused, ignored
}

func buildClub(base *core.Settings, factories map[string]driver.Factory, hook func(*driver.Config), spec core.ClubSpec) (*core.Server, error) {
	st, err := base.WithClub(spec, drivers.ClubCRMKeys(base.CRMType))
	if err != nil {
		return nil, err
	}
	if hook != nil {
		hook(&st.Driver)
	}
	d, err := core.BuildDriver(st, factories)
	if err != nil {
		var ce *core.ConfigError
		if errors.As(err, &ce) {
			for i := range ce.Problems {
				ce.Problems[i] = "клуб «" + st.ClubID + "»: " + ce.Problems[i]
			}
		}
		return nil, err
	}
	return core.NewServer(st, d, st.Driver.Logger), nil
}

// ---- discovery ----

// DiscoverRequest is the body of a discovery request from the hub.
type DiscoverRequest struct {
	Studio      string `json:"studio,omitempty"`
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

// DiscoverResponse is the gateway's answer: the account's studios and, for
// the asked studio, a draft club config (not saved anywhere).
type DiscoverResponse struct {
	Studios       []StudioDTO            `json:"studios"`
	StudiosReason string                 `json:"studios_reason,omitempty"`
	Studio        string                 `json:"studio,omitempty"`
	Draft         *core.ClubSpec         `json:"draft"`
	PriceLists    []driver.PriceListInfo `json:"price_lists"`
	Notices       []driver.Notice        `json:"notices"`
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
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		d, err := acct.Discover(ctx, driver.DiscoverQuery{Studio: strings.TrimSpace(req.Studio)})
		if err != nil {
			reason := core.ReadyReason(err)
			log.Warn("discovery failed", "reason", reason, "error", err)
			core.WriteError(w, r, http.StatusBadGateway, reason, "the CRM could not be read: "+reason)
			return
		}
		served := map[string]string{} // studio -> club
		for _, id := range clubs.IDs() {
			if srv, ok := clubs.Server(id); ok {
				if ac, ok := srv.Driver().(driver.AccountClub); ok && ac.Studio() != "" {
					served[strings.ToLower(ac.Studio())] = id
				}
			}
		}
		out := DiscoverResponse{Studios: []StudioDTO{}, StudiosReason: d.StudiosReason, Studio: d.Studio,
			PriceLists: append([]driver.PriceListInfo{}, d.PriceLists...), Notices: []driver.Notice{}}
		for _, s := range d.Studios {
			club := served[strings.ToLower(s.ID)]
			out.Studios = append(out.Studios, StudioDTO{ID: s.ID, Name: s.Name, Enabled: club != "", Club: club})
		}
		if d.Studio != "" {
			spec, notes := core.DraftClub(d, req.Timezone, req.SlotMinutes)
			out.Draft = &spec
			out.Notices = append(out.Notices, notes...)
		} else {
			out.Notices = append(out.Notices, d.Notices...)
		}
		core.WriteJSON(w, http.StatusOK, out)
	})
}
