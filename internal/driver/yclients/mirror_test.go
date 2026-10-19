package yclients_test

import (
	"context"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
	"testing"
	"time"
)

func TestMirrorBlocksAndOwnership(t *testing.T) {
	f := newFixture(t, opts{})
	m := driver.Mirrorer(f.drv)
	start := time.Now().Add(48 * time.Hour).Truncate(time.Hour)
	req := driver.BlockRequest{Interval: driver.Interval{Court: "court-1", Start: start, End: start.Add(3 * time.Hour)}, Source: "lunda", SourceID: "l-source"}
	if e := m.CreateBlock(context.Background(), req); e != nil {
		t.Fatal(e)
	}
	q := driver.OccupancyQuery{Courts: []string{"court-1"}, From: start, To: start.Add(3 * time.Hour)}
	inv, e := m.Inventory(context.Background(), q)
	if e != nil || !inv.Complete || len(inv.Items) != 1 {
		t.Fatal(inv, e)
	}
	block := inv.Items[0]
	if block.Class != driver.MirrorBooking || block.SourceID != "l-source" || !driver.SameInterval(block.Interval, req.Interval) {
		t.Fatal(block)
	}
	if e := m.DeleteBlock(context.Background(), block); e != nil {
		t.Fatal(e)
	}
	id := f.stub.Seed(yclientsstub.Record{StaffID: staff1, Start: start, Length: 3600, Comment: "[cg-mirror:lunda:l-source]", ClientPhone: "71111111111", ClientName: "club"})
	_ = id
	inv, e = m.Inventory(context.Background(), q)
	if e != nil || len(inv.Items) != 1 || inv.Items[0].Class != driver.ClubBooking {
		t.Fatal(inv, e)
	}
	forged := inv.Items[0]
	forged.Class = driver.MirrorBooking
	forged.Source = "lunda"
	forged.SourceID = "l-source"
	if e := m.DeleteBlock(context.Background(), forged); e == nil {
		t.Fatal("club record deleted")
	}
}
