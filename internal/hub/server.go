package hub

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
	"github.com/Padelapp-Club/court-gateway/internal/version"
)

// Server is the hub's HTTP side.
type Server struct {
	cfg      *Settings
	store    *Store
	broker   *Broker
	monitor  *Monitor
	auth     *auth.Authenticator
	log      *slog.Logger
	now      func() time.Time
	opKey    [sha256.Size]byte
	hasOp    bool
	regLim   *failLimiter
	inflight inflight
}

// inflight counts the requests in flight per key (a partner at a gateway).
type inflight struct {
	mu sync.Mutex
	n  map[string]int
}

// acquire takes a place for key if fewer than limit are taken.
func (f *inflight) acquire(key string, limit int) (release func(), ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n[key] >= limit {
		return nil, false
	}
	if f.n == nil {
		f.n = map[string]int{}
	}
	f.n[key]++
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.n[key]--; f.n[key] == 0 {
			delete(f.n, key)
		}
	}, true
}

// Options for NewServer.
type Options struct {
	Now    func() time.Time
	Logger *slog.Logger
	// Version is the hub's own version, reported in status.json.
	Version string
}

// NewServer wires the store, broker, monitor and partner auth together.
func NewServer(cfg *Settings, store *Store, opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{cfg: cfg, store: store, log: opts.Logger, now: opts.Now, regLim: newFailLimiter(20, time.Minute)}
	s.broker = NewBroker(cfg.PollWait, opts.Now)
	store.mu.Lock()
	store.retire = s.broker.Retire
	store.mu.Unlock()
	s.monitor = NewMonitor(store, s.broker, MonitorConfig{
		Interval: cfg.Interval, OfflineAfter: cfg.OfflineAfter, ReadyFailures: cfg.ReadyFailures,
		LatestVersion: cfg.LatestVersion, Version: opts.Version, StatusDir: cfg.StatusDir,
		Now: opts.Now, Logger: opts.Logger,
	})
	if len(cfg.Partners) > 0 {
		a, err := auth.New(cfg.Partners, auth.Options{WriteError: core.WriteError, Now: opts.Now})
		if err != nil {
			return nil, err
		}
		s.auth = a
	}
	if cfg.OperatorKey != "" {
		s.opKey, s.hasOp = sha256.Sum256([]byte(cfg.OperatorKey)), true
	}
	return s, nil
}

// Monitor returns the heartbeat monitor (its Run is started by the caller).
func (s *Server) Monitor() *Monitor { return s.monitor }

func (s *Server) Store() *Store { return s.store }

// Handler returns the hub's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		core.WriteJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if err := s.store.Ready(); err != nil {
			s.log.Error("the hub's state is unavailable", "error", err)
			core.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST "+tunnel.PathRegister, s.handleRegister)
	mux.HandleFunc("GET "+tunnel.PathHubKey, func(w http.ResponseWriter, _ *http.Request) {
		core.WriteJSON(w, http.StatusOK, map[string]string{"hub_key": core.EncodeHubKey(s.store.HubKey())})
	})
	mux.HandleFunc("POST "+tunnel.PathPoll, s.gateway(s.handlePoll))
	mux.HandleFunc("POST "+tunnel.PathReply, s.gateway(s.handleReply))
	mux.HandleFunc("GET "+tunnel.PathWhoami, s.gateway(s.handleWhoami))
	mux.HandleFunc("POST "+tunnel.PathHeartbeat, s.gateway(s.handleHeartbeat))
	mux.HandleFunc("GET "+tunnel.PathConfig, s.gateway(s.handleConfig))
	mux.HandleFunc("POST "+tunnel.PathRelease, s.gateway(s.handleRelease))
	mux.HandleFunc("GET /hub/status", s.operator(s.handleStatus))
	mux.HandleFunc("POST /hub/installations", s.operator(s.handleCreateInstallation))
	mux.HandleFunc("GET /hub/installations/{inst}", s.operator(s.handleGetInstallation))
	mux.HandleFunc("DELETE /hub/installations/{inst}", s.operator(s.handleDeleteInstallation))
	mux.HandleFunc("POST /hub/installations/{inst}/join-codes", s.operator(s.handleCreateJoinCode))
	mux.HandleFunc("PUT /hub/installations/{inst}/clubs/{club}", s.operator(s.handlePutClub))
	mux.HandleFunc("DELETE /hub/installations/{inst}/clubs/{club}", s.operator(s.handleDeleteClub))
	mux.HandleFunc("POST /hub/installations/{inst}/discover", s.operator(s.handleDiscover))
	var api http.Handler = http.HandlerFunc(s.handlePartner)
	if s.auth != nil {
		api = s.auth.Wrap(api)
	} else {
		api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			core.WriteError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "unknown API key")
		})
	}
	mux.Handle("/v1/", api)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		core.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	})
	return core.Observe(s.log, s.now, mux)
}

