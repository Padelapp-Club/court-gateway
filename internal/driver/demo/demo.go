// Package demo is an in-memory teaching driver. It shows the minimum a CRM
// driver has to do, and it runs the same contract suite as real drivers,
// which proves the core does not depend on any particular CRM.
//
// Nothing is persisted: restarting the process forgets every booking.
package demo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

type record struct {
	court     string
	start     time.Time
	end       time.Time
	partner   string // empty for bookings made "by the club"
	cancelled bool
	paid      bool
}

// Driver is the demo driver.
type Driver struct {
	mu      sync.Mutex
	courts  map[string]bool
	studio  string
	records map[string]*record
	now     func() time.Time
}

var _ driver.Driver = (*Driver)(nil)

// ClubCRMKeys are the club-level keys of the crm section.
var ClubCRMKeys = []string{"studio"}

// New is the driver.Factory for crm.type "demo".
func New(cfg driver.Config) (driver.Driver, error) { return NewDriver(cfg) }

// NewDriver builds a demo driver. Its only driver-specific key is the
// optional crm.studio, which names one of the demo account's studios.
func NewDriver(cfg driver.Config) (*Driver, error) {
	var errs []string
	if err := cfg.CRM.RejectUnknown("crm", "studio"); err != nil {
		errs = append(errs, err.Error())
	}
	st, err := cfg.CRM.String("crm", "studio", false)
	if err != nil {
		errs = append(errs, err.Error())
	} else if st != "" && studioByID(st) == nil {
		errs = append(errs, fmt.Sprintf("crm.studio: «%s» — в учебной CRM есть студии %s", st, studioIDs()))
	}
	d := &Driver{courts: map[string]bool{}, studio: st, records: map[string]*record{}, now: cfg.Now}
	if d.now == nil {
		d.now = time.Now
	}
	for i, c := range cfg.Courts {
		if err := c.Params.RejectUnknown(fmt.Sprintf("courts[%d]", i)); err != nil {
			errs = append(errs, err.Error())
		}
		d.courts[c.ID] = true
	}
	if len(cfg.Courts) == 0 {
		errs = append(errs, "courts: нужен хотя бы один корт")
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return d, nil
}

// Studio implements driver.AccountClub.
func (d *Driver) Studio() string { return d.studio }

// Capabilities: the demo CRM supports payment marks.
func (d *Driver) Capabilities() driver.Capabilities { return driver.Capabilities{Payments: true} }

// Ready implements driver.Driver: the demo CRM is always reachable.
func (d *Driver) Ready(context.Context) error { return nil }

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "d-" + hex.EncodeToString(b[:])
}

// Seed adds a booking that was not made through the gateway, as a club
// administrator would. It returns the booking id.
func (d *Driver) Seed(court string, start, end time.Time) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := newID()
	d.records[id] = &record{court: court, start: start, end: end}
	return id
}

// Occupancy implements driver.Driver. The demo store is always complete.
func (d *Driver) Occupancy(_ context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := map[string]bool{}
	for _, c := range q.Courts {
		if !d.courts[c] {
			return driver.Occupancy{}, driver.ErrNotFound
		}
		want[c] = true
	}
	occ := driver.Occupancy{Complete: true}
	for id, r := range d.records {
		if !r.cancelled && want[r.court] && r.start.Before(q.To) && r.end.After(q.From) {
			occ.Intervals = append(occ.Intervals, driver.Interval{ID: id, Court: r.court, Start: r.start, End: r.end})
		}
	}
	return occ, nil
}

// PriceTable implements driver.Driver: 2000 per hour from 07:00 to 23:00
// every day, plus 500 per hour on weekday evenings from 18:00.
func (d *Driver) PriceTable(_ context.Context, court string) (driver.PriceTable, error) {
	if !d.courts[court] {
		return driver.PriceTable{}, driver.ErrNotFound
	}
	weekdays := driver.Weekdays{true, true, true, true, true, false, false}
	return driver.PriceTable{
		Base:        []driver.PriceRule{{Days: driver.AllWeekdays, From: 7 * 60, To: 23 * 60, PerHour: 2000_00}},
		Adjustments: []driver.PriceAdjustment{{Days: weekdays, From: 18 * 60, To: 23 * 60, PerHourDelta: 500_00}},
	}, nil
}

// Create implements driver.Driver. The note is accepted and dropped: the
// demo has nowhere to show it.
func (d *Driver) Create(_ context.Context, req driver.CreateRequest) (driver.Booking, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.courts[req.Court] {
		return driver.Booking{}, driver.ErrNotFound
	}
	for _, r := range d.records {
		if !r.cancelled && r.court == req.Court && r.start.Before(req.End) && r.end.After(req.Start) {
			return driver.Booking{}, driver.ErrSlotUnavailable
		}
	}
	id := newID()
	d.records[id] = &record{court: req.Court, start: req.Start, end: req.End, partner: req.Partner}
	return driver.Booking{ID: id, Court: req.Court, Start: req.Start, End: req.End, Status: driver.StatusActive}, nil
}

// Get implements driver.Driver.
func (d *Driver) Get(_ context.Context, id string) (driver.Booking, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.records[id]
	if r == nil {
		return driver.Booking{}, driver.ErrNotFound
	}
	st := driver.StatusActive
	if r.cancelled {
		st = driver.StatusCancelled
	}
	return driver.Booking{ID: id, Court: r.court, Start: r.start, End: r.end, Status: st}, nil
}

// Cancel implements driver.Driver.
func (d *Driver) Cancel(_ context.Context, id, partner string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.records[id]
	if r == nil || r.cancelled {
		return nil
	}
	if r.partner == "" || r.partner != partner {
		return driver.ErrNotOwned
	}
	if !r.start.After(d.now()) {
		return driver.ErrStarted
	}
	r.cancelled = true
	return nil
}

// MarkPaid implements driver.Driver.
func (d *Driver) MarkPaid(_ context.Context, id, partner string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.records[id]
	if r == nil || r.cancelled {
		return driver.ErrNotFound
	}
	if r.partner == "" || r.partner != partner {
		return driver.ErrNotOwned
	}
	r.paid = true
	return nil
}
