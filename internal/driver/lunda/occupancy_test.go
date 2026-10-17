package lunda_test

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// backendID is what the backend takes for a gateway booking id.
var backendID = regexp.MustCompile(`^[a-z]{1,3}-[A-Za-z0-9_-]+$`)

func findIv(occ driver.Occupancy, start time.Time) (driver.Interval, bool) {
	for _, iv := range occ.Intervals {
		if iv.Start.Equal(start) {
			return iv, true
		}
	}
	return driver.Interval{}, false
}

// Every slot occupies its court, whatever its type: someone's DRAFT (a
// hold of a few minutes), a PLACEHOLDER, a BLOCKED stretch, a type never
// seen. A slot of a booking has the booking's id; another, a stable id of
// its own that is never a booking's.
func TestEverySlotTypeOccupies(t *testing.T) {
	f, d := loggedIn(t)
	day := futureDay()
	types := map[string]int{"DRAFT": 8, "PLACEHOLDER": 10, "BLOCKED": 12, "SOMETHING_NEW": 14}
	for typ, hh := range types {
		f.stub.SeedSlot(court1, typ, day.Add(time.Duration(hh)*time.Hour), day.Add(time.Duration(hh)*time.Hour+30*time.Minute),
			map[string]any{"draftTTL": 180, "clientUid": clubClient})
	}
	booking := seedClubBooking(f.stub, court1, day.Add(16*time.Hour), day.Add(17*time.Hour))
	q := driver.OccupancyQuery{Courts: []string{"court-1", "court-2"}, From: day, To: day.AddDate(0, 0, 1)}
	occ, err := d.Occupancy(context.Background(), q)
	if err != nil || !occ.Complete {
		t.Fatalf("occupancy: %+v %v", occ, err)
	}
	seen := map[string]bool{}
	for typ, hh := range types {
		iv, ok := findIv(occ, day.Add(time.Duration(hh)*time.Hour))
		if !ok || iv.Court != "court-1" || !iv.End.Equal(day.Add(time.Duration(hh)*time.Hour+30*time.Minute)) {
			t.Fatalf("%s slot is not occupied: %+v", typ, occ)
		}
		if !backendID.MatchString(iv.ID) || iv.ID[:3] != "ls-" || seen[iv.ID] {
			t.Fatalf("%s slot id %q", typ, iv.ID)
		}
		seen[iv.ID] = true
	}
	if iv, ok := findIv(occ, day.Add(16*time.Hour)); !ok || iv.ID != "l-"+booking {
		t.Fatalf("booking: %+v", occ)
	}
	again, _ := d.Occupancy(context.Background(), q)
	for _, iv := range again.Intervals {
		if _, ok := findIv(occ, iv.Start); !ok || !seen[iv.ID] && iv.ID != "l-"+booking {
			t.Fatalf("ids are not stable: %+v vs %+v", again, occ)
		}
	}
}

// A court left out of a day's schedule (blocked for the day) is occupied
// that whole day; the other days and courts are not.
func TestCourtMissingFromTheDayIsOccupied(t *testing.T) {
	f, d := loggedIn(t)
	day := futureDay()
	f.stub.BlockCourt(court2, day.Format("2006-01-02"))
	occ, err := d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1", "court-2"}, From: day.Add(10 * time.Hour), To: day.Add(36 * time.Hour)})
	if err != nil || !occ.Complete || len(occ.Intervals) != 1 {
		t.Fatalf("occupancy: %+v %v", occ, err)
	}
	iv := occ.Intervals[0]
	if iv.Court != "court-2" || !iv.Start.Equal(day) || !iv.End.Equal(day.AddDate(0, 0, 1)) || !backendID.MatchString(iv.ID) {
		t.Fatalf("the blocked day: %+v", iv)
	}
}

// Beyond the club's booking depth Lunda may not show everything: the
// answer is incomplete, and later days are not read.
func TestBeyondBookingDepthIsIncomplete(t *testing.T) {
	f, d := loggedIn(t)
	f.stub.BookingDepth = 2
	day := futureDay() // three days ahead
	occ, err := d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: day.Add(10 * time.Hour), To: day.AddDate(0, 0, 5)})
	if err != nil || occ.Complete {
		t.Fatalf("occupancy: %+v %v", occ, err)
	}
	if n := f.stub.CallsTo("/club/booking/slots-load"); n != 2 { // the day before, and the first day
		t.Fatalf("%d days read past the depth", n)
	}
}

