package lunda

import (
	"context"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

func (d *Driver) Inventory(ctx context.Context, q driver.OccupancyQuery) (driver.Inventory, error) {
	occ, err := d.occupancy(ctx, q, true)
	if err != nil {
		return driver.Inventory{}, err
	}
	out := driver.Inventory{Complete: occ.Complete}
	seen := map[string]bool{}
	for _, iv := range occ.Intervals {
		if strings.HasPrefix(iv.ID, "ls-") {
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
	return out, nil
}
func (d *Driver) Inspect(ctx context.Context, id string) ([]driver.InventoryItem, error) {
	uid, ok := parseID(id)
	if !ok {
		return nil, driver.ErrNotSupported
	}
	b, found, err := d.load(ctx, uid)
	if err != nil || !found {
		return nil, err
	}
	if b.cancelled() {
		return nil, nil
	}
	note := ""
	if b.Notes != nil {
		note = *b.Notes
	}
	it := driver.ClassifyNote(note, d.service != "" && b.customer() == d.service, d.service != "" && b.customer() == d.service)
	var ivs []driver.Interval
	for _, s := range b.Slots {
		start, ok1 := parseLocal(s.Begin, d.loc)
		end, ok2 := parseLocal(s.End, d.loc)
		if !ok1 || !ok2 || !end.After(start) {
			return nil, driver.Upstream(driver.UpstreamInvalid, "Lunda: invalid mirror interval")
		}
		ivs = append(ivs, driver.Interval{ID: id, Court: d.byUID[s.CourtUID], Start: start, End: end})
	}
	if len(ivs) == 0 {
		return nil, driver.Upstream(driver.UpstreamInvalid, "Lunda: missing slots")
	}
	var out []driver.InventoryItem
	for _, iv := range merge(ivs) {
		v := it
		v.Interval = iv
		out = append(out, v)
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
func (d *Driver) Horizon(ctx context.Context) (time.Duration, error) {
	cfg, err := readClubSettings(ctx, d.c)
	if err != nil {
		return 0, err
	}
	return time.Duration(min(14, cfg.depthDays(14))) * 24 * time.Hour, nil
}