func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---- gateway side ----

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	client := clientIP(r, s.cfg.TrustedProxies)
	if !s.regLim.allow(client, s.now()) {
		w.Header().Set("Retry-After", "60")
		core.WriteError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "too many failed registrations from this address, try again later")
		return
	}
	var req tunnel.RegisterRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed registration")
		return
	}
	g, secret, err := s.store.Redeem(req.Code)
	if errors.Is(err, ErrStateUnavailable) {
		// not a verdict on the code, nor the client's failure (the code may
		// be spent already if the state failed after taking it)
		s.log.Error("registration failed", "error", err)
		w.Header().Set("Retry-After", "10")
		core.WriteError(w, r, http.StatusServiceUnavailable, "HUB_UNAVAILABLE", "the hub cannot register a gateway right now, retry")
		return
	}
	if err != nil {
		s.regLim.fail(client, s.now())
		if errors.Is(err, ErrJoinCode) {
			core.WriteError(w, r, http.StatusForbidden, "JOIN_CODE_REJECTED", "the join code is unknown, already used or expired")
			return
		}
		s.log.Error("registration failed", "error", err)
		core.WriteError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	s.log.Info("gateway registered", "installation", g.Installation, "version", req.Version)
	out := tunnel.RegisterResponse{Installation: g.Installation, Name: g.Name, Secret: secret, HubKey: core.EncodeHubKey(s.store.HubKey())}
	out.Sign(s.store.key, req.Code)
	core.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) gateway(next func(http.ResponseWriter, *http.Request, GatewayRecord)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _ := auth.Bearer(r)
		g, err := s.store.Authenticate(key)
		switch {
		case errors.Is(err, ErrStateUnavailable):
			// not a verdict on the secret: the gateway retries, keeps it
			s.log.Error("cannot check a gateway secret", "error", err)
			w.Header().Set("Retry-After", "10")
			core.WriteError(w, r, http.StatusServiceUnavailable, "HUB_UNAVAILABLE", "the hub cannot check the secret right now, retry")
			return
		case err != nil:
			w.Header().Set("WWW-Authenticate", `Bearer realm="tunnel"`)
			core.WriteError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "unknown gateway secret")
			return
		}
		next(w, r, g)
	}
}

func meta(r *http.Request) ConnMeta {
	clip := func(v string) string {
		v = strings.TrimSpace(v)
		if len(v) > 40 {
			v = v[:40]
		}
		return v
	}
	polls, err := strconv.Atoi(r.Header.Get(tunnel.HeaderPolls))
	if err != nil || polls < 1 || polls > 32 {
		polls = core.DefaultHubPolls
	}
	return ConnMeta{
		Version:  clip(r.Header.Get(tunnel.HeaderVersion)),
		Build:    clip(r.Header.Get(tunnel.HeaderBuild)),
		Platform: clip(r.Header.Get(tunnel.HeaderPlatform)),
		Polls:    polls,
		Instance: instance(r),
	}
}

// instance is the gateway process that sent r ("-" when it does not say).
func instance(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get(tunnel.HeaderInstance))
	if len(v) > 64 {
		v = v[:64]
	}
	return cmp.Or(v, "-")
}

