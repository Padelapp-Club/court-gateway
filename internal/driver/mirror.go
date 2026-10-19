package driver

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// Inventory is private to reconciliation, never a partner response. Each
// item is one whole interval, including intervals crossing the query edges.
type Inventory struct {
	Complete bool
	Items    []InventoryItem
}
type InventoryClass uint8

const (
	ClubBooking InventoryClass = iota
	PartnerBooking
	MirrorBooking
	DamagedMirror
)

type InventoryItem struct {
	Interval
	Class    InventoryClass
	Source   string
	SourceID string
	Partner  string
	PairID   string
}
type BlockRequest struct {
	Interval
	Source   string
	SourceID string
}

// Mirrorer inventories occupancy with ownership evidence and writes only
// its own blocks. Inspect returns all current intervals of an id; no such
// active booking is an empty list. ErrNotSupported requires two complete
// passes before removal. DeleteBlock rechecks class, marker and interval.
type Mirrorer interface {
	Inventory(context.Context, OccupancyQuery) (Inventory, error)
	Inspect(context.Context, string) ([]InventoryItem, error)
	CreateBlock(context.Context, BlockRequest) error
	DeleteBlock(context.Context, InventoryItem) error
}

// Horizon is the current booking depth, measured from the signed time.
// RemovalCounter counts physical records removed by one logical block deletion.
type RemovalCounter interface{ RemovalCount(InventoryItem) int }

type Horizon interface {
	Horizon(context.Context) (time.Duration, error)
}
type Reconciler interface{ Reconcile(context.Context) error }
type MultiResourcer interface{ Resources(string) []string }
type CourtLocker func(context.Context, string) (func(), error)

var mirrorRE = regexp.MustCompile(`^\[cg-mirror:(viva|yclients|fitness1c|lunda):([A-Za-z0-9_-]+)\](?:$|\s)`)
var partnerRE = regexp.MustCompile(`^\[cg:([a-z0-9][a-z0-9_-]{0,31})\](?:$|\s)`)
var pairRE = regexp.MustCompile(`^\[cg-pair:([A-Za-z0-9_-]+)\](?:$|\s)`)

func MirrorMarker(source, id string) (string, error) {
	s := "[cg-mirror:" + source + ":" + id + "]"
	if mirrorRE.FindStringSubmatch(s) == nil {
		return "", ErrNotOwned
	}
	return s, nil
}

// ClassifyNote requires independent object-class evidence from the CRM.
// A damaged mirror is quarantined even when the object class is foreign.
func ClassifyNote(note string, mirrorOwner, partnerOwner bool) InventoryItem {
	note = strings.TrimSpace(note)
	out := InventoryItem{Class: ClubBooking}
	if strings.HasPrefix(note, "[cg-mirror") {
		m := mirrorRE.FindStringSubmatch(note)
		if m == nil {
			out.Class = DamagedMirror
			return out
		}
		if mirrorOwner {
			out.Class, out.Source, out.SourceID = MirrorBooking, m[1], m[2]
		}
		return out
	}
	if m := partnerRE.FindStringSubmatch(note); m != nil && partnerOwner {
		out.Class, out.Partner = PartnerBooking, m[1]
		if p := pairRE.FindStringSubmatch(strings.TrimSpace(note[len(m[0]):])); p != nil {
			out.PairID = p[1]
		}
	}
	return out
}
func SameInterval(a, b Interval) bool {
	return a.Court == b.Court && a.Start.Equal(b.Start) && a.End.Equal(b.End)
}
func SameBlock(a, b InventoryItem) bool {
	return a.ID == b.ID && a.Class == MirrorBooking && b.Class == MirrorBooking && a.Source == b.Source && a.SourceID == b.SourceID && SameInterval(a.Interval, b.Interval)
}

// ReadInventory uses the driver's completeness rules, then point reads to
// classify records. A record changed between the reads uses its new spans.
func ReadInventory(ctx context.Context, d Driver, m Mirrorer, q OccupancyQuery) (Inventory, error) {
	occ, err := d.Occupancy(ctx, q)
	if err != nil {
		return Inventory{}, err
	}
	return ClassifyInventory(ctx, m, q, occ)
}

func ClassifyInventory(ctx context.Context, m Mirrorer, q OccupancyQuery, occ Occupancy) (Inventory, error) {
	out := Inventory{Complete: occ.Complete}
	seen := map[string]bool{}
	for _, iv := range occ.Intervals {
		if seen[iv.ID] {
			continue
		}
		seen[iv.ID] = true
		items, err := m.Inspect(ctx, iv.ID)
		if err != nil {
			return Inventory{}, err
		}
		for _, item := range items {
			if item.Start.Before(q.To) && item.End.After(q.From) {
				out.Items = append(out.Items, item)
			}
		}
	}
	return out, nil
}
