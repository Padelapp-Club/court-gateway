// Package lundastub is a fake Lunda club admin API for tests: the login
// (QR and phone + code, with the RSA exchange: every token is answered
// encrypted in chunks with the client's public key), the club switch,
// renewal on refresh-required, the schedule, tariffs and the calendar,
// and bookings (create, price adjustment, notes, load, cancel), as
// observed under a staff login on 2026-10-17. Answers wrap their data in
// {"result": ...}; requests carry {"parameters": {...}}.
//
// Serve it with httptest.NewServer(stub); the driver's api_url is then
// <server>/api. Every request is logged (Calls), whatever its path, so a
// test can assert that nothing outside the driver's operations was called.
package lundastub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// APIPrefix is where the API is mounted.
const APIPrefix = "/api"

// Club is a club a staff member may act for.
type Club struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// Call is one request the stub received.
type Call struct {
	Method string
	Path   string // below APIPrefix, e.g. "/club/booking/booking-create"
	Query  string
	Token  string // X-Club-Auth-Token, "" when none
	Params map[string]any
}

type qrLogin struct {
	pub    *rsa.PublicKey
	status string
}

type slot struct {
	court      string
	typ        string
	begin, end time.Time
	extra      map[string]any
}

// Booking is a booking as the stub keeps it.
type Booking struct {
	ID         string
	Court      string
	Begin, End time.Time
	Customer   string
	Goal       string
	Price      float64 // rubles
	Adjustment float64
	Status     string // NOT_PAID, PAID, CANCELLED
	Notes      string
	Payments   []any
	Extra      map[string]any // more fields, as Lunda would answer them (client data)
}

type failure struct {
	method, path string
	status       int
	body         string
}

// Stub is the fake server. Exported fields are configuration; set them
// before serving.
type Stub struct {
	Club  Club
	Clubs []Club // the clubs a login answers (default: Club only)
	// Code is what the phone login accepts.
	Code string
	// Now is the stub's clock (default time.Now); Zone the club's.
	Now  func() time.Time
	Zone *time.Location
	// BookingDepth in days: later days answer beyondBookingDepth.
	BookingDepth int
	// AvailableHours of every day.
	AvailableHours []int
	// PricePerHour of a new booking, in rubles.
	PricePerHour float64
	// RefreshKeepsToken answers a renewal the same token (as Lunda does
	// when it did not ask for one).
	RefreshKeepsToken bool
	// OmitNotesOnLoad leaves notes out of booking-load.
	OmitNotesOnLoad bool

	mu       sync.Mutex
	courts   []Club // uid, name
	blocked  map[string]map[string]bool
	slots    []slot
	bookings map[string]*Booking
	qr       map[string]*qrLogin
	temporal map[string]string // temporal token -> phone
	logins   map[string]bool   // login tokens before a club is chosen
	tokens   map[string]string // club tokens -> club uid
	due      map[string]bool   // club tokens whose answers carry refresh-required
	tariffs  map[string]json.RawMessage
	calendar map[int]json.RawMessage
	visitors []map[string]any
	fails    []failure
	calls    []Call
	renewals int
}

// New returns a stub of club.
func New(club Club) *Stub {
	return &Stub{
		Club: club, Code: "246810", BookingDepth: 14, AvailableHours: []int{7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22},
		PricePerHour: 1500, Zone: time.FixedZone("MSK", 3*3600),
		blocked: map[string]map[string]bool{}, bookings: map[string]*Booking{}, qr: map[string]*qrLogin{},
		temporal: map[string]string{}, logins: map[string]bool{}, tokens: map[string]string{}, due: map[string]bool{},
		tariffs: map[string]json.RawMessage{}, calendar: map[int]json.RawMessage{},
	}
}

