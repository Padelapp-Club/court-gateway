package yclients_test

import (
	"context"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// CreateBlock depends on the court's services as YCLIENTS gives them now,
// not on what an earlier call of the driver read: a driver that read the
// court with a 60-minute service and one that never read it both make a
// 45-minute block once the court offers 45 minutes.
func TestCreateBlockUsesTheCurrentServices(t *testing.T) {
	staff := func(minutes int) yclientsstub.Staff {
		return yclientsstub.Staff{ID: staff1, Name: "Корт 1", Bookable: true, Prepaid: "allowed", Services: []yclientsstub.Service{
			{ID: 101, Title: "Аренда", SeanceLength: minutes * 60, Price: 1000},
		}}
	}
	f := newFixture(t, opts{stub: func(s *yclientsstub.Stub) { s.AddStaff(staff(60)) }})
	ctx := context.Background()
	start := time.Date(2030, 1, 15, 12, 0, 0, 0, f.built.Settings.Location)
	if _, err := f.drv.MirrorInventory(ctx, driver.OccupancyQuery{Courts: []string{"court-1"}, From: start, To: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.stub.AddStaff(staff(45)) // the club now offers exactly 45 minutes
	req := driver.BlockRequest{Court: "court-1", Start: start, End: start.Add(45 * time.Minute), Source: driver.MirrorMarker{Src: "lunda", ID: "current"}}
	_, warmErr := f.drv.CreateBlock(ctx, req)
	cold := newFixture(t, opts{stub: func(s *yclientsstub.Stub) { s.AddStaff(staff(45)) }})
	_, coldErr := cold.drv.CreateBlock(ctx, req)
	if warmErr != nil || coldErr != nil {
		t.Fatalf("same services (45 minutes): driver that read the court before %v, fresh driver %v; want both made", warmErr, coldErr)
	}
}
