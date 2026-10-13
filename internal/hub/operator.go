package hub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// The operator API: the operator's own system (an admin panel) manages
// installations, their join codes and their clubs. Every endpoint needs
// the operator key (see Server.operator).

// InstallationDTO is an installation as the operator API returns it.
type InstallationDTO struct {
	ID            string          `json:"installation_id"`
	Name          string          `json:"name"`
	CreatedAt     string          `json:"created_at"`
	ConfigVersion int64           `json:"config_version"`
	Clubs         []core.ClubSpec `json:"clubs"`
	// Gateway is the registered gateway's status; null before the first
	// join code is redeemed.
	Gateway *GatewayStatus `json:"gateway"`
}

// JoinCodeDTO is a freshly created join code, shown once.
type JoinCodeDTO struct {
	Code         string `json:"code"`
	Installation string `json:"installation_id"`
	ExpiresAt    string `json:"expires_at"`
}

// ClubPutDTO answers a club change.
type ClubPutDTO struct {
	Installation  string `json:"installation_id"`
	Club          string `json:"club_id,omitempty"`
	ConfigVersion int64  `json:"config_version"`
}

type problemsBody struct {
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	Problems  []string `json:"problems"`
	RequestID string   `json:"request_id"`
}

const maxOperatorBody = 1 << 20

func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return true // an empty body is an empty object
		}
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON body: "+jsonProblem(err))
		return false
	}
	return true
}

// jsonProblem keeps a decoder error short and free of the body's content.
func jsonProblem(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "json: unknown field "); i >= 0 {
		return msg[i+len("json: "):]
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return "wrong type for " + te.Field
	}
	return "not valid JSON"
}

func (s *Server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrInvalidID):
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_ID", err.Error())
	case errors.Is(err, ErrInvalidName):
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_NAME", err.Error())
	case errors.Is(err, ErrInvalidTTL):
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_TTL", err.Error())
	case errors.Is(err, ErrUnknownInstallation):
		core.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such installation")
	case errors.Is(err, ErrUnknownClub):
		core.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "no such club in this installation")
	case errors.Is(err, ErrInstallationExists):
		core.WriteError(w, r, http.StatusConflict, "INSTALLATION_EXISTS", err.Error())
	case errors.Is(err, ErrClubTaken):
		core.WriteError(w, r, http.StatusConflict, "CLUB_TAKEN", err.Error())
	default:
		s.log.Error("operator request failed", "error", err)
		core.WriteError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}

func (s *Server) installationDTO(in Installation) InstallationDTO {
	out := InstallationDTO{ID: in.ID, Name: in.Name, CreatedAt: ts(in.Created), ConfigVersion: in.Version, Clubs: in.ClubList()}
	if g, ok := s.store.Get(in.ID); ok {
		row := s.statusRow(g)
		out.Gateway = &row
	}
	return out
}

