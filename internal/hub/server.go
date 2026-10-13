package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
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
	cfg     *Settings
	store   *Store
	broker  *Broker
	monitor *Monitor
	auth    *auth.Authenticator
	log     *slog.Logger
	now     func() time.Time
	opKey   [sha256.Size]byte
	hasOp   bool
	regLim  *failLimiter
}

// Options for NewServer.
type Options struct {
	Now    func() time.Time
	Logger *slog.Logger
	// Sinks override the sinks built from the settings (tests).
	Sinks []Sink
	// SinkClient is the HTTP client for alert sinks.
	SinkClient *http.Client
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
	sinks := opts.Sinks
	if sinks == nil {
		sinks = SinksFromSettings(cfg, opts.SinkClient)
	}
	s.monitor = NewMonitor(store, s.broker, sinks, MonitorConfig{
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

// Store returns the hub's state.
func (s *Server) Store() *Store { return s.store }

// Broker returns the broker.
func (s *Server) Broker() *Broker { return s.broker }

// Handler returns the hub's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		core.WriteJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) {
		core.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST "+tunnel.PathRegister, s.handleRegister)
	mux.HandleFunc("POST "+tunnel.PathPoll, s.gateway(s.handlePoll))
	mux.HandleFunc("POST "+tunnel.PathReply, s.gateway(s.handleReply))
	mux.HandleFunc("GET "+tunnel.PathWhoami, s.gateway(s.handleWhoami))
	mux.HandleFunc("POST "+tunnel.PathHeartbeat, s.gateway(s.handleHeartbeat))
	mux.HandleFunc("GET "+tunnel.PathConfig, s.gateway(s.handleConfig))
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
	if !s.regLim.allow(s.now()) {
		core.WriteError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "too many failed registrations, try again later")
		return
	}
	var req tunnel.RegisterRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed registration")
		return
	}
	g, secret, err := s.store.Redeem(req.Code)
	if err != nil {
		s.regLim.fail(s.now())
		if errors.Is(err, ErrJoinCode) {
			core.WriteError(w, r, http.StatusForbidden, "JOIN_CODE_REJECTED", "the join code is unknown, already used or expired")
			return
		}
		s.log.Error("registration failed", "error", err)
		core.WriteError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	s.log.Info("gateway registered", "installation", g.Installation, "version", req.Version)
	core.WriteJSON(w, http.StatusOK, tunnel.RegisterResponse{Installation: g.Installation, Name: g.Name, Secret: secret})
}

func (s *Server) gateway(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _ := auth.Bearer(r)
		inst, ok := s.store.Authenticate(key)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tunnel"`)
			core.WriteError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "unknown gateway secret")
			return
		}
		next(w, r, inst)
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
	return ConnMeta{
		Version:  clip(r.Header.Get(tunnel.HeaderVersion)),
		Build:    clip(r.Header.Get(tunnel.HeaderBuild)),
		Platform: clip(r.Header.Get(tunnel.HeaderPlatform)),
	}
}

// handlePoll answers 200 at once and flushes the headers, so the gateway
// knows it is connected without waiting; the body follows when a request
// arrives (a JSON tunnel.Request) or the wait ends (an empty body).
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request, club string) {
	m := meta(r)
	if s.cfg.LatestVersion != "" && m.Build == tunnel.BuildRelease && version.Newer(s.cfg.LatestVersion, m.Version) {
		w.Header().Set(tunnel.HeaderUpdateHint, s.cfg.LatestVersion)
	}
	w.Header().Set(tunnel.HeaderConfigVersion, strconv.FormatInt(s.store.ConfigVersion(club), 10))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	rq := s.broker.Poll(r.Context(), club, m)
	if rq == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(rq); err != nil {
		s.log.Warn("poll answer not delivered", "club", club, "request_id", rq.RequestID)
	}
}

func (s *Server) handleReply(w http.ResponseWriter, r *http.Request, club string) {
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
	if !s.broker.Reply(club, resp) {
		core.WriteError(w, r, http.StatusGone, "NO_WAITER", "nobody waits for this reply")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleHeartbeat records a gateway's report of its own self-check. The
// hub never asks a gateway for its readiness: the gateway reports it.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, club string) {
	var hb tunnel.Heartbeat
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, tunnel.MaxHeartbeatBody)).Decode(&hb); err != nil {
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed heartbeat")
		return
	}
	s.monitor.Heartbeat(club, hb)
	w.Header().Set(tunnel.HeaderConfigVersion, strconv.FormatInt(s.store.ConfigVersion(club), 10))
	w.WriteHeader(http.StatusNoContent)
}