func (s *Stub) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// NewID returns a random UUID v4.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// newToken is an opaque token of 336 characters, as Lunda's.
func newToken() string {
	b := make([]byte, 252)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// AddCourt adds a court to the club.
func (s *Stub) AddCourt(uid, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.courts = append(s.courts, Club{UID: uid, Name: name})
}

// BlockCourt leaves the court out of the schedule of date (2006-01-02), as
// a court blocked for whole days is.
func (s *Stub) BlockCourt(uid, date string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocked[uid] == nil {
		s.blocked[uid] = map[string]bool{}
	}
	s.blocked[uid][date] = true
}

// SeedSlot puts a slot of no booking (DRAFT, BLOCKED, or any type) in the
// schedule; extra fields are answered as they are.
func (s *Stub) SeedSlot(court, typ string, begin, end time.Time, extra map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots = append(s.slots, slot{court: court, typ: typ, begin: begin, end: end, extra: extra})
}

// SeedBooking stores a booking as the club's staff would make it and
// returns its id.
func (s *Stub) SeedBooking(b Booking) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.ID == "" {
		b.ID = NewID()
	}
	if b.Status == "" {
		b.Status = "NOT_PAID"
	}
	if b.Goal == "" {
		b.Goal = "GAME"
	}
	if b.Payments == nil {
		b.Payments = []any{}
	}
	s.bookings[b.ID] = &b
	return b.ID
}

// Booking returns a copy of a booking (nil when unknown).
func (s *Stub) Booking(id string) *Booking {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bookings[id]
	if !ok {
		return nil
	}
	c := *b
	return &c
}

// Bookings returns copies of every booking.
func (s *Stub) Bookings() []Booking {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Booking
	for _, b := range s.bookings {
		out = append(out, *b)
	}
	return out
}

// SetTariff stores the answer of tariff/load?uid= for uid (the result
// object) and lists it.
func (s *Stub) SetTariff(uid string, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tariffs[uid] = json.RawMessage(result)
}

// SetCalendar stores the answer of calendar/load?year= (the result object).
func (s *Stub) SetCalendar(year int, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calendar[year] = json.RawMessage(result)
}

// AddVisitor adds a client of the club (profileUid, firstName, lastName,
// phone, ...), as the visitor list answers it.
func (s *Stub) AddVisitor(v map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.visitors = append(s.visitors, v)
}

// ConfirmQR confirms every QR login waiting, as the staff member's app does.
func (s *Stub) ConfirmQR() { s.setQR("SUCCESSFUL") }

// DeclineQR declines every QR login waiting.
func (s *Stub) DeclineQR() { s.setQR("DECLINED") }

// ExpireQR expires every QR login waiting.
func (s *Stub) ExpireQR() { s.setQR("EXPIRED") }

func (s *Stub) setQR(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.qr {
		if q.status == "PENDING" {
			q.status = status
		}
	}
}

// IssueSession opens a club session without a login and returns its token
// (for tests that start logged in).
func (s *Stub) IssueSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := newToken()
	s.tokens[t] = s.Club.UID
	return t
}

// RequireRefresh makes every answer to a live session carry
// refresh-required: true until it is renewed.
func (s *Stub) RequireRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t := range s.tokens {
		s.due[t] = true
	}
}

// Revoke ends every session: 401 from then on.
func (s *Stub) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens, s.logins, s.due = map[string]string{}, map[string]bool{}, map[string]bool{}
}

// Sessions returns the live club tokens.
func (s *Stub) Sessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for t := range s.tokens {
		out = append(out, t)
	}
	return out
}

// Renewals returns how many auth/refresh calls renewed a session.
func (s *Stub) Renewals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewals
}

// FailNext makes the next request of method to path (below APIPrefix)
// answer status and body instead.
func (s *Stub) FailNext(method, path string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = append(s.fails, failure{method: method, path: path, status: status, body: body})
}

// Calls returns every request so far.
func (s *Stub) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CallsTo counts the requests to path.
func (s *Stub) CallsTo(path string) int {
	n := 0
	for _, c := range s.Calls() {
		if c.Path == path {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func result(w http.ResponseWriter, v any) { writeJSON(w, http.StatusOK, map[string]any{"result": v}) }

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"errors": []map[string]string{{"message": msg}}})
}

