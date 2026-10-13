package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// ReadyTimeout bounds the readiness probe.
const ReadyTimeout = 5 * time.Second

// driverTimeout bounds every driver call made for an API request.
const driverTimeout = 30 * time.Second

// Server is the CRM-agnostic HTTP API. It is a plain http.Handler factory:
// it makes no assumption about how or where it is served.
type Server struct {
	cfg  *Settings
	drv  driver.Driver
	log  *slog.Logger
	now  func() time.Time
	auth *auth.Authenticator
	caps driver.Capabilities
}

// Option customises a Server.
type Option func(*Server)

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// NewServer builds the API around a driver.
func NewServer(cfg *Settings, drv driver.Driver, log *slog.Logger, opts ...Option) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{cfg: cfg, drv: drv, log: log, now: time.Now, caps: drv.Capabilities()}
	for _, o := range opts {
		o(s)
	}
	a, err := auth.New(cfg.Partners, auth.Options{WriteError: writeError, Now: s.now})
	if err != nil {
		return nil, err
	}
	s.auth = a
	return s, nil
}

// Handler returns the complete handler: request ids, access log, panic
// recovery, unauthenticated health endpoints and the partner API behind
// the auth middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.handleLive)
	mux.HandleFunc("GET /health/ready", s.handleReady)
	mux.Handle("/health/", methodNotAllowed())
	mux.Handle("/v1/", s.auth.Wrap(s.APIHandler()))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	})
	return s.observe(mux)
}

// APIHandler returns the partner API without authentication. It expects the
// partner name in the request context (see auth.PartnerName) and is meant to
// be wrapped by the auth middleware.
func (s *Server) APIHandler() http.Handler {
	mux := http.NewServeMux()
	const club = "/v1/clubs/{club}"
	mux.HandleFunc("GET /v1/clubs", s.handleClubs)
	mux.HandleFunc("GET "+club, s.withClub(s.handleClub))
	mux.HandleFunc("GET "+club+"/courts", s.withClub(s.handleCourts))
	mux.HandleFunc("GET "+club+"/occupancy", s.withClub(s.handleOccupancy))
	mux.HandleFunc("GET "+club+"/prices", s.withClub(s.handlePrices))
	mux.HandleFunc("GET "+club+"/quote", s.withClub(s.handleQuote))
	mux.HandleFunc("POST "+club+"/bookings", s.withClub(s.handleCreate))
	mux.HandleFunc("GET "+club+"/bookings/{id}", s.withClub(s.handleGet))
	mux.HandleFunc("DELETE "+club+"/bookings/{id}", s.withClub(s.handleCancel))
	mux.HandleFunc("POST "+club+"/bookings/{id}/payment", s.withClub(s.handlePayment))
	for _, p := range []string{"/v1/clubs", club, club + "/courts", club + "/occupancy", club + "/prices",
		club + "/quote", club + "/bookings", club + "/bookings/{id}", club + "/bookings/{id}/payment"} {
		mux.Handle(p, methodNotAllowed())
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	})
	return s.notePartner(mux)
}

func methodNotAllowed() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed for this endpoint")
	})
}

func (s *Server) withClub(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("club") != s.cfg.ClubID {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "no such club")
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

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		ri := &reqInfo{id: newRequestID()}
		w.Header().Set("X-Request-ID", ri.id)
		rec := &statusRecorder{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri))
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic in handler", "request_id", ri.id, "path", r.URL.Path)
				if rec.status == 0 {
					writeError(rec, r, http.StatusInternalServerError, "INTERNAL", "internal error")
				}
			}
			s.log.Info("request",
				"request_id", ri.id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"partner", ri.partner,
				"duration_ms", s.now().Sub(start).Milliseconds(),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

func (s *Server) notePartner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := auth.PartnerName(r.Context()); ok {
			info(r).partner = name
		}
		next.ServeHTTP(w, r)
	})
}

// ---- responses ----

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, errorBody{Code: code, Message: message, RequestID: info(r).id})
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
	writeError(w, r, status, code, msg)
}

func classify(err error) (int, string, string) {
	var ue *driver.UpstreamError
	switch {
	case errors.Is(err, driver.ErrNotFound):
		return http.StatusNotFound, "NOT_FOUND", "not found"
	case errors.Is(err, driver.ErrNotOwned):
		return http.StatusForbidden, "NOT_OWNED", "the booking was not created through this gateway by this partner"
	case errors.Is(err, driver.ErrSlotUnavailable):
		return http.StatusConflict, "SLOT_UNAVAILABLE", "the court is occupied in this interval"
	case errors.Is(err, driver.ErrNotSupported):
		return http.StatusNotImplemented, "NOT_SUPPORTED", "the club's CRM does not support this operation"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", "the club's CRM did not answer in time"
	case errors.As(err, &ue):
		switch ue.Kind {
		case driver.UpstreamInvalid:
			return http.StatusBadGateway, "UPSTREAM_INVALID", "the club's CRM returned a response the gateway cannot interpret"
		case driver.UpstreamAuth:
			return http.StatusBadGateway, "UPSTREAM_AUTH", "the gateway cannot log in to the club's CRM"
		case driver.UpstreamRejected:
			return http.StatusBadGateway, "UPSTREAM_REJECTED", "the club's CRM rejected the request"
		default:
			return http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "the club's CRM is unavailable"
		}
	default:
		return http.StatusInternalServerError, "INTERNAL", "internal error"
	}
}

// readyReason maps a readiness failure to a short code with no secrets.
func readyReason(err error) string {
	var ue *driver.UpstreamError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "CRM_TIMEOUT"
	case errors.As(err, &ue):
		switch ue.Kind {
		case driver.UpstreamAuth:
			return "CRM_AUTH_FAILED"
		case driver.UpstreamInvalid:
			return "CRM_INVALID_RESPONSE"
		case driver.UpstreamRejected:
			return "CRM_REJECTED"
		default:
			return "CRM_UNREACHABLE"
		}
	default:
		return "CRM_ERROR"
	}
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), ReadyTimeout)
	defer cancel()
	if err := s.drv.Ready(ctx); err != nil {
		reason := readyReason(err)
		s.log.Warn("not ready", "request_id", info(r).id, "reason", reason, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
