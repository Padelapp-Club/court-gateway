package yclients_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// YCLIENTS masks phones ("+7900XXXXX00"),
// so a club's client whose phone reads as the service client's cannot be
// told from it. A club's copy of the booking's record at the same time for
// such a client is never deleted by the undo: with an answer naming the
// gateway's record only that one goes; with no answer both stay.
func TestUndoNeverDeletesAMaskTwin(t *testing.T) {
	const twin = "79001234500" // reads +7900XXXXX00, as clientPhone 79000000000 does
	for _, c := range []struct {
		name     string
		set      func(*yclientsstub.Stub)
		wantLeft int
	}{
		{"500 after the write: both stay", func(s *yclientsstub.Stub) { s.BookThenFail(http.StatusInternalServerError) }, 2},
		{"one id fewer: the named one goes", func(s *yclientsstub.Stub) { s.ShortIDs(true) }, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, oneHourCourt(opts{stub: func(s *yclientsstub.Stub) {
				s.CopyAfterBook(0, twin)
				c.set(s)
			}}))
			d := ycDay(f)
			end := ycAt(d, 11, 0)
			if c.wantLeft < 0 { // two records, the answer names the first
				end = ycAt(d, 12, 0)
			}
			if _, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 0), End: end, Kind: driver.KindGame, Partner: "booker"}); err == nil {
				t.Fatalf("the create passed")
			}
			found := false
			for _, iv := range liveOnCourt1(t, f, d) {
				var id int64
				if _, err := fmtSscanf(iv.ID, &id); err == nil {
					if r, ok := f.stub.Record(id); ok && r.ClientPhone == twin {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("the club's record of the twin client was deleted: %+v", liveOnCourt1(t, f, d))
			}
			if n := len(liveOnCourt1(t, f, d)); c.wantLeft > 0 && n != c.wantLeft || c.wantLeft < 0 && n != 1 {
				t.Fatalf("%d records left, want %d (the named ones deleted, the twin kept)", n, map[bool]int{true: 1, false: c.wantLeft}[c.wantLeft < 0])
			}
		})
	}
}

func fmtSscanf(id string, n *int64) (int, error) { return fmt.Sscanf(id, "y-%d", n) }

// When a candidate of the undo cannot be read,
// what the answer did not name is not deleted — the unreadable one may be
// the gateway's own, and the readable one the club's twin.
func TestUndoUnreadCandidateKeepsTheTwin(t *testing.T) {
	const twin = "79001234500"
	f := newFixture(t, oneHourCourt(opts{stub: func(s *yclientsstub.Stub) {
		s.CopyAfterBook(0, twin)
		s.BookThenFail(http.StatusInternalServerError)
		s.FailNext("/api/v1/record/", http.StatusInternalServerError) // the undo's first read: the gateway's record
	}}))
	d := ycDay(f)
	if _, err := f.drv.Create(ycCtx, driver.CreateRequest{Court: "court-1", Start: ycAt(d, 10, 0), End: ycAt(d, 11, 0), Kind: driver.KindGame, Partner: "booker"}); err == nil {
		t.Fatalf("the create passed")
	}
	if w := ycWrites(f, http.MethodDelete, "/record/"); len(w) != 0 {
		t.Fatalf("the undo deleted %d records with a candidate unread", len(w))
	}
	if n := len(liveOnCourt1(t, f, d)); n != 2 {
		t.Fatalf("%d records left, want both (the gateway's, occupied, and the club's twin)", n)
	}
}
