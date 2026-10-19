package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// ReadyTimeout bounds the readiness probe.
const ReadyTimeout = 5 * time.Second

// driverTimeout bounds every driver call made for an API request.
const driverTimeout = 30 * time.Second

// Server is the partner API of one club around its driver. Clubs serves
// every club of an installation and adds health, auth and the club list.
type Server struct {
	cfg *Settings
	drv driver.Driver
	log *slog.Logger
	// clock is the hub's time (driver.Config.Clock): "now" of every
	// booking decision
	clock driver.Clock
	caps  driver.Capabilities
	// mismatch: the last readiness check found the club's config does not
	// match its CRM; partners get 503 until a check passes again.
	mismatch atomic.Bool
}

// noteCheck records a readiness result: a mismatch blocks the club, a
// pass (or a mere alert) unblocks it, a check that could not be made
// changes nothing.
func (s *Server) noteCheck(err error) {
	var me *driver.MisconfigError
	var ca *driver.ConfigAlert
	switch {
	case err == nil, errors.As(err, &ca):
		s.mismatch.Store(false)
	case errors.As(err, &me):
		s.mismatch.Store(true)
	}
}

// NewServer builds the API of one club around its driver.
func NewServer(cfg *Settings, drv driver.Driver, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{cfg: cfg, drv: drv, log: log, clock: cfg.Driver.DecisionClock(), caps: drv.Capabilities()}
}

// Narrowed is the server of cfg and drv, a narrower config of s's club: it
// stays blocked if s is, until its own readiness check passes.
func (s *Server) Narrowed(cfg *Settings, drv driver.Driver) *Server {
	n := NewServer(cfg, drv, cfg.Driver.Logger)
	n.mismatch.Store(s.mismatch.Load())
	return n
}

func (s *Server) Settings() *Settings { return s.cfg }

func (s *Server) Driver() driver.Driver { return s.drv }

// APIHandler returns the club's partner API without authentication. It
// expects the partner name in the request context (see auth.PartnerName);
// Clubs routes /v1/clubs/{club}/... here.
func (s *Server) APIHandler() http.Handler {
	mux := http.NewServeMux()
	const club = "/v1/clubs/{club}"
	routes := []struct {
		method, path string
		h            http.HandlerFunc
	}{
		{"GET", club, s.handleClub},
		{"GET", club + "/courts", s.handleCourts},
		{"GET", club + "/occupancy", s.handleOccupancy},
		{"GET", club + "/prices", s.handlePrices},
		{"GET", club + "/quote", s.handleQuote},
		{"POST", club + "/bookings", s.handleCreate},
		{"GET", club + "/bookings/{id}", s.handleGet},
		{"DELETE", club + "/bookings/{id}", s.handleCancel},
		{"POST", club + "/bookings/{id}/payment", s.handlePayment},
	}
	paths := map[string]bool{}
	for _, r := range routes {
		mux.HandleFunc(r.method+" "+r.path, s.withClub(r.h))
		if !paths[r.path] {
			paths[r.path] = true
			mux.Handle(r.path, methodNotAllowed()) // any other method
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	})
	return mux
}

func methodNotAllowed() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed for this endpoint")
	})
}

func (s *Server) withClub(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("club") != s.cfg.ClubID {
			WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such club")
			return
		}
		h(w, r)
	}
}

// ---- request context, ids, access log ----

type reqInfo struct {
	id      string
	partner string
}

type reqInfoKey struct{}

func info(r *http.Request) *reqInfo {
	if v, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
		return v
	}
	return &reqInfo{}
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "rq-unknown"
	}
	return "rq-" + hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (Flush).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

type upstreamIDKey struct{}

// WithRequestID returns a context carrying a request id assigned upstream
// (by the hub). Observe uses it instead of a new id, so the partner, the
// hub and the gateway log the same id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, upstreamIDKey{}, id)
}