// handleConfig gives a gateway its installation's club configuration.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request, inst string) {
	out := tunnel.RemoteConfig{Clubs: []json.RawMessage{}}
	if in, ok := s.store.Installation(inst); ok {
		out.Version = in.Version
		for _, c := range in.ClubList() {
			raw, _ := json.Marshal(c)
			out.Clubs = append(out.Clubs, raw)
		}
	}
	core.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request, inst string) {
	info := s.broker.Info(inst)
	h := s.monitor.Health(inst)
	out := tunnel.Whoami{Installation: inst, Connected: info.Connected, Latest: s.cfg.LatestVersion,
		Clubs: []string{}, AppliedConfigVersion: h.ConfigVersion, ConfigError: h.ConfigError, ClubsServed: h.ClubsServed}
	if out.ClubsServed == nil {
		out.ClubsServed = []string{}
	}
	if in, ok := s.store.Installation(inst); ok {
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
	if !ok || !ValidClubID(club) || !s.cfg.partnerMayReach(partner, club) {
		core.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such club")
		return
	}
	inst, routed := s.store.Route(club)
	if !routed {
		core.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such club")
		return
	}
	if _, registered := s.store.Get(inst); !registered || !s.broker.Connected(inst) {
		w.Header().Set("Retry-After", "60")
		core.WriteError(w, r, http.StatusServiceUnavailable, "CLUB_OFFLINE", "the club's gateway is not connected")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, tunnel.MaxRequestBody))
	if err != nil {
		core.WriteError(w, r, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body is too large")
		return
	}
	rq := tunnel.Request{
		ID: newCallID(), RequestID: core.RequestID(r), Method: r.Method, Path: p, Query: r.URL.RawQuery,
		Header: map[string]string{}, Body: body, Partner: partner,
		TimeoutMS: int(s.cfg.RequestTimeout / time.Millisecond),
	}
	for _, h := range tunnel.ForwardedHeaders() {
		if v := r.Header.Get(h); v != "" {
			rq.Header[h] = v
		}
	}
	resp, err := s.broker.Do(r.Context(), inst, rq, 10*time.Second, s.cfg.RequestTimeout+5*time.Second)
	switch {
	case errors.Is(err, ErrOffline):
		w.Header().Set("Retry-After", "60")
		core.WriteError(w, r, http.StatusServiceUnavailable, "CLUB_OFFLINE", "the club's gateway is not connected")
		return
	case errors.Is(err, ErrTimeout):
		core.WriteError(w, r, http.StatusGatewayTimeout, "CLUB_TIMEOUT", "the club's gateway did not answer in time")
		return
	case err != nil:
		return // the partner went away
	}
	for _, h := range append(tunnel.ForwardedHeaders(), "Cache-Control") {
		if v := resp.Header[h]; v != "" {
			w.Header().Set(h, v)
		}
	}
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
	LastCheck     string `json:"last_check,omitempty"`
	RegisteredAt  string `json:"registered_at"`
	// The club config the gateway runs (from its heartbeats).
	AppliedConfigVersion int64    `json:"applied_config_version"`
	ConfigError          string   `json:"config_error,omitempty"`
	ClubsServed          []string `json:"clubs_served"`
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
		RegisteredAt: ts(g.RegisteredAt), LastCheck: ts(h.CheckedAt), ReadyReason: h.Reason,
		AppliedConfigVersion: h.ConfigVersion, ConfigError: h.ConfigError, ClubsServed: h.ClubsServed,
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

// failLimiter refuses work after too many failures in a window; it slows
// down guessing join codes without keeping per-client state.
type failLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	start  time.Time
	count  int
}

func newFailLimiter(max int, window time.Duration) *failLimiter {
	return &failLimiter{max: max, window: window}
}

func (l *failLimiter) roll(now time.Time) {
	if now.Sub(l.start) >= l.window {
		l.start, l.count = now, 0
	}
}

func (l *failLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	return l.count < l.max
}

func (l *failLimiter) fail(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	l.count++
}

// Run serves the hub until ctx is done: the HTTP(S) listener and the
// heartbeat monitor (which writes the status files).
func Run(ctx context.Context, cfg *Settings, srv *Server, log *slog.Logger) error {
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      cfg.PollWait + cfg.RequestTimeout + 30*time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	mctx, stop := context.WithCancel(ctx)
	defer stop()
	go srv.Monitor().Run(mctx)
	errc := make(chan error, 1)
	go func() {
		if cfg.TLSCertFile != "" {
			errc <- httpSrv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errc <- httpSrv.ListenAndServe()
		}
	}()
	if cfg.TLSCertFile == "" {
		log.Warn("hub serves plain HTTP: put a TLS-terminating proxy in front of it; gateways only connect over https")
	}
	log.Info("hub started", "listen", cfg.Listen, "gateways", len(srv.store.Gateways()))
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shCtx)
	}
}
