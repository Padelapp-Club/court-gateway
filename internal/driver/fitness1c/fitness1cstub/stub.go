// Package fitness1cstub is an in-memory stand-in for the client API v3 of
// a club's 1C:Fitness instance, as far as the gateway uses it and as the
// backend adapter's runs observed it (D14 facts §13–§27): HTTP Basic plus
// the apikey header (an HTML 401 otherwise), the {result, data} envelope
// with numeric application errors, a rental grid where a confirmed rental
// is a missing half-hour and a temporary reservation is is_busy, rooms
// repeated once per day of the asked range, client tokens issued by
// phone, rentals idempotent per client and slot, 1060 for a taken room and
// 1037 for a repeated cancel. What no run observed is marked so.
package fitness1cstub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Service is a rental service of the club.
type Service struct {
	ID              string
	Title           string
	Seance          int    // minutes
	Price           string // per seance, rubles, as 1C writes it
	PaymentRequired bool
}

type room struct{ id, title string }

type client struct {
	id, phone, token string
}

type rentalRec struct {
	id, room, service, client string
	start, end                time.Time
	temporary, canceled       bool
	// personal data a club rental carries: it must never leave the gateway
	private map[string]any
}

// Stub is the 1C instance. Zero or more clubs share it; the zero value is
// not usable, use New.
type Stub struct {
	mu       sync.Mutex
	user     string
	password string
	apiKey   string
	clubID   string
	clubName string
	zone     *time.Location
	// FirstSlot and LastSlot bound the daily grid in minutes from local
	// midnight (the start of the first and of the last half-hour).
	FirstSlot, LastSlot int
	services            []Service
	rooms               []room
	clients             map[string]*client // by phone
	rentals             map[string]*rentalRec
	blocks              []rentalRec // occupancy without a rental (a class, a closed hour)
	// Now is 1C's clock (rental status "passes"); time.Now when nil.
	Now func() time.Time

	calls    map[string]int
	failures map[string][]Failure
}

// Failure is a scripted answer to the next request of a path.
type Failure struct {
	Status int // HTTP status; 0 with Code = 400
	Code   int // 1C's application error, 0 for none
	Drop   bool
	// Apply runs the request first and drops the answer (a lost response).
	Apply bool
	// Body replaces the answer's body (with Status, 200 when 0).
	Body string
	// Delay holds the answer back (a slow 1C), then answers normally.
	Delay time.Duration
}

// New builds an instance with one club.
func New(user, password, apiKey, clubID, clubName string, zone *time.Location) *Stub {
	return &Stub{
		user: user, password: password, apiKey: apiKey, clubID: strings.ToLower(clubID), clubName: clubName, zone: zone,
		FirstSlot: 0, LastSlot: 23*60 + 30,
		clients: map[string]*client{}, rentals: map[string]*rentalRec{},
		calls: map[string]int{}, failures: map[string][]Failure{},
	}
}

// NewID returns a 1C-looking id.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// AddService adds a rental service.
func (s *Stub) AddService(svc Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc.ID = strings.ToLower(svc.ID)
	s.services = append(s.services, svc)
}

// AddRoom adds a rental room.
func (s *Stub) AddRoom(id, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rooms = append(s.rooms, room{strings.ToLower(id), title})
}

// AddClient registers a client by phone (as reg_and_auth_client would).
func (s *Stub) AddClient(phone string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &client{id: NewID(), phone: phone, token: newToken()}
	s.clients[phone] = c
	return c.id
}

// RevokeTokens makes every client token stale: 1C answers 4003 to it.
func (s *Stub) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		c.token = newToken()
	}
}

// SetPassword changes the 1C user's password.
func (s *Stub) SetPassword(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = p
}

// SeedRental plants a rental of the club's own client (as the club's admin
// makes it), full of personal data; temporary makes it a 15-minute
// reservation (is_busy in the grid) instead of a confirmed one (a gap).
func (s *Stub) SeedRental(roomID string, start, end time.Time, temporary bool, private map[string]any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &rentalRec{id: NewID(), room: strings.ToLower(roomID), client: "club-client", start: start, end: end, temporary: temporary, private: private}
	if len(s.services) > 0 {
		r.service = s.services[0].ID
	}
	s.rentals[r.id] = r
	return r.id
}

