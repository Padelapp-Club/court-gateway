// Package vivastub is a fake VivaCRM for tests: a Keycloak-like token
// endpoint and the admin API endpoints the viva driver uses. It mimics the
// behaviour the driver depends on, as observed on a live account: lists
// select records by their START date in the studio's local day (dateFrom
// and dateTo included), the exercise list comes back EMPTY when dateFrom
// is a past day while the break list does not, and reads by id always
// work.
//
// Serve it with httptest.NewServer(stub); then the driver's auth_url is
// <server>/token and api_url is <server>/api/v1.
package vivastub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// APIPrefix is where the admin API is mounted.
const APIPrefix = "/api/v1"

// TokenPath is the token endpoint.
const TokenPath = "/token"

type override struct {
	method, prefix string
	status         int
	body           string
}

// Stub is the fake server state. Exported fields are configuration; set
// them before serving.
type Stub struct {
	Username string
	Password string
	ClientID string
	Studio   string
	// TokenTTL is the expires_in returned at login, in seconds.
	TokenTTL int
	// Now is the stub's clock (defaults to time.Now).
	Now func() time.Time
	// Zone is the studio's time zone, whose dates the lists select by and
	// whose offset GET /studios reports (defaults to UTC+3).
	Zone *time.Location
	// OmitCommentOnRead removes "comment" from reads by id.
	OmitCommentOnRead bool
	// StudioName and WorkTime (weekday name -> {from, to} as
	// "HH:MM+03:00"; nil omits workTime) describe Studio in GET /studios.
	StudioName string
	WorkTime   map[string][2]string

	mu        sync.Mutex
	others    []map[string]any  // more studios of the account (listed only)
	rooms     map[string]string // id -> name
	tokens    map[string]bool
	exercises map[string]map[string]any
	breaks    map[string]map[string]any
	pricing   map[string]json.RawMessage
	overrides []override
	logins    int
	attempts  int
	calls     []string
}

// New returns a stub with the given credentials and studio.
func New(username, password, studio string) *Stub {
	return &Stub{
		Username:  username,
		Password:  password,
		ClientID:  "React-auth-dev",
		Studio:    studio,
		TokenTTL:  300,
		rooms:     map[string]string{},
		tokens:    map[string]bool{},
		exercises: map[string]map[string]any{},
		breaks:    map[string]map[string]any{},
		pricing:   map[string]json.RawMessage{},
	}
}

func (s *Stub) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// NewID returns a random UUID v4 string.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// AddRoom registers a room.
func (s *Stub) AddRoom(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rooms[strings.ToLower(id)] = name
}

// RemoveRoom takes a room out of the studio.
func (s *Stub) RemoveRoom(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rooms, strings.ToLower(id))
}

// AddStudio lists one more studio of the account in GET /studios, with
// rooms (id -> name). Its rooms are not bookable in the stub.
func (s *Stub) AddStudio(id, name string, rooms map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.others = append(s.others, studioObject(id, name, 3, nil, rooms))
}

func studioObject(id, name string, offset float64, workTime map[string][2]string, rooms map[string]string) map[string]any {
	ids := make([]string, 0, len(rooms))
	for rid := range rooms {
		ids = append(ids, rid)
	}
	sort.Strings(ids)
	list := []map[string]any{}
	for _, rid := range ids {
		list = append(list, map[string]any{
			"id": rid, "name": rooms[rid], "studioId": id, "orgId": "00000000-0000-4000-8000-0000000000f0",
			"totalCapacity": 4, "color": "#000000", "workTime": nil, "rates": []any{}, "directions": []any{},
			"roomPartition": nil, "customFields": []any{}, "photos": []any{}, "mainPhoto": nil,
		})
	}
	st := map[string]any{"id": id, "name": name, "offset": offset, "schedule": nil, "rooms": list}
	if workTime != nil {
		daily := map[string]any{}
		for day, ft := range workTime {
			daily[day] = map[string]string{"from": ft[0], "to": ft[1]}
		}
		st["workTime"] = map[string]any{"dailyWorkTime": daily}
	}
	return st
}

func (s *Stub) studios() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := s.StudioName
	if name == "" {
		name = "Studio"
	}
	_, off := s.now().In(s.zone()).Zone()
	out := []map[string]any{studioObject(s.Studio, name, float64(off)/3600, s.WorkTime, s.rooms)}
	return append(out, s.others...)
}

// SetPricing stores a price list body for GET /pricing/{id}.
func (s *Stub) SetPricing(id string, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pricing[id] = json.RawMessage(body)
}

// SeedExercise stores an exercise exactly as given (any extra fields are
// returned as-is) and returns its id. "id" is generated when absent.
func (s *Stub) SeedExercise(rec map[string]any) string { return s.seed(s.exercises, rec) }

// SeedBreak stores a break like SeedExercise.
func (s *Stub) SeedBreak(rec map[string]any) string { return s.seed(s.breaks, rec) }

func (s *Stub) seed(m map[string]map[string]any, rec map[string]any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := rec["id"].(string)
	if id == "" {
		id = NewID()
		rec["id"] = id
	}
	m[id] = rec
	return id
}

