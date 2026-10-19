package yclients_test

// Regression tests for the rules of the PadelApp backend's TypeScript
// YCLIENTS adapter (yclients-crm.service.ts and its occupancy/push specs), one test per
// rule, on the in-memory stub.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// staff3 is a court whose services are only 1 h and 1.5 h (no 30 min, no
// 2 h): a 2 h booking takes the same service twice.
const staff3 = "9910003"

var ycCtx = context.Background()

// ycTileCourt puts court-1 on staff3.
func ycTileCourt(o opts, more ...func(*yclientsstub.Staff)) opts {
	o.courts = []core.CourtSpec{
		{ID: "court-1", Name: "Корт 1", Params: map[string]any{"staff_id": staff3}},
		{ID: "court-2", Name: "Корт 2", Params: map[string]any{"staff_id": staff2}},
	}
	prev := o.stub
	o.stub = func(s *yclientsstub.Stub) {
		st := yclientsstub.Staff{ID: staff3, Name: "Корт 3", Bookable: true, Prepaid: "allowed", Services: []yclientsstub.Service{
			{ID: 33002, Title: "Аренда 1 ч", SeanceLength: 3600, Price: 2000},
			{ID: 33003, Title: "Аренда 1,5 ч", SeanceLength: 5400, Price: 3000},
		}}
		for _, m := range more {
			m(&st)
		}
		s.AddStaff(st)
		if prev != nil {
			prev(s)
		}
	}
	return o
}

// ycDay is the club's midnight three days ahead.
func ycDay(f *fixture) time.Time {
	loc := f.built.Settings.Location
	n := time.Now().In(loc).AddDate(0, 0, 3)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
}

func ycAt(d time.Time, h, m int) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, d.Location())
}

func (f *fixture) ycOcc(from, to time.Time, courts ...string) (driver.Occupancy, error) {
	return f.drv.Occupancy(ycCtx, driver.OccupancyQuery{Courts: courts, From: from, To: to})
}

func ycFind(o driver.Occupancy, id string) (driver.Interval, bool) {
	for _, iv := range o.Intervals {
		if iv.ID == id {
			return iv, true
		}
	}
	return driver.Interval{}, false
}

func ycSpan(t *testing.T, iv driver.Interval, start, end time.Time) {
	t.Helper()
	if !iv.Start.Equal(start) || !iv.End.Equal(end) {
		t.Fatalf("interval %s = %s..%s, want %s..%s", iv.ID, iv.Start, iv.End, start, end)
	}
}

func ycWrites(f *fixture, method, prefix string) []yclientsstub.Write {
	var out []yclientsstub.Write
	for _, w := range f.stub.Writes() {
		if w.Method == method && strings.HasPrefix(w.Path, prefix) {
			out = append(out, w)
		}
	}
	return out
}

func ycUpstream(t *testing.T, err error, kind driver.UpstreamKind) {
	t.Helper()
	var ue *driver.UpstreamError
	if !errors.As(err, &ue) || ue.Kind != kind {
		t.Fatalf("err = %v, want an upstream error of kind %d", err, kind)
	}
}

func (f *fixture) ycSeed(staff string, start time.Time, length time.Duration, comment, phone string) int64 {
	return f.stub.Seed(yclientsstub.Record{StaffID: staff, Start: start, Length: int(length / time.Second),
		Comment: comment, ClientName: "PadelApp", ClientPhone: phone})
}

// ---- occupancy ----

// Records of the configured courts are intervals with their exact
// start and end, under "y-<record>".
func TestRecordsAreExactIntervals(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	a := f.seedClubBooking(staff1, ycAt(d, 10, 5), ycAt(d, 11, 35))
	b := f.seedClubBooking(staff2, ycAt(d, 18, 0), ycAt(d, 19, 30))
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1", "court-2")
	if err != nil {
		t.Fatal(err)
	}
	if !occ.Complete || len(occ.Intervals) != 2 {
		t.Fatalf("occupancy = %+v, want 2 intervals", occ)
	}
	ia, ok := ycFind(occ, fmt.Sprintf("y-%d", a))
	if !ok || ia.Court != "court-1" {
		t.Fatalf("record %d not on court-1: %+v", a, occ)
	}
	ycSpan(t, ia, ycAt(d, 10, 5), ycAt(d, 11, 35))
	ib, ok := ycFind(occ, fmt.Sprintf("y-%d", b))
	if !ok || ib.Court != "court-2" {
		t.Fatalf("record %d not on court-2: %+v", b, occ)
	}
	ycSpan(t, ib, ycAt(d, 18, 0), ycAt(d, 19, 30))
}

