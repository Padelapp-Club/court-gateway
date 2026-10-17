// Package yclientsstub is an in-memory YCLIENTS for tests: the calls the
// yclients driver makes, answered the way the D11 probes of a live branch
// (2270517, October 2026) saw YCLIENTS answer — the envelope, the paging of
// records, schedules with bare local times, 422 on a busy time, deleted
// records still readable, masked client phones. What the probes did not
// see is marked [ASSUMPTION] where the stub decides it.
package yclientsstub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Service is one service of a court's catalog. From/To, when To > 0, are
// the minutes of the local day the service is in effect (book_services
// with datetime lists only those in effect then).
type Service struct {
	ID           int64
	Title        string
	SeanceLength int // seconds
	Price        float64
	From, To     int
}

// Staff is a court of the branch, with the D11 restriction fields.
type Staff struct {
	ID           string
	Name         string
	Bookable     bool
	Fired        bool
	Hidden       bool
	Prepaid      string
	ScheduleTill string // "" = a year ahead
	Services     []Service
}

// Day is one day of a court's schedule.
type Day struct {
	Working bool
	Slots   [][2]string // "07:00", "00:00"
}

// Record is a record of the journal.
type Record struct {
	ID          int64
	StaffID     string
	Start       time.Time
	Length      int // seconds
	Deleted     bool
	Comment     string
	ClientName  string
	ClientPhone string // digits
	ClientEmail string
	Services    []map[string]any
	Attendance  int
	Extra       map[string]any // more fields of a club's record (personal data)
}

// Write is one write request the stub took, for differential tests.
type Write struct {
	Method string
	Path   string
	Body   json.RawMessage
}

// Stub is the in-memory YCLIENTS. Lock-free fields are set before use.
type Stub struct {
	Partner   string
	UserToken string // a static user token; "" = only logins
	Login     string
	Password  string
	Company   string
	Title     string
	Zone      *time.Location
	TZName    string // timezone_name of the card; "" = Zone's name

	// Card restrictions (D11): Active false = inactive, TimetableOff.
	Inactive     bool
	TimetableOff bool

	mu        sync.Mutex
	staff     map[string]*Staff
	order     []string
	schedule  map[string]map[string]Day // staff -> date -> day; absent = DefaultDay
	records   map[int64]*Record
	nextID    int64
	tokens    map[string]bool // issued by /auth
	logins    int
	writes    []Write
	failNext  map[string]int             // path prefix -> status, once
	failMsg   map[string]string          // path prefix -> meta.message of that failure
	bookFail  int                        // book_record makes the records, then answers this
	bookRaw   string                     // book_record makes the records, then answers this body (201)
	dupIDs    bool                       // book_record names its first record for every record
	copyAfter *clubCopy                  // book_record: the club copies the first record at once
	shortIDs  bool                       // book_record names one record fewer
	pageSize  int                        // 0 = as asked
	totalLies int                        // added to total_count
	listDel   bool                       // records lists include deleted records
	omitDays  map[string]map[string]bool // staff -> date: left out of the schedule answer
	// DefaultDay is a court's day without an explicit schedule.
	DefaultDay Day
}

// New builds a branch. It accepts the partner token, and either the user
// token or the login and password.
func New(partner, userToken, login, password, company string, zone *time.Location) *Stub {
	return &Stub{
		Partner: partner, UserToken: userToken, Login: login, Password: password, Company: company, Title: "Тестовый филиал", Zone: zone,
		staff: map[string]*Staff{}, schedule: map[string]map[string]Day{}, records: map[int64]*Record{},
		nextID: 2017300000, tokens: map[string]bool{}, failNext: map[string]int{}, failMsg: map[string]string{},
		DefaultDay: Day{Working: true, Slots: [][2]string{{"07:00", "00:00"}}},
	}
}

// AddStaff adds a court.
func (s *Stub) AddStaff(st Staff) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := st
	s.staff[st.ID] = &cp
	s.order = append(s.order, st.ID)
}

// SetDay sets a court's schedule on a local date "2006-01-02".
func (s *Stub) SetDay(staff, date string, d Day) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schedule[staff] == nil {
		s.schedule[staff] = map[string]Day{}
	}
	s.schedule[staff][date] = d
}