// Exercise returns a copy of a stored exercise.
func (s *Stub) Exercise(id string) map[string]any { return s.get(s.exercises, id) }

// Break returns a copy of a stored break.
func (s *Stub) Break(id string) map[string]any { return s.get(s.breaks, id) }

func (s *Stub) get(m map[string]map[string]any, id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := m[id]
	if r == nil {
		return nil
	}
	return copyMap(r)
}

// FailNext makes the next request whose method matches and whose path
// (below the API prefix, or the token path) starts with prefix answer with
// status and body instead.
func (s *Stub) FailNext(method, prefix string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overrides = append(s.overrides, override{method: method, prefix: prefix, status: status, body: body})
}

// RevokeTokens invalidates every issued token, as an expiry would.
func (s *Stub) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]bool{}
}

// LoginAttempts returns how many logins were tried, failed ones included.
func (s *Stub) LoginAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// Logins returns how many successful logins happened.
func (s *Stub) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

// Calls returns "METHOD path?query" of every API call so far.
func (s *Stub) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func copyMap(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "message": "not found"})
}

// ServeHTTP implements http.Handler.
func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	rel := strings.TrimPrefix(path, APIPrefix)
	s.mu.Lock()
	if path != TokenPath {
		s.calls = append(s.calls, r.Method+" "+rel+"?"+r.URL.RawQuery)
	} else {
		s.attempts++
	}
	for i, o := range s.overrides {
		if o.method == r.Method && (strings.HasPrefix(rel, o.prefix) || (path == TokenPath && o.prefix == TokenPath)) {
			s.overrides = append(s.overrides[:i], s.overrides[i+1:]...)
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(o.status)
			_, _ = w.Write([]byte(o.body))
			return
		}
	}
	s.mu.Unlock()

	if path == TokenPath {
		s.handleToken(w, r)
		return
	}
	if !strings.HasPrefix(path, APIPrefix+"/") {
		notFound(w)
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	switch {
	case len(parts) == 1 && (parts[0] == "exercises" || parts[0] == "breaks") && r.Method == http.MethodGet:
		s.handleList(w, r, parts[0])
	case len(parts) == 1 && parts[0] == "exercises" && r.Method == http.MethodPost:
		s.handleCreateExercise(w, r)
	case len(parts) == 1 && parts[0] == "breaks" && r.Method == http.MethodPost:
		s.handleCreateBreak(w, r)
	case len(parts) == 2 && (parts[0] == "exercises" || parts[0] == "breaks") && r.Method == http.MethodGet:
		s.handleRead(w, parts[0], parts[1])
	case len(parts) == 2 && (parts[0] == "exercises" || parts[0] == "breaks") && r.Method == http.MethodDelete:
		s.handleDelete(w, parts[0], parts[1])
	case len(parts) == 1 && parts[0] == "studios" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.studios())
	case len(parts) == 2 && parts[0] == "studios" && r.Method == http.MethodGet:
		for _, st := range s.studios() {
			if st["id"] == parts[1] {
				writeJSON(w, http.StatusOK, st)
				return
			}
		}
		notFound(w)
	case len(parts) == 1 && parts[0] == "pricing" && r.Method == http.MethodGet:
		s.handlePricingList(w, r)
	case len(parts) == 2 && parts[0] == "pricing" && r.Method == http.MethodGet:
		s.handlePricing(w, parts[1])
	default:
		notFound(w)
	}
}

func (s *Stub) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	f := r.PostForm
	if f.Get("grant_type") != "password" || f.Get("client_id") != s.ClientID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client"})
		return
	}
	if f.Get("username") != s.Username || f.Get("password") != s.Password {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_grant"})
		return
	}
	tok := "tok-" + NewID()
	s.mu.Lock()
	s.tokens[tok] = true
	s.logins++
	ttl := s.TokenTTL
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok, "expires_in": ttl, "refresh_expires_in": ttl, "token_type": "Bearer", "scope": "profile",
	})
}

func (s *Stub) authorized(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[tok]
}