// SeedClientRental plants a rental of the client with phone on service, as
// someone other than the gateway would make it (the club moving or adding
// one at the reception).
func (s *Stub) SeedClientRental(phone, roomID, serviceID string, start, end time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &rentalRec{id: NewID(), room: strings.ToLower(roomID), service: strings.ToLower(serviceID), client: s.clients[phone].id, start: start, end: end}
	s.rentals[r.id] = r
	return r.id
}

// MoveRental moves a rental to another room, service and time, as the
// club's admin would.
func (s *Stub) MoveRental(id, roomID, serviceID string, start, end time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rentals[id]; r != nil {
		r.room, r.service, r.start, r.end = strings.ToLower(roomID), strings.ToLower(serviceID), start, end
	}
}

// SeedBlock plants occupancy that is no rental (a class, a closed hour):
// missing from the grid, in no client's appointments.
func (s *Stub) SeedBlock(roomID string, start, end time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks = append(s.blocks, rentalRec{room: strings.ToLower(roomID), start: start, end: end})
}

// CancelRental cancels a rental as the club's admin would.
func (s *Stub) CancelRental(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.rentals[id]; r != nil {
		r.canceled = true
	}
}

// RentalView is a rental as a test reads it back.
type RentalView struct {
	ID, Room, Service string
	// Phone is the phone of the client who made it ("" for a club's
	// rental, SeedRental).
	Phone      string
	Start, End time.Time
	Canceled   bool
	// Temporary and Private are the rest of the record (a 15-minute
	// reservation; the personal data of a club rental), for a test that
	// compares a rental whole.
	Temporary bool
	Private   map[string]any
}

// RentalViews returns every rental, cancelled ones too.
func (s *Stub) RentalViews() []RentalView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RentalView
	for _, r := range s.rentals {
		v := RentalView{ID: r.id, Room: r.room, Service: r.service, Start: r.start, End: r.end, Canceled: r.canceled, Temporary: r.temporary, Private: maps.Clone(r.private)}
		for _, c := range s.clients {
			if c.id == r.client {
				v.Phone = c.phone
			}
		}
		out = append(out, v)
	}
	return out
}

// Rentals returns the ids of the rentals not cancelled.
func (s *Stub) Rentals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, r := range s.rentals {
		if !r.canceled {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Fail scripts the answers to the next requests of path ("/rental/" and
// so on, without the API prefix; "POST /rental/" for one method).
func (s *Stub) Fail(path string, f ...Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[path] = append(s.failures[path], f...)
}

// Calls counts the requests of path ("GET /rental_rooms_times/").
func (s *Stub) Calls(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[key]
}

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Stub) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

const prefix = "/hs/api/v3"

func ok(w http.ResponseWriter, data any, extra map[string]any) {
	body := map[string]any{"result": true, "data": data}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"result": false, "error": code, "error_message": msg})
}

// ServeHTTP implements http.Handler.
func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, found := strings.CutPrefix(r.URL.Path, prefix)
	if !found {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.calls[r.Method+" "+path]++
	f, scripted := s.nextFailure(r.Method, path)
	user, pass, basic := r.BasicAuth()
	authed := basic && user == s.user && pass == s.password && r.Header.Get("apikey") == s.apiKey
	s.mu.Unlock()
	if scripted && f.Delay > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(f.Delay):
		}
		scripted = false
	}
	if scripted && !f.Apply {
		switch {
		case f.Drop:
			hijackClose(w)
		case f.Code != 0:
			fail(w, f.Code, "сбой по сценарию")
		case f.Body != "":
			w.WriteHeader(max(f.Status, http.StatusOK))
			_, _ = w.Write([]byte(f.Body))
		default:
			w.WriteHeader(f.Status)
		}
		return
	}
	if !authed {
		// IIS answers with an HTML page (D14 §13.1)
		w.Header().Set("WWW-Authenticate", `Basic realm="1C:Enterprise 8.3"`)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("<html><body>401.5 Unauthorized</body></html>"))
		return
	}
	if scripted && f.Apply {
		s.route(httpDiscard{}, r, path)
		hijackClose(w)
		return
	}
	s.route(w, r, path)
}