// Seed adds a record as the club would make it and returns its id.
func (s *Stub) Seed(r Record) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	r.ID = s.nextID
	cp := r
	s.records[r.ID] = &cp
	return r.ID
}

// Record returns a copy of a record.
func (s *Stub) Record(id int64) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return Record{}, false
	}
	return *r, true
}

// Writes returns the write requests taken so far.
func (s *Stub) Writes() []Write {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Write(nil), s.writes...)
}

// Logins counts POST /auth calls.
func (s *Stub) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

// FailNext answers the next request whose path starts with prefix with
// status (an error envelope).
func (s *Stub) FailNext(prefix string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[prefix] = status
}

// FailNextWith is FailNext with meta.message text of the caller's.
func (s *Stub) FailNextWith(prefix string, status int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[prefix] = status
	s.failMsg[prefix] = message
}

// BookThenFail makes book_record create its records and answer status
// (0: answer as usual), as a 5xx after the write would.
func (s *Stub) BookThenFail(status int) { s.mu.Lock(); s.bookFail = status; s.mu.Unlock() }

type clubCopy struct {
	offset time.Duration
	phone  string
}

// BookThenRaw makes book_record create its records and answer 201 with
// body as it is ("" = as usual).
func (s *Stub) BookThenRaw(body string) { s.mu.Lock(); s.bookRaw = body; s.mu.Unlock() }

// DupIDs makes book_record name its first record for every record.
func (s *Stub) DupIDs(on bool) { s.mu.Lock(); s.dupIDs = on; s.mu.Unlock() }

// CopyAfterBook makes the club copy the first record book_record creates,
// before its answer: the same court and comment, offset later, for the
// client phone (digits).
func (s *Stub) CopyAfterBook(offset time.Duration, phone string) {
	s.mu.Lock()
	s.copyAfter = &clubCopy{offset, phone}
	s.mu.Unlock()
}

// SetServices replaces a court's catalog.
func (s *Stub) SetServices(staff string, svc []Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staff[staff].Services = svc
}

// Delete deletes a record as the club would in the journal.
func (s *Stub) Delete(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.records[id]; r != nil {
		r.Deleted = true
	}
}

// ShortIDs makes book_record name one record fewer than it created.
func (s *Stub) ShortIDs(on bool) { s.mu.Lock(); s.shortIDs = on; s.mu.Unlock() }

// PageSize caps every records page (to test paging).
func (s *Stub) PageSize(n int) { s.mu.Lock(); s.pageSize = n; s.mu.Unlock() }

// LieTotal adds n to every total_count (a promise the pages do not keep).
func (s *Stub) LieTotal(n int) { s.mu.Lock(); s.totalLies = n; s.mu.Unlock() }

// ListDeleted makes records lists include deleted records (deleted: true),
// as a live YCLIENTS may.
func (s *Stub) ListDeleted(on bool) { s.mu.Lock(); s.listDel = on; s.mu.Unlock() }

// OmitDay leaves a court's local date "2006-01-02" out of schedule answers.
func (s *Stub) OmitDay(staff, date string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.omitDays == nil {
		s.omitDays = map[string]map[string]bool{}
	}
	if s.omitDays[staff] == nil {
		s.omitDays[staff] = map[string]bool{}
	}
	s.omitDays[staff][date] = true
}

// RevokeTokens forgets the user tokens logins issued.
func (s *Stub) RevokeTokens() { s.mu.Lock(); s.tokens = map[string]bool{}; s.mu.Unlock() }

func writeJSON(w http.ResponseWriter, status int, success bool, data, meta any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if meta == nil {
		meta = []any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": success, "data": data, "meta": meta})
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, false, nil, map[string]any{"message": msg})
}

var (
	reCompany   = regexp.MustCompile(`^/api/v1/company/(\d+)/$`)
	reStaff     = regexp.MustCompile(`^/api/v1/company/(\d+)/staff$`)
	reRecords   = regexp.MustCompile(`^/api/v1/records/(\d+)$`)
	reSchedule  = regexp.MustCompile(`^/api/v1/schedule/(\d+)/(\d+)/(\d{4}-\d{2}-\d{2})/(\d{4}-\d{2}-\d{2})$`)
	reServices  = regexp.MustCompile(`^/api/v1/book_services/(\d+)$`)
	reBook      = regexp.MustCompile(`^/api/v1/book_record/(\d+)$`)
	reRecord    = regexp.MustCompile(`^/api/v1/record/(\d+)/(\d+)$`)
	reAuthUser  = regexp.MustCompile(`^Bearer ([^,\s]+), User (\S+)$`)
	reAuthBoth  = regexp.MustCompile(`^Bearer ([^,\s]+)(, User (\S+))?$`)
	timeFormat  = "2006-01-02T15:04:05Z07:00"
	staticToken = "stub-issued-"
)

