// Package lundastub is a fake Lunda club admin API for tests: the login
// (QR and phone + code, with the RSA exchange: every token is answered
// encrypted in chunks with the client's public key), the club switch,
// renewal on refresh-required, the schedule, tariffs and the calendar,
// and bookings (create, price adjustment, notes, load, cancel), as
// observed under a staff login on 2026-10-17. Answers wrap their data in
// {"result": ...}; requests carry {"parameters": {...}}.
//
// Serve it with httptest.NewServer(stub); the driver's api_url is then
// <server>/api, and the admin's WebSocket is at <server>/api-ws/
// websocket-listen. Every request is logged (Calls), whatever its path, so
// a test can assert that nothing outside the driver's operations was
// called; a WebSocket opening is logged with method "WS".
//
// As Lunda (seen 2026-10-17): a session's token is bound to the
// User-Agent of its login (another one: 401 "missing auth token"), and a
// club session that registers no CLUB_BOOKING ticket within
// SecurityWindow of its switch-club is refused from then on (403 "missing
// security details").
package lundastub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
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
	Method    string // "WS" for a WebSocket opening
	Path      string // below APIPrefix, e.g. "/club/booking/booking-create"; WSPath for the WebSocket
	Query     string
	Token     string // X-Club-Auth-Token, "" when none
	UserAgent string
	Origin    string
	Params    map[string]any
}

// WSPath is where the admin's WebSocket is served.
const WSPath = "/api-ws/websocket-listen"

// session is a club session (or a login before its club is chosen).
type session struct {
	club    string    // "" for a login before switch-club
	ua      string    // the User-Agent it is bound to ("": bound at first use)
	issued  time.Time // real time, for SecurityWindow
	secured bool      // a ticket was registered with it
}

// TariffInfo is one price of a tariff version: a category of day, an hour
// of the day, a price in rubles.
type TariffInfo struct {
	Category string  `json:"tariffCategory"`
	Hour     int     `json:"hour"`
	Price    float64 `json:"price"`
}

type qrLogin struct {
	pub     *rsa.PublicKey
	status  string
	session string // the X_SESSION_ID cookie qr/start set
	ua      string // the User-Agent of qr/start: the login is bound to it
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
	query        string // "" matches any
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
	// Category is the court category of the courts AddCourt adds.
	Category string
	// TimeZoneID is the club's zone in club/settings ("": none).
	TimeZoneID string
	// SecurityWindow (real time) is how long a club session lives without
	// a registered ticket; 0: forever.
	SecurityWindow time.Duration
	// WSPing is how often the WebSocket pings (default 50 ms).
	WSPing time.Duration
	// RefuseWS answers a WebSocket opening 403.
	RefuseWS bool

	mu       sync.Mutex
	courts   []court
	blocked  map[string]map[string]bool
	slots    []slot
	bookings map[string]*Booking
	qr       map[string]*qrLogin
	temporal map[string]string                  // temporal token -> phone
	tokens   map[string]*session                // live tokens, logins and club sessions
	due      map[string]bool                    // club tokens whose answers carry refresh-required
	tariffs  map[string]map[string][]TariffInfo // category -> begin date -> prices
	calendar map[int]map[string][]string        // year -> category -> dates
	visitors []map[string]any
	fails    []failure
	calls    []Call
	renewals int
	tickets  map[string]bool // registered, not used yet
	issued   []string        // every ticket registered
	wsOpen   int             // WebSockets open now
	wsOpened int             // WebSockets opened in all
	wsPongs  int
	wsConns  []net.Conn
}

type court struct {
	uid, name, category string
	number              int
}

