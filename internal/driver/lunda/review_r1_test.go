package lunda_test

// Regressions of the first review round (B1–B6): each asserts the correct
// behaviour where the reviewer's probe showed the defect.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
)

// intercept answers the requests to path with answer, everything else as
// the stub does.
func intercept(f *acctFixture, path string, answer func(w http.ResponseWriter, r *http.Request)) {
	base := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api"+path {
			answer(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// errorLogged reports a log line at ERROR containing all of parts.
func errorLogged(logs string, parts ...string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"level":"ERROR"`) {
			continue
		}
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			return true
		}
	}
	return false
}

// B1: a create whose outcome is unknown cancels nothing. The club books
// its own tournaments on the same service client: a booking of that
// client found on the court and time — here the club's, made between the
// gateway's check and its create — may not be the gateway's. The partner
// gets an error naming it, the club must check it, and it is logged at
// ERROR; no client is named.
func TestUnknownOutcomeNeverCancelsAForeignBooking(t *testing.T) {
	f, d := loggedIn(t)
	req := createReq(futureDay(), 10)
	var clubs string
	intercept(f, "/club/booking/booking-create", func(w http.ResponseWriter, r *http.Request) {
		clubs = f.stub.SeedBooking(lundastub.Booking{Court: court1, Begin: req.Start, End: req.End, Customer: serviceUID,
			Notes: "Турнир клуба", Price: 1500, Extra: map[string]any{"customer": map[string]any{"firstName": "PRIVACY-MARKER-FIRSTNAME-91c2"}}})
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := d.Create(context.Background(), req)
	if upstreamKind(err) != driver.UpstreamUnavailable || !strings.Contains(err.Error(), "l-"+clubs) || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("create: %v", err)
	}
	if got := f.stub.Booking(clubs); got.Status == "CANCELLED" || f.stub.CallsTo("/club/booking/booking-cancel") != 0 {
		t.Fatalf("the club's booking was cancelled: %+v", got)
	}
	if logs := f.logs.String(); !errorLogged(logs, "outcome", "l-"+clubs) || strings.Contains(logs, "PRIVACY-MARKER") {
		t.Fatalf("logs: %s", logs)
	}
	// nothing there: still an unknown outcome, still nothing cancelled
	g, e := loggedIn(t)
	intercept(g, "/club/booking/booking-create", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if _, err := e.Create(context.Background(), req); err == nil || !strings.Contains(err.Error(), "unknown") || !errorLogged(g.logs.String(), "outcome") {
		t.Fatalf("create with nothing found: %v", err)
	}
}

// B4: a 200 whose body does not read, or has no booking id, is an unknown
// outcome too — the booking may have been made: it is named, not
// cancelled, and not left unmentioned.
func TestMalformedCreateAnswerIsUnknownOutcome(t *testing.T) {
	for name, body := range map[string]string{"truncated": `{"result":`, "no id": `{"result":{"status":"NOT_PAID"}}`} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			stub := f.srv.Config.Handler
			intercept(f, "/club/booking/booking-create", func(w http.ResponseWriter, r *http.Request) {
				stub.ServeHTTP(httptest.NewRecorder(), r) // Lunda makes it
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, body)
			})
			_, err := d.Create(context.Background(), createReq(futureDay(), 10))
			bs := f.stub.Bookings()
			if len(bs) != 1 || upstreamKind(err) != driver.UpstreamInvalid || !strings.Contains(err.Error(), "l-"+bs[0].ID) {
				t.Fatalf("create: %v; bookings %+v", err, bs)
			}
			if bs[0].Status == "CANCELLED" || f.stub.CallsTo("/club/booking/booking-cancel") != 0 || !errorLogged(f.logs.String(), "l-"+bs[0].ID) {
				t.Fatalf("booking %+v, logs %s", bs[0], f.logs.String())
			}
		})
	}
}

// B5: the rollback of a failed create goes by the decision clock, as
// Cancel does: a booking that has started meanwhile is not cancelled — it
// stays, the error says so and it is logged at ERROR; with the hub's time
// not known nothing is cancelled either.
func TestRollbackNeverCancelsAStartedBooking(t *testing.T) {
	f, d := loggedIn(t)
	req := createReq(futureDay(), 10)
	f.clk.add(req.Start.Sub(f.clk.now()) - time.Second)
	intercept(f, "/club/booking/update-courts-price-adjustment", func(w http.ResponseWriter, r *http.Request) {
		f.clk.add(2 * time.Second)
		w.WriteHeader(http.StatusBadRequest)
	})
	_, err := d.Create(context.Background(), req)
	bs := f.stub.Bookings()
	if err == nil || len(bs) != 1 || bs[0].Status == "CANCELLED" || f.stub.CallsTo("/club/booking/booking-cancel") != 0 {
		t.Fatalf("create: %v; bookings %+v", err, bs)
	}
	if !strings.Contains(err.Error(), "STAYS IN LUNDA") || !strings.Contains(err.Error(), "l-"+bs[0].ID) || !errorLogged(f.logs.String(), "l-"+bs[0].ID) {
		t.Fatalf("error %v, logs %s", err, f.logs.String())
	}

	g := newFixture(t, fixtureOpts{clock: func() (time.Time, bool) { return time.Now(), false }})
	g.stub.FailNext(http.MethodPost, "/club/booking/update-courts-price-adjustment", http.StatusBadRequest, `{}`)
	if _, err := g.drv.Create(context.Background(), req); err == nil || !strings.Contains(err.Error(), "STAYS IN LUNDA") || g.stub.CallsTo("/club/booking/booking-cancel") != 0 {
		t.Fatalf("hub time unknown: %v", err)
	}
}

// B2: a court whose slots are absent or null is an answer that does not
// read — never a free court; only an explicit empty list is free.
func TestCourtWithoutSlotsIsNeverFree(t *testing.T) {
	day := futureDay().Format("2006-01-02")
	for name, tc := range map[string]struct {
		suffix string
		free   bool
	}{"slots absent": {"", false}, "slots null": {`,"slots":null`, false}, "slots empty": {`,"slots":[]`, true}} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			seedClubBooking(f.stub, court1, futureDay().Add(10*time.Hour), futureDay().Add(11*time.Hour))
			f.stub.FailNextQuery(http.MethodGet, "/club/booking/slots-load", "date="+day, http.StatusOK,
				`{"result":{"courts":[{"courtUid":"`+court1+`"`+tc.suffix+`},{"courtUid":"`+court2+`","slots":[]}]}}`)
			occ, err := d.Occupancy(context.Background(), occQuery(futureDay()))
			switch {
			case tc.free && (err != nil || !occ.Complete || len(occ.Intervals) != 0):
				t.Fatalf("an empty list: %+v %v", occ, err)
			case !tc.free && upstreamKind(err) != driver.UpstreamInvalid:
				t.Fatalf("read as %+v %v", occ, err)
			}
		})
	}
}

// B3: a booking running past midnight is seen however late into the
// morning the range starts: the day before is always read.
func TestOvernightBookingSeenAllMorning(t *testing.T) {
	f, d := loggedIn(t)
	day := futureDay()
	id := seedClubBooking(f.stub, court1, day.Add(-2*time.Hour), day.Add(9*time.Hour)) // 22:00 the day before to 09:00
	for _, from := range []time.Duration{time.Hour, 7 * time.Hour, 8*time.Hour + 30*time.Minute} {
		occ, err := d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: day.Add(from), To: day.Add(from + time.Hour)})
		if err != nil || len(occ.Intervals) != 1 || occ.Intervals[0].ID != "l-"+id {
			t.Fatalf("from %s: %+v %v", from, occ, err)
		}
	}
	// and a range after it ends does not show it
	occ, err := d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: day.Add(9 * time.Hour), To: day.Add(10 * time.Hour)})
	if err != nil || len(occ.Intervals) != 0 {
		t.Fatalf("after its end: %+v %v", occ, err)
	}
}

// nextWeekday is the first date from today+1 that falls on wd.
func nextWeekday(wd time.Weekday) time.Time {
	d := futureDay().AddDate(0, 0, -2)
	for d.Weekday() != wd {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// B6: a date the calendar prices apart is priced by its category, through
// PriceTable.Dates: a holiday at the holiday rate, a working Saturday at
// the working-day rate, a category without prices as no price; a date of
// its weekday's own category stays weekly. The quote a partner gets on the
// holiday is the holiday's.
func TestHolidayPricedByItsCategory(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	holiday, pre, saturday := nextWeekday(time.Wednesday), nextWeekday(time.Thursday), nextWeekday(time.Saturday)
	plain := nextWeekday(time.Friday)
	days := []map[string]string{
		{"date": holiday.Format("2006-01-02"), "category": "HOLIDAY"},
		{"date": pre.Format("2006-01-02"), "category": "PRE_HOLIDAY"},
		{"date": saturday.Format("2006-01-02"), "category": "REGULAR"},
		{"date": plain.Format("2006-01-02"), "category": "REGULAR"},
	}
	byYear := map[int][]map[string]string{}
	for _, d := range days {
		y, _ := strconv.Atoi(d["date"][:4])
		byYear[y] = append(byYear[y], d)
	}
	for year, ds := range byYear {
		cal, _ := json.Marshal(map[string]any{"year": year, "days": ds})
		f.stub.SetCalendar(year, string(cal))
	}
	pt, err := f.drv.PriceTable(context.Background(), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]driver.PriceRule{
		holiday.Format("2006-01-02"):  {{Days: driver.AllWeekdays, From: 420, To: 1380, PerHour: 400000}},
		pre.Format("2006-01-02"):      {},
		saturday.Format("2006-01-02"): {{Days: driver.AllWeekdays, From: 420, To: 960, PerHour: 200000}, {Days: driver.AllWeekdays, From: 960, To: 1380, PerHour: 300000}},
	}
	if len(pt.Dates) != len(want) {
		t.Fatalf("dates: %+v", pt.Dates)
	}
	for date, rules := range want {
		got, ok := pt.Dates[date]
		if !ok || len(got) != len(rules) {
			t.Fatalf("%s: %+v, want %+v", date, got, rules)
		}
		for i := range rules {
			if got[i] != rules[i] {
				t.Fatalf("%s: %+v, want %+v", date, got, rules)
			}
		}
	}
	// the partner's quote on the holiday: 1.5 h at 4000
	h := f.built.Handler()
	start := time.Date(holiday.Year(), holiday.Month(), holiday.Day(), 10, 0, 0, 0, holiday.Location())
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/clubs/test-club/quote?court=court-1&start="+qe(start)+"&end="+qe(start.Add(90*time.Minute)), nil)
	r.Header.Set("Authorization", "Bearer "+contract.ReadKey)
	h.ServeHTTP(rec, r)
	var q struct {
		Total float64 `json:"total"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &q) != nil || q.Total != 6000 {
		t.Fatalf("holiday quote: %d %s", rec.Code, rec.Body)
	}
}

func qe(t time.Time) string {
	return strings.NewReplacer("+", "%2B", ":", "%3A").Replace(t.Format(time.RFC3339))
}

// B6: a calendar that does not read gives no prices at all — never the
// weekday's prices on what may be a holiday.
func TestUnreadableCalendarGivesNoPrices(t *testing.T) {
	for name, fail := range map[string]func(*lundastub.Stub){
		"unavailable": func(s *lundastub.Stub) {
			s.FailNext(http.MethodGet, "/club/settings/calendar/load", http.StatusInternalServerError, `{}`)
		},
		"refused": func(s *lundastub.Stub) {
			s.FailNext(http.MethodGet, "/club/settings/calendar/load", http.StatusBadRequest, `{}`)
		},
		"no days": func(s *lundastub.Stub) {
			s.FailNext(http.MethodGet, "/club/settings/calendar/load", http.StatusOK, `{"result":{"year":2026}}`)
		},
		"bad day": func(s *lundastub.Stub) {
			s.FailNext(http.MethodGet, "/club/settings/calendar/load", http.StatusOK, `{"result":{"days":[{"date":"soon","category":"HOLIDAY"}]}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			fail(f.stub)
			pt, err := d.PriceTable(context.Background(), "court-1")
			var ue *driver.UpstreamError
			if !errors.As(err, &ue) || len(pt.Base) != 0 {
				t.Fatalf("price table: %+v %v", pt, err)
			}
		})
	}
}
