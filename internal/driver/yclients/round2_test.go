package yclients_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// Review of the round-1 fix (067f6d5): one regression per blocker.

func oneHourCourt(o opts) opts {
	prev := o.stub
	o.stub = func(s *yclientsstub.Stub) {
		s.SetServices(staff1, []yclientsstub.Service{{ID: 44001, Title: "1 ч", SeanceLength: 3600, Price: 2000}})
		if prev != nil {
			prev(s)
		}
	}
	return o
}

// liveOnCourt1 is every live interval of court-1 on day d.
func liveOnCourt1(t *testing.T, f *fixture, d time.Time) []driver.Interval {
	t.Helper()
	occ, err := f.d11Occ(d, d.AddDate(0, 0, 1), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	return occ.Intervals
}

// R1, R2: whatever book_record answers after it wrote — an unreadable
// 201, success=false, one id for two records — the create fails and no
// record of it is left.
func TestReviewR2AnyFailedAnswerIsUndone(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*yclientsstub.Stub)
	}{
		{"unreadable 201", func(s *yclientsstub.Stub) { s.BookThenRaw("{not json") }},
		{"success false with 200", func(s *yclientsstub.Stub) { s.BookThenFail(http.StatusOK) }},
		{"a 400 after the write", func(s *yclientsstub.Stub) { s.BookThenFail(http.StatusBadRequest) }},
		{"the first id twice", func(s *yclientsstub.Stub) { s.DupIDs(true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, oneHourCourt(opts{stub: c.set}))
			d := d11Day(f)
			_, err := f.drv.Create(d11ctx, driver.CreateRequest{Court: "court-1", Start: d11At(d, 10, 0), End: d11At(d, 12, 0), Kind: driver.KindGame, Partner: "booker"})
			if err == nil {
				t.Fatalf("the create passed")
			}
			if ivs := liveOnCourt1(t, f, d); len(ivs) != 0 {
				t.Fatalf("after a failed create the court holds %+v", ivs)
			}
		})
	}
}

// R3: the undo of a failed create deletes only that booking's records: a
// club's copy of one — the comment kept, for another client, or at
// another time — stays.
func TestReviewR2UndoSparesClubCopies(t *testing.T) {
	for _, c := range []struct {
		name   string
		offset time.Duration
		phone  string
	}{
		{"another client, later", 5 * time.Hour, "79991112233"},
		{"another client, same time", 0, "79991112233"},
		{"our client, later", 5 * time.Hour, clientPhone},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, oneHourCourt(opts{stub: func(s *yclientsstub.Stub) {
				s.CopyAfterBook(c.offset, c.phone)
				s.BookThenFail(http.StatusInternalServerError)
			}}))
			d := d11Day(f)
			_, err := f.drv.Create(d11ctx, driver.CreateRequest{Court: "court-1", Start: d11At(d, 10, 0), End: d11At(d, 11, 0), Kind: driver.KindGame, Partner: "booker"})
			if err == nil {
				t.Fatalf("the create passed")
			}
			ivs := liveOnCourt1(t, f, d)
			if len(ivs) != 1 || !ivs[0].Start.Equal(d11At(d, 10, 0).Add(c.offset)) {
				t.Fatalf("after the undo the court holds %+v, want only the club's copy", ivs)
			}
			var id int64
			fmt.Sscanf(ivs[0].ID, "y-%d", &id)
			if r, ok := f.stub.Record(id); !ok || r.ClientPhone != c.phone {
				t.Fatalf("the record left is not the club's copy: %+v", r)
			}
		})
	}
}