// New returns a stub of club.
func New(club Club) *Stub {
	return &Stub{
		Club: club, Code: "246810", BookingDepth: 14, AvailableHours: []int{7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22},
		PricePerHour: 1500, Zone: time.FixedZone("MSK", 3*3600),
		blocked: map[string]map[string]bool{}, bookings: map[string]*Booking{}, qr: map[string]*qrLogin{},
		temporal: map[string]string{}, tokens: map[string]*session{}, due: map[string]bool{},
		tariffs: map[string]map[string][]TariffInfo{}, calendar: map[int]map[string][]string{},
		Category: "00000000-0000-4000-8000-00000000ca71", TimeZoneID: "Europe/Moscow", SecurityWindow: 5 * time.Second,
		tickets: map[string]bool{},
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

// AddCourt adds a court of Category to the club, numbered in order.
func (s *Stub) AddCourt(uid, name string) { s.AddCourtIn(uid, name, s.Category) }

// AddCourtIn adds a court of category to the club, numbered in order.
func (s *Stub) AddCourtIn(uid, name, category string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.courts = append(s.courts, court{uid: uid, name: name, category: category, number: len(s.courts) + 1})
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

// EditBooking changes a booking as the club's staff would in Lunda.
func (s *Stub) EditBooking(id string, edit func(*Booking)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.bookings[id]; ok {
		edit(b)
	}
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

// SetTariff stores a version of the tariff of a court category, priced
// from begin (2006-01-02) on.
func (s *Stub) SetTariff(category, begin string, prices []TariffInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tariffs[category] == nil {
		s.tariffs[category] = map[string][]TariffInfo{}
	}
	s.tariffs[category][begin] = prices
}

// SetCalendar stores the calendar of a year: category -> dates.
func (s *Stub) SetCalendar(year int, dates map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calendar[year] = dates
}

// AddVisitor adds a client of the club (profileUid, name, phone, ...), as
// the visitor list answers it.
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
	s.tokens[t] = &session{club: s.Club.UID, issued: time.Now()}
	return t
}

// Tickets returns every ticket registered.
func (s *Stub) Tickets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.issued...)
}

// WebSockets returns how many WebSockets are open now, were opened in all,
// and how many pongs answered the stub's pings.
func (s *Stub) WebSockets() (open, opened, pongs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wsOpen, s.wsOpened, s.wsPongs
}

// DropWebSockets closes every open WebSocket, as a restart of Lunda would.
func (s *Stub) DropWebSockets() {
	s.mu.Lock()
	conns := s.wsConns
	s.wsConns = nil
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// RequireRefresh makes every answer to a live session carry
// refresh-required: true until it is renewed.
func (s *Stub) RequireRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, ss := range s.tokens {
		if ss.club != "" {
			s.due[t] = true
		}
	}
}

// Revoke ends every session: 401 from then on.
func (s *Stub) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens, s.due = map[string]*session{}, map[string]bool{}
}