func (s *Stub) nextFailure(method, path string) (Failure, bool) {
	for _, k := range []string{method + " " + path, path} {
		if q := s.failures[k]; len(q) > 0 {
			s.failures[k] = q[1:]
			return q[0], true
		}
	}
	return Failure{}, false
}

func hijackClose(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

type httpDiscard struct{}

func (httpDiscard) Header() http.Header         { return http.Header{} }
func (httpDiscard) Write(p []byte) (int, error) { return len(p), nil }
func (httpDiscard) WriteHeader(int)             {}

func (s *Stub) route(w http.ResponseWriter, r *http.Request, path string) {
	q := r.URL.Query()
	switch r.Method + " " + path {
	case "GET /clubs/":
		s.clubs(w)
	case "GET /rental_services/":
		s.servicesList(w, q.Get("club_id"))
	case "GET /rental_rooms/":
		s.roomsList(w, q.Get("club_id"))
	case "GET /rental_rooms_times/":
		s.grid(w, q)
	case "GET /client_usertoken/":
		s.userToken(w, q.Get("phone"))
	case "GET /appointments/":
		s.appointments(w, r)
	case "POST /rental/":
		s.createRental(w, r)
	case "GET /rental/":
		s.getRental(w, q.Get("rental_id"))
	case "DELETE /rental/":
		s.deleteRental(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *Stub) clubs(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok(w, []map[string]any{{
		"id": s.clubID, "title": s.clubName, "time_zone": s.zone.String(),
		// work_time is not the grid's (D14 §15.1): the driver must not use it
		"work_time": []map[string]any{{"active": true, "day": 1, "start": "00:00", "end": "23:59"}},
	}}, nil)
}

func (s *Stub) knownClub(id string) bool { return strings.EqualFold(id, s.clubID) }

func (s *Stub) servicesList(w http.ResponseWriter, club string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.knownClub(club) {
		fail(w, 1025, "Не найдена структурная единица")
		return
	}
	list := []map[string]any{}
	for _, v := range s.services {
		list = append(list, map[string]any{"id": v.ID, "title": v.Title, "price": v.Price, "seance_length": v.Seance,
			"payment_required": v.PaymentRequired, "optional_services": []any{}})
	}
	ok(w, list, nil)
}

func (s *Stub) roomsList(w http.ResponseWriter, club string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.knownClub(club) {
		fail(w, 1025, "Не найдена структурная единица")
		return
	}
	list := []map[string]any{}
	for _, rm := range s.rooms {
		list = append(list, map[string]any{"id": rm.id, "title": rm.title, "price": 875})
	}
	ok(w, list, nil)
}

func (s *Stub) service(id string) *Service {
	for i := range s.services {
		if s.services[i].ID == strings.ToLower(id) {
			return &s.services[i]
		}
	}
	return nil
}

// occupant is what holds [start, end) of a room: a rental not cancelled
// or a block, other than except.
func (s *Stub) occupant(roomID string, start, end time.Time, except string) (*rentalRec, bool) {
	for _, r := range s.rentals {
		if !r.canceled && r.room == roomID && r.id != except && r.start.Before(end) && r.end.After(start) {
			return r, true
		}
	}
	for i := range s.blocks {
		b := &s.blocks[i]
		if b.room == roomID && b.start.Before(end) && b.end.After(start) {
			return b, true
		}
	}
	return nil, false
}

func (s *Stub) grid(w http.ResponseWriter, q map[string][]string) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.knownClub(get("club_id")) {
		fail(w, 1025, "Не найдена структурная единица")
		return
	}
	svc := s.service(get("service_id"))
	if svc == nil {
		fail(w, 1118, "Неверный список")
		return
	}
	from, err1 := time.ParseInLocation("2006-01-02 15:04", get("start_date"), s.zone)
	to, err2 := time.ParseInLocation("2006-01-02 15:04", get("end_date"), s.zone)
	if err1 != nil || err2 != nil || !to.After(from) {
		fail(w, 1121, "Неверные даты")
		return
	}
	step := time.Duration(svc.Seance) * time.Minute
	data := []map[string]any{}
	var days []time.Time
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
	}
	for _, rm := range s.rooms {
		slots := []map[string]any{}
		for _, d := range days {
			for m := s.FirstSlot; m <= s.LastSlot; m += svc.Seance {
				t := d.Add(time.Duration(m) * time.Minute)
				occ, taken := s.occupant(rm.id, t, t.Add(step), "")
				slot := map[string]any{"time": t.Format("15:04"), "seance_length": svc.Seance, "date_time": t.Format("2006-01-02 15:04"),
					"rental_id": nil, "price": nil, "service_title": nil, "is_busy": false}
				switch {
				case taken && occ.id != "" && occ.temporary:
					slot["is_busy"], slot["rental_id"] = true, occ.id
					if v, ok := occ.private["service_title"]; ok {
						slot["service_title"] = v
					}
				case taken:
					continue // a confirmed rental or a block: the half-hour is not listed
				default:
					p, _ := strconv.ParseFloat(svc.Price, 64)
					slot["price"] = p
				}
				slots = append(slots, slot)
			}
		}
		// 1C puts each room once per day of the range, each with the whole range
		for range days {
			data = append(data, map[string]any{"room": map[string]any{"id": rm.id, "title": rm.title}, "durations": slots})
		}
	}
	ok(w, data, nil)
}

func (s *Stub) userToken(w http.ResponseWriter, phone string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.clients[phone]
	if c == nil {
		fail(w, 1010, "Клиент не найден")
		return
	}
	ok(w, map[string]any{"usertoken": c.token}, nil)
}

func (s *Stub) byToken(tok string) *client {
	if tok == "" {
		return nil
	}
	for _, c := range s.clients {
		if c.token == tok {
			return c
		}
	}
	return nil
}

func (s *Stub) status(r *rentalRec) string {
	switch now := s.now(); {
	case r.canceled:
		return "canceled"
	case !now.Before(r.end):
		return "ended"
	case !now.Before(r.start):
		return "passes"
	}
	return "planned"
}

func (s *Stub) appointments(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken(r.Header.Get("usertoken"))
	if c == nil {
		fail(w, 4003, "Неверные данные авторизации")
		return
	}
	q := r.URL.Query()
	var statuses []string
	if raw := q.Get("statuses"); raw != "" {
		if json.Unmarshal([]byte(raw), &statuses) != nil {
			fail(w, 1121, "Неверный список статусов")
			return
		}
	}
	want := map[string]bool{}
	for _, st := range statuses {
		want[st] = true
	}
	var list []*rentalRec
	for _, rr := range s.rentals {
		if rr.client == c.id && (len(want) == 0 || want[s.status(rr)]) {
			list = append(list, rr)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].start.Before(list[j].start) || (list[i].start.Equal(list[j].start) && list[i].id < list[j].id)
	})
	// [UNVERIFIED] requested_offset is an offset in appointments
	offset, _ := strconv.Atoi(q.Get("requested_offset"))
	size, _ := strconv.Atoi(q.Get("page_size"))
	if size <= 0 || size > 30 {
		size = 30
	}
	total := len(list)
	if offset > total {
		offset = total
	}
	page := list[offset:min(offset+size, total)]
	data := []map[string]any{}
	for _, rr := range page {
		svc := s.service(rr.service)
		title := ""
		if svc != nil {
			title = svc.Title
		}
		data = append(data, map[string]any{
			"club_id": s.clubID, "appointment_id": rr.id, "type": "rental", "status": s.status(rr),
			"service": map[string]any{"id": rr.service, "title": title}, "room": map[string]any{"id": rr.room, "title": "Корт"},
			"start_date": rr.start.In(s.zone).Format("2006-01-02 15:04"), "end_date": rr.end.In(s.zone).Format("2006-01-02 15:04"),
			"duration": int(rr.end.Sub(rr.start) / time.Minute), "cost": 0,
		})
	}
	ok(w, data, map[string]any{"pagination_response": map[string]any{"requested_offset": offset, "page_size": size, "total_results": total}})
}