// RequestID returns the id Observe assigned to r ("" outside Observe).
func RequestID(r *http.Request) string { return info(r).id }

// SetPartner records the partner name for the access log line.
func SetPartner(r *http.Request, name string) { info(r).partner = name }

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Observe assigns request ids, writes the access log line and recovers
// panics. It is shared by the gateway and the hub so both keep one log and
// error format.
func Observe(log *slog.Logger, now func() time.Time, next http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := now()
		ri := &reqInfo{id: newRequestID()}
		if up, ok := r.Context().Value(upstreamIDKey{}).(string); ok && requestIDRe.MatchString(up) {
			ri.id = up
		}
		w.Header().Set("X-Request-ID", ri.id)
		rec := &statusRecorder{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri))
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic in handler", "request_id", ri.id, "path", r.URL.Path)
				if rec.status == 0 {
					WriteError(rec, r, http.StatusInternalServerError, "INTERNAL", "internal error")
				}
			}
			log.Info("request",
				"request_id", ri.id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"partner", ri.partner,
				"duration_ms", now().Sub(start).Milliseconds(),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

// ---- responses ----

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// WriteJSON writes v as a JSON response that must not be cached.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the API error body {code, message, request_id}.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteJSON(w, status, errorBody{Code: code, Message: message, RequestID: info(r).id})
}

// driverError maps a driver error to an API error. Upstream details are
// logged (they never contain secrets or personal data by the driver
// contract) but not returned.
func (s *Server) driverError(w http.ResponseWriter, r *http.Request, op string, err error) {
	status, code, msg := classify(err)
	lvl := slog.LevelInfo
	if status >= 500 {
		lvl = slog.LevelWarn
	}
	s.log.Log(r.Context(), lvl, "driver call failed", "request_id", info(r).id, "op", op, "code", code, "error", err)
	WriteError(w, r, status, code, msg)
}

// crmFailure is what a CRM failure is to a partner (status, code,
// message) and to readiness (ready), by its kind.
type crmFailure struct {
	status           int
	code, msg, ready string
}

