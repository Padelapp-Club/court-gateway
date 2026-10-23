// Package pair serves a club kept in two CRMs of one installation — its
// primary CRM (Viva, YCLIENTS or 1C:Fitness) and Lunda, the mirror — as one
// driver.Driver, so for partners the club is one gateway club:
//
//   - occupancy is the union of both, complete only when both are;
//   - a partner's booking is made in the primary, then in the mirror; when
//     the mirror refuses, the primary's is cancelled again and the partner
//     refused (never half a booking; a cancel that fails is tried again by
//     every pass until the booking starts);
//   - a cancel cancels the partner's booking in both;
//   - reads, prices and hours are the primary's.
//
// Reconcile is one pass of the reconciliation loop (reconcile.go): what the
// club booked directly in one CRM is blocked in the other.
// The pair keeps no CRM data between calls: which block mirrors which
// booking is read every pass from the blocks' markers in the CRMs.
package pair

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Defaults of Options.
const (
	DefaultMaxRemovals = 3
	DefaultMaxCreates  = 50
	DefaultLockWait    = 10 * time.Second
)

// rollbackTimeout bounds the cancel of the primary's half of a booking the
// mirror refused; it runs on after the partner's request ends.
const rollbackTimeout = 20 * time.Second

// Options configure a Pair.
type Options struct {
	// PrimaryType and MirrorType are the crm.type of each CRM: a mirror's
	// marker names the type of the CRM of its source.
	PrimaryType, MirrorType string
	// Courts are the club's court ids; every one is a room in both CRMs.
	Courts []string
	// Location and Hours are the club's zone and hours (core.Settings):
	// busy time a CRM does not explain is mirrored within the hours only.
	Location *time.Location
	Hours    [7]*[2]int
	// MaxRemovals is the fuse of a pass, MaxCreates the most mirrors it
	// makes (DefaultMaxRemovals, DefaultMaxCreates when 0).
	MaxRemovals, MaxCreates int
	// Clock is the decision clock (the hub's time): a pass without it does
	// nothing.
	Clock driver.Clock
	// Lock takes the process-wide lock of a court's CRM resources, the one
	// a partner's create holds (core.LockResources); LockWait bounds the
	// wait (DefaultLockWait).
	Lock     func(ctx context.Context, keys []string) (func(), error)
	LockWait time.Duration
	Logger   *slog.Logger
	// Memory is what the club's passes leave each other (nil: a new one).
	// The pair built for a new config of the club takes the Memory of the
	// pair before it (core keeps one per club), so the operator raising
	// the fuse does not restart the two passes a markerless removal needs.
	Memory *Memory
}

// Memory is what one pass of a club leaves the next, in memory only —
// never in a file, never in a CRM: the evidence of passes in a row (a
// mirror found invalid without a point read, a partner's booking found in
// one CRM), the passes cut off by MaxCreates, the rollbacks to try again,
// and the alarms. It outlives
// a pair: every new config of the club from the hub (the operator raising
// mirror.max_removals among them) builds a new pair, and handing it the
// Memory of the one before keeps that evidence. The passes of every
// pair of one Memory run one at a time. A restart loses it: a removal then
// waits for passes in a row again, and a failed rollback is not tried
// again (its half is left to the half-booking alarm).
type Memory struct {
	pass sync.Mutex // one pass of the club at a time

	mu        sync.Mutex
	alarms    []Alarm    // of the last pass
	sticky    []sticky   // raised outside a pass, kept for a few passes
	memo      memo       // what the previous pass saw
	rollbacks []rollback // primary's halves whose rollback failed (Create)
}

// NewMemory is the Memory of a club with no pass yet.
func NewMemory() *Memory { return &Memory{} }

// Pair is the driver of a club in two CRMs. Both drivers must be
// driver.Mirrorer.
type Pair struct {
	a, b   driver.Driver
	am, bm driver.Mirrorer
	o      Options
	log    *slog.Logger

	*Memory
}

var (
	_ driver.Driver         = (*Pair)(nil)
	_ driver.Resourcer      = (*Pair)(nil)
	_ driver.MultiResourcer = (*Pair)(nil)
	_ driver.ConfigChecker  = (*Pair)(nil)
)