// A day that cannot be read makes the answer incomplete.
func TestUnreadDayIsIncomplete(t *testing.T) {
	f, d := loggedIn(t)
	day := futureDay()
	f.stub.FailNextQuery(http.MethodGet, "/club/booking/slots-load", "date="+day.Format("2006-01-02"), http.StatusServiceUnavailable, `{}`)
	occ, err := d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: day.Add(10 * time.Hour), To: day.Add(11 * time.Hour)})
	if err != nil || occ.Complete {
		t.Fatalf("occupancy: %+v %v", occ, err)
	}
}

// An answer the driver cannot read is never "free".
func TestMalformedScheduleIsInvalid(t *testing.T) {
	day := futureDay().Format("2006-01-02")
	court := `"courtUid":"` + court1 + `"`
	for name, body := range map[string]string{
		"no courts":           `{"result":{"beyondBookingDepth":false}}`,
		"not json":            `<html>maintenance</html>`,
		"courts not a list":   `{"result":{"courts":{}}}`,
		"slot without type":   `{"result":{"courts":[{` + court + `,"slots":[{"begin":"` + day + `T10:00:00","end":"` + day + `T11:00:00"}]}]}}`,
		"slot with bad time":  `{"result":{"courts":[{` + court + `,"slots":[{"type":"BOOKING","begin":"soon","end":"later"}]}]}}`,
		"end before begin":    `{"result":{"courts":[{` + court + `,"slots":[{"type":"DRAFT","begin":"` + day + `T11:00:00","end":"` + day + `T10:00:00"}]}]}}`,
		"malformed bookingId": `{"result":{"courts":[{` + court + `,"slots":[{"type":"BOOKING","bookingId":"a b","begin":"` + day + `T10:00:00","end":"` + day + `T11:00:00"}]}]}}`,
		"court without uid":   `{"result":{"courts":[{"slots":[]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			f.stub.FailNextQuery(http.MethodGet, "/club/booking/slots-load", "date="+day, http.StatusOK, body)
			from := futureDay().Add(10 * time.Hour)
			occ, err := d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: from, To: from.Add(time.Hour)})
			if upstreamKind(err) != driver.UpstreamInvalid {
				t.Fatalf("occupancy: %+v %v", occ, err)
			}
		})
	}
}

// Early in the morning the day before is read too: a booking running past
// midnight is listed under the day it begins.
func TestBookingPastMidnightIsSeen(t *testing.T) {
	f, d := loggedIn(t)
	day := futureDay()
	id := seedClubBooking(f.stub, court1, day.Add(-time.Hour), day.Add(2*time.Hour))
	occ, err := d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: day.Add(time.Hour), To: day.Add(3 * time.Hour)})
	if err != nil || len(occ.Intervals) != 1 || occ.Intervals[0].ID != "l-"+id || !occ.Intervals[0].Start.Equal(day.Add(-time.Hour)) {
		t.Fatalf("occupancy: %+v %v", occ, err)
	}
}

// A booking listed slot by slot is one interval.
func TestSlotsOfOneBookingAreMerged(t *testing.T) {
	f, d := loggedIn(t)
	day := futureDay().Format("2006-01-02")
	id := "00000000-0000-4000-8000-0000000000f1"
	f.stub.FailNextQuery(http.MethodGet, "/club/booking/slots-load", "date="+day, http.StatusOK, `{"result":{"courts":[{"courtUid":"`+court1+`","slots":[
		{"type":"BOOKING","bookingId":"`+id+`","begin":"`+day+`T10:00:00","end":"`+day+`T10:30:00"},
		{"type":"BOOKING","bookingId":"`+id+`","begin":"`+day+`T10:30:00","end":"`+day+`T11:00:00"}]},{"courtUid":"`+court2+`","slots":[]}]}}`)
	from := futureDay().Add(9 * time.Hour)
	occ, err := d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: from, To: from.Add(3 * time.Hour)})
	if err != nil || len(occ.Intervals) != 1 || occ.Intervals[0].ID != "l-"+id || occ.Intervals[0].End.Sub(occ.Intervals[0].Start) != time.Hour {
		t.Fatalf("occupancy: %+v %v", occ, err)
	}
}