// A coach's records (staff that is no court) are not looked at,
// even when they cannot be read.
func TestCoachRecordsIgnoredEvenMalformed(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	f.stub.Seed(yclientsstub.Record{StaffID: staffCoach, Start: ycAt(d, 9, 0), Length: 0})
	f.stub.Seed(yclientsstub.Record{StaffID: staffCoach, Start: ycAt(d, 12, 0), Length: 3600, Extra: map[string]any{"datetime": "не дата"}})
	f.seedClubBooking(staffCoach, ycAt(d, 14, 0), ycAt(d, 15, 0))
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1", "court-2")
	if err != nil {
		t.Fatalf("a coach's record failed the read: %v", err)
	}
	if len(occ.Intervals) != 0 {
		t.Fatalf("a coach's record became occupancy: %+v", occ.Intervals)
	}
}

// A record with deleted=true in the list is not occupancy.
func TestDeletedRecordIgnored(t *testing.T) {
	f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) { s.ListDeleted(true) }})
	d := ycDay(f)
	gone := f.stub.Seed(yclientsstub.Record{StaffID: staff1, Start: ycAt(d, 10, 0), Length: 3600, Deleted: true})
	live := f.seedClubBooking(staff1, ycAt(d, 12, 0), ycAt(d, 13, 0))
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ycFind(occ, fmt.Sprintf("y-%d", gone)); ok || len(occ.Intervals) != 1 {
		t.Fatalf("deleted record %d is occupancy: %+v", gone, occ.Intervals)
	}
	if _, ok := ycFind(occ, fmt.Sprintf("y-%d", live)); !ok {
		t.Fatalf("live record missing: %+v", occ.Intervals)
	}
}

// A break in the schedule (working 07:00–16:00 and 21:15–00:00)
// is occupied though no record is there: 00:00–07:00 and 16:00–21:15, "yh-".
func TestScheduleHoleIsOccupied(t *testing.T) {
	var d time.Time
	f := newFixture(t, opts{})
	d = ycDay(f)
	f.stub.SetDay(staff1, d.Format("2006-01-02"), yclientsstub.Day{Working: true, Slots: [][2]string{{"07:00", "16:00"}, {"21:15", "00:00"}}})
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(occ.Intervals) != 2 {
		t.Fatalf("intervals = %+v, want the two holes", occ.Intervals)
	}
	for _, want := range [][2]time.Time{{d, ycAt(d, 7, 0)}, {ycAt(d, 16, 0), ycAt(d, 21, 15)}} {
		id := fmt.Sprintf("yh-%s-%d", staff1, want[0].Unix())
		iv, ok := ycFind(occ, id)
		if !ok || iv.Court != "court-1" {
			t.Fatalf("hole %s missing: %+v", id, occ.Intervals)
		}
		ycSpan(t, iv, want[0], want[1])
	}
}

// Is_working 0, and a day the schedule answer lacks, are occupied whole.
func TestNotWorkingAndMissingDayOccupiedWhole(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	f.stub.SetDay(staff1, d.Format("2006-01-02"), yclientsstub.Day{Working: false, Slots: [][2]string{{"07:00", "00:00"}}})
	f.stub.OmitDay(staff2, d.Format("2006-01-02"))
	// two days, so the schedule answer is not empty: the next day works
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 2), "court-1", "court-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(occ.Intervals) != 2 {
		t.Fatalf("intervals = %+v, want each court's whole day", occ.Intervals)
	}
	for _, c := range []struct{ court, staff string }{{"court-1", staff1}, {"court-2", staff2}} {
		iv, ok := ycFind(occ, fmt.Sprintf("yh-%s-%d", c.staff, d.Unix()))
		if !ok || iv.Court != c.court {
			t.Fatalf("%s: whole day not occupied: %+v", c.court, occ.Intervals)
		}
		ycSpan(t, iv, d, d.AddDate(0, 0, 1))
	}
}