// POST /hub/installations {"installation_id", "name"}
func (s *Server) handleCreateInstallation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"installation_id"`
		Name string `json:"name"`
	}
	if !decodeStrict(w, r, 4096, &req) {
		return
	}
	in, err := s.store.CreateInstallation(strings.TrimSpace(req.ID), req.Name)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.log.Info("installation created", "installation", in.ID)
	core.WriteJSON(w, http.StatusCreated, s.installationDTO(in))
}

// GET /hub/installations/{inst}
func (s *Server) handleGetInstallation(w http.ResponseWriter, r *http.Request) {
	in, ok := s.store.Installation(r.PathValue("inst"))
	if !ok {
		s.storeError(w, r, ErrUnknownInstallation)
		return
	}
	core.WriteJSON(w, http.StatusOK, s.installationDTO(in))
}

// DELETE /hub/installations/{inst}
func (s *Server) handleDeleteInstallation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("inst")
	if err := s.store.DeleteInstallation(id); err != nil {
		s.storeError(w, r, err)
		return
	}
	s.log.Info("installation deleted", "installation", id)
	w.WriteHeader(http.StatusNoContent)
}

// POST /hub/installations/{inst}/join-codes {"ttl"?}
func (s *Server) handleCreateJoinCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TTL string `json:"ttl"`
	}
	if !decodeStrict(w, r, 4096, &req) {
		return
	}
	var ttl time.Duration
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil || d <= 0 {
			s.storeError(w, r, ErrInvalidTTL)
			return
		}
		ttl = d
	}
	jc, err := s.store.CreateJoinCode(r.PathValue("inst"), ttl)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.log.Info("join code created", "installation", jc.Installation, "expires", ts(jc.Expires))
	core.WriteJSON(w, http.StatusCreated, JoinCodeDTO{Code: jc.Code, Installation: jc.Installation, ExpiresAt: ts(jc.Expires)})
}

// PUT /hub/installations/{inst}/clubs/{club} with a core.ClubSpec body.
func (s *Server) handlePutClub(w http.ResponseWriter, r *http.Request) {
	inst, club := r.PathValue("inst"), r.PathValue("club")
	if _, ok := s.store.Installation(inst); !ok {
		s.storeError(w, r, ErrUnknownInstallation)
		return
	}
	var spec core.ClubSpec
	if !decodeStrict(w, r, maxOperatorBody, &spec) {
		return
	}
	if spec.ID != "" && spec.ID != club {
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "the id in the body differs from the club id in the path")
		return
	}
	spec.ID = club
	if !ValidClubID(club) {
		s.storeError(w, r, ErrInvalidID)
		return
	}
	if err := core.ValidateClubSpec(spec, drivers.AllClubCRMKeys()); err != nil {
		var ce *core.ConfigError
		problems := []string{err.Error()}
		if errors.As(err, &ce) {
			problems = ce.Problems
		}
		core.WriteJSON(w, http.StatusBadRequest, problemsBody{Code: "INVALID_CLUB_CONFIG", Message: "the club config is invalid",
			Problems: problems, RequestID: core.RequestID(r)})
		return
	}
	created, v, err := s.store.PutClub(inst, spec)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.log.Info("club config stored", "installation", inst, "club", club, "config_version", v)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	core.WriteJSON(w, status, ClubPutDTO{Installation: inst, Club: club, ConfigVersion: v})
}

// DELETE /hub/installations/{inst}/clubs/{club}
func (s *Server) handleDeleteClub(w http.ResponseWriter, r *http.Request) {
	inst, club := r.PathValue("inst"), r.PathValue("club")
	v, err := s.store.DeleteClub(inst, club)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.log.Info("club removed", "installation", inst, "club", club, "config_version", v)
	core.WriteJSON(w, http.StatusOK, ClubPutDTO{Installation: inst, Club: club, ConfigVersion: v})
}

// POST /hub/installations/{inst}/discover {"studio"?, "timezone"?, "slot_minutes"?}
// runs the CRM discovery on the installation's gateway, through its
// tunnel, and returns its answer: the account's studios and a draft club
// config. Nothing is stored.
func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	inst := r.PathValue("inst")
	if _, ok := s.store.Installation(inst); !ok {
		s.storeError(w, r, ErrUnknownInstallation)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		core.WriteError(w, r, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body is too large")
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	if !json.Valid(body) {
		core.WriteError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON body")
		return
	}
	if _, registered := s.store.Get(inst); !registered || !s.broker.Connected(inst) {
		w.Header().Set("Retry-After", "60")
		core.WriteError(w, r, http.StatusServiceUnavailable, "GATEWAY_OFFLINE", "the installation's gateway is not connected")
		return
	}
	// within the HTTP server's write timeout (poll_wait + request_timeout + 30s)
	timeout := s.cfg.RequestTimeout + s.cfg.PollWait
	rq := tunnel.Request{
		ID: newCallID(), RequestID: core.RequestID(r), Method: http.MethodPost, Path: tunnel.PathDiscover,
		Header: map[string]string{"Content-Type": "application/json"}, Body: body, TimeoutMS: int(timeout / time.Millisecond),
	}
	resp, err := s.broker.Do(r.Context(), inst, rq, 10*time.Second, timeout+5*time.Second)
	switch {
	case errors.Is(err, ErrOffline):
		w.Header().Set("Retry-After", "60")
		core.WriteError(w, r, http.StatusServiceUnavailable, "GATEWAY_OFFLINE", "the installation's gateway is not connected")
		return
	case errors.Is(err, ErrBusy):
		w.Header().Set("Retry-After", "5")
		core.WriteError(w, r, http.StatusServiceUnavailable, "GATEWAY_BUSY", "the gateway is busy with other requests, retry")
		return
	case errors.Is(err, ErrTimeout):
		core.WriteError(w, r, http.StatusGatewayTimeout, "GATEWAY_TIMEOUT", "the gateway did not answer in time")
		return
	case err != nil:
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
