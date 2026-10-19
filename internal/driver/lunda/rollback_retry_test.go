package lunda_test

// The price table's range, and rollbacks and cancel retries that go by
// the booking as it is now.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
)

func quote(t *testing.T, f *fixture, start time.Time) (int, int64) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/clubs/test-club/quote?court=court-1&start="+qe(start)+"&end="+qe(start.Add(time.Hour)), nil)
	r.Header.Set("Authorization", "Bearer "+contract.ReadKey)
	f.built.Handler().ServeHTTP(rec, r)
	var q struct {
		Total int64 `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	return rec.Code, q.Total
}

// The price table covers exactly the dates it priced, from yesterday to
// today plus the booking depth of club/settings, holidays included; a
// date past it has no price — a holiday there is never quoted at its
// weekday's rate.
func TestDatesOutsideTheReadCalendarHaveNoPrice(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	msk := f.built.Settings.Location
	f.clk.mu.Lock()
	f.clk.t = time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)
	f.clk.mu.Unlock()
	f.stub.BookingDepth = 14
	f.stub.SetCalendar(2026, map[string][]string{"HOLIDAY": {"2026-10-28", "2026-12-28"}})
	f.stub.SetCalendar(2027, map[string][]string{"HOLIDAY": {"2027-01-01"}})
	pt, err := f.drv.PriceTable(context.Background(), "court-1")
	if err != nil || pt.DatesFrom != "2026-10-16" || pt.DatesTo != "2026-10-31" || len(pt.Dates) != 1 {
		t.Fatalf("table: from %q to %q dates %v: %v", pt.DatesFrom, pt.DatesTo, pt.Dates, err)
	}
	if code, total := quote(t, f, time.Date(2026, 10, 28, 10, 0, 0, 0, msk)); code != http.StatusOK || total != 4000 {
		t.Fatalf("2026-10-28: %d %d", code, total)
	}
	// a holiday, and a plain weekday, past the booking depth: no price
	for _, day := range []time.Time{time.Date(2026, 12, 28, 10, 0, 0, 0, msk), time.Date(2026, 11, 4, 10, 0, 0, 0, msk)} {
		if code, total := quote(t, f, day); code == http.StatusOK {
			t.Fatalf("%s priced: %d", day.Format("2006-01-02"), total)
		}
	}
	// at the year's end the next year's calendar is read too
	f.clk.mu.Lock()
	f.clk.t = time.Date(2026, 12, 25, 6, 0, 0, 0, time.UTC)
	f.clk.mu.Unlock()
	pt, err = f.drv.PriceTable(context.Background(), "court-1")
	if err != nil || pt.DatesTo != "2027-01-08" || len(pt.Dates) != 2 {
		t.Fatalf("in December: to %q dates %v: %v", pt.DatesTo, pt.Dates, err)
	}
	if code, total := quote(t, f, time.Date(2027, 1, 1, 10, 0, 0, 0, msk)); code != http.StatusOK || total != 4000 {
		t.Fatalf("2027-01-01 read in December: %d %d", code, total)
	}
}

// The rollback goes by the booking as it is now: one the club moved
// to a start that has passed is not cancelled; one that cannot be read
// again is not cancelled either. Both stay, said and logged at ERROR.
func TestRollbackGoesByTheBookingsCurrentStart(t *testing.T) {
	t.Run("moved to a start that has passed", func(t *testing.T) {
		f, d := loggedIn(t)
		intercept(f, "/club/booking/update-courts-price-adjustment", func(w http.ResponseWriter, r *http.Request) {
			b := f.stub.Bookings()[0]
			b.Begin, b.End = f.clk.now().Add(-time.Minute), f.clk.now().Add(time.Hour)
			f.stub.SeedBooking(b) // the club moves it
			w.WriteHeader(http.StatusBadRequest)
		})
		_, err := d.Create(context.Background(), createReq(futureDay(), 10))
		b := f.stub.Bookings()[0]
		if b.Status == "CANCELLED" || f.stub.CallsTo("/club/booking/booking-cancel") != 0 {
			t.Fatalf("cancelled: %+v", b)
		}
		if err == nil || !strings.Contains(err.Error(), "STAYS IN LUNDA") || !errorLogged(f.logs.String(), "l-"+b.ID) {
			t.Fatalf("error %v, logs %s", err, f.logs.String())
		}
	})
	t.Run("not readable again", func(t *testing.T) {
		f, d := loggedIn(t)
		f.stub.FailNext(http.MethodPost, "/club/booking/update-courts-price-adjustment", http.StatusBadRequest, `{}`)
		f.stub.FailNext(http.MethodGet, "/club/booking/booking-load", http.StatusInternalServerError, `{}`)
		_, err := d.Create(context.Background(), createReq(futureDay(), 10))
		b := f.stub.Bookings()[0]
		if b.Status == "CANCELLED" || f.stub.CallsTo("/club/booking/booking-cancel") != 0 || err == nil || !strings.Contains(err.Error(), "STAYS IN LUNDA") {
			t.Fatalf("booking %+v, error %v", b, err)
		}
	})
}

// A cancel refused with the session and sent again with a session saved
// meanwhile first re-reads the booking: started by then, it is not sent
// again; not started, it is.
func TestCancelRetryRechecksTheStart(t *testing.T) {
	for name, tc := range map[string]struct {
		startsIn  time.Duration
		cancelled bool
	}{"started meanwhile": {time.Second, false}, "still ahead": {time.Hour, true}} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			now := f.clk.now()
			id := f.stub.SeedBooking(lundastub.Booking{Court: court1, Begin: now.Add(tc.startsIn), End: now.Add(tc.startsIn + time.Hour), Customer: serviceUID,
				Notes: "[cg:booker] test", Status: "PAID"})
			attempts := 0
			stub := f.srv.Config.Handler
			intercept(f, "/club/booking/booking-cancel", func(w http.ResponseWriter, r *http.Request) {
				if attempts++; attempts == 1 {
					f.clk.add(2 * time.Second)
					f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName)) // a login meanwhile
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				stub.ServeHTTP(w, r)
			})
			err := d.Cancel(context.Background(), "l-"+id, "booker")
			got := f.stub.Booking(id)
			switch {
			case tc.cancelled && (err != nil || got.Status != "CANCELLED" || attempts != 2):
				t.Fatalf("not resent: %v %+v %d", err, got, attempts)
			case !tc.cancelled && (!errors.Is(err, driver.ErrStarted) || got.Status == "CANCELLED" || attempts != 1):
				t.Fatalf("resent after the start: %v %+v %d", err, got, attempts)
			}
		})
	}
}

// A cancel resent with a replacement session re-establishes
// that the booking is still the gateway's, as it is NOW: the club may have
// given it to one of its clients, rewritten its notes or moved it to a
// court that is not the club's config. Then it is not sent again.
func TestCancelRetryRechecksOwnership(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*lundastub.Booking)
		want error
	}{
		"another client": {func(b *lundastub.Booking) { b.Customer = "00000000-0000-4000-8000-0000000000aa" }, driver.ErrNotOwned},
		"marker removed": {func(b *lundastub.Booking) { b.Notes = "игра клуба" }, driver.ErrNotOwned},
		"another court":  {func(b *lundastub.Booking) { b.Court = "00000000-0000-4000-8000-0000000000c9" }, driver.ErrCourtNotConfigured},
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			now := f.clk.now()
			id := f.stub.SeedBooking(lundastub.Booking{Court: court1, Begin: now.Add(time.Hour), End: now.Add(2 * time.Hour), Customer: serviceUID,
				Notes: "[cg:booker] test", Status: "PAID"})
			attempts := 0
			stub := f.srv.Config.Handler
			intercept(f, "/club/booking/booking-cancel", func(w http.ResponseWriter, r *http.Request) {
				if attempts++; attempts == 1 {
					f.stub.EditBooking(id, tc.edit) // the club, meanwhile
					f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName))
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				stub.ServeHTTP(w, r)
			})
			err := d.Cancel(context.Background(), "l-"+id, "booker")
			if got := f.stub.Booking(id); !errors.Is(err, tc.want) || got.Status == "CANCELLED" || attempts != 1 {
				t.Fatalf("resent a cancel of a booking no longer ours: %v %+v attempts=%d", err, got, attempts)
			}
		})
	}
}