// Records are read page by page until total_count is reached.
func TestRecordsPaging(t *testing.T) {
	f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) { s.PageSize(2) }})
	d := ycDay(f)
	want := map[string]bool{}
	for h := 8; h < 13; h++ {
		want[fmt.Sprintf("y-%d", f.seedClubBooking(staff1, ycAt(d, h, 0), ycAt(d, h, 30)))] = true
	}
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(occ.Intervals) != len(want) {
		t.Fatalf("intervals = %d, want %d: %+v", len(occ.Intervals), len(want), occ.Intervals)
	}
	for _, iv := range occ.Intervals {
		if !want[iv.ID] {
			t.Fatalf("unexpected interval %+v", iv)
		}
	}
}

// Pages that end short of the promised total_count are an
// incomplete answer: an error, never a partial occupancy.
func TestTotalCountNotKeptIsError(t *testing.T) {
	f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) { s.PageSize(2); s.LieTotal(1) }})
	d := ycDay(f)
	for h := 8; h < 11; h++ {
		f.seedClubBooking(staff1, ycAt(d, h, 0), ycAt(d, h, 30))
	}
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	ycUpstream(t, err, driver.UpstreamInvalid)
	if occ.Complete || len(occ.Intervals) != 0 {
		t.Fatalf("a partial answer came back: %+v", occ)
	}
}

// Fail-closed: one court's schedule failing fails the whole read.
func TestScheduleFailureFailsWholeRead(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	f.seedClubBooking(staff1, ycAt(d, 10, 0), ycAt(d, 11, 0))
	f.stub.FailNext("/api/v1/schedule/"+company+"/"+staff2+"/", http.StatusInternalServerError)
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1", "court-2")
	if err == nil || len(occ.Intervals) != 0 {
		t.Fatalf("occupancy = %+v, err = %v; want an error and nothing", occ, err)
	}
}

// Fail-closed: a configured court's record with an unreadable datetime
// fails the read.
func TestUnreadableDatetimeIsError(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	f.stub.Seed(yclientsstub.Record{StaffID: staff1, Start: ycAt(d, 10, 0), Length: 3600, Extra: map[string]any{"datetime": "2026-13-45 25:00"}})
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	ycUpstream(t, err, driver.UpstreamInvalid)
	if len(occ.Intervals) != 0 {
		t.Fatalf("intervals came back: %+v", occ.Intervals)
	}
}