func parseTime(v any) (time.Time, bool) {
	str, _ := v.(string)
	if t, err := time.Parse(time.RFC3339Nano, str); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func roomID(rec map[string]any) string {
	if room, ok := rec["room"].(map[string]any); ok {
		id, _ := room["id"].(string)
		return id
	}
	return ""
}

func (s *Stub) zone() *time.Location {
	if s.Zone != nil {
		return s.Zone
	}
	return time.FixedZone("UTC+3", 3*3600)
}

func localDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

func (s *Stub) handleList(w http.ResponseWriter, r *http.Request, kind string) {
	q := r.URL.Query()
	loc := s.zone()
	from, err1 := time.ParseInLocation("2006-01-02", q.Get("dateFrom"), loc)
	to, err2 := time.ParseInLocation("2006-01-02", q.Get("dateTo"), loc)
	if q.Get("studioId") == "" || err1 != nil || err2 != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}
	if q.Get("studioId") != s.Studio {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	today := localDay(s.now(), loc)
	includeCanceled := q.Get("includeCanceled") == "true"
	room := q.Get("roomId")
	s.mu.Lock()
	src := s.exercises
	if kind == "breaks" {
		src = s.breaks
	}
	out := []map[string]any{}
	for _, rec := range src {
		start, ok := parseTime(rec["timeFrom"])
		if !ok {
			// malformed seeded data is passed through as-is
			out = append(out, copyMap(rec))
			continue
		}
		day := localDay(start, loc)
		if day.Before(from) || day.After(to) || (kind == "exercises" && day.Before(today)) {
			continue // exercises of past days are left out
		}
		if room != "" && !strings.EqualFold(roomID(rec), room) {
			continue
		}
		if c, _ := rec["canceled"].(bool); c && !includeCanceled {
			continue
		}
		out = append(out, copyMap(rec))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["timeFrom"]) < fmt.Sprint(out[j]["timeFrom"]) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Stub) decode(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return nil, false
	}
	return body, true
}

func (s *Stub) roomRef(id string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.rooms[strings.ToLower(id)]
	if !ok {
		return nil, false
	}
	return map[string]any{"id": strings.ToLower(id), "name": name}, true
}

func (s *Stub) handleCreateExercise(w http.ResponseWriter, r *http.Request) {
	b, ok := s.decode(w, r)
	if !ok {
		return
	}
	_, okDir := b["direction"].(float64)
	_, okType := b["type"].(float64)
	_, okMax := b["maxClientsCount"].(float64)
	_, okReq := b["requirements"].([]any)
	_, okFrom := parseTime(b["timeFrom"])
	_, okTo := parseTime(b["timeTo"])
	roomIDStr, _ := b["roomId"].(string)
	room, okRoom := s.roomRef(roomIDStr)
	if !okDir || !okType || !okMax || !okReq || !okFrom || !okTo || !okRoom {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}
	id := NewID()
	rec := map[string]any{
		"id":              id,
		"timeFrom":        b["timeFrom"],
		"timeTo":          b["timeTo"],
		"room":            room,
		"studio":          map[string]any{"id": s.Studio, "name": "Studio"},
		"direction":       map[string]any{"id": b["direction"], "name": "Direction"},
		"type":            map[string]any{"id": b["type"], "name": "Type"},
		"comment":         b["comment"],
		"clientsCount":    0,
		"maxClientsCount": b["maxClientsCount"],
		"canceled":        false,
	}
	s.mu.Lock()
	s.exercises[id] = rec
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "direction": rec["direction"], "type": rec["type"], "timeFrom": rec["timeFrom"], "timeTo": rec["timeTo"],
	})
}

func (s *Stub) handleCreateBreak(w http.ResponseWriter, r *http.Request) {
	b, ok := s.decode(w, r)
	if !ok {
		return
	}
	title, okTitle := b["title"].(string)
	_, okFrom := parseTime(b["timeFrom"])
	_, okTo := parseTime(b["timeTo"])
	_, okMerge := b["mergable"].(bool)
	roomIDStr, _ := b["roomId"].(string)
	room, okRoom := s.roomRef(roomIDStr)
	if !okTitle || !okFrom || !okTo || !okMerge || !okRoom {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}
	id := NewID()
	rec := map[string]any{
		"id":       id,
		"title":    title,
		"timeFrom": b["timeFrom"],
		"timeTo":   b["timeTo"],
		"room":     room,
		"studio":   map[string]any{"id": s.Studio, "name": "Studio"},
		"comment":  b["comment"],
		"canceled": false,
	}
	s.mu.Lock()
	s.breaks[id] = rec
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Stub) handleRead(w http.ResponseWriter, kind, id string) {
	s.mu.Lock()
	src := s.exercises
	if kind == "breaks" {
		src = s.breaks
	}
	rec := src[strings.ToLower(id)]
	var out map[string]any
	if rec != nil {
		out = copyMap(rec)
	}
	s.mu.Unlock()
	if out == nil {
		notFound(w)
		return
	}
	if s.OmitCommentOnRead {
		delete(out, "comment")
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Stub) handleDelete(w http.ResponseWriter, kind, id string) {
	s.mu.Lock()
	src := s.exercises
	if kind == "breaks" {
		src = s.breaks
	}
	rec := src[strings.ToLower(id)]
	if rec != nil {
		rec["canceled"] = true
	}
	s.mu.Unlock()
	if rec == nil {
		notFound(w)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Stub) handlePricingList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("studioId") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}
	s.mu.Lock()
	ids := make([]string, 0, len(s.pricing))
	for id := range s.pricing {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)
	content := []map[string]any{}
	if r.URL.Query().Get("studioId") == s.Studio {
		for _, id := range ids {
			content = append(content, map[string]any{"id": id, "name": "Price list", "usages": map[string]any{}})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": content, "totalElements": len(content)})
}

func (s *Stub) handlePricing(w http.ResponseWriter, id string) {
	s.mu.Lock()
	body, ok := s.pricing[id]
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
