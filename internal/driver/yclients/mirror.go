package yclients

import (
	"context"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"strings"
)

func (d *Driver) Inventory(ctx context.Context, q driver.OccupancyQuery) (driver.Inventory, error) {
	occ, err := d.Occupancy(ctx, q)
	if err != nil {
		return driver.Inventory{}, err
	}
	out := driver.Inventory{Complete: occ.Complete}
	seen := map[string]bool{}
	for _, iv := range occ.Intervals {
		if strings.HasPrefix(iv.ID, "yh-") {
			out.Items = append(out.Items, driver.InventoryItem{Interval: iv})
			continue
		}
		if seen[iv.ID] {
			continue
		}
		seen[iv.ID] = true
		items, err := d.Inspect(ctx, iv.ID)
		if err != nil {
			return driver.Inventory{}, err
		}
		for _, it := range items {
			if it.Start.Before(q.To) && it.End.After(q.From) {
				out.Items = append(out.Items, it)
			}
		}
	}
	records, err := d.fetchRecords(ctx, localDay(q.From, d.loc).AddDate(0, 0, -1), localDay(q.To, d.loc))
	if err != nil {
		return driver.Inventory{}, err
	}
	for _, r := range records {
		if _, mapped := d.staff[r.StaffID.String()]; mapped || r.Deleted {
			continue
		}
		if r.Comment == nil {
			continue
		}
		class := driver.ClassifyNote(*r.Comment, true, false).Class
		if class != driver.MirrorBooking && class != driver.DamagedMirror {
			continue
		}
		items, e := d.Inspect(ctx, "y-"+r.ID.String())
		if e != nil {
			return driver.Inventory{}, e
		}
		for _, it := range items {
			if it.Start.Before(q.To) && it.End.After(q.From) {
				out.Items = append(out.Items, it)
			}
		}
	}
	return out, nil
}
func (d *Driver) Inspect(ctx context.Context, id string) ([]driver.InventoryItem, error) {
	ids, ok := recordIDs(id)
	if !ok {
		return nil, driver.ErrNotSupported
	}
	parts, _, err := d.readParts(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []driver.InventoryItem
	for _, p := range parts {
		note := ""
		if p.rec.Comment != nil {
			note = *p.rec.Comment
		}
		owner := p.rec.Client != nil && phoneFits(p.rec.Client.Phone, d.client.phone)
		it := driver.ClassifyNote(note, owner, owner)
		it.Interval = driver.Interval{ID: "y-" + p.id, Court: d.staff[p.rec.StaffID.String()], Start: p.start, End: p.end}
		out = append(out, it)
	}
	// A complete tiled booking is one interval; a partial or edited one is
	// retained as its records, never widened over a hole.
	if len(out) > 1 {
		first := out[0]
		same := true
		for _, it := range out[1:] {
			if it.Class != first.Class || it.Source != first.Source || it.SourceID != first.SourceID || it.Court != first.Court || !it.Start.Equal(first.End) {
				same = false
				break
			}
			first.End = it.End
		}
		if same {
			first.ID = id
			out = []driver.InventoryItem{first}
		}
	}
	return out, nil
}
func (d *Driver) CreateBlock(ctx context.Context, r driver.BlockRequest) error {
	m, err := driver.MirrorMarker(r.Source, r.SourceID)
	if err != nil {
		return err
	}
	_, err = d.Create(ctx, driver.CreateRequest{Court: r.Court, Start: r.Start, End: r.End, Kind: driver.KindGame, Partner: m})
	return err
}
func (d *Driver) DeleteBlock(ctx context.Context, want driver.InventoryItem) error {
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

func (d *Driver) RemovalCount(it driver.InventoryItem) int {
	ids, ok := recordIDs(it.ID)
	if !ok {
		return 1
	}
	return len(ids)
}