// A record that starts the evening before and runs past midnight is
// occupancy of the next day's window (records are read from the day before).
func TestRecordPastMidnightSeenNextDay(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	id := f.seedClubBooking(staff1, ycAt(d, 0, 0).Add(-time.Hour), ycAt(d, 1, 0))
	occ, err := f.ycOcc(d, ycAt(d, 6, 0), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	iv, ok := ycFind(occ, fmt.Sprintf("y-%d", id))
	if !ok {
		t.Fatalf("the record past midnight is not seen: %+v", occ.Intervals)
	}
	ycSpan(t, iv, d.Add(-time.Hour), ycAt(d, 1, 0))
}

// ---- booking ----

type ycBookBody struct {
	Phone        string `json:"phone"`
	Comment      string `json:"comment"`
	Appointments []struct {
		ID       int     `json:"id"`
		Services []int64 `json:"services"`
		StaffID  int64   `json:"staff_id"`
		Datetime string  `json:"datetime"`
	} `json:"appointments"`
}

var ycTwoRecords = regexp.MustCompile(`^y-(\d+)-(\d+)$`)

// A repeated service (1 h + 1 h for 2 h) is two records back to back,
// one booking "y-<a>-<b>": one interval in occupancy and Get, both deleted
// by Cancel.
func TestTwoHoursIsTwoRecordsOneBooking(t *testing.T) {
	f := newFixture(t, ycTileCourt(opts{}))
	d := ycDay(f)
	start, end := ycAt(d, 10, 0), ycAt(d, 12, 0)
	b, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: start, End: end, Kind: driver.KindGame, Partner: "booker"})
	if err != nil {
		t.Fatal(err)
	}
	w := ycWrites(f, http.MethodPost, "/book_record/")
	if len(w) != 1 {
		t.Fatalf("book_record writes = %d, want 1", len(w))
	}
	var body ycBookBody
	if err := json.Unmarshal(w[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Appointments) != 2 {
		t.Fatalf("appointments = %d, want 2: %s", len(body.Appointments), w[0].Body)
	}
	a0, a1 := body.Appointments[0], body.Appointments[1]
	if len(a0.Services) != 1 || len(a1.Services) != 1 || a0.Services[0] != 33002 || a1.Services[0] != 33002 {
		t.Fatalf("services = %v, %v; want [33002] twice", a0.Services, a1.Services)
	}
	t0, _ := time.Parse(time.RFC3339, a0.Datetime)
	t1, _ := time.Parse(time.RFC3339, a1.Datetime)
	if !t0.Equal(start) || !t1.Equal(start.Add(time.Hour)) {
		t.Fatalf("datetimes = %s, %s; want %s and +1h", a0.Datetime, a1.Datetime, start)
	}
	m := ycTwoRecords.FindStringSubmatch(b.ID)
	if m == nil {
		t.Fatalf("booking id = %q, want y-<a>-<b>", b.ID)
	}
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(occ.Intervals) != 1 {
		t.Fatalf("intervals = %+v, want one", occ.Intervals)
	}
	if occ.Intervals[0].ID != b.ID {
		t.Fatalf("interval id = %q, want %q", occ.Intervals[0].ID, b.ID)
	}
	ycSpan(t, occ.Intervals[0], start, end)
	got, err := f.drv.Get(ycCtx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Start.Equal(start) || !got.End.Equal(end) || got.Status != driver.StatusActive || got.Court != "court-1" {
		t.Fatalf("Get = %+v", got)
	}
	if err := f.drv.Cancel(ycCtx, b.ID, "booker"); err != nil {
		t.Fatal(err)
	}
	for _, s := range m[1:] {
		var id int64
		fmt.Sscan(s, &id)
		if r, ok := f.stub.Record(id); !ok || !r.Deleted {
			t.Fatalf("record %d not deleted by Cancel", id)
		}
	}
}

// A length the services cannot make (45 min of 1 h and 1.5 h) fails
// before any book_record.
func TestUntileableNoBookRecord(t *testing.T) {
	f := newFixture(t, ycTileCourt(opts{}))
	d := ycDay(f)
	_, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 0), End: ycAt(d, 10, 45), Kind: driver.KindGame, Partner: "booker"})
	if err == nil {
		t.Fatal("45 min booked with 1 h and 1.5 h services")
	}
	if w := f.stub.Writes(); len(w) != 0 {
		t.Fatalf("writes = %+v, want none", w)
	}
}

// Book_record naming fewer records than asked fails the create; the
// records it named are deleted again, and so is the one it made without
// naming it (found by the booking's set tag).
func TestShortIDsCompensated(t *testing.T) {
	f := newFixture(t, ycTileCourt(opts{stub: func(s *yclientsstub.Stub) { s.ShortIDs(true) }}))
	d := ycDay(f)
	_, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 0), End: ycAt(d, 12, 0), Kind: driver.KindGame, Partner: "booker"})
	ycUpstream(t, err, driver.UpstreamInvalid)
	dels := ycWrites(f, http.MethodDelete, "/record/"+company+"/")
	if len(dels) != 2 {
		t.Fatalf("DELETE writes = %+v, want both records of the failed create", dels)
	}
	for _, w := range dels {
		var id int64
		fmt.Sscan(strings.TrimPrefix(w.Path, "/record/"+company+"/"), &id)
		if r, ok := f.stub.Record(id); !ok || !r.Deleted {
			t.Fatalf("record %d is not deleted", id)
		}
	}
}

