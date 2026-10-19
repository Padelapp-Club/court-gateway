package fitness1c

import (
	"context"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

func (d *Driver) MirrorConfigured() bool { return d.mirror != nil }
func (d *Driver) Inventory(ctx context.Context, q driver.OccupancyQuery) (driver.Inventory, error) {
	if d.mirror == nil {
		return driver.Inventory{}, driver.ErrBookingNotConfigured
	}
	occ, err := d.Occupancy(ctx, q)
	if err != nil {
		return driver.Inventory{}, err
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return driver.Inventory{}, err
	}
	own, a, err := d.ownRentals(ctx, loc)
	if err != nil {
		return driver.Inventory{}, err
	}
	blocks, b, err := d.mirror.ownRentals(ctx, loc)
	if err != nil {
		return driver.Inventory{}, err
	}
	out := driver.Inventory{Complete: occ.Complete && a && b}
	classes := map[string]driver.InventoryClass{}
	for _, r := range own {
		classes[ownID(r.id)] = driver.PartnerBooking
	}
	for _, r := range blocks {
		classes[ownID(r.id)] = driver.MirrorBooking
	}
	for _, iv := range occ.Intervals {
		out.Items = append(out.Items, driver.InventoryItem{Interval: iv, Class: classes[iv.ID], Partner: d.c.partner})
	}
	for _, r := range blocks {
		if _, ok := d.byRoom[r.room]; !ok && r.Start.Before(q.To) && r.End.After(q.From) {
			out.Items = append(out.Items, driver.InventoryItem{Interval: driver.Interval{ID: ownID(r.id), Start: r.Start, End: r.End}, Class: driver.MirrorBooking})
		}
	}
	return out, nil
}
func (d *Driver) Inspect(ctx context.Context, id string) ([]driver.InventoryItem, error) {
	// The grid has no booking ids, and a markerless mirror has no source id.
	return nil, driver.ErrNotSupported
}
func (d *Driver) CreateBlock(ctx context.Context, r driver.BlockRequest) error {
	if d.mirror == nil {
		return driver.ErrBookingNotConfigured
	}
	_, err := d.mirror.Create(ctx, driver.CreateRequest{Court: r.Court, Start: r.Start, End: r.End, Kind: driver.KindGame, Partner: "cg-mirror"})
	return err
}
func (d *Driver) DeleteBlock(ctx context.Context, want driver.InventoryItem) error {
	if d.mirror == nil || want.Class != driver.MirrorBooking {
		return driver.ErrNotOwned
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return err
	}
	blocks, complete, err := d.mirror.ownRentals(ctx, loc)
	if err != nil {
		return err
	}
	if !complete {
		return driver.Upstream(driver.UpstreamInvalid, "1C: incomplete mirror inventory")
	}
	for _, r := range blocks {
		got := driver.InventoryItem{Interval: driver.Interval{ID: ownID(r.id), Court: d.byRoom[r.room], Start: r.Start, End: r.End}, Class: driver.MirrorBooking}
		if driver.SameBlock(got, want) {
			return d.mirror.Cancel(ctx, want.ID, "cg-mirror")
		}
	}
	return driver.ErrNotOwned
}
