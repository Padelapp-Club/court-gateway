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

// Clean-room review, round 1 (c82ab7f): one regression per blocker.

// B1: a booking that would take more records than its id can name is
// refused before anything is written; one that fits keeps a working id.
func TestReviewR1BookingNeedsAtMostEightRecords(t *testing.T) {
	f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) {
		s.SetServices(staff1, []yclientsstub.Service{{ID: 41001, Title: "30 мин", SeanceLength: 1800, Price: 1000}})
	}})
	d := d11Day(f)
	_, err := f.drv.Create(d11ctx, driver.CreateRequest{Court: "court-1", Start: d11At(d, 10, 0), End: d11At(d, 14, 30), Kind: driver.KindTournament, Partner: "booker"})
	if err == nil {
		t.Fatalf("a booking of 9 records was made")
	}
	if w := d11Writes(f, http.MethodPost, "/book_record/"); len(w) != 0 {
		t.Fatalf("book_record sent for a booking of 9 records: %d", len(w))
	}
	b, err := f.drv.Create(d11ctx, driver.CreateRequest{Court: "court-1", Start: d11At(d, 15, 0), End: d11At(d, 19, 0), Kind: driver.KindTournament, Partner: "booker"})
	if err != nil {
		t.Fatalf("8 records: %v", err)
	}
	if st, m := d11HTTP(t, f, http.MethodDelete, "/v1/clubs/test-club/bookings/"+b.ID, contract.BookKey, nil); st != http.StatusNoContent {
		t.Fatalf("cancel of an 8-record booking: %d %v", st, m)
	}
	occ, err := f.d11Occ(d, d.AddDate(0, 0, 1), "court-1")
	if err != nil || len(occ.Intervals) != 0 {
		t.Fatalf("after the cancel: %v %+v", err, occ.Intervals)
	}
}

// B2: a create whose answer names fewer records than it made, or that got
// a 5xx after the write, leaves no record of it in the journal.
func TestReviewR1FailedCreateLeavesNoRecord(t *testing.T) {
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
			d := d11Day(f)
			// a club's own record nearby must survive the clean-up
			club := f.seedClubBooking(staff1, d11At(d, 14, 0), d11At(d, 15, 0))
			_, err := f.drv.Create(d11ctx, driver.CreateRequest{Court: "court-1", Start: d11At(d, 10, 0), End: d11At(d, 12, 0), Kind: driver.KindGame, Partner: "booker"})
			if err == nil {
				t.Fatalf("the create passed")
			}
			occ, err := f.d11Occ(d, d.AddDate(0, 0, 1), "court-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(occ.Intervals) != 1 || occ.Intervals[0].ID != fmt.Sprintf("y-%d", club) {
				t.Fatalf("after a failed create the court holds %+v, want only the club's record", occ.Intervals)
			}
		})
	}
}

// B3: the text YCLIENTS writes into an error answer (it may echo a
// client's data) reaches neither an error nor the log.
func TestReviewR1UpstreamMessageNeverLeaks(t *testing.T) {
	const pii = "PRIVATE-CLIENT-REVIEW@example.test"
	f := newFixture(t, opts{})
	d := d11Day(f)
	f.stub.FailNextWith("/api/v1/records/", http.StatusBadRequest, "Invalid client email: "+pii)
	st, _ := d11HTTP(t, f, http.MethodGet, "/v1/clubs/test-club/occupancy?from="+strings.ReplaceAll(d.Format(time.RFC3339), "+", "%2B")+"&to="+strings.ReplaceAll(d.AddDate(0, 0, 1).Format(time.RFC3339), "+", "%2B"), contract.ReadKey, nil)
	if st < 400 {
		t.Fatalf("a failed read answered %d", st)
	}
	f.stub.FailNextWith("/api/v1/records/", http.StatusBadRequest, "Invalid client email: "+pii)
	_, err := f.d11Occ(d, d.AddDate(0, 0, 1), "court-1")
	if err == nil || strings.Contains(err.Error(), pii) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(f.built.Logs.String(), pii) {
		t.Fatalf("the log carries YCLIENTS' message text")
	}
}

// B4: the paid mark of a booking one of whose records the club deleted is
// refused, with nothing written (D11 refuses a deleted record).
func TestReviewR1PaidMarkRefusedWhenAPartIsDeleted(t *testing.T) {
	f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) {
		s.SetServices(staff1, []yclientsstub.Service{{ID: 43001, Title: "1 ч", SeanceLength: 3600, Price: 2000}})
	}})
	d := d11Day(f)
	b, err := f.drv.Create(d11ctx, driver.CreateRequest{Court: "court-1", Start: d11At(d, 10, 0), End: d11At(d, 12, 0), Kind: driver.KindGame, Partner: "booker"})
	if err != nil {
		t.Fatal(err)
	}
	var first, second int64
	if _, err := fmt.Sscanf(b.ID, "y-%d-%d", &first, &second); err != nil {
		t.Fatalf("id %s: %v", b.ID, err)
	}
	f.stub.Delete(second)
	err = f.drv.MarkPaid(d11ctx, b.ID, "booker")
	var ue *driver.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("MarkPaid with a deleted record = %v", err)
	}
	if w := d11Writes(f, http.MethodPut, "/record/"); len(w) != 0 {
		t.Fatalf("PUT sent: %d", len(w))
	}
}