func ycHTTP(t *testing.T, f *fixture, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.built.Handler().ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

// YCLIENTS refusing a busy time (422) is SLOT_UNAVAILABLE, 409.
func TestBusyTimeIsSlotUnavailable(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	// the driver: a real overlap in the journal
	f.seedClubBooking(staff1, ycAt(d, 10, 0), ycAt(d, 11, 0))
	_, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 30), End: ycAt(d, 11, 30), Kind: driver.KindGame, Partner: "booker"})
	if !errors.Is(err, driver.ErrSlotUnavailable) {
		t.Fatalf("err = %v, want ErrSlotUnavailable", err)
	}
	// the API: occupancy reads free, book_record answers 422
	f.stub.FailNext("/api/v1/book_record/", http.StatusUnprocessableEntity)
	st, m := ycHTTP(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, map[string]any{
		"court": "court-2", "start": ycAt(d, 14, 0).Format(time.RFC3339), "end": ycAt(d, 15, 0).Format(time.RFC3339), "kind": "game"})
	if st != http.StatusConflict || m["code"] != "SLOT_UNAVAILABLE" {
		t.Fatalf("status %d, body %v; want 409 SLOT_UNAVAILABLE", st, m)
	}
}

// The comment starts with the partner's marker; the note is cut so
// the marker, the note, a newline and the paid mark fit 500 characters.
func TestCommentMarkerAndCut(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	// through the API: the marker names the calling partner
	st, m := ycHTTP(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, map[string]any{
		"court": "court-1", "start": ycAt(d, 9, 0).Format(time.RFC3339), "end": ycAt(d, 10, 0).Format(time.RFC3339), "kind": "game", "note": "Иван"})
	if st != http.StatusCreated && st != http.StatusOK {
		t.Fatalf("create: %d %v", st, m)
	}
	w := ycWrites(f, http.MethodPost, "/book_record/")
	var body ycBookBody
	if len(w) != 1 || json.Unmarshal(w[0].Body, &body) != nil {
		t.Fatalf("book_record writes = %+v", w)
	}
	// the marker, the booking's set tag (one record), the note
	if !regexp.MustCompile(`^\[cg:booker\] \[cg-set:[0-9a-f]{8}/1\] Иван$`).MatchString(body.Comment) {
		t.Fatalf("comment = %q, want %q", body.Comment, "[cg:booker] [cg-set:<key>/1] Иван")
	}
	// a long note is cut
	note := strings.Repeat("я", 600)
	b, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 12, 0), End: ycAt(d, 13, 0), Kind: driver.KindGame, Note: note, Partner: "booker"})
	if err != nil {
		t.Fatal(err)
	}
	w = ycWrites(f, http.MethodPost, "/book_record/")
	if len(w) != 2 || json.Unmarshal(w[1].Body, &body) != nil {
		t.Fatalf("book_record writes = %d", len(w))
	}
	if !regexp.MustCompile(`^\[cg:booker\] \[cg-set:[0-9a-f]{8}/1\] я`).MatchString(body.Comment) {
		t.Fatalf("comment starts %q", body.Comment[:20])
	}
	if n := utf8.RuneCountInString(body.Comment + "\n" + yclients.PaidMark); n > 500 {
		t.Fatalf("comment + paid mark = %d characters, want at most 500", n)
	}
	if err := f.drv.MarkPaid(ycCtx, b.ID, "booker"); err != nil {
		t.Fatal(err)
	}
	var id int64
	fmt.Sscan(strings.TrimPrefix(b.ID, "y-"), &id)
	r, _ := f.stub.Record(id)
	if n := utf8.RuneCountInString(r.Comment); n > 500 || !strings.HasSuffix(r.Comment, "\n"+yclients.PaidMark) {
		t.Fatalf("paid comment: %d characters, ends %q", n, r.Comment[len(r.Comment)-40:])
	}
}

// ---- ownership ----

// Our marker on a record of another client is not ours: not
// deleted.
func TestOwnershipNeedsOurPhone(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	id := f.ycSeed(staff1, ycAt(d, 10, 0), time.Hour, "[cg:booker] подделка", "79990000001")
	err := f.drv.Cancel(ycCtx, fmt.Sprintf("y-%d", id), "booker")
	if !errors.Is(err, driver.ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned", err)
	}
	if r, _ := f.stub.Record(id); r.Deleted || len(ycWrites(f, http.MethodDelete, "/")) != 0 {
		t.Fatal("another client's record was deleted")
	}
}

