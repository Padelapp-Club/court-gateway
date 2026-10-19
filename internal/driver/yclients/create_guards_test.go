package yclients_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// Guards of a create and of the paid mark: a booking's id must name all
// its records, a failed create leaves no record, YCLIENTS' error text never
// leaks, and a booking with a deleted record is not marked paid.

// A booking that would take more records than its id can name is
// refused before anything is written; one that fits keeps a working id.
func TestBookingNeedsAtMostEightRecords(t *testing.T) {
	f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) {
		s.SetServices(staff1, []yclientsstub.Service{{ID: 41001, Title: "30 мин", SeanceLength: 1800, Price: 1000}})
	}})
	d := ycDay(f)
	_, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 0), End: ycAt(d, 14, 30), Kind: driver.KindTournament, Partner: "booker"})
	if err == nil {
		t.Fatalf("a booking of 9 records was made")
	}
	if w := ycWrites(f, http.MethodPost, "/book_record/"); len(w) != 0 {
		t.Fatalf("book_record sent for a booking of 9 records: %d", len(w))
	}
	b, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 15, 0), End: ycAt(d, 19, 0), Kind: driver.KindTournament, Partner: "booker"})
	if err != nil {
		t.Fatalf("8 records: %v", err)
	}
	if st, m := ycHTTP(t, f, http.MethodDelete, "/v1/clubs/test-club/bookings/"+b.ID, contract.BookKey, nil); st != http.StatusNoContent {
		t.Fatalf("cancel of an 8-record booking: %d %v", st, m)
	}
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	if err != nil || len(occ.Intervals) != 0 {
		t.Fatalf("after the cancel: %v %+v", err, occ.Intervals)
	}
}

// A create whose answer names fewer records than it made, or that got
// a 5xx after the write, leaves no record of it in the journal.
func TestFailedCreateLeavesNoRecord(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*yclientsstub.Stub)
	}{
		{"one id fewer", func(s *yclientsstub.Stub) { s.ShortIDs(true) }},
		{"500 after the write", func(s *yclientsstub.Stub) { s.BookThenFail(http.StatusInternalServerError) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) {
				s.SetServices(staff1, []yclientsstub.Service{{ID: 42001, Title: "1 ч", SeanceLength: 3600, Price: 2000}})
			}})
			c.set(f.stub)
			d := ycDay(f)
			// a club's own record nearby must survive the clean-up
			club := f.seedClubBooking(staff1, ycAt(d, 14, 0), ycAt(d, 15, 0))
			_, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 0), End: ycAt(d, 12, 0), Kind: driver.KindGame, Partner: "booker"})
			if err == nil {
				t.Fatalf("the create passed")
			}
			occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(occ.Intervals) != 1 || occ.Intervals[0].ID != fmt.Sprintf("y-%d", club) {
				t.Fatalf("after a failed create the court holds %+v, want only the club's record", occ.Intervals)
			}
		})
	}
}

// The text YCLIENTS writes into an error answer (it may echo a
// client's data) reaches neither an error nor the log.
func TestUpstreamMessageNeverLeaks(t *testing.T) {
	const pii = "PRIVATE-CLIENT-REVIEW@example.test"
	f := newFixture(t, opts{})
	d := ycDay(f)
	f.stub.FailNextWith("/api/v1/records/", http.StatusBadRequest, "Invalid client email: "+pii)
	st, _ := ycHTTP(t, f, http.MethodGet, "/v1/clubs/test-club/occupancy?from="+strings.ReplaceAll(d.Format(time.RFC3339), "+", "%2B")+"&to="+strings.ReplaceAll(d.AddDate(0, 0, 1).Format(time.RFC3339), "+", "%2B"), contract.ReadKey, nil)
	if st < 400 {
		t.Fatalf("a failed read answered %d", st)
	}
	f.stub.FailNextWith("/api/v1/records/", http.StatusBadRequest, "Invalid client email: "+pii)
	_, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	if err == nil || strings.Contains(err.Error(), pii) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(f.built.Logs.String(), pii) {
		t.Fatalf("the log carries YCLIENTS' message text")
	}
}

// The paid mark of a booking one of whose records the club deleted is
// refused, with nothing written (the backend adapter refuses a deleted
// record too).
func TestPaidMarkRefusedWhenAPartIsDeleted(t *testing.T) {
	f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) {
		s.SetServices(staff1, []yclientsstub.Service{{ID: 43001, Title: "1 ч", SeanceLength: 3600, Price: 2000}})
	}})
	d := ycDay(f)
	b, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 0), End: ycAt(d, 12, 0), Kind: driver.KindGame, Partner: "booker"})
	if err != nil {
		t.Fatal(err)
	}
	var first, second int64
	if _, err := fmt.Sscanf(b.ID, "y-%d-%d", &first, &second); err != nil {
		t.Fatalf("id %s: %v", b.ID, err)
	}
	f.stub.Delete(second)
	err = f.drv.MarkPaid(ycCtx, b.ID, "booker")
	var ue *driver.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("MarkPaid with a deleted record = %v", err)
	}
	if w := ycWrites(f, http.MethodPut, "/record/"); len(w) != 0 {
		t.Fatalf("PUT sent: %d", len(w))
	}
}