// auth checks the Authorization header: the partner token always, a valid
// user token when user is set. It answers 401 itself.
func (s *Stub) auth(w http.ResponseWriter, r *http.Request, user bool) bool {
	h := r.Header.Get("Authorization")
	if r.Header.Get("Accept") != "application/vnd.yclients.v2+json" {
		fail(w, http.StatusBadRequest, "Accept")
		return false
	}
	m := reAuthBoth.FindStringSubmatch(h)
	if m == nil || m[1] != s.Partner {
		fail(w, http.StatusUnauthorized, "Партнёр не авторизован")
		return false
	}
	if !user {
		return true
	}
	u := reAuthUser.FindStringSubmatch(h)
	if u == nil || !(u[2] == s.UserToken && s.UserToken != "" || s.tokens[u[2]]) {
		fail(w, http.StatusUnauthorized, "Пользователь не авторизован")
		return false
	}
	return true
}

func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	for p, st := range s.failNext {
		if strings.HasPrefix(path, p) {
			delete(s.failNext, p)
			msg := "injected failure"
			if m, ok := s.failMsg[p]; ok {
				msg = m
				delete(s.failMsg, p)
			}
			fail(w, st, msg)
			return
		}
	}
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodGet && path != "/api/v1/auth" {
		s.writes = append(s.writes, Write{Method: r.Method, Path: strings.TrimPrefix(path, "/api/v1"), Body: json.RawMessage(append([]byte(nil), body...))})
	}
	company := func(id string) bool {
		if id != s.Company {
			fail(w, http.StatusNotFound, "Филиал не найден")
			return false
		}
		return true
	}
	switch {
	case path == "/api/v1/auth" && r.Method == http.MethodPost:
		if !s.auth(w, r, false) {
			return
		}
		s.logins++
		var in struct{ Login, Password string }
		_ = json.Unmarshal(body, &in)
		if s.Login == "" || in.Login != s.Login || in.Password != s.Password {
			fail(w, http.StatusUnauthorized, "Неверный логин или пароль")
			return
		}
		tok := fmt.Sprintf("%s%d", staticToken, s.logins)
		s.tokens[tok] = true
		writeJSON(w, http.StatusCreated, true, map[string]any{"id": 1, "user_token": tok, "name": "Сотрудник"}, nil)
	case path == "/api/v1/companies" && r.Method == http.MethodGet:
		if !s.auth(w, r, true) {
			return
		}
		writeJSON(w, http.StatusOK, true, []any{map[string]any{"id": json.Number(s.Company), "title": s.Title}}, map[string]any{"total_count": 1})
	case reCompany.MatchString(path) && r.Method == http.MethodGet:
		if !s.auth(w, r, false) || !company(reCompany.FindStringSubmatch(path)[1]) {
			return
		}
		active := 1
		if s.Inactive {
			active = 0
		}
		tz := s.TZName
		if tz == "" {
			tz = s.Zone.String()
		}
		writeJSON(w, http.StatusOK, true, map[string]any{"id": json.Number(s.Company), "title": s.Title, "active": active,
			"timetable_off": s.TimetableOff, "timezone_name": tz, "allow_change_prepaid_record": false, "allow_delete_prepaid_record": false}, nil)
	case reStaff.MatchString(path) && r.Method == http.MethodGet:
		if !s.auth(w, r, true) || !company(reStaff.FindStringSubmatch(path)[1]) {
			return
		}
		var out []any
		for _, id := range s.order {
			st := s.staff[id]
			till := st.ScheduleTill
			if till == "" {
				till = time.Now().In(s.Zone).AddDate(1, 0, 0).Format("2006-01-02")
			}
			hidden := 0
			if st.Hidden {
				hidden = 1
			}
			out = append(out, map[string]any{"id": json.Number(st.ID), "name": st.Name, "is_bookable": st.Bookable, "is_fired": st.Fired,
				"is_deleted": false, "hidden": hidden, "prepaid": st.Prepaid, "schedule_till": till})
		}
		writeJSON(w, http.StatusOK, true, out, nil)
	case reRecords.MatchString(path) && r.Method == http.MethodGet:
		if !s.auth(w, r, true) || !company(reRecords.FindStringSubmatch(path)[1]) {
			return
		}
		s.listRecords(w, r)
	case reSchedule.MatchString(path) && r.Method == http.MethodGet:
		m := reSchedule.FindStringSubmatch(path)
		if !s.auth(w, r, true) || !company(m[1]) {
			return
		}
		if s.staff[m[2]] == nil {
			fail(w, http.StatusNotFound, "Сотрудник не найден")
			return
		}
		from, _ := time.ParseInLocation("2006-01-02", m[3], s.Zone)
		to, _ := time.ParseInLocation("2006-01-02", m[4], s.Zone)
		var out []any
		for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
			if s.omitDays[m[2]][d.Format("2006-01-02")] {
				continue
			}
			day, ok := s.schedule[m[2]][d.Format("2006-01-02")]
			if !ok {
				day = s.DefaultDay
			}
			slots := []any{}
			for _, sl := range day.Slots {
				slots = append(slots, map[string]string{"from": sl[0], "to": sl[1]})
			}
			working := 0
			if day.Working {
				working = 1
			}
			out = append(out, map[string]any{"date": d.Format("2006-01-02"), "is_working": working, "slots": slots})
		}
		writeJSON(w, http.StatusOK, true, out, nil)
	case reServices.MatchString(path) && r.Method == http.MethodGet:
		if !s.auth(w, r, false) || !company(reServices.FindStringSubmatch(path)[1]) {
			return
		}
		s.listServices(w, r)
	case reBook.MatchString(path) && r.Method == http.MethodPost:
		if !s.auth(w, r, false) || !company(reBook.FindStringSubmatch(path)[1]) {
			return
		}
		s.book(w, body)
	case reRecord.MatchString(path):
		m := reRecord.FindStringSubmatch(path)
		if !s.auth(w, r, true) || !company(m[1]) {
			return
		}
		id, _ := strconv.ParseInt(m[2], 10, 64)
		s.oneRecord(w, r, id, body)
	default:
		fail(w, http.StatusNotFound, "Не найдено")
	}
}