// Another partner's marker is not the caller's.
func TestOwnershipOtherPartner(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	id := f.ycSeed(staff1, ycAt(d, 10, 0), time.Hour, "[cg:other] чужая", clientPhone)
	if err := f.drv.Cancel(ycCtx, fmt.Sprintf("y-%d", id), "booker"); !errors.Is(err, driver.ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned", err)
	}
	if r, _ := f.stub.Record(id); r.Deleted {
		t.Fatal("another partner's record was deleted")
	}
}

// A stretch without working time ("yh-") is nobody's booking.
func TestOwnershipHoleID(t *testing.T) {
	f := newFixture(t, opts{})
	if err := f.drv.Cancel(ycCtx, "yh-"+staff1+"-1790000000", "booker"); !errors.Is(err, driver.ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned", err)
	}
}

// A booking that has started is not cancelled.
func TestCancelStarted(t *testing.T) {
	f := newFixture(t, opts{})
	start := f.clk.now().Truncate(time.Hour).Add(2 * time.Hour)
	b, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: start, End: start.Add(time.Hour), Kind: driver.KindGame, Partner: "booker"})
	if err != nil {
		t.Fatal(err)
	}
	f.clk.add(150 * time.Minute)
	if err := f.drv.Cancel(ycCtx, b.ID, "booker"); !errors.Is(err, driver.ErrStarted) {
		t.Fatalf("err = %v, want ErrStarted", err)
	}
	if len(ycWrites(f, http.MethodDelete, "/")) != 0 {
		t.Fatal("a started booking was deleted")
	}
}

// Without the hub's time nothing is decided.
func TestCancelClockUnknown(t *testing.T) {
	f := newFixture(t, opts{clock: func() (time.Time, bool) { return time.Now(), false }})
	d := ycDay(f)
	b, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 0), End: ycAt(d, 11, 0), Kind: driver.KindGame, Partner: "booker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.drv.Cancel(ycCtx, b.ID, "booker"); !errors.Is(err, driver.ErrClockUnknown) {
		t.Fatalf("err = %v, want ErrClockUnknown", err)
	}
	if len(ycWrites(f, http.MethodDelete, "/")) != 0 {
		t.Fatal("deleted without a known clock")
	}
}

// ---- paid mark ----

func (f *fixture) ycGetRecord(t *testing.T, id int64) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/record/%s/%d", f.srv.URL, company, id), nil)
	req.Header.Set("Accept", "application/vnd.yclients.v2+json")
	req.Header.Set("Authorization", "Bearer "+partnerToken+", User "+userToken)
	res, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var env struct{ Data map[string]any }
	dec := json.NewDecoder(res.Body)
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil || env.Data == nil {
		t.Fatalf("GET record %d: %v", id, err)
	}
	return env.Data
}

func ycJSON(raw []byte) map[string]any {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&m)
	return m
}

func ycEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// PUT /record carries the fresh GET's fields unchanged
// (attendance as it was), no SMS, never over another booking, the comment
// with the mark and the configured phone, not the mask; a repeat writes
// nothing; a deleted booking is not found.
func TestPaidMarkBody(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	id := f.stub.Seed(yclientsstub.Record{StaffID: staff1, Start: ycAt(d, 10, 0), Length: 3600, Comment: "[cg:booker] заметка",
		ClientName: "PadelApp", ClientPhone: clientPhone, Attendance: 1,
		Services: []map[string]any{{"id": 31002, "cost": 1800, "first_cost": 2000, "discount": 10, "title": "Аренда 1 ч", "amount": 1}}})
	bid := fmt.Sprintf("y-%d", id)
	fresh := f.ycGetRecord(t, id)
	if err := f.drv.MarkPaid(ycCtx, bid, "booker"); err != nil {
		t.Fatal(err)
	}
	puts := ycWrites(f, http.MethodPut, "/record/")
	if len(puts) != 1 {
		t.Fatalf("PUT writes = %d, want 1", len(puts))
	}
	put := ycJSON(puts[0].Body)
	for _, k := range []string{"staff_id", "datetime", "seance_length", "attendance"} {
		if !ycEq(put[k], fresh[k]) {
			t.Errorf("%s = %v, fresh GET %v", k, put[k], fresh[k])
		}
	}
	if fmt.Sprint(put["attendance"]) != "1" {
		t.Errorf("attendance = %v, want 1 as it was", put["attendance"])
	}
	wantSv := []map[string]any{}
	for _, s := range fresh["services"].([]any) {
		sm := s.(map[string]any)
		wantSv = append(wantSv, map[string]any{"id": sm["id"], "cost": sm["cost"], "first_cost": sm["first_cost"], "discount": sm["discount"]})
	}
	if !ycEq(put["services"], wantSv) {
		t.Errorf("services = %v, want %v", put["services"], wantSv)
	}
	if fmt.Sprint(put["save_if_busy"]) != "0" || fmt.Sprint(put["send_sms"]) != "0" {
		t.Errorf("save_if_busy = %v, send_sms = %v; want 0 and 0", put["save_if_busy"], put["send_sms"])
	}
	if put["comment"] != fresh["comment"].(string)+"\n"+yclients.PaidMark {
		t.Errorf("comment = %q", put["comment"])
	}
	cl, _ := put["client"].(map[string]any)
	if cl == nil || cl["phone"] != clientPhone {
		t.Errorf("client = %v, want phone %s (fresh GET shows %v)", put["client"], clientPhone, fresh["client"])
	}
	// repeat: the mark is there, nothing written
	if err := f.drv.MarkPaid(ycCtx, bid, "booker"); err != nil {
		t.Fatal(err)
	}
	if n := len(ycWrites(f, http.MethodPut, "/record/")); n != 1 {
		t.Fatalf("PUT writes after a repeat = %d, want 1", n)
	}
	// deleted in the journal
	if err := f.drv.Cancel(ycCtx, bid, "booker"); err != nil {
		t.Fatal(err)
	}
	if err := f.drv.MarkPaid(ycCtx, bid, "booker"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("MarkPaid of a deleted booking: %v, want ErrNotFound", err)
	}
	if n := len(ycWrites(f, http.MethodPut, "/record/")); n != 1 {
		t.Fatalf("PUT on a deleted record: %d writes", n)
	}
}

// ---- auth and keys ----

// A login's token the CRM stops accepting is replaced by one new login.
func TestReloginOnceAfterRevoke(t *testing.T) {
	f := newFixture(t, opts{keys: keySources[1]})
	d := ycDay(f)
	if _, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1"); err != nil {
		t.Fatal(err)
	}
	if n := f.stub.Logins(); n != 1 {
		t.Fatalf("logins = %d, want 1", n)
	}
	f.clk.add(2 * time.Minute) // a login younger than a minute is not replaced (TestReloginNotAtEveryRequest)
	f.stub.RevokeTokens()
	if _, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1"); err != nil {
		t.Fatalf("after a revoke: %v", err)
	}
	if n := f.stub.Logins(); n != 2 {
		t.Fatalf("logins = %d, want 2", n)
	}
}

var ycWrongPassword = keySource{name: "wrong password",
	env: map[string]string{yclients.PartnerTokenEnv: partnerToken, "CRM_PASSWORD": "wrong-password-5d1e"}, crm: ", login: " + stubLogin}

// A refused login is UpstreamAuth and is not tried again at once (the
// account is not locked by retries).
func TestWrongPasswordNoImmediateRetry(t *testing.T) {
	f := newFixture(t, opts{keys: ycWrongPassword})
	d := ycDay(f)
	_, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	ycUpstream(t, err, driver.UpstreamAuth)
	_, err = f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	ycUpstream(t, err, driver.UpstreamAuth)
	if n := f.stub.Logins(); n != 1 {
		t.Fatalf("POST /auth = %d, want 1", n)
	}
}

