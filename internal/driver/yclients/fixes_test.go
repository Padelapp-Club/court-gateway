package yclients_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// A set tag that names fewer parts than carry it (a club copied a record
// with its comment) must not hide any of them: every record is occupancy.
func TestSetWithMorePartsThanItsTagHidesNone(t *testing.T) {
	f := newFixture(t, opts{})
	d := ycDay(f)
	tag := "[cg:booker] [cg-set:0a1b2c3d/2] x"
	var ids []int64
	for h := 10; h < 13; h++ {
		ids = append(ids, f.ycSeed(staff1, ycAt(d, h, 0), time.Hour, tag, clientPhone))
	}
	occ, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, ok := ycFind(occ, fmt.Sprintf("y-%d", id)); !ok {
			t.Fatalf("record %d of an over-full set is missing: %+v", id, occ.Intervals)
		}
	}
	// and a whole set still reads as one booking
	g := newFixture(t, opts{})
	a := g.ycSeed(staff1, ycAt(d, 10, 0), time.Hour, tag, clientPhone)
	b := g.ycSeed(staff1, ycAt(d, 11, 0), time.Hour, tag, clientPhone)
	occ, err = g.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
	if err != nil {
		t.Fatal(err)
	}
	if iv, ok := ycFind(occ, fmt.Sprintf("y-%d-%d", a, b)); !ok || !iv.End.Equal(ycAt(d, 12, 0)) || len(occ.Intervals) != 1 {
		t.Fatalf("a whole set is not one booking: %+v", occ.Intervals)
	}
}

// A user token refused right after its login is not replaced by another
// login at every request (a user without the rights would log in at each
// call); a minute later it is.
func TestReloginNotAtEveryRequest(t *testing.T) {
	f := newFixture(t, opts{keys: keySources[1]})
	d := ycDay(f)
	if _, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1"); err != nil {
		t.Fatal(err)
	}
	f.stub.RevokeTokens()
	for i := 0; i < 3; i++ {
		_, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1")
		ycUpstream(t, err, driver.UpstreamAuth)
	}
	if n := f.stub.Logins(); n != 1 {
		t.Fatalf("POST /auth = %d within a minute of the login, want 1", n)
	}
	f.clk.add(2 * time.Minute)
	if _, err := f.ycOcc(d, d.AddDate(0, 0, 1), "court-1"); err != nil {
		t.Fatalf("a minute later: %v", err)
	}
	if n := f.stub.Logins(); n != 2 {
		t.Fatalf("POST /auth = %d, want 2", n)
	}
}