func (s *Stub) createRental(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken(r.Header.Get("usertoken"))
	if c == nil {
		fail(w, 4003, "Неверные данные авторизации")
		return
	}
	var body struct {
		DateTime  string  `json:"date_time"`
		ServiceID string  `json:"service_id"`
		RoomID    string  `json:"room_id"`
		Count     float64 `json:"count"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		fail(w, 1121, "Неверный запрос")
		return
	}
	svc := s.service(body.ServiceID)
	start, err := time.ParseInLocation("2006-01-02 15:04", body.DateTime, s.zone)
	known := false
	for _, rm := range s.rooms {
		known = known || rm.id == strings.ToLower(body.RoomID)
	}
	if svc == nil || err != nil || !known || body.Count < 1 || body.Count != float64(int(body.Count)) {
		fail(w, 1118, "Неверный список покупок")
		return
	}
	end := start.Add(time.Duration(int(body.Count)*svc.Seance) * time.Minute)
	roomID := strings.ToLower(body.RoomID)
	if occ, taken := s.occupant(roomID, start, end, ""); taken {
		if occ.client == c.id && occ.start.Equal(start) && occ.end.Equal(end) && occ.service == svc.ID {
			s.writeCreated(w, occ, svc, c) // idempotent per client and slot (D14 §27)
			return
		}
		fail(w, 1060, "В выбранном помещении контролируется занятость. В указанное время помещение занято")
		return
	}
	rr := &rentalRec{id: NewID(), room: roomID, service: svc.ID, client: c.id, start: start, end: end, temporary: svc.PaymentRequired}
	s.rentals[rr.id] = rr
	s.writeCreated(w, rr, svc, c)
}

func (s *Stub) writeCreated(w http.ResponseWriter, rr *rentalRec, svc *Service, c *client) {
	status := "reserved"
	if rr.temporary {
		status = "temporarily_reserved_need_payment"
	}
	ok(w, map[string]any{
		"rental":               map[string]any{"id": rr.id, "title": svc.Title, "service": map[string]any{"id": svc.ID, "title": svc.Title}, "date_time": rr.start.In(s.zone).Format("2006-01-02 15:04:05")},
		"customer":             map[string]any{"id": c.id, "client_name": "Padelapp"},
		"temporarily_reserved": rr.temporary, "status": status, "online": false,
		"count": int(rr.end.Sub(rr.start)/time.Minute) / svc.Seance,
	}, nil)
}

// getRental answers GET /rental/. [UNVERIFIED] the code for an unknown id:
// no run asked for one; 1100 here.
func (s *Stub) getRental(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rr := s.rentals[strings.ToLower(id)]
	if rr == nil {
		fail(w, 1100, "Занятие не найдено")
		return
	}
	data := map[string]any{
		"appointment_id": rr.id, "rental_id": rr.id, "canceled": rr.canceled, "reason_for_cancellation": nil,
		"status": map[bool]any{true: nil, false: "reserved"}[rr.canceled], "payment_required": rr.temporary,
		"service": map[string]any{"id": rr.service, "title": "Аренда"}, "room": map[string]any{"id": rr.room, "title": "Корт"},
		"type": "rental", "start_date": rr.start.In(s.zone).Format("2006-01-02 15:04"), "end_date": rr.end.In(s.zone).Format("2006-01-02 15:04"),
		"duration": int(rr.end.Sub(rr.start) / time.Minute), "cost": 0,
	}
	if rr.private != nil {
		data["customer"] = rr.private
	}
	ok(w, data, nil)
}

func (s *Stub) deleteRental(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken(r.Header.Get("usertoken"))
	if c == nil {
		fail(w, 4003, "Неверные данные авторизации")
		return
	}
	rr := s.rentals[strings.ToLower(r.URL.Query().Get("rental_id"))]
	switch {
	case rr == nil:
		fail(w, 1100, "Занятие не найдено")
	case rr.client != c.id:
		// [UNVERIFIED] what 1C answers a client cancelling another's rental
		fail(w, 4003, "Неверные данные авторизации")
	case rr.canceled:
		fail(w, 1037, "Занятие уже отменено")
	default:
		rr.canceled = true
		ok(w, map[string]any{"status": nil, "temporarily_reserved": false}, nil)
	}
}

// String is for test failure messages.
func (s *Stub) String() string { return fmt.Sprintf("fitness1cstub(%s)", s.clubID) }