func (s *Stub) listRecords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err1 := time.ParseInLocation("2006-01-02", q.Get("start_date"), s.Zone)
	to, err2 := time.ParseInLocation("2006-01-02", q.Get("end_date"), s.Zone)
	count, _ := strconv.Atoi(q.Get("count"))
	page, _ := strconv.Atoi(q.Get("page"))
	if err1 != nil || err2 != nil || count <= 0 || page <= 0 {
		fail(w, http.StatusBadRequest, "bad query")
		return
	}
	if s.pageSize > 0 && s.pageSize < count {
		count = s.pageSize
	}
	to = to.AddDate(0, 0, 1)
	var list []*Record
	for _, rec := range s.records {
		// [ASSUMPTION] records are selected by the local date of their start;
		// deleted ones are left out unless ListDeleted (D11 saw deleted:true
		// only by id, and the driver skips it either way)
		if !rec.Start.Before(from) && rec.Start.Before(to) && (!rec.Deleted || s.listDel) {
			list = append(list, rec)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Start.Equal(list[j].Start) {
			return list[i].Start.Before(list[j].Start)
		}
		return list[i].ID < list[j].ID
	})
	total := len(list)
	lo := min((page-1)*count, total)
	hi := min(lo+count, total)
	out := []any{}
	for _, rec := range list[lo:hi] {
		out = append(out, s.view(rec, false))
	}
	writeJSON(w, http.StatusOK, true, out, map[string]any{"page": page, "total_count": total + s.totalLies})
}