// mirrorChecker is implemented by Mirrorers that need more config to keep
// blocks than to serve alone (1C:Fitness: the client of its blocks).
type mirrorChecker interface {
	MirrorCheck() error
}

// New builds the pair of a club's primary driver a and mirror driver b.
// A driver that cannot keep mirrors is refused: a club is never served by
// two CRMs without the loop that keeps them in step.
func New(a, b driver.Driver, o Options) (*Pair, error) {
	var problems []string
	am, ok := a.(driver.Mirrorer)
	if !ok {
		problems = append(problems, fmt.Sprintf("crm.type: CRM «%s» не умеет держать отражения броней — в паре с второй CRM не работает", crmName(o.PrimaryType)))
	}
	bm, ok := b.(driver.Mirrorer)
	if !ok {
		problems = append(problems, fmt.Sprintf("mirror.type: CRM «%s» не умеет держать отражения броней — второй CRM быть не может", crmName(o.MirrorType)))
	}
	for _, d := range []driver.Driver{a, b} {
		if c, ok := d.(mirrorChecker); ok {
			if err := c.MirrorCheck(); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	if o.MaxRemovals <= 0 {
		o.MaxRemovals = DefaultMaxRemovals
	}
	if o.MaxCreates <= 0 {
		o.MaxCreates = DefaultMaxCreates
	}
	if o.LockWait <= 0 {
		o.LockWait = DefaultLockWait
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.Clock == nil {
		o.Clock = func() (time.Time, bool) { return time.Now(), true }
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mem := o.Memory
	if mem == nil {
		mem = NewMemory()
	}
	return &Pair{a: a, b: b, am: am, bm: bm, o: o, log: log, Memory: mem}, nil
}

// Primary and Mirror are the pair's two drivers.
func (p *Pair) Primary() driver.Driver { return p.a }
func (p *Pair) Mirror() driver.Driver  { return p.b }

// crmName is a CRM as a club knows it.
func crmName(crmType string) string {
	if s, ok := driver.SetupFor(crmType); ok && s.Name != "" {
		return s.Name
	}
	return crmType
}

// Capabilities are the primary's: prices and payment marks come from it.
func (p *Pair) Capabilities() driver.Capabilities { return p.a.Capabilities() }

// both runs fa and fb at once.
func both(fa, fb func()) {
	var wg sync.WaitGroup
	wg.Go(fa)
	wg.Go(fb)
	wg.Wait()
}

// Ready implements driver.Driver: both CRMs are ready. A failure of the
// mirror is a *driver.MirrorError (readiness says MIRROR_<reason>); config
// alerts of either, and the alarms of the reconciliation, are one
// *driver.ConfigAlert while both CRMs are served.
func (p *Pair) Ready(ctx context.Context) error {
	var ea, eb error
	both(func() { ea = p.a.Ready(ctx) }, func() { eb = p.b.Ready(ctx) })
	return p.merge(ea, eb, p.alarmAlert())
}

// CheckConfig implements driver.ConfigChecker: each CRM's own check. A
// mirror's mismatch is the club's mismatch, its problems named as the
// mirror's.
func (p *Pair) CheckConfig(ctx context.Context) error {
	check := func(d driver.Driver) error {
		if c, ok := d.(driver.ConfigChecker); ok {
			return c.CheckConfig(ctx)
		}
		return nil
	}
	var ea, eb error
	both(func() { ea = check(p.a) }, func() { eb = check(p.b) })
	return p.merge(ea, eb, nil)
}

// merge combines the results of the two CRMs: the primary's failure, else
// the mirror's, else every alert (the loop's alarms first) as one.
func (p *Pair) merge(ea, eb error, loop *driver.ConfigAlert) error {
	var ca *driver.ConfigAlert
	var me *driver.MisconfigError
	switch {
	case ea != nil && !errors.As(ea, &ca):
		return ea
	case eb != nil && errors.As(eb, &me):
		out := &driver.MisconfigError{}
		for _, pr := range me.Problems {
			out.Problems = append(out.Problems, "mirror ("+crmName(p.o.MirrorType)+"): "+pr)
		}
		return out
	case eb != nil && !errors.As(eb, &ca):
		return &driver.MirrorError{Err: eb}
	}
	var alerts []*driver.ConfigAlert
	if loop != nil {
		alerts = append(alerts, loop)
	}
	if errors.As(ea, &ca) {
		alerts = append(alerts, ca)
	}
	if errors.As(eb, &ca) {
		mirrored := &driver.ConfigAlert{Code: "MIRROR_" + ca.Code}
		for _, pr := range ca.Problems {
			mirrored.Problems = append(mirrored.Problems, crmName(p.o.MirrorType)+": "+pr)
		}
		alerts = append(alerts, mirrored)
	}
	if len(alerts) == 0 {
		return nil
	}
	out := &driver.ConfigAlert{Code: alerts[0].Code}
	for _, a := range alerts {
		out.Problems = append(out.Problems, a.Problems...)
	}
	if loop != nil {
		out.Detail = loop.Detail // only the loop's texts go to the hub
	}
	return out
}

// Occupancy implements driver.Driver: the union of both CRMs, complete
// only when both answers are. An interval of the mirror that repeats one
// of the primary exactly (the mirror's half of a partner's booking, a
// block of the loop) is left out: the primary's id stands for it.
func (p *Pair) Occupancy(ctx context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	var oa, ob driver.Occupancy
	var ea, eb error
	both(func() { oa, ea = p.a.Occupancy(ctx, q) }, func() { ob, eb = p.b.Occupancy(ctx, q) })
	switch {
	case ea != nil:
		return driver.Occupancy{}, ea
	case eb != nil:
		return driver.Occupancy{}, &driver.MirrorError{Err: eb}
	}
	type key struct {
		court      string
		start, end int64
	}
	seen := map[key]bool{}
	out := driver.Occupancy{Complete: oa.Complete && ob.Complete, Intervals: append([]driver.Interval{}, oa.Intervals...)}
	for _, iv := range oa.Intervals {
		seen[key{iv.Court, iv.Start.UnixNano(), iv.End.UnixNano()}] = true
	}
	for _, iv := range ob.Intervals {
		if !seen[key{iv.Court, iv.Start.UnixNano(), iv.End.UnixNano()}] {
			out.Intervals = append(out.Intervals, iv)
		}
	}
	return out, nil
}

// PriceTable implements driver.Driver: the primary's prices.
func (p *Pair) PriceTable(ctx context.Context, court string) (driver.PriceTable, error) {
	return p.a.PriceTable(ctx, court)
}

// Create implements driver.Driver: the booking is made in the primary,
// then in the mirror (the core holds the lock of the court in both from
// its occupancy check on). When the mirror refuses or fails, the
// primary's is cancelled again under its own context and the partner gets
// the mirror's error; when that cancel fails too, the booking is in one
// CRM only: the partner gets an error that says so, the log an ERROR and
// the hub an alarm, and the passes try the cancel again (retryRollback).
// The partner's id is the primary's: the mirror's half is found by its
// court, time and the partner's marker.
func (p *Pair) Create(ctx context.Context, req driver.CreateRequest) (driver.Booking, error) {
	bk, err := p.a.Create(ctx, req)
	if err != nil {
		return driver.Booking{}, err
	}
	if _, err := p.b.Create(ctx, req); err != nil {
		rctx, stop := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer stop()
		if cerr := p.a.Cancel(rctx, bk.ID, req.Partner); cerr != nil {
			p.log.Error("a booking stays in the primary CRM only: the mirror refused it and its cancel failed",
				"booking", bk.ID, "court", req.Court, "start", req.Start.UTC().Format(time.RFC3339), "mirror_error", err, "cancel_error", cerr)
			a := Alarm{Code: AlarmHalfBooking, Problem: fmt.Sprintf(
				"корт %s, %s: бронь партнёра %s %s есть только в %s — %s её не приняла, а отменить её не удалось; шлюз повторяет отмену на каждой сверке до начала брони — если тревога не уходит, отмените бронь вручную",
				req.Court, p.span(req.Start, req.End), req.Partner, bk.ID, crmName(p.o.PrimaryType), crmName(p.o.MirrorType))}
			p.stick(a)
			p.mu.Lock()
			p.rollbacks = append(p.rollbacks, rollback{id: bk.ID, partner: req.Partner, court: req.Court, start: req.Start, alarm: a})
			p.mu.Unlock()
			return driver.Booking{}, fmt.Errorf("%w; the booking %s stays in the primary CRM: its cancel failed (%v)", &driver.MirrorError{Err: err}, bk.ID, cerr)
		}
		p.log.Warn("a booking the mirror refused was cancelled in the primary CRM again", "booking", bk.ID, "court", req.Court, "error", err)
		return driver.Booking{}, &driver.MirrorError{Err: err}
	}
	return bk, nil
}

// rollback is the primary's half of a partner's booking the mirror refused
// whose cancel failed.
type rollback struct {
	id, partner, court string
	start              time.Time
	alarm              Alarm
}

// retryRollback cancels again, under the court's lock, the first of the
// primary's halves whose rollback failed (Create) — one a pass, so a CRM
// that hangs on cancels takes at most one cancel's time from the pass's
// reads. The partner was refused, so the half is no one's booking. One that
// has started by now is dropped, never cancelled (a started booking never
// is), as is one the primary refuses for good: their alarm then runs out as
// any raised outside a pass. While a half waits, its alarm is kept up.
func (p *Pair) retryRollback(ctx context.Context, now time.Time) {
	p.mu.Lock()
	p.rollbacks = slices.DeleteFunc(p.rollbacks, func(r rollback) bool {
		if now.Before(r.start) {
			return false
		}
		p.log.Error("a booking stays in the primary CRM only: it started before its cancel went through", "booking", r.id, "court", r.court)
		return true
	})
	if len(p.rollbacks) == 0 {
		p.mu.Unlock()
		return
	}
	r := p.rollbacks[0]
	p.mu.Unlock()
	err := p.cancelHalf(ctx, r)
	p.mu.Lock()
	defer p.mu.Unlock()
	// a Create may have added halves meanwhile: r is taken out by its id
	p.rollbacks = slices.DeleteFunc(p.rollbacks, func(x rollback) bool { return x.id == r.id })
	switch {
	case err == nil || errors.Is(err, driver.ErrNotFound):
		p.log.Warn("a booking the mirror refused was cancelled in the primary CRM on a retry", "booking", r.id, "court", r.court)
		p.unstickLocked(r.alarm)
	case errors.Is(err, driver.ErrStarted) || errors.Is(err, driver.ErrNotOwned):
		p.log.Error("a booking stays in the primary CRM only: its cancel is refused", "booking", r.id, "court", r.court, "error", err)
	default:
		p.log.Warn("the cancel of a booking the mirror refused failed again; a later pass tries again", "booking", r.id, "court", r.court, "error", err)
		p.rollbacks = append(p.rollbacks, r) // behind the others
	}
	for _, w := range p.rollbacks {
		p.keepUpLocked(w.alarm)
	}
}

// pendingRollbacks are the ids of the primary's halves whose rollback waits.
func (p *Pair) pendingRollbacks() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]bool{}
	for _, r := range p.rollbacks {
		out[r.id] = true
	}
	return out
}

// cancelHalf cancels r in the primary under the court's lock.
func (p *Pair) cancelHalf(ctx context.Context, r rollback) error {
	unlock, err := p.lock(ctx, r.court)
	if err != nil {
		return err
	}
	defer unlock()
	rctx, stop := context.WithTimeout(ctx, rollbackTimeout)
	defer stop()
	return p.a.Cancel(rctx, r.id, r.partner)
}

// Get implements driver.Driver: the primary's booking.
func (p *Pair) Get(ctx context.Context, id string) (driver.Booking, error) { return p.a.Get(ctx, id) }

// Cancel implements driver.Driver: the partner's booking is cancelled in
// both CRMs, each by its own rules (its marker, not started). The mirror's
// half is the partner's own booking of the mirror on the same court and
// time; it is looked up before the primary's is cancelled. Success means
// both are gone (one gone already is no error); otherwise an error, and a
// retry is safe.
func (p *Pair) Cancel(ctx context.Context, id string, partner string) error {
	bk, err := p.a.Get(ctx, id)
	if errors.Is(err, driver.ErrNotFound) {
		return p.a.Cancel(ctx, id, partner) // gone, or no booking: as the primary says
	}
	if err != nil {
		return err
	}
	// the court's lock in both CRMs, the one a partner's create and the
	// loop's writes hold: no write of the loop on the court goes in
	// between the two cancels
	unlock, err := p.lock(ctx, bk.Court)
	if err != nil {
		return err
	}
	defer unlock()
	halves, err := p.halves(ctx, bk, partner)
	if err != nil {
		return &driver.MirrorError{Err: err}
	}
	if err := p.a.Cancel(ctx, id, partner); err != nil {
		return err
	}
	for _, h := range halves {
		if err := p.b.Cancel(ctx, h.ID, partner); err != nil {
			return &driver.MirrorError{Err: err}
		}
	}
	return nil
}

// halves finds the mirror's half of a partner's booking: the partner's own
// bookings of the mirror on its court with its start and end.
func (p *Pair) halves(ctx context.Context, bk driver.Booking, partner string) ([]driver.MirrorItem, error) {
	inv, err := p.bm.MirrorInventory(ctx, driver.OccupancyQuery{Courts: []string{bk.Court}, From: bk.Start, To: bk.End})
	if err != nil {
		return nil, err
	}
	var out []driver.MirrorItem
	for _, it := range inv.Items {
		if it.Class == driver.ClassOwn && it.Partner == partner && it.Court == bk.Court && it.Start.Equal(bk.Start) && it.End.Equal(bk.End) {
			out = append(out, it)
		}
	}
	return out, nil
}

// MarkPaid implements driver.Driver: the primary's mark; nothing is ever
// paid in the mirror (the gateway never writes a payment to Lunda's till).
func (p *Pair) MarkPaid(ctx context.Context, id string, partner string) error {
	return p.a.MarkPaid(ctx, id, partner)
}

// resourceOf is the CRM resource of court in d (as the core names it).
func resourceOf(d driver.Driver, court string) string {
	if r, ok := d.(driver.Resourcer); ok {
		return r.Resource(court)
	}
	return fmt.Sprintf("%p %s", d, court)
}

// Resources implements driver.MultiResourcer: the court's room in each
// CRM; the core locks both for a partner's create, the loop for its own.
func (p *Pair) Resources(court string) []string {
	return []string{resourceOf(p.a, court), resourceOf(p.b, court)}
}

// Resource implements driver.Resourcer: both rooms in one name (a court
// whose room changes in either CRM is another court).
func (p *Pair) Resource(court string) string { return strings.Join(p.Resources(court), " + ") }

// Studio implements driver.AccountClub: the primary's club of its account.
func (p *Pair) Studio() string {
	if ac, ok := p.a.(driver.AccountClub); ok {
		return ac.Studio()
	}
	return ""
}

// Close stops what either driver runs on its own.
func (p *Pair) Close() {
	for _, d := range []driver.Driver{p.a, p.b} {
		if c, ok := d.(interface{ Close() }); ok {
			c.Close()
		}
	}
}

// lock takes the lock of court in both CRMs, within LockWait.
func (p *Pair) lock(ctx context.Context, court string) (func(), error) {
	if p.o.Lock == nil {
		return func() {}, nil
	}
	lctx, cancel := context.WithTimeout(ctx, p.o.LockWait)
	defer cancel()
	return p.o.Lock(lctx, p.Resources(court))
}

// span writes an interval in the club's clock, for the operator.
func (p *Pair) span(start, end time.Time) string {
	s, e := start.In(p.o.Location), end.In(p.o.Location)
	if s.Format("2006-01-02") == e.Format("2006-01-02") {
		return s.Format("2006-01-02 15:04") + "–" + e.Format("15:04")
	}
	return s.Format("2006-01-02 15:04") + "–" + e.Format("2006-01-02 15:04")
}