var crmFailures = map[driver.UpstreamKind]crmFailure{
	driver.UpstreamUnavailable: {http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the club's CRM is unavailable", "CRM_UNREACHABLE"},
	driver.UpstreamAuth:        {http.StatusBadGateway, "UPSTREAM_AUTH", "the gateway cannot log in to the club's CRM", "CRM_AUTH_FAILED"},
	driver.UpstreamLogin:       {http.StatusBadGateway, "UPSTREAM_AUTH", "the gateway cannot log in to the club's CRM", "CRM_AUTH_ERROR"},
	driver.UpstreamInvalid:     {http.StatusBadGateway, "UPSTREAM_INVALID", "the club's CRM returned a response the gateway cannot interpret", "CRM_INVALID_RESPONSE"},
	driver.UpstreamRejected:    {http.StatusBadGateway, "UPSTREAM_REJECTED", "the club's CRM rejected the request", "CRM_REJECTED"},
}

func failureOf(ue *driver.UpstreamError) crmFailure {
	if f, ok := crmFailures[ue.Kind]; ok {
		return f
	}
	return crmFailures[driver.UpstreamUnavailable]
}

func classify(err error) (int, string, string) {
	var ue *driver.UpstreamError
	switch {
	case errors.Is(err, driver.ErrNotFound):
		return http.StatusNotFound, "NOT_FOUND", "not found"
	case errors.Is(err, driver.ErrNotOwned):
		return http.StatusForbidden, "NOT_OWNED", "the booking was not created through this gateway by this partner"
	case errors.Is(err, driver.ErrCourtNotConfigured):
		return http.StatusConflict, "COURT_NOT_CONFIGURED", "the booking is on a court that is not configured for this club"
	case errors.Is(err, driver.ErrClockUnknown):
		return http.StatusServiceUnavailable, "CLOCK_UNKNOWN", "the club's gateway does not know the hub's time yet, retry"
	case errors.Is(err, driver.ErrStarted):
		return http.StatusConflict, "BOOKING_STARTED", "the booking has already started and cannot be cancelled"
	case errors.Is(err, driver.ErrSlotUnavailable):
		return http.StatusConflict, "SLOT_UNAVAILABLE", "the court is occupied in this interval"
	case errors.Is(err, driver.ErrBookingNotConfigured):
		return http.StatusServiceUnavailable, "BOOKING_NOT_CONFIGURED", "the club has not finished setting up bookings through the gateway"
	case errors.Is(err, driver.ErrNotSupported):
		return http.StatusNotImplemented, "NOT_SUPPORTED", "the club's CRM does not support this operation"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", "the club's CRM did not answer in time"
	case errors.As(err, &ue):
		f := failureOf(ue)
		return f.status, f.code, f.msg
	default:
		return http.StatusInternalServerError, "INTERNAL", "internal error"
	}
}

// ReadyError is a readiness failure of the clubs named: the first club
// (in id order) whose check failed, or, when the only findings are
// config alerts, every club with one.
type ReadyError struct {
	Clubs []string
	Err   error
	// alerts are the config alerts of every club in Clubs (Err is the
	// first's), for ReadyDetail
	alerts []error
}

func (e *ReadyError) Error() string {
	return "клуб " + strings.Join(e.Clubs, ", ") + ": " + e.Err.Error()
}

func (e *ReadyError) Unwrap() error { return e.Err }

// Bounds of ReadyDetail: each text, and all of them (in runes).
const (
	readyDetailText = 500
	readyDetailMax  = 2000
)

// ReadyDetail is what the operator is to do about a readiness failure, in
// Russian, for the heartbeat, which carries it to the hub: the Detail
// texts of its config alerts (driver.ConfigAlert — the reconciliation's
// alarms), each club's prefixed
// with the club when several have one; control characters dropped, each
// text and the whole bounded. Nothing else of an error is ever taken: an
// upstream error's words or a CRM's answer may hold anything.
func ReadyDetail(err error) string {
	detail := func(e error) []string {
		var ca *driver.ConfigAlert
		if !errors.As(e, &ca) {
			return nil
		}
		var out []string
		for _, d := range ca.Detail {
			if d = cleanDetail(d, readyDetailText); d != "" {
				out = append(out, d)
			}
		}
		return out
	}
	var texts []string
	var re *ReadyError
	if errors.As(err, &re) && len(re.alerts) > 1 && len(re.alerts) == len(re.Clubs) {
		for i, e := range re.alerts {
			for _, d := range detail(e) {
				texts = append(texts, "клуб "+re.Clubs[i]+": "+d)
			}
		}
	} else {
		texts = detail(err)
	}
	return cleanDetail(strings.Join(texts, "; "), readyDetailMax)
}

// cleanDetail drops control characters and keeps at most max runes.
func cleanDetail(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// ReadyClubs returns the clubs a readiness failure is about (none for the
// account's own login check).
func ReadyClubs(err error) []string {
	var re *ReadyError
	if errors.As(err, &re) {
		return re.Clubs
	}
	return nil
}

// ReadyReason maps a readiness failure to a short code with no secrets.
func ReadyReason(err error) string {
	var ue *driver.UpstreamError
	var me *driver.MisconfigError
	var ca *driver.ConfigAlert
	var mi *driver.MirrorError
	switch {
	case errors.As(err, &mi):
		// the mirror CRM of a pair: its own reason, named as the mirror's
		return "MIRROR_" + ReadyReason(mi.Err)
	case errors.As(err, &ca):
		return ca.Code
	case errors.As(err, &me):
		return "CONFIG_MISMATCH"
	case errors.Is(err, context.DeadlineExceeded):
		return "CRM_TIMEOUT"
	case errors.As(err, &ue):
		return failureOf(ue).ready
	default:
		return "CRM_ERROR"
	}
}