// view is a record as YCLIENTS shows it; the client's phone masked when
// read one by one (as GET /record does).
func (s *Stub) view(rec *Record, masked bool) map[string]any {
	phone := "+" + rec.ClientPhone
	if masked && len(rec.ClientPhone) > 6 {
		p := rec.ClientPhone
		phone = "+" + p[:4] + strings.Repeat("X", len(p)-6) + p[len(p)-2:]
	}
	services := rec.Services
	if services == nil {
		services = []map[string]any{}
	}
	m := map[string]any{
		"id": rec.ID, "company_id": json.Number(s.Company), "staff_id": json.Number(rec.StaffID),
		"datetime": rec.Start.In(s.Zone).Format(timeFormat), "date": rec.Start.In(s.Zone).Format("2006-01-02 15:04:05"),
		"length": rec.Length, "seance_length": rec.Length, "deleted": rec.Deleted, "comment": rec.Comment,
		"attendance": rec.Attendance, "services": services,
		"client": map[string]any{"name": rec.ClientName, "phone": phone, "email": rec.ClientEmail},
	}
	for k, v := range rec.Extra {
		m[k] = v
	}
	return m
}

func (s *Stub) listServices(w http.ResponseWriter, r *http.Request) {
	st := s.staff[r.URL.Query().Get("staff_id")]
	if st == nil {
		writeJSON(w, http.StatusOK, true, map[string]any{"services": []any{}}, nil)
		return
	}
	var at *time.Time
	if v := r.URL.Query().Get("datetime"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			fail(w, http.StatusBadRequest, "datetime")
			return
		}
		at = &t
	}
	out := []any{}
	for _, sv := range st.Services {
		if at != nil && sv.To > 0 {
			lt := at.In(s.Zone)
			m := lt.Hour()*60 + lt.Minute()
			if m < sv.From || m >= sv.To {
				continue
			}
		}
		out = append(out, map[string]any{"id": sv.ID, "title": sv.Title, "seance_length": sv.SeanceLength, "price_min": sv.Price, "price_max": sv.Price, "active": 1})
	}
	writeJSON(w, http.StatusOK, true, map[string]any{"services": out}, nil)
}

func (s *Stub) serviceOf(st *Staff, id int64) (Service, bool) {
	for _, sv := range st.Services {
		if sv.ID == id {
			return sv, true
		}
	}
	return Service{}, false
}

// free: the court works all of [start, end) and no live record overlaps.
func (s *Stub) free(staff string, start, end time.Time, except int64) bool {
	for _, rec := range s.records {
		if rec.ID != except && !rec.Deleted && rec.StaffID == staff && rec.Start.Before(end) && rec.Start.Add(time.Duration(rec.Length)*time.Second).After(start) {
			return false
		}
	}
	for t := start; t.Before(end); t = t.Add(5 * time.Minute) {
		if !s.working(staff, t) {
			return false
		}
	}
	return true
}

func (s *Stub) working(staff string, t time.Time) bool {
	lt := t.In(s.Zone)
	for _, back := range []int{0, -1} { // a slot running past midnight belongs to the day before
		d := time.Date(lt.Year(), lt.Month(), lt.Day()+back, 0, 0, 0, 0, s.Zone)
		day, ok := s.schedule[staff][d.Format("2006-01-02")]
		if !ok {
			day = s.DefaultDay
		}
		if !day.Working {
			continue
		}
		for _, sl := range day.Slots {
			fh, _ := strconv.Atoi(sl[0][:2])
			fm, _ := strconv.Atoi(sl[0][3:])
			th, _ := strconv.Atoi(sl[1][:2])
			tm, _ := strconv.Atoi(sl[1][3:])
			from := time.Date(d.Year(), d.Month(), d.Day(), fh, fm, 0, 0, s.Zone)
			to := time.Date(d.Year(), d.Month(), d.Day(), th, tm, 0, 0, s.Zone)
			if !to.After(from) {
				to = to.AddDate(0, 0, 1)
			}
			if !t.Before(from) && t.Before(to) {
				return true
			}
		}
	}
	return false
}

