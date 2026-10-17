package yclients_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// Review of the delta (32a46fe): YCLIENTS masks phones ("+7900XXXXX00"),
// so a club's client whose phone reads as the service client's cannot be
// told from it. A club's copy of the booking's record at the same time for
// such a client is never deleted by the undo: with an answer naming the
// gateway's record only that one goes; with no answer both stay.
func TestReviewR3UndoNeverDeletesAMaskTwin(t *testing.T) {
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
			d := d11Day(f)
			end := d11At(d, 11, 0)
			if c.wantLeft < 0 { // two records, the answer names the first
				end = d11At(d, 12, 0)
			}
			if _, err := f.drv.Create(d11ctx, driver.CreateRequest{Court: "court-1", Start: d11At(d, 10, 0), End: end, Kind: driver.KindGame, Partner: "booker"}); err == nil {
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
		})
	}
}

func fmtSscanf(id string, n *int64) (int, error) { return fmt.Sscanf(id, "y-%d", n) }
