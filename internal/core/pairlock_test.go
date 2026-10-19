package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
)

// pairDriver is a court of two CRMs (driver.MultiResourcer).
type pairDriver struct{ slowDriver }

func (pairDriver) Resources(court string) []string {
	return []string{"primary " + court, "mirror " + court}
}

// A partner's create of a pair's court takes the lock of the court's room
// in each CRM: while the reconciliation loop holds the mirror's room
// (LockResources), the create waits and answers 503 BUSY.
func TestCreateLocksEveryRoomOfAPair(t *testing.T) {
	defer func(d time.Duration) { createLockWait = d }(createLockWait)
	createLockWait = 50 * time.Millisecond
	srv := NewServer(&Settings{ClubID: "club-a", Location: time.UTC, SlotMinutes: 60, Courts: []Court{{ID: "court-1", Name: "C1"}}}, pairDriver{}, nil)
	unlock, err := LockResources(context.Background(), []string{"mirror court-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now().UTC().Truncate(time.Hour).Add(48 * time.Hour)
	body := fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
	r := httptest.NewRequest(http.MethodPost, "/v1/clubs/club-a/bookings", strings.NewReader(body))
	r = r.WithContext(auth.WithPartnerName(r.Context(), "booker"))
	w := httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"BUSY"`) {
		t.Fatalf("a create went on while the mirror's room was held: %d %s", w.Code, w.Body)
	}
}