func (s *Stub) book(w http.ResponseWriter, body []byte) {
	var in struct {
		Phone        string `json:"phone"`
		Fullname     string `json:"fullname"`
		Email        string `json:"email"`
		Comment      string `json:"comment"`
		Appointments []struct {
			ID       int     `json:"id"`
			Services []int64 `json:"services"`
			StaffID  int64   `json:"staff_id"`
			Datetime string  `json:"datetime"`
		} `json:"appointments"`
	}
	if json.Unmarshal(body, &in) != nil || in.Phone == "" || in.Fullname == "" || len(in.Appointments) == 0 {
		fail(w, http.StatusBadRequest, "Неверные данные")
		return
	}
	type planned struct {
		staff    string
		start    time.Time
		length   int
		services []map[string]any
	}
	var plans []planned
	for _, a := range in.Appointments {
		staff := strconv.FormatInt(a.StaffID, 10)
		st := s.staff[staff]
		start, err := time.Parse(time.RFC3339, a.Datetime)
		if st == nil || err != nil || len(a.Services) == 0 {
			fail(w, http.StatusUnprocessableEntity, "Неверная запись")
			return
		}
		// the same service twice in one appointment counts once (D11 probe)
		seen := map[int64]bool{}
		p := planned{staff: staff, start: start}
		for _, id := range a.Services {
			sv, ok := s.serviceOf(st, id)
			if !ok {
				fail(w, http.StatusUnprocessableEntity, "Услуга недоступна")
				return
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			p.length += sv.SeanceLength
			p.services = append(p.services, map[string]any{"id": id, "cost": sv.Price, "first_cost": sv.Price, "discount": 0})
		}
		plans = append(plans, p)
	}
	for _, p := range plans {
		if !s.free(p.staff, p.start, p.start.Add(time.Duration(p.length)*time.Second), 0) {
			fail(w, http.StatusUnprocessableEntity, "Выбранное время занято")
			return
		}
	}
	var out []any
	phone := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, in.Phone)
	for i, p := range plans {
		s.nextID++
		s.records[s.nextID] = &Record{ID: s.nextID, StaffID: p.staff, Start: p.start, Length: p.length, Comment: in.Comment,
			ClientName: in.Fullname, ClientPhone: phone, ClientEmail: in.Email, Services: p.services}
		if s.shortIDs && i == len(plans)-1 {
			continue
		}
		out = append(out, map[string]any{"id": in.Appointments[i].ID, "record_id": s.nextID, "record_hash": "h"})
	}
	if c := s.copyAfter; c != nil && len(plans) > 0 {
		first := s.records[s.nextID-int64(len(plans))+1]
		s.nextID++
		cp := *first
		cp.ID, cp.Start, cp.ClientPhone, cp.ClientName = s.nextID, first.Start.Add(c.offset), c.phone, "Клиент клуба"
		s.records[cp.ID] = &cp
	}
	if s.dupIDs {
		for i := range out {
			out[i].(map[string]any)["record_id"] = out[0].(map[string]any)["record_id"]
		}
	}
	if s.bookFail != 0 {
		fail(w, s.bookFail, "injected failure after the write")
		return
	}
	if s.bookRaw != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, s.bookRaw)
		return
	}
	writeJSON(w, http.StatusCreated, true, out, nil)
}

func (s *Stub) oneRecord(w http.ResponseWriter, r *http.Request, id int64, body []byte) {
	rec := s.records[id]
	switch r.Method {
	case http.MethodGet:
		if rec == nil {
			fail(w, http.StatusNotFound, "Не найдено")
			return
		}
		writeJSON(w, http.StatusOK, true, s.view(rec, true), nil)
	case http.MethodDelete:
		if rec == nil || rec.Deleted {
			fail(w, http.StatusNotFound, "Не найдено")
			return
		}
		rec.Deleted = true
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		if rec == nil || rec.Deleted {
			fail(w, http.StatusNotFound, "Не найдено")
			return
		}
		var in struct {
			Comment    *string `json:"comment"`
			Attendance *int    `json:"attendance"`
			Client     *struct {
				Phone string `json:"phone"`
			} `json:"client"`
		}
		if json.Unmarshal(body, &in) != nil || in.Comment == nil || in.Client == nil || strings.ContainsAny(in.Client.Phone, "Xx*") {
			fail(w, http.StatusUnprocessableEntity, "Неверные данные")
			return
		}
		rec.Comment = *in.Comment
		if in.Attendance != nil {
			rec.Attendance = *in.Attendance
		}
		writeJSON(w, http.StatusOK, true, s.view(rec, true), nil)
	default:
		fail(w, http.StatusMethodNotAllowed, "method")
	}
}