// admitted refuses a second running copy of a gateway (Monitor.Admit);
// release is nil when it does.
func (s *Server) admitted(w http.ResponseWriter, r *http.Request, g GatewayRecord) (release func()) {
	if release = s.monitor.Admit(g.Installation, g.gen, instance(r)); release == nil {
		w.Header().Set("Retry-After", "60")
		core.WriteError(w, r, http.StatusConflict, "DUPLICATE_GATEWAY", "another copy of this gateway, with the same secret, is connected; only one may run")
	}
	return release
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request, g GatewayRecord) {
	s.monitor.Release(g.Installation, g.gen, instance(r))
	w.WriteHeader(http.StatusNoContent)
}

// handlePoll flushes 200 at once, so the gateway knows it is connected; the
// body is a tunnel.Request when one arrives, empty when the wait ends.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request, g GatewayRecord) {
	release := s.admitted(w, r, g)
	if release == nil {
		return
	}
	defer release()
	m := meta(r)
	if s.cfg.LatestVersion != "" && m.Build == tunnel.BuildRelease && version.Newer(s.cfg.LatestVersion, m.Version) {
		w.Header().Set(tunnel.HeaderUpdateHint, s.cfg.LatestVersion)
	}
	s.signTime(w, r, g)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	call := s.broker.Poll(r.Context(), g.Installation, g.gen, m)
	if call == nil {
		return
	}
	// signed for this poll, by its nonce: the gateway runs it only as the
	// answer to this poll (tunnel.Verifier.Request)
	rq := *call
	rq.Sign(s.store.key, g.Installation, g.registration(), r.Header.Get(tunnel.HeaderNonce), s.now())
	if err := json.NewEncoder(w).Encode(rq); err != nil {
		s.log.Warn("poll answer not delivered", "installation", g.Installation, "request_id", rq.RequestID)
	}
}

// signTime sets the config version on an answer and, when the gateway
// sent a nonce, the hub's time signed with it (tunnel.SignTime).
func (s *Server) signTime(w http.ResponseWriter, r *http.Request, g GatewayRecord) {
	version := strconv.FormatInt(s.store.ConfigVersion(g.Installation), 10)
	w.Header().Set(tunnel.HeaderConfigVersion, version)
	w.Header().Set(tunnel.HeaderConfigEpoch, s.store.epoch)
	if nonce := r.Header.Get(tunnel.HeaderNonce); nonce != "" && len(nonce) <= 64 {
		w.Header().Set(tunnel.HeaderHubTime, tunnel.SignTime(s.store.key, g.Installation, g.registration(), nonce, version+" "+s.store.epoch, s.now()))
	}
}

func (s *Server) handleReply(w http.ResponseWriter, r *http.Request, g GatewayRecord) {
	var resp tunnel.Response
	body := http.MaxBytesReader(w, r.Body, tunnel.MaxResponseBody*4/3+64<<10)
	if err := json.NewDecoder(body).Decode(&resp); err != nil || resp.ID == "" || resp.Status < 100 || resp.Status > 599 {
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed reply")
		return
	}
	if len(resp.Body) > tunnel.MaxResponseBody {
		resp = tunnel.Response{ID: resp.ID, Status: http.StatusBadGateway,
			Header: map[string]string{"Content-Type": "application/json; charset=utf-8"},
			Body:   []byte(`{"code":"RESPONSE_TOO_LARGE","message":"the gateway response is too large"}`)}
	}
	if !s.broker.Reply(g.Installation, resp) {
		core.WriteError(w, r, http.StatusGone, "NO_WAITER", "nobody waits for this reply")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleHeartbeat records a gateway's report of its own self-check. The
// hub never asks a gateway for its readiness: the gateway reports it.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, g GatewayRecord) {
	var hb tunnel.Heartbeat
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, tunnel.MaxHeartbeatBody)).Decode(&hb); err != nil {
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed heartbeat")
		return
	}
	// a copy refused (not bound) is alive all the same: a restarted
	// gateway is not offline while the old copy's call runs out
	release := s.admitted(w, r, g)
	s.monitor.Heartbeat(g.Installation, hb, release != nil)
	if release != nil {
		release()
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleConfig gives a gateway its installation's club configuration.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request, g GatewayRecord) {
	out := tunnel.RemoteConfig{Epoch: s.store.epoch, Clubs: []json.RawMessage{}}
	if in, ok := s.store.Installation(g.Installation); ok {
		out.Version = in.Version
		for _, c := range in.ClubList() {
			raw, _ := json.Marshal(c)
			out.Clubs = append(out.Clubs, raw)
		}
	}
	out.Sign(s.store.key, g.Installation, g.registration(), s.now())
	core.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request, g GatewayRecord) {
	s.signTime(w, r, g)
	info := s.broker.Info(g.Installation)
	h := s.monitor.Health(g.Installation)
	out := tunnel.Whoami{Installation: g.Installation, Connected: info.Connected,
		Clubs: []string{}, AppliedConfigVersion: h.ConfigVersion, ConfigError: h.ConfigError, ClubsServed: h.Clubs}
	if out.ClubsServed == nil {
		out.ClubsServed = []string{}
	}
	if in, ok := s.store.Installation(g.Installation); ok {
		out.ConfigVersion = in.Version
		out.Clubs = core.SortedClubIDs(in.ClubList())
	}
	if !info.LastSeen.IsZero() {
		out.LastSeen = info.LastSeen.UTC().Format(time.RFC3339)
	}
	core.WriteJSON(w, http.StatusOK, out)
}

