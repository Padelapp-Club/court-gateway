package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// horizonDriver answers every occupancy read with occ and counts the creates
// that reach it.
type horizonDriver struct {
	slowDriver
	occ     driver.Occupancy
	created *int
}

func (d horizonDriver) Occupancy(context.Context, driver.OccupancyQuery) (driver.Occupancy, error) {
	return d.occ, nil
}

func (d horizonDriver) Create(context.Context, driver.CreateRequest) (driver.Booking, error) {
	*d.created++
	return driver.Booking{ID: "e-00000000-0000-4000-8000-0000000000e2"}, nil
}

func horizonServer(occ driver.Occupancy, created *int) (*Server, time.Time) {
	start := time.Now().UTC().Truncate(time.Hour).Add(20 * 24 * time.Hour)
	now := start.Add(-20 * 24 * time.Hour)
	st := &Settings{ClubID: "club-a", Location: time.UTC, SlotMinutes: 60, Courts: []Court{{ID: "court-1", Name: "C1"}},
		Driver: driver.Config{Clock: func() (time.Time, bool) { return now, true }}}
	return NewServer(st, horizonDriver{occ: occ, created: created}, nil), start
}

func horizonCreate(srv *Server, start time.Time, kind string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":%q}`,
		start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339), kind)
	ctx, cancel := context.WithTimeout(auth.WithPartnerName(context.Background(), "booker"), 5*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/clubs/club-a/bookings", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(w, r)
	return w
}

// REQ-400: a day beyond the CRM's booking horizon for
// players whose occupancy the driver read in full is read — the core takes
// completeness from Complete alone, never from the horizon flag, for every
// driver. Beyond the horizon only a tournament block is written; a player's
// game is refused by the core itself, whatever the backend does.
func TestBeyondHorizonIsCompleteAndOnlyTournamentsAreWritten(t *testing.T) {
	for _, c := range []struct {
		name    string
		occ     driver.Occupancy
		kind    string
		code    int
		errCode string
		created int
	}{
		{"full data beyond the horizon: a tournament block is written", driver.Occupancy{Complete: true, BeyondHorizon: true}, "tournament", http.StatusCreated, "", 1},
		{"full data beyond the horizon: a player's game is not", driver.Occupancy{Complete: true, BeyondHorizon: true}, "game", http.StatusConflict, "BEYOND_BOOKING_HORIZON", 0},
		{"incomplete data beyond the horizon: nothing is written", driver.Occupancy{Complete: false, BeyondHorizon: true}, "tournament", http.StatusConflict, "OCCUPANCY_UNKNOWN", 0},
		{"within the horizon: a game is written as before", driver.Occupancy{Complete: true}, "game", http.StatusCreated, "", 1},
	} {
		created := 0
		srv, start := horizonServer(c.occ, &created)
		w := horizonCreate(srv, start, c.kind)
		if w.Code != c.code || created != c.created || (c.errCode != "" && !strings.Contains(w.Body.String(), `"`+c.errCode+`"`)) {
			t.Fatalf("%s: %d %s, %d created", c.name, w.Code, w.Body, created)
		}
	}
}

// Beyond the horizon an occupied interval stays occupied: no block over it.
func TestOccupiedBeyondHorizonIsNotWritten(t *testing.T) {
	created := 0
	_, start := horizonServer(driver.Occupancy{}, &created)
	occ := driver.Occupancy{Complete: true, BeyondHorizon: true,
		Intervals: []driver.Interval{{ID: "l-club", Court: "court-1", Start: start, End: start.Add(time.Hour)}}}
	srv, _ := horizonServer(occ, &created)
	if w := horizonCreate(srv, start, "tournament"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"SLOT_UNAVAILABLE"`) || created != 0 {
		t.Fatalf("%d %s, %d created", w.Code, w.Body, created)
	}
}

// Beyond the horizon the occupancy answer is complete when the data is: the horizon does not make it incomplete.
func TestOccupancyAnswerCompletenessIgnoresTheHorizon(t *testing.T) {
	for _, occ := range []driver.Occupancy{{Complete: true, BeyondHorizon: true}, {Complete: false, BeyondHorizon: true}, {Complete: true}} {
		created := 0
		srv, start := horizonServer(occ, &created)
		q := fmt.Sprintf("/v1/clubs/club-a/occupancy?from=%s&to=%s", start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
		r := httptest.NewRequest(http.MethodGet, q, nil).WithContext(auth.WithPartnerName(context.Background(), "reader"))
		w := httptest.NewRecorder()
		srv.APIHandler().ServeHTTP(w, r)
		var out struct {
			Complete bool `json:"complete"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%+v: %d %s", occ, w.Code, w.Body)
		}
		if out.Complete != occ.Complete {
			t.Fatalf("%+v answered as %+v", occ, out)
		}
	}
}