// No token or password shows in the logs or in an error.
func TestSecretsNeverLeak(t *testing.T) {
	secrets := []string{partnerToken, userToken, stubPassword, "wrong-password-5d1e", "stub-issued-"}
	check := func(t *testing.T, f *fixture, errs []error) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(f.built.Logs.String(), s) {
				t.Errorf("logs contain %q", s)
			}
			for _, err := range errs {
				if err != nil && strings.Contains(err.Error(), s) {
					t.Errorf("error %q contains %q", err, s)
				}
			}
		}
	}
	for _, ks := range []keySource{keySources[0], keySources[1], ycWrongPassword} {
		t.Run(ks.name, func(t *testing.T) {
			f := newFixture(t, opts{keys: ks})
			d := ycDay(f)
			var errs []error
			occ := func() { _, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1"); errs = append(errs, err) }
			occ()
			f.stub.RevokeTokens()
			occ()
			f.stub.FailNext("/api/v1/records/", http.StatusUnauthorized)
			occ()
			f.stub.FailNext("/api/v1/schedule/", http.StatusForbidden)
			occ()
			f.stub.FailNext("/api/v1/records/", http.StatusInternalServerError)
			occ()
			_, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 0), End: ycAt(d, 11, 0), Kind: driver.KindGame, Partner: "booker"})
			errs = append(errs, err, f.drv.CheckConfig(ycCtx), f.drv.Ready(ycCtx), f.drv.Cancel(ycCtx, "y-1", "booker"))
			check(t, f, errs)
		})
	}
}

// ---- config check ----

func ycMisconfig(t *testing.T, err error, want string) {
	t.Helper()
	var me *driver.MisconfigError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want a MisconfigError", err)
	}
	if !strings.Contains(me.Error(), want) {
		t.Fatalf("misconfig %q does not say %q", me, want)
	}
}

// What makes the branch's bookings impossible or wrong is a
// misconfiguration; a hidden court is an alert.
func TestConfigCheck(t *testing.T) {
	t.Run("matching config passes", func(t *testing.T) {
		f := newFixture(t, ycTileCourt(opts{}))
		if err := f.drv.CheckConfig(ycCtx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("branch in another zone", func(t *testing.T) {
		f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) { s.TZName = "Europe/Moscow" }})
		ycMisconfig(t, f.drv.CheckConfig(ycCtx), "Europe/Moscow")
	})
	t.Run("hidden court", func(t *testing.T) {
		f := newFixture(t, ycTileCourt(opts{}, func(s *yclientsstub.Staff) { s.Hidden = true }))
		var ca *driver.ConfigAlert
		if err := f.drv.CheckConfig(ycCtx); !errors.As(err, &ca) || ca.Code != "STAFF_HIDDEN" {
			t.Fatalf("err = %v, want ConfigAlert STAFF_HIDDEN", err)
		}
	})
	t.Run("prepaid required", func(t *testing.T) {
		f := newFixture(t, ycTileCourt(opts{}, func(s *yclientsstub.Staff) { s.Prepaid = "required" }))
		ycMisconfig(t, f.drv.CheckConfig(ycCtx), "предоплата")
	})
	t.Run("unknown staff", func(t *testing.T) {
		f := newFixture(t, opts{courts: []core.CourtSpec{
			{ID: "court-1", Name: "Корт 1", Params: map[string]any{"staff_id": "9919999"}},
		}})
		ycMisconfig(t, f.drv.CheckConfig(ycCtx), "сотрудника-корта 9919999 нет в филиале")
	})
	t.Run("inactive branch", func(t *testing.T) {
		f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) { s.Inactive = true }})
		ycMisconfig(t, f.drv.CheckConfig(ycCtx), "неактивен")
	})
}

// ---- throttle ----

// The process's requests are spaced by MinInterval (YCLIENTS' per-IP limit).
func TestThrottle(t *testing.T) {
	f := newFixture(t, opts{})
	saved := *yclients.MinInterval
	*yclients.MinInterval = 50 * time.Millisecond
	defer func() { *yclients.MinInterval = saved }()
	begin := time.Now()
	for i := 0; i < 5; i++ {
		// one GET /record each (404: nothing to cancel)
		if err := f.drv.Cancel(ycCtx, fmt.Sprintf("y-%d", 100+i), "booker"); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(begin); el < 200*time.Millisecond {
		t.Fatalf("5 requests took %s, want at least 200ms", el)
	}
	if n := len(f.stub.Writes()); n != 0 {
		t.Fatalf("writes = %d", n)
	}
}
