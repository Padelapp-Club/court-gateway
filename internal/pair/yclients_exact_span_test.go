package pair_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
	"github.com/Padelapp-Club/court-gateway/internal/pair/pairtest"
)

// passes runs the loop's first pass and two more, logging each.
func passes(t *testing.T, env *pairtest.Env) {
	t.Helper()
	loop := startLoop(t, env, 25*time.Second)
	for i := range 3 {
		if i > 0 {
			loop.clk.next(t, 1)
			loop.last = loop.pass(t)
		}
		rep := loop.last
		t.Logf("pass %d: created %d removed %d alarms %v, YCLIENTS mirrors %s", i+1, len(rep.Created), len(rep.Removed), rep.Alarms, pairtest.Spans(mirrorsOn(t, env.Primary, "court-1")))
	}
}

// A court whose services make up a Lunda booking's length gets a mirror
// over the booking's exact span, whatever another court offers: court-1
// has 45 and 60 minutes, court-2 60 only, and a club booking of YCLIENTS
// starts on court-1 right where the 45-minute Lunda booking ends. A mirror
// stretched to 60 minutes would run into that booking and never be made.
func TestYclientsMirrorNotStretchedByAnotherCourtsServices(t *testing.T) {
	env := pairtest.YclientsLunda(t, pairtest.Options{At: "10:00"})
	y := env.Primary.(*pairtest.YclientsStub)
	y.Stub.AddStaff(yclientsstub.Staff{ID: pairtest.YStaff1, Name: "Корт 1", Bookable: true, Prepaid: "allowed", Services: []yclientsstub.Service{
		{ID: 31001, Title: "45 min", SeanceLength: 2700, Price: 1000}, {ID: 31002, Title: "60 min", SeanceLength: 3600, Price: 2000},
	}})
	y.Stub.AddStaff(yclientsstub.Staff{ID: pairtest.YStaff2, Name: "Корт 2", Bookable: true, Prepaid: "allowed", Services: []yclientsstub.Service{
		{ID: 32001, Title: "60 min", SeanceLength: 3600, Price: 2000},
	}})
	s := env.Day(2, 12, 0)
	env.Mirror.Book(t, "court-1", s, s.Add(45*time.Minute))
	local := env.Primary.Book(t, "court-1", s.Add(45*time.Minute), s.Add(105*time.Minute))
	before := env.Primary.Raw(t, local)
	passes(t, env)
	if env.Primary.Raw(t, local) != before {
		t.Fatal("the club's YCLIENTS booking changed")
	}
	if !pairtest.Has(mirrorsOn(t, env.Primary, "court-1"), pairtest.Mirror, "court-1", s, s.Add(45*time.Minute)) {
		t.Fatal("the Lunda booking 12:00–12:45 has no mirror in YCLIENTS although court-1's services make up its length")
	}
}

// Back to back Lunda bookings 12:00–12:45 and 12:45–13:45: the second is
// blocked in YCLIENTS over its whole span — no mirror of the first holds
// 12:45–13:00 and keeps it from being made — so 13:00–13:45 is not left
// free in YCLIENTS, and partners are not sold it either.
func TestYclientsMirrorOfNextBookingIsNotHeldByTheEarlierOne(t *testing.T) {
	env := pairtest.YclientsLunda(t, pairtest.Options{At: "10:00"})
	s := env.Day(2, 12, 0)
	env.Mirror.Book(t, "court-1", s, s.Add(45*time.Minute))
	env.Mirror.Book(t, "court-1", s.Add(45*time.Minute), s.Add(105*time.Minute))
	passes(t, env)
	q := driver.OccupancyQuery{Courts: []string{"court-1"}, From: s.Add(time.Hour), To: s.Add(105 * time.Minute)}
	occ, err := env.Pair.Primary().Occupancy(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("YCLIENTS occupancy 13:00–13:45: %+v", occ)
	if st, _ := env.Create(t, "court-1", s.Add(time.Hour), s.Add(90*time.Minute)); st != http.StatusConflict {
		t.Fatalf("a partner booking 13:00–13:30: HTTP %d, want %d", st, http.StatusConflict)
	}
	if len(occ.Intervals) == 0 {
		t.Fatal("13:00–13:45 is free in YCLIENTS after three complete passes")
	}
}