// encrypt cuts the token into chunks and encrypts each with pub, as Lunda
// answers a token.
func encrypt(pub *rsa.PublicKey, tok string) []string {
	var out []string
	for i := 0; i < len(tok); i += 100 {
		part := tok[i:min(i+100, len(tok))]
		ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte(part), nil)
		if err != nil {
			panic(err)
		}
		out = append(out, base64.StdEncoding.EncodeToString(ct))
	}
	return out
}

func parseKey(v any) (*rsa.PublicKey, bool) {
	str, _ := v.(string)
	der, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		return nil, false
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, false
	}
	pub, ok := k.(*rsa.PublicKey)
	return pub, ok && pub.N.BitLen() == 2048
}

func str(p map[string]any, k string) string { s, _ := p[k].(string); return s }

// ServeHTTP implements http.Handler.
func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, APIPrefix)
	tok := r.Header.Get("X-Club-Auth-Token")
	var body struct {
		Parameters map[string]any `json:"parameters"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	p := body.Parameters
	if p == nil {
		p = map[string]any{}
	}
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: path, Query: r.URL.RawQuery, Token: tok, Params: p})
	for i, f := range s.fails {
		if f.method == r.Method && f.path == path {
			s.fails = append(s.fails[:i], s.fails[i+1:]...)
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
			return
		}
	}
	s.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, APIPrefix+"/") {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	type route struct{ method, path string }
	open := map[route]func(http.ResponseWriter, map[string]any){
		{http.MethodPost, "/auth/qr/start"}:     s.qrStart,
		{http.MethodPost, "/auth/qr/status"}:    s.qrStatus,
		{http.MethodPost, "/profile/send-push"}: s.sendPush,
		{http.MethodPost, "/profile/auth"}:      s.auth,
	}
	if h, ok := open[route{r.Method, path}]; ok {
		h(w, p)
		return
	}
	s.mu.Lock()
	_, club := s.tokens[tok]
	login := s.logins[tok]
	due := s.due[tok]
	s.mu.Unlock()
	if r.Method == http.MethodPost && path == "/profile/switch-club" && (club || login) {
		s.switchClub(w, tok, p)
		return
	}
	if !club {
		fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if due {
		w.Header().Set("refresh-required", "true")
	}
	q := r.URL.Query()
	switch rt := (route{r.Method, path}); rt {
	case route{http.MethodPost, "/auth/refresh"}:
		s.refresh(w, tok, p)
	case route{http.MethodGet, "/profile/current"}:
		result(w, map[string]any{"uid": "00000000-0000-4000-8000-00000000staf", "firstName": "Staff", "clubUid": s.Club.UID})
	case route{http.MethodGet, "/court/settings/load-list"}:
		s.mu.Lock()
		list := []map[string]any{}
		for _, c := range s.courts {
			list = append(list, map[string]any{"uid": c.UID, "name": c.Name, "categoryName": "Падел", "onlineBookingDisabled": false})
		}
		s.mu.Unlock()
		result(w, list)
	case route{http.MethodGet, "/club/booking/slots-load"}:
		s.slotsLoad(w, q.Get("date"))
	case route{http.MethodGet, "/club/settings/tariff/load-list"}:
		s.mu.Lock()
		list := []map[string]any{}
		ids := make([]string, 0, len(s.tariffs))
		for id := range s.tariffs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			list = append(list, map[string]any{"uid": id, "name": "Тариф " + id[:4]})
		}
		s.mu.Unlock()
		result(w, list)
	case route{http.MethodGet, "/club/settings/tariff/load"}:
		s.mu.Lock()
		t, ok := s.tariffs[q.Get("uid")]
		s.mu.Unlock()
		if !ok {
			fail(w, http.StatusNotFound, "tariff not found")
			return
		}
		result(w, t)
	case route{http.MethodGet, "/club/settings/calendar/load"}:
		y, _ := strconv.Atoi(q.Get("year"))
		s.mu.Lock()
		c, ok := s.calendar[y]
		s.mu.Unlock()
		if !ok {
			c = json.RawMessage(`{"year":` + strconv.Itoa(y) + `,"days":[]}`)
		}
		result(w, c)
	case route{http.MethodPost, "/club/visitor/list"}:
		s.visitorList(w, p)
	case route{http.MethodGet, "/club/booking/booking-load"}:
		s.bookingLoad(w, q.Get("bookingId"))
	case route{http.MethodPost, "/club/booking/booking-create"}:
		s.create(w, p)
	case route{http.MethodPost, "/club/booking/update-courts-price-adjustment"}:
		s.adjust(w, p)
	case route{http.MethodPost, "/club/booking/booking-notes-update"}:
		s.notes(w, p)
	case route{http.MethodDelete, "/club/booking/booking-cancel"}:
		s.cancel(w, p)
	case route{http.MethodPost, "/club/booking/add-ext-payment"}:
		// served, so a driver calling it would "succeed": only the call log
		// tells — which is what the tests read
		s.addPayment(w, p)
	default:
		fail(w, http.StatusNotFound, "not found")
	}
}

func (s *Stub) qrStart(w http.ResponseWriter, p map[string]any) {
	pub, ok := parseKey(p["publicKey"])
	if !ok {
		fail(w, http.StatusBadRequest, "publicKey is invalid")
		return
	}
	b := make([]byte, 90)
	_, _ = rand.Read(b)
	t := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.qr[t] = &qrLogin{pub: pub, status: "PENDING"}
	s.mu.Unlock()
	result(w, map[string]any{"token": t})
}

func (s *Stub) clubs() []Club {
	if len(s.Clubs) > 0 {
		return s.Clubs
	}
	return []Club{s.Club}
}

func (s *Stub) qrStatus(w http.ResponseWriter, p map[string]any) {
	s.mu.Lock()
	q, ok := s.qr[str(p, "token")]
	if !ok {
		s.mu.Unlock()
		fail(w, http.StatusBadRequest, "unknown token")
		return
	}
	out := map[string]any{"status": q.status, "token": []string{}, "clubs": []Club{}}
	if q.status == "SUCCESSFUL" {
		t := newToken()
		s.logins[t] = true
		out["token"], out["clubs"] = encrypt(q.pub, t), s.clubs()
		delete(s.qr, str(p, "token"))
	}
	s.mu.Unlock()
	result(w, out)
}

func (s *Stub) sendPush(w http.ResponseWriter, p map[string]any) {
	if str(p, "countryCode") == "" || str(p, "phone") == "" {
		fail(w, http.StatusBadRequest, "phone is required")
		return
	}
	t := newToken()[:40]
	s.mu.Lock()
	s.temporal[t] = str(p, "countryCode") + str(p, "phone")
	s.mu.Unlock()
	result(w, map[string]any{"temporalToken": t})
}

func (s *Stub) auth(w http.ResponseWriter, p map[string]any) {
	pub, ok := parseKey(p["publicKey"])
	s.mu.Lock()
	phone, known := s.temporal[str(p, "temporalToken")]
	if !ok || !known || phone != str(p, "countryCode")+str(p, "phone") || str(p, "code") != s.Code {
		s.mu.Unlock()
		fail(w, http.StatusBadRequest, "wrong code")
		return
	}
	delete(s.temporal, str(p, "temporalToken"))
	t := newToken()
	s.logins[t] = true
	clubs := s.clubs()
	s.mu.Unlock()
	result(w, map[string]any{"token": encrypt(pub, t), "clubs": clubs})
}

func (s *Stub) switchClub(w http.ResponseWriter, tok string, p map[string]any) {
	pub, ok := parseKey(p["publicKey"])
	if !ok {
		fail(w, http.StatusBadRequest, "publicKey is invalid")
		return
	}
	uid := str(p, "uid")
	known := false
	for _, c := range s.clubs() {
		known = known || c.UID == uid
	}
	if !known {
		fail(w, http.StatusBadRequest, "no such club")
		return
	}
	t := newToken()
	s.mu.Lock()
	delete(s.logins, tok)
	s.tokens[t] = uid
	s.mu.Unlock()
	result(w, map[string]any{"token": encrypt(pub, t)})
}

func (s *Stub) refresh(w http.ResponseWriter, tok string, p map[string]any) {
	pub, ok := parseKey(p["publicKey"])
	if !ok {
		fail(w, http.StatusBadRequest, "publicKey is invalid")
		return
	}
	s.mu.Lock()
	next := tok
	if s.due[tok] && !s.RefreshKeepsToken {
		next = newToken()
		s.tokens[next] = s.tokens[tok]
		delete(s.tokens, tok) // the old token stops working
	}
	if s.due[tok] {
		s.renewals++
	}
	delete(s.due, tok)
	s.mu.Unlock()
	w.Header().Del("refresh-required")
	result(w, map[string]any{"token": encrypt(pub, next)})
}

func (s *Stub) local(t time.Time) string { return t.In(s.Zone).Format("2006-01-02T15:04:05") }

func (s *Stub) parseLocal(v any) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02T15:04:05", fmt.Sprint(v), s.Zone)
	return t, err == nil
}

func (s *Stub) slotsLoad(w http.ResponseWriter, date string) {
	day, err := time.ParseInLocation("2006-01-02", date, s.Zone)
	if err != nil {
		fail(w, http.StatusBadRequest, "date is required")
		return
	}
	next := day.AddDate(0, 0, 1)
	n := s.now().In(s.Zone)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, s.Zone)
	s.mu.Lock()
	defer s.mu.Unlock()
	courts := []map[string]any{}
	for _, c := range s.courts {
		if s.blocked[c.UID][date] {
			continue
		}
		slots := []map[string]any{}
		for _, b := range s.bookings {
			if b.Court != c.UID || b.Status == "CANCELLED" || !b.Begin.Before(next) || b.Begin.Before(day) {
				continue
			}
			m := map[string]any{"type": "BOOKING", "bookingId": b.ID, "begin": s.local(b.Begin), "end": s.local(b.End), "goal": b.Goal,
				"payed": b.Status == "PAID", "cancellationAvailable": true, "prebooking": false, "clientUid": b.Customer,
				"managerUid": "00000000-0000-4000-8000-0000000000aa", "notesAbstract": b.Notes}
			for k, v := range b.Extra {
				if _, set := m[k]; !set {
					m[k] = v
				}
			}
			slots = append(slots, m)
		}
		for _, sl := range s.slots {
			if sl.court != c.UID || !sl.begin.Before(next) || sl.begin.Before(day) {
				continue
			}
			m := map[string]any{"type": sl.typ, "begin": s.local(sl.begin), "end": s.local(sl.end)}
			for k, v := range sl.extra {
				m[k] = v
			}
			slots = append(slots, m)
		}
		courts = append(courts, map[string]any{"courtUid": c.UID, "courtName": c.Name, "categoryName": "Падел", "slots": slots})
	}
	result(w, map[string]any{"timeFrameSize": 30, "beyondBookingDepth": day.After(today.AddDate(0, 0, s.BookingDepth)),
		"availableHours": s.AvailableHours, "courts": courts})
}

func (s *Stub) visitorList(w http.ResponseWriter, p map[string]any) {
	pat := strings.ToLower(str(p, "freeTextPattern"))
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []map[string]any{}
	for _, v := range s.visitors {
		if strings.Contains(strings.ToLower(fmt.Sprint(v["firstName"], " ", v["lastName"], " ", v["phone"])), pat) {
			items = append(items, v)
		}
	}
	result(w, map[string]any{"items": items, "total": len(items)})
}

// bookingJSON is a booking as booking-load answers it (with the
// customer's data, which the driver must never pass on).
func (s *Stub) bookingJSON(b *Booking) map[string]any {
	m := map[string]any{"id": b.ID, "bookingType": "COURT", "placeholder": false, "price": b.Price + b.Adjustment, "status": b.Status,
		"datasource": "CLUB_APP", "goal": b.Goal, "customerUid": b.Customer, "payments": b.Payments, "financialItems": []any{},
		"cancellationDeadline": s.local(b.Begin.Add(-24 * time.Hour)),
		"slots":                []map[string]any{{"courtUid": b.Court, "begin": s.local(b.Begin), "end": s.local(b.End), "pinned": false}}}
	if !s.OmitNotesOnLoad {
		m["notes"] = b.Notes
	}
	for k, v := range b.Extra {
		if _, set := m[k]; !set {
			m[k] = v
		}
	}
	return m
}

func (s *Stub) bookingLoad(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bookings[id]
	if !ok {
		fail(w, http.StatusNotFound, "booking not found")
		return
	}
	result(w, s.bookingJSON(b))
}

func (s *Stub) create(w http.ResponseWriter, p map[string]any) {
	if str(p, "customerUid") == "" {
		fail(w, http.StatusBadRequest, "customerUid is required")
		return
	}
	slots, _ := p["slots"].([]any)
	if len(slots) != 1 {
		fail(w, http.StatusBadRequest, "one slot is expected")
		return
	}
	sl, _ := slots[0].(map[string]any)
	begin, ok1 := s.parseLocal(sl["begin"])
	end, ok2 := s.parseLocal(sl["end"])
	court := str(sl, "courtUid")
	s.mu.Lock()
	defer s.mu.Unlock()
	known := false
	for _, c := range s.courts {
		known = known || c.UID == court
	}
	if !ok1 || !ok2 || !end.After(begin) || !known {
		fail(w, http.StatusBadRequest, "invalid slot")
		return
	}
	for _, b := range s.bookings {
		if b.Court == court && b.Status != "CANCELLED" && b.Begin.Before(end) && b.End.After(begin) {
			fail(w, http.StatusConflict, "the slot is occupied")
			return
		}
	}
	for _, o := range s.slots {
		if o.court == court && o.begin.Before(end) && o.end.After(begin) {
			fail(w, http.StatusConflict, "the slot is occupied")
			return
		}
	}
	price := math.Round(end.Sub(begin).Hours()*s.PricePerHour*100) / 100
	goal := str(p, "goal")
	b := &Booking{ID: NewID(), Court: court, Begin: begin, End: end, Customer: str(p, "customerUid"), Goal: goal, Price: price,
		Status: "NOT_PAID", Payments: []any{}}
	if price == 0 {
		b.Status = "PAID"
	}
	s.bookings[b.ID] = b
	result(w, s.bookingJSON(b))
}

func (s *Stub) adjust(w http.ResponseWriter, p map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bookings[str(p, "bookingId")]
	if !ok {
		fail(w, http.StatusNotFound, "booking not found")
		return
	}
	adj, ok := p["priceAdjustment"].(float64)
	if !ok || b.Price+adj < 0 {
		fail(w, http.StatusBadRequest, "invalid priceAdjustment")
		return
	}
	b.Adjustment = adj
	if b.Price+adj == 0 {
		b.Status = "PAID"
	}
	result(w, s.bookingJSON(b))
}

func (s *Stub) notes(w http.ResponseWriter, p map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bookings[str(p, "bookingId")]
	if !ok {
		fail(w, http.StatusNotFound, "booking not found")
		return
	}
	b.Notes = str(p, "notes")
	result(w, s.bookingJSON(b))
}

func (s *Stub) cancel(w http.ResponseWriter, p map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bookings[str(p, "bookingId")]
	if !ok {
		fail(w, http.StatusNotFound, "booking not found")
		return
	}
	if str(p, "bookingType") != "COURT" {
		fail(w, http.StatusBadRequest, "bookingType is required")
		return
	}
	b.Status = "CANCELLED"
	result(w, s.bookingJSON(b))
}

func (s *Stub) addPayment(w http.ResponseWriter, p map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bookings[str(p, "bookingId")]
	if !ok {
		fail(w, http.StatusNotFound, "booking not found")
		return
	}
	b.Payments = append(b.Payments, map[string]any{"type": "OFFLINE_PAYMENT", "amount": p["amount"]})
	b.Status = "PAID"
	result(w, s.bookingJSON(b))
}
