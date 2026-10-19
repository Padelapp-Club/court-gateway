package viva

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

func (d *Driver) Inventory(ctx context.Context, q driver.OccupancyQuery) (driver.Inventory, error) {
	occ, err := d.occupancy(ctx, q, true)
	if err != nil {
		return driver.Inventory{}, err
	}
	return driver.ClassifyInventory(ctx, d, q, occ)
}
func (d *Driver) Inspect(ctx context.Context, id string) ([]driver.InventoryItem, error) {
	kind, uuid, ok := parseID(id)
	if !ok {
		return nil, driver.ErrNotFound
	}
	s, raw, found, err := d.read(ctx, kind, uuid)
	if err != nil || !found {
		return nil, err
	}
	if s.Canceled != nil && *s.Canceled {
		return nil, nil
	}
	var note string
	if r, ok := raw["comment"]; ok && string(r) != "null" && json.Unmarshal(r, &note) != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "Viva: invalid comment")
	}
	classified := driver.ClassifyNote(note, kind == "breaks", true)
	if classified.Class == driver.PartnerBooking && d.checkOwnership(kind, raw, classified.Partner) != nil {
		classified.Class = driver.ClubBooking
	}
	ivs, err := d.intervals(kind, id[:2], s, d.byRoom)
	if err != nil {
		return nil, err
	}
	if len(ivs) == 0 && classified.Class == driver.MirrorBooking { // retain evidence of a removed mapping
		st, _ := parseTime(s.TimeFrom)
		en, _ := parseTime(s.TimeTo)
		ivs = []driver.Interval{{ID: id, Start: st, End: en}}
	}
	out := []driver.InventoryItem{}
	for _, iv := range ivs {
		item := classified
		item.Interval = iv
		out = append(out, item)
	}
	return out, nil
}
func (d *Driver) CreateBlock(ctx context.Context, r driver.BlockRequest) error {
	m, err := driver.MirrorMarker(r.Source, r.SourceID)
	if err != nil {
		return err
	}
	_, err = d.Create(ctx, driver.CreateRequest{Court: r.Court, Start: r.Start, End: r.End, Kind: driver.Kind("mirror"), Partner: m})
	return err
}
func (d *Driver) DeleteBlock(ctx context.Context, want driver.InventoryItem) error {
	if !strings.HasPrefix(want.ID, "b-") {
		return driver.ErrNotOwned
	}
	got, err := d.Inspect(ctx, want.ID)
	if err != nil {
		return err
	}
	if len(got) == 0 {
		return nil
	}
	if len(got) != 1 || !driver.SameBlock(got[0], want) {
		return driver.ErrNotOwned
	}
	m, err := driver.MirrorMarker(want.Source, want.SourceID)
	if err != nil {
		return err
	}
	return d.Cancel(ctx, want.ID, m)
}