// Sessions returns the live club tokens.
func (s *Stub) Sessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for t, ss := range s.tokens {
		if ss.club != "" {
			out = append(out, t)
		}
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

// FailNextQuery is FailNext for a request whose raw query is query (e.g.
// "date=2026-10-20").
func (s *Stub) FailNextQuery(method, path, query string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = append(s.fails, failure{method: method, path: path, query: query, status: status, body: body})
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
	writeJSON(w, status, map[string]any{"developerErrorMessage": msg, "errorMessageType": "ERROR", "errorType": "BAD_REQUEST", "userErrorMessage": msg})
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
	if r.URL.Path == WSPath {
		s.serveWS(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, APIPrefix)
	tok := r.Header.Get("X-Club-Auth-Token")
	ua := r.Header.Get("User-Agent")
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
	s.calls = append(s.calls, Call{Method: r.Method, Path: path, Query: r.URL.RawQuery, Token: tok, UserAgent: ua, Origin: r.Header.Get("Origin"), Params: p})
	for i, f := range s.fails {
		if f.method == r.Method && f.path == path && (f.query == "" || f.query == r.URL.RawQuery) {
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
	open := map[route]func(http.ResponseWriter, *http.Request, map[string]any){
		{http.MethodPost, "/auth/qr/start"}:     s.qrStart,
		{http.MethodPost, "/auth/qr/status"}:    s.qrStatus,
		{http.MethodPost, "/profile/send-push"}: s.sendPush,
		{http.MethodPost, "/profile/auth"}:      s.auth,
	}
	if h, ok := open[route{r.Method, path}]; ok {
		h(w, r, p)
		return
	}
	s.mu.Lock()
	ss, known := s.tokens[tok]
	if known && ss.ua == "" {
		ss.ua = ua // a session issued without a login: bound at first use
	}
	switch {
	case !known || ss.ua != ua:
		s.mu.Unlock()
		fail(w, http.StatusUnauthorized, "missing auth token")
		return
	case ss.club != "" && !ss.secured && s.SecurityWindow > 0 && time.Since(ss.issued) > s.SecurityWindow:
		s.mu.Unlock()
		fail(w, http.StatusForbidden, "missing security details: "+NewID())
		return
	}
	club := ss.club != ""
	due := s.due[tok]
	s.mu.Unlock()
	if r.Method == http.MethodPost && path == "/profile/switch-club" {
		s.switchClub(w, r, tok, p)
		return
	}
	if !club {
		fail(w, http.StatusUnauthorized, "missing auth token")
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
		infos := []map[string]any{}
		for _, c := range s.courts {
			_, priced := s.tariffs[c.category]
			infos = append(infos, map[string]any{"active": true, "courtUid": c.uid, "title": c.name, "number": c.number, "description": "",
				"categoryInfo": map[string]any{"active": true, "clubUid": s.Club.UID, "courtNumbers": []int{c.number}, "hasActiveTariff": priced,
					"id": c.category, "name": "Падел", "sportType": "PADEL"}})
		}
		s.mu.Unlock()
		result(w, map[string]any{"closureTypes": map[string]string{"INDOOR": "Крытый", "OUTDOOR": "Открытый", "OUTDOOR_ROOFED": "Под крышей"}, "infos": infos})
	case route{http.MethodGet, "/club/settings"}:
		result(w, map[string]any{"clubUid": s.Club.UID, "name": s.Club.Name, "timeZoneId": s.TimeZoneID, "phone": "+70000000000",
			"bookingSettings": map[string]any{"bookingDepth": s.BookingDepth, "cancellationDeadline": 24, "minDuration": 60, "maxDuration": 180},
			"address":         map[string]any{"city": map[string]any{"caption": "Город", "code": "city", "type": "CITY"}}})
	case route{http.MethodGet, "/club/websocket/ticket/register"}:
		if q.Get("ticketType") != "CLUB_BOOKING" || !q.Has("relatedEntityUid") {
			fail(w, http.StatusBadRequest, "ticketType is required")
			return
		}
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		t := hex.EncodeToString(b)
		s.mu.Lock()
		ss.secured = true
		s.tickets[t] = true
		s.issued = append(s.issued, t)
		s.mu.Unlock()
		result(w, map[string]any{"ticket": t, "expiredDate": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
	case route{http.MethodGet, "/club/booking/slots-load"}:
		s.slotsLoad(w, q.Get("date"))
	case route{http.MethodGet, "/club/settings/tariff/load-list"}:
		if q.Get("courtCategoryId") == "" {
			fail(w, http.StatusBadRequest, "court category id is required")
			return
		}
		s.mu.Lock()
		infos := []map[string]any{}
		var begins []string
		for b := range s.tariffs[q.Get("courtCategoryId")] {
			begins = append(begins, b)
		}
		sort.Strings(begins)
		for _, b := range begins {
			infos = append(infos, map[string]any{"beginDate": b})
		}
		s.mu.Unlock()
		result(w, map[string]any{"infos": infos})
	case route{http.MethodGet, "/club/settings/tariff/load"}:
		s.mu.Lock()
		prices, ok := s.tariffs[q.Get("courtCategoryId")][q.Get("beginDate")]
		s.mu.Unlock()
		if !ok {
			fail(w, http.StatusNotFound, "tariff not found")
			return
		}
		result(w, map[string]any{"view": map[string]any{"uid": NewID(), "courtCategoryId": q.Get("courtCategoryId"), "beginDate": q.Get("beginDate"),
			"infos": prices, "modifications": []any{}}})
	case route{http.MethodGet, "/club/settings/calendar/load"}:
		y, err := strconv.Atoi(q.Get("year"))
		if err != nil {
			fail(w, http.StatusBadRequest, "year is required")
			return
		}
		s.mu.Lock()
		cal := map[string]any{}
		captions := map[string]string{}
		for _, cat := range []string{"HOLIDAY", "PRE_HOLIDAY", "PRE_WEEKEND", "REGULAR", "SPECIAL", "WEEKEND"} {
			dates := append([]string{}, s.calendar[y][cat]...)
			cal[cat] = map[string]any{"dates": dates}
			captions[cat] = strings.ToLower(cat)
		}
		s.mu.Unlock()
		result(w, map[string]any{"clubUid": s.Club.UID, "tariffCategories": captions, "view": map[string]any{"tariffCalendar": cal}})
	case route{http.MethodPost, "/club/visitor/list"}:
		s.visitorList(w, p)
	case route{http.MethodGet, "/club/booking/booking-load"}:
		if q.Get("bookingType") != "COURT" { // as Lunda: the type is required
			fail(w, http.StatusBadRequest, "bookingType is required")
			return
		}
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

func (s *Stub) qrStart(w http.ResponseWriter, r *http.Request, p map[string]any) {
	pub, ok := parseKey(p["publicKey"])
	if !ok {
		fail(w, http.StatusBadRequest, "publicKey is invalid")
		return
	}
	b := make([]byte, 90)
	_, _ = rand.Read(b)
	t := base64.RawURLEncoding.EncodeToString(b)
	sid := newToken()
	s.mu.Lock()
	s.qr[t] = &qrLogin{pub: pub, status: "PENDING", session: sid, ua: r.Header.Get("User-Agent")}
	s.mu.Unlock()
	// as Lunda: the login is tied to this cookie; qr/status without it
	// answers EXPIRED
	http.SetCookie(w, &http.Cookie{Name: "X_SESSION_ID", Value: sid, Path: "/", MaxAge: 60, Secure: true, HttpOnly: true})
	result(w, map[string]any{"token": t})
}

func (s *Stub) clubs() []Club {
	if len(s.Clubs) > 0 {
		return s.Clubs
	}
	return []Club{s.Club}
}

func (s *Stub) qrStatus(w http.ResponseWriter, r *http.Request, p map[string]any) {
	s.mu.Lock()
	q, ok := s.qr[str(p, "token")]
	if !ok {
		s.mu.Unlock()
		fail(w, http.StatusBadRequest, "unknown token")
		return
	}
	if c, err := r.Cookie("X_SESSION_ID"); err != nil || c.Value != q.session {
		s.mu.Unlock()
		result(w, map[string]any{"status": "EXPIRED", "token": []string{}, "clubs": []Club{}})
		return
	}
	out := map[string]any{"status": q.status, "token": []string{}, "clubs": []Club{}}
	if q.status == "SUCCESSFUL" {
		t := newToken()
		s.tokens[t] = &session{ua: q.ua, issued: time.Now()}
		out["token"], out["clubs"] = encrypt(q.pub, t), s.clubs()
		delete(s.qr, str(p, "token"))
	}
	s.mu.Unlock()
	result(w, out)
}

func (s *Stub) sendPush(w http.ResponseWriter, _ *http.Request, p map[string]any) {
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

func (s *Stub) auth(w http.ResponseWriter, r *http.Request, p map[string]any) {
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
	s.tokens[t] = &session{ua: r.Header.Get("User-Agent"), issued: time.Now()}
	clubs := s.clubs()
	s.mu.Unlock()
	result(w, map[string]any{"token": encrypt(pub, t), "clubs": clubs})
}

func (s *Stub) switchClub(w http.ResponseWriter, r *http.Request, tok string, p map[string]any) {
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
	if s.tokens[tok].club == "" {
		delete(s.tokens, tok) // a login is used once
	}
	s.tokens[t] = &session{club: uid, ua: r.Header.Get("User-Agent"), issued: time.Now()}
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
		renewed := *s.tokens[tok]
		s.tokens[next] = &renewed
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
		if s.blocked[c.uid][date] {
			continue
		}
		slots := []map[string]any{}
		for _, b := range s.bookings {
			if b.Court != c.uid || b.Status == "CANCELLED" || !b.Begin.Before(next) || b.Begin.Before(day) {
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
			if sl.court != c.uid || !sl.begin.Before(next) || sl.begin.Before(day) {
				continue
			}
			m := map[string]any{"type": sl.typ, "begin": s.local(sl.begin), "end": s.local(sl.end)}
			for k, v := range sl.extra {
				m[k] = v
			}
			slots = append(slots, m)
		}
		courts = append(courts, map[string]any{"courtUid": c.uid, "courtName": c.name, "categoryName": "Падел", "slots": slots})
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
		if strings.Contains(strings.ToLower(fmt.Sprint(v["name"], " ", v["phone"])), pat) {
			item := map[string]any{"banned": false, "category": "STANDARD", "tags": []any{}}
			for k, val := range v {
				item[k] = val
			}
			items = append(items, item)
		}
	}
	result(w, map[string]any{"items": items, "totalCount": len(items)})
}

// bookingJSON is a booking as booking-load answers it (with the
// customer's data, which the driver must never pass on).
func (s *Stub) bookingJSON(b *Booking) map[string]any {
	// as Lunda: no top-level price — each court of the booking carries its
	// own (rubles), and a price adjustment is a further entry of its own,
	// the "Court discount", the court's price left as it was. [UNVERIFIED]
	// the discount entry's fields beyond its title and its negative price.
	courts := []map[string]any{{"courtUid": b.Court, "price": b.Price}}
	if b.Adjustment != 0 {
		courts = append(courts, map[string]any{"title": "Court discount", "price": b.Adjustment})
	}
	m := map[string]any{"id": b.ID, "bookingType": "COURT", "placeholder": false, "status": b.Status, "courts": courts,
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
		known = known || c.uid == court
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
	if str(p, "bookingType") != "COURT" { // as Lunda: the type is required
		fail(w, http.StatusBadRequest, "bookingType is required")
		return
	}
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

// serveWS is the admin's WebSocket: a registered, unused ticket opens it
// (101); the stub pings every WSPing and counts the pongs; frames of the
// client are read and dropped.
func (s *Stub) serveWS(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("ticket")
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: "WS", Path: WSPath, UserAgent: r.Header.Get("User-Agent"), Origin: r.Header.Get("Origin")})
	valid := s.tickets[t]
	delete(s.tickets, t)
	refuse := s.RefuseWS
	s.mu.Unlock()
	key := r.Header.Get("Sec-WebSocket-Key")
	if refuse || !valid || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		fail(w, http.StatusForbidden, "invalid ticket")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		fail(w, http.StatusInternalServerError, "no hijack")
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
	_ = rw.Flush()
	s.mu.Lock()
	s.wsOpen++
	s.wsOpened++
	s.wsConns = append(s.wsConns, conn)
	ping := s.WSPing
	s.mu.Unlock()
	if ping <= 0 {
		ping = 50 * time.Millisecond
	}
	done := make(chan struct{})
	var wmu sync.Mutex
	send := func(op byte, payload []byte) error { // a server's frames are not masked
		wmu.Lock()
		defer wmu.Unlock()
		_, err := conn.Write(append([]byte{0x80 | op, byte(len(payload))}, payload...))
		return err
	}
	go func() {
		t := time.NewTicker(ping)
		defer t.Stop()
		_ = send(0x1, []byte(`{"type":"HELLO"}`))
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if send(0x9, []byte("ping")) != nil {
					return
				}
			}
		}
	}()
	defer func() {
		close(done)
		conn.Close()
		s.mu.Lock()
		s.wsOpen--
		s.mu.Unlock()
	}()
	br := rw.Reader
	for {
		var h [2]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return
		}
		if h[1]&0x80 == 0 { // a client's frame must be masked
			return
		}
		n := int(h[1] & 0x7f)
		if n > 125 {
			return // the client sends control frames only
		}
		buf := make([]byte, 4+n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		switch h[0] & 0x0f {
		case 0xA:
			s.mu.Lock()
			s.wsPongs++
			s.mu.Unlock()
		case 0x8:
			_ = send(0x8, nil)
			return
		}
	}
}
