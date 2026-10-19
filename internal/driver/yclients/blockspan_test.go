package yclients_test

import (
	"context"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// BlockSpan rounds a mirror's length up to the shortest the courts'
// services (as the last MirrorInventory read them) make up on every court,
// at most 30 minutes past the source's end; the start never moves. A length
// they make up is itself; none within 30 minutes: the span itself (the
// create then fails).
func TestBlockSpanCoversWithTheCourtsServices(t *testing.T) {
	svc := func(id int64, minutes int) yclientsstub.Service {
		return yclientsstub.Service{ID: id, Title: "Аренда", SeanceLength: minutes * 60, Price: float64(minutes) * 30}
	}
	only := func(court1, court2 []yclientsstub.Service) func(*yclientsstub.Stub) {
		return func(s *yclientsstub.Stub) {
			s.AddStaff(yclientsstub.Staff{ID: staff1, Name: "Корт 1", Bookable: true, Prepaid: "allowed", Services: court1})
			s.AddStaff(yclientsstub.Staff{ID: staff2, Name: "Корт 2", Bookable: true, Prepaid: "allowed", Services: court2})
		}
	}
	for _, tc := range []struct {
		name  string
		stub  func(*yclientsstub.Stub) // nil: 30, 60, 90 and 120 minutes on both courts
		cases map[time.Duration]time.Duration
	}{
		{"30/60/90/120", nil, map[time.Duration]time.Duration{
			45 * time.Minute: time.Hour, 50 * time.Minute: time.Hour, time.Hour: time.Hour,
			75 * time.Minute: 90 * time.Minute, 5 * time.Minute: 30 * time.Minute, 30 * time.Minute: 30 * time.Minute,
			100 * time.Minute:               120 * time.Minute,
			90*time.Second + 44*time.Minute: 90*time.Second + 44*time.Minute, // not whole minutes: as it is
		}},
		{"60/90 only", only([]yclientsstub.Service{svc(1, 60), svc(2, 90)}, []yclientsstub.Service{svc(3, 60), svc(4, 90)}), map[time.Duration]time.Duration{
			30 * time.Minute: time.Hour,        // 30 minutes over: the bound
			29 * time.Minute: 29 * time.Minute, // 31 minutes over: none, as it is
			45 * time.Minute: time.Hour, 2 * time.Hour: 2 * time.Hour, 135 * time.Minute: 150 * time.Minute,
		}},
		{"courts differ", only([]yclientsstub.Service{svc(1, 45), svc(2, 60)}, []yclientsstub.Service{svc(3, 60)}), map[time.Duration]time.Duration{
			45 * time.Minute: time.Hour, // makes up on court 1 only: what makes up on both
			time.Hour:        time.Hour,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, opts{stub: tc.stub})
			start := time.Date(2030, 1, 15, 12, 0, 0, 0, f.built.Settings.Location)
			// nothing read yet: every span as it is (no pass rounds before
			// its inventory)
			if s, e := f.drv.BlockSpan(start, start.Add(45*time.Minute)); !s.Equal(start) || !e.Equal(start.Add(45*time.Minute)) {
				t.Fatalf("before any inventory: %s–%s", s, e)
			}
			if _, err := f.drv.MirrorInventory(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1", "court-2"}, From: start, To: start.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			for l, want := range tc.cases {
				s, e := f.drv.BlockSpan(start, start.Add(l))
				if !s.Equal(start) || e.Sub(s) != want {
					t.Errorf("%s: %s–%s, want %s from %s", l, s.Format("15:04:05"), e.Format("15:04:05"), want, start.Format("15:04"))
				}
				// a block's span is its own span: valid() and CreateBlock agree
				if s2, e2 := f.drv.BlockSpan(s, e); !s2.Equal(s) || !e2.Equal(e) {
					t.Errorf("%s: BlockSpan of %s–%s is %s–%s", l, s, e, s2, e2)
				}
			}
		})
	}
}

// CreateBlock writes the span BlockSpan gives — a 45-minute source is
// blocked by one record of the 1-hour service — and returns it as the
// inventory reads it, so the next pass finds it valid.
func TestCreateBlockWritesTheCoveringSpan(t *testing.T) {
	f := newFixture(t, opts{})
	ctx := context.Background()
	start := time.Date(2030, 1, 15, 12, 0, 0, 0, f.built.Settings.Location)
	q := driver.OccupancyQuery{Courts: []string{"court-1"}, From: start, To: start.Add(2 * time.Hour)}
	if _, err := f.drv.MirrorInventory(ctx, q); err != nil {
		t.Fatal(err)
	}
	src := driver.MirrorMarker{Src: "lunda", ID: "l-45"}
	it, err := f.drv.CreateBlock(ctx, driver.BlockRequest{Court: "court-1", Start: start, End: start.Add(45 * time.Minute), Source: src})
	if err != nil {
		t.Fatal(err)
	}
	if !it.Start.Equal(start) || !it.End.Equal(start.Add(time.Hour)) {
		t.Fatalf("CreateBlock returned %s–%s, want 12:00–13:00", it.Start, it.End)
	}
	inv, err := f.drv.MirrorInventory(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	var mirrors []driver.MirrorItem
	for _, m := range inv.Items {
		if m.Class == driver.ClassMirror {
			mirrors = append(mirrors, m)
		}
	}
	if len(mirrors) != 1 || mirrors[0].ID != it.ID || !mirrors[0].Start.Equal(it.Start) || !mirrors[0].End.Equal(it.End) || *mirrors[0].Source != src {
		t.Fatalf("inventory reads %+v, CreateBlock returned %+v", mirrors, it)
	}
	var live []yclientsstub.Record
	for _, r := range f.stub.Records() {
		if !r.Deleted {
			live = append(live, r)
		}
	}
	if len(live) != 1 || live[0].Length != 3600 || !live[0].Start.Equal(start) {
		t.Fatalf("records %+v, want one of an hour at 12:00", live)
	}
}