// ---- partner side ----

type clubListItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
}

func (s *Server) handlePartner(w http.ResponseWriter, r *http.Request) {
	partner, _ := auth.PartnerName(r.Context())
	core.SetPartner(r, partner)
	p := r.URL.Path
	if p == "/v1/clubs" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			core.WriteError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed for this endpoint")
			return
		}
		list := []clubListItem{}
		for _, c := range s.store.ClubRefs() {
			if s.cfg.partnerMayReach(partner, c.ID) {
				list = append(list, clubListItem{ID: c.ID, Name: c.Name, Online: s.broker.Connected(c.Installation)})
			}
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"clubs": list})
		return
	}
	rest, ok := strings.CutPrefix(p, "/v1/clubs/")
	club, _, _ := strings.Cut(rest, "/")
	// only canonical paths go on: an encoded slash or dot segment could
	// name another club than the one checked here (the gateway refuses
	// them too)
	canonical := r.URL.RawPath == "" && path.Clean(p) == p && !strings.Contains(p, "%")
	if !ok || !canonical || !core.ValidID(club) || !s.cfg.partnerMayReach(partner, club) {
		core.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such club")
		return
	}
	inst, routed := s.store.Route(club)
	if !routed {
		core.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such club")
		return
	}
	g, err := s.store.Gateway(inst)
	if err != nil || !s.broker.Connected(inst) {
		w.Header().Set("Retry-After", "60")
		core.WriteError(w, r, http.StatusServiceUnavailable, "CLUB_OFFLINE", "the club's gateway is not connected")
		return
	}
	// one partner may hold all polls of a gateway but one, so the other
	// partners of its clubs still get through
	polls := s.broker.Info(inst).Meta.Polls
	release, ok := s.inflight.acquire(partner+" "+inst, max(polls-1, 1))
	if !ok {
		w.Header().Set("Retry-After", "1")
		core.WriteError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests in flight for this API key at this club's gateway")
		return
	}
	defer release()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, tunnel.MaxRequestBody))
	if err != nil {
		core.WriteError(w, r, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body is too large")
		return
	}
	rq := tunnel.Request{
		ID: newCallID(), RequestID: core.RequestID(r), Method: r.Method, Path: p, Query: r.URL.RawQuery,
		Header: map[string]string{}, Body: body, Partner: partner, Scopes: s.cfg.partnerScopes(partner),
		TimeoutMS: int(s.cfg.RequestTimeout / time.Millisecond),
	}
	for _, h := range tunnel.RequestHeaders {
		if v := r.Header.Get(h); v != "" {
			rq.Header[h] = v
		}
	}
	resp, err := s.broker.Do(r.Context(), inst, g.gen, rq, 10*time.Second, s.cfg.RequestTimeout+5*time.Second)
	if writeBrokerError(w, r, err, "CLUB", "the club's gateway") {
		return
	}
	for _, h := range tunnel.ResponseHeaders {
		v := resp.Header[h]
		switch {
		case h == "Content-Type":
			continue // set below: every partner API answer is JSON
		case h == "Location" && !strings.HasPrefix(v, "/v1/clubs/"+club+"/"):
			continue // only the gateway's own relative booking path
		case h == "Location":
			v = s.cfg.publicPath + v // as partners see the hub (behind a prefix)
		}
		if v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// ---- operator ----

// GatewayStatus is one row of /hub/status: one installation's gateway.
type GatewayStatus struct {
	Installation string `json:"installation"`
	Name         string `json:"name"`
	Connected    bool   `json:"connected"`
	LastSeen     string `json:"last_seen,omitempty"`
	// State is pending, online, not_ready or offline (from heartbeats).
	State         string `json:"state"`
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
	OfflineSince  string `json:"offline_since,omitempty"`
	Version       string `json:"version,omitempty"`
	Build         string `json:"build,omitempty"`
	Platform      string `json:"platform,omitempty"`
	Outdated      bool   `json:"outdated"`
	Ready         *bool  `json:"ready"`
	ReadyReason   string `json:"ready_reason,omitempty"`
	// ReadyReasonClubs are the clubs ReadyReason is about, when it is
	// about clubs (e.g. TIMEZONE_MISMATCH).
	ReadyReasonClubs []string `json:"ready_reason_clubs,omitempty"`
	LastCheck        string   `json:"last_check,omitempty"`
	RegisteredAt     string   `json:"registered_at"`
	// The club config the gateway runs (from its heartbeats).
	AppliedConfigVersion int64             `json:"applied_config_version"`
	ConfigError          string            `json:"config_error,omitempty"`
	ClubErrors           map[string]string `json:"club_errors,omitempty"`
	ClubsServed          []string          `json:"clubs_served"`
	// Duplicate: a second copy of the gateway, with the same secret, was
	// refused within offline_after (last at DuplicateSeenAt).
	Duplicate       bool   `json:"duplicate"`
	DuplicateSeenAt string `json:"duplicate_seen_at,omitempty"`
	// RefusedRequests: requests the gateway refused because the hub had
	// not signed them for it, since it started (from its heartbeats).
	RefusedRequests int64 `json:"refused_requests"`
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// operator guards the operator API with the operator key (Bearer),
// compared in constant time.
func (s *Server) operator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _ := auth.Bearer(r)
		presented := sha256.Sum256([]byte(key))
		if !s.hasOp || subtle.ConstantTimeCompare(presented[:], s.opKey[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="operator"`)
			core.WriteError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "operator key required")
			return
		}
		next(w, r)
	}
}

func (s *Server) statusRow(g GatewayRecord) GatewayStatus {
	info := s.broker.Info(g.Installation)
	h := s.monitor.Health(g.Installation)
	row := GatewayStatus{
		Installation: g.Installation, Name: g.Name, Connected: info.Connected, LastSeen: ts(info.LastSeen),
		State: h.State, LastHeartbeat: ts(h.LastHeartbeat), OfflineSince: ts(h.OfflineSince),
		Version: cleanVersion(info.Meta.Version), Build: cleanBuild(info.Meta.Build), Platform: info.Meta.Platform,
		RegisteredAt: ts(g.RegisteredAt), LastCheck: ts(h.CheckedAt), ReadyReason: h.Reason, ReadyReasonClubs: h.ReasonClubs,
		AppliedConfigVersion: h.ConfigVersion, ConfigError: h.ConfigError, ClubErrors: h.ClubErrors, ClubsServed: h.Clubs,
		Duplicate: h.Duplicate, DuplicateSeenAt: ts(h.DuplicateAt), RefusedRequests: h.RefusedRequests,
	}
	if row.ClubsServed == nil {
		row.ClubsServed = []string{}
	}
	if h.Version != "" {
		row.Version, row.Build = h.Version, h.Build
	}
	if h.Checked {
		ready := h.Ready
		row.Ready = &ready
	}
	if s.cfg.LatestVersion != "" && row.Version != "" {
		row.Outdated = version.Compare(row.Version, s.cfg.LatestVersion) < 0
	}
	return row
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	rows := []GatewayStatus{}
	for _, g := range s.store.Gateways() {
		rows = append(rows, s.statusRow(g))
	}
	core.WriteJSON(w, http.StatusOK, map[string]any{"time": ts(s.now()), "latest_version": s.cfg.LatestVersion, "gateways": rows})
}

// failLimiter refuses a client after too many failures in a window: join
// codes are slow to guess, and no client locks out the others.
type failLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	clients map[netip.Addr]*failWindow
}

type failWindow struct {
	start time.Time
	count int
}

func newFailLimiter(max int, window time.Duration) *failLimiter {
	return &failLimiter{max: max, window: window, clients: map[netip.Addr]*failWindow{}}
}

func (l *failLimiter) allow(client netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.clients[client]
	return w == nil || now.Sub(w.start) >= l.window || w.count < l.max
}

func (l *failLimiter) fail(client netip.Addr, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.clients) > 4096 {
		for c, w := range l.clients {
			if now.Sub(w.start) >= l.window {
				delete(l.clients, c)
			}
		}
	}
	w := l.clients[client]
	if w == nil || now.Sub(w.start) >= l.window {
		w = &failWindow{start: now}
		l.clients[client] = w
	}
	w.count++
}

// clientIP is the peer, or behind trusted proxies the last
// X-Forwarded-For address that is not one.
func clientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	addr = addr.Unmap()
	if !isTrusted(addr) {
		return addr
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return addr
		}
		if addr = a.Unmap(); !isTrusted(addr) {
			return addr
		}
	}
	return addr
}

// DiscoverTimeout is how long the gateway may read its CRM for a
// discovery (tunnel.MaxRequestTimeout allows it).
const DiscoverTimeout = 3 * time.Minute

// writeTimeout covers the longest answer: a partner request after a full
// poll wait, or a discovery, plus the margin of the broker's wait.
func writeTimeout(cfg *Settings) time.Duration {
	return max(cfg.PollWait+cfg.RequestTimeout, DiscoverTimeout) + 30*time.Second
}

// Run serves the hub until ctx is done: the HTTP(S) listener and the
// heartbeat monitor (which writes the status files).
func Run(ctx context.Context, cfg *Settings, srv *Server, log *slog.Logger) error {
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      writeTimeout(cfg),
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	mctx, stop := context.WithCancel(ctx)
	defer stop()
	go srv.Monitor().Run(mctx)
	// every request runs under base: a stopping hub ends the long polls
	// and the waits at once instead of waiting for them
	base, endRequests := context.WithCancel(context.Background())
	defer endRequests()
	httpSrv.BaseContext = func(net.Listener) context.Context { return base }
	errc := make(chan error, 1)
	go func() {
		if cfg.TLSCertFile != "" {
			errc <- httpSrv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errc <- httpSrv.ListenAndServe()
		}
	}()
	if cfg.TLSCertFile == "" {
		log.Warn("hub serves plain HTTP: put a TLS-terminating proxy in front of it; gateways use plain http only to a loopback address")
	}
	log.Info("hub started", "listen", cfg.Listen, "gateways", len(srv.store.Gateways()))
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		endRequests()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := httpSrv.Shutdown(shCtx)
		if err == nil {
			log.Info("hub stopped")
		}
		return err
	}
}

// writeBrokerError answers a failed Broker.Do with the codes of who
// (CLUB_OFFLINE, GATEWAY_BUSY, …) and reports whether err was one;
// nothing is written when the caller went away.
func writeBrokerError(w http.ResponseWriter, r *http.Request, err error, who, gateway string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrOffline):
		w.Header().Set("Retry-After", "60")
		core.WriteError(w, r, http.StatusServiceUnavailable, who+"_OFFLINE", gateway+" is not connected")
	case errors.Is(err, ErrBusy):
		w.Header().Set("Retry-After", "5")
		core.WriteError(w, r, http.StatusServiceUnavailable, who+"_BUSY", gateway+" is busy with other requests, retry")
	case errors.Is(err, ErrTimeout):
		core.WriteError(w, r, http.StatusGatewayTimeout, who+"_TIMEOUT", gateway+" did not answer in time")
	case errors.Is(err, ErrReplaced):
		core.WriteError(w, r, http.StatusBadGateway, "OUTCOME_UNKNOWN", "outcome unknown after gateway replacement")
	}
	return true
}
