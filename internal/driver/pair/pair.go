// Package pair combines a primary CRM with Lunda without keeping a booking
// registry. Primary ids remain public; Lunda notes link the two halves.
package pair

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

const MirrorType = "lunda"
const MirrorCourtKey = "lunda_court"

func Supported(primary, mirror string) bool {
	return mirror == MirrorType && (primary == "viva" || primary == "yclients" || primary == "fitness1c")
}

type Options struct {
	PrimaryType             string
	Primary, Mirror         driver.Driver
	Courts                  []string
	Clock                   driver.Clock
	Lock                    driver.CourtLocker
	MaxRemovals, MaxCreates int
	Logger                  *slog.Logger
}
type Driver struct {
	a, b              driver.Driver
	ma, mb            driver.Mirrorer
	typ               string
	courts            []string
	clock             driver.Clock
	lock              driver.CourtLocker
	removals, creates int
	log               *slog.Logger
	pass              sync.Mutex
	mu                sync.Mutex
	alarm, writeAlarm error
	// Only consecutive-pass evidence, never a source/target mapping. A
	// restart discards it and requires two new complete passes.
	absent map[string]bool
	halves map[string]bool
	capped int
}

func New(o Options) (*Driver, error) {
	ma, aok := o.Primary.(driver.Mirrorer)
	mb, bok := o.Mirror.(driver.Mirrorer)
	if !aok || !bok {
		return nil, fmt.Errorf("mirror: обе CRM должны поддерживать инвентаризацию и отражения")
	}
	if !Supported(o.PrimaryType, MirrorType) {
		return nil, fmt.Errorf("mirror: основная CRM — Viva, YCLIENTS или 1С")
	}
	if v, ok := o.Primary.(interface{ MirrorConfigured() bool }); ok && !v.MirrorConfigured() {
		return nil, fmt.Errorf("crm.mirror_client_phone и crm.mirror_service: нужны отдельный клиент отражений 1С и его услуга")
	}
	if o.Clock == nil || o.Lock == nil || len(o.Courts) == 0 {
		return nil, fmt.Errorf("mirror: нужны часы, блокировка и сопоставленные корты")
	}
	if o.MaxRemovals < 0 || o.MaxCreates < 1 {
		return nil, fmt.Errorf("mirror: неверные ограничения записей")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Driver{a: o.Primary, b: o.Mirror, ma: ma, mb: mb, typ: o.PrimaryType, courts: o.Courts, clock: o.Clock, lock: o.Lock, removals: o.MaxRemovals, creates: o.MaxCreates, log: o.Logger, absent: map[string]bool{}, halves: map[string]bool{}}, nil
}
func (d *Driver) Capabilities() driver.Capabilities { return d.a.Capabilities() }
func resource(d driver.Driver, c string) string {
	if r, ok := d.(driver.Resourcer); ok {
		return r.Resource(c)
	}
	return fmt.Sprintf("%p %s", d, c)
}
func (d *Driver) Resources(c string) []string { return []string{resource(d.a, c), resource(d.b, c)} }
func (d *Driver) Resource(c string) string    { return strings.Join(d.Resources(c), " | ") }
func (d *Driver) Studio() string {
	if a, ok := d.a.(driver.AccountClub); ok {
		return a.Studio()
	}
	return ""
}
func (d *Driver) horizon(ctx context.Context) (time.Duration, error) {
	h := 14 * 24 * time.Hour
	for _, v := range []driver.Driver{d.a, d.b} {
		if x, ok := v.(driver.Horizon); ok {
			v, e := x.Horizon(ctx)
			if e != nil {
				return 0, e
			}
			h = min(h, v)
		}
	}
	if h <= 0 {
		return 0, driver.Upstream(driver.UpstreamInvalid, "mirror: invalid horizon")
	}
	return h, nil
}
func (d *Driver) Occupancy(ctx context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	a, e := d.a.Occupancy(ctx, q)
	if e != nil {
		return driver.Occupancy{}, e
	}
	b, e := d.b.Occupancy(ctx, q)
	if e != nil {
		return driver.Occupancy{}, e
	}
	h, e := d.horizon(ctx)
	if e != nil {
		return driver.Occupancy{}, e
	}
	now, known := d.clock()
	return driver.Occupancy{Complete: a.Complete && b.Complete && known && !q.To.After(now.Add(h)), Intervals: append(a.Intervals, b.Intervals...)}, nil
}
func (d *Driver) PriceTable(ctx context.Context, c string) (driver.PriceTable, error) {
	return d.a.PriceTable(ctx, c)
}
func (d *Driver) Get(ctx context.Context, id string) (driver.Booking, error) { return d.a.Get(ctx, id) }
func (d *Driver) MarkPaid(ctx context.Context, id, p string) error           { return d.a.MarkPaid(ctx, id, p) }
func (d *Driver) CheckConfig(ctx context.Context) error {
	for _, v := range []driver.Driver{d.a, d.b} {
		if c, ok := v.(driver.ConfigChecker); ok {
			if e := c.CheckConfig(ctx); e != nil {
				return e
			}
		}
	}
	return nil
}
func (d *Driver) Ready(ctx context.Context) error {
	if e := d.a.Ready(ctx); e != nil {
		return e
	}
	if e := d.b.Ready(ctx); e != nil {
		return e
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return errors.Join(d.alarm, d.writeAlarm)
}
func alert(code, msg string) error { return &driver.ConfigAlert{Code: code, Problems: []string{msg}} }
func (d *Driver) writeFailure() {
	e := alert("MIRROR_HALF_BOOKING", "Возможна бронь только в одной CRM. Клубу и оператору: проверьте обе CRM; автоматическая отмена запрещена.")
	d.mu.Lock()
	d.writeAlarm = e
	d.mu.Unlock()
	d.log.Error(e.Error())
}
func (d *Driver) Create(ctx context.Context, r driver.CreateRequest) (driver.Booking, error) {
	h, e := d.horizon(ctx)
	if e != nil {
		return driver.Booking{}, e
	}
	now, known := d.clock()
	if !known {
		return driver.Booking{}, driver.ErrClockUnknown
	}
	if r.End.After(now.Add(h)) {
		return driver.Booking{}, driver.ErrSlotUnavailable
	}
	a, e := d.a.Create(ctx, r)
	if e != nil {
		d.writeFailure()
		return driver.Booking{}, e
	}
	r.Note = "[cg-pair:" + a.ID + "] " + r.Note
	if _, e = d.b.Create(ctx, r); e == nil {
		return a, nil
	}
	rollback, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer stop()
	if ce := d.a.Cancel(rollback, a.ID, r.Partner); ce != nil {
		d.writeFailure()
		return driver.Booking{}, errors.Join(e, ce)
	}
	// A lost response from Lunda can still have left its half behind.
	d.writeFailure()
	return driver.Booking{}, e
}
func (d *Driver) Cancel(ctx context.Context, id, p string) error {
	now, known := d.clock()
	if !known {
		return driver.ErrClockUnknown
	}
	h, e := d.horizon(ctx)
	if e != nil {
		return e
	}
	inv, e := d.mb.Inventory(ctx, driver.OccupancyQuery{Courts: d.courts, From: now, To: now.Add(h)})
	if e != nil {
		return e
	}
	if !inv.Complete {
		return driver.Upstream(driver.UpstreamInvalid, "mirror: incomplete cancellation lookup")
	}
	var matches []driver.InventoryItem
	for _, it := range inv.Items {
		if it.Class == driver.PartnerBooking && it.Partner == p && it.PairID == id {
			matches = append(matches, it)
		}
	}
	// Cancel the primary even if Get says cancelled; Get alone cannot prove
	// the second half is gone. Each CRM rechecks the caller's ownership.
	b, ge := d.a.Get(ctx, id)
	if ge != nil && !errors.Is(ge, driver.ErrNotFound) {
		return ge
	}
	court := b.Court
	if court == "" && len(matches) > 0 {
		court = matches[0].Court
	}
	if court == "" {
		return d.a.Cancel(ctx, id, p)
	}
	unlock, e := d.lock(ctx, court)
	if e != nil {
		return e
	}
	defer unlock()
	var errs []error
	if e := d.a.Cancel(ctx, id, p); e != nil {
		errs = append(errs, e)
	}
	for _, it := range matches {
		if it.Court != court {
			errs = append(errs, driver.ErrCourtNotConfigured)
			continue
		}
		if e := d.b.Cancel(ctx, it.ID, p); e != nil {
			errs = append(errs, e)
		}
	}
	e = errors.Join(errs...)
	if e != nil {
		d.writeFailure()
	}
	return e
}

// Close releases driver-owned sessions in local callers and tests. Shared
// installation accounts retain their single session until service exit.
func (d *Driver) Close() {
	for _, v := range []driver.Driver{d.a, d.b} {
		if c, ok := v.(interface{ Close() }); ok {
			c.Close()
		}
	}
}
