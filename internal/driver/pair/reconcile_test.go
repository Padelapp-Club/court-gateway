package pair

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// model is the common scenario surface: CRM-specific HTTP fixtures can
// implement these operations without changing the reconciliation contract.
type model struct {
	items                       []driver.InventoryItem
	complete                    bool
	reads                       int
	inspect                     func(string) ([]driver.InventoryItem, error)
	beforeRead                  func()
	createErr, cancelErr        error
	created, deleted, cancelled int
	onWrite                     func()
	order                       *[]string
	name                        string
}

func (m *model) Capabilities() driver.Capabilities { return driver.Capabilities{Currency: "RUB"} }
func (m *model) Ready(context.Context) error       { return nil }
func (m *model) PriceTable(context.Context, string) (driver.PriceTable, error) {
	return driver.PriceTable{}, nil
}
func (m *model) Get(ctx context.Context, id string) (driver.Booking, error) {
	for _, it := range m.items {
		if it.ID == id {
			return driver.Booking{ID: id, Court: it.Court, Start: it.Start, End: it.End, Status: driver.StatusActive}, nil
		}
	}
	return driver.Booking{}, driver.ErrNotFound
}
func (m *model) Create(ctx context.Context, r driver.CreateRequest) (driver.Booking, error) {
	if m.onWrite != nil {
		m.onWrite()
	}
	if m.order != nil {
		*m.order = append(*m.order, m.name)
	}
	if m.createErr != nil {
		return driver.Booking{}, m.createErr
	}
	b := driver.Booking{ID: fmt.Sprintf("b-%d", len(m.items)), Court: r.Court, Start: r.Start, End: r.End, Status: driver.StatusActive}
	m.items = append(m.items, driver.InventoryItem{Interval: driver.Interval{ID: b.ID, Court: b.Court, Start: b.Start, End: b.End}, Class: driver.PartnerBooking, Partner: r.Partner})
	return b, nil
}
func (m *model) Cancel(ctx context.Context, id, p string) error {
	if m.cancelErr != nil {
		return m.cancelErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.cancelled++
	for i, it := range m.items {
		if it.ID == id {
			m.items = append(m.items[:i], m.items[i+1:]...)
			break
		}
	}
	return nil
}
func (m *model) MarkPaid(context.Context, string, string) error { return nil }
func (m *model) Occupancy(ctx context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	if m.beforeRead != nil {
		f := m.beforeRead
		m.beforeRead = nil
		f()
	}
	out := driver.Occupancy{Complete: m.complete}
	for _, it := range m.items {
		if it.Start.Before(q.To) && it.End.After(q.From) {
			out.Intervals = append(out.Intervals, it.Interval)
		}
	}
	return out, nil
}
func (m *model) Inventory(context.Context, driver.OccupancyQuery) (driver.Inventory, error) {
	m.reads++
	return driver.Inventory{Complete: m.complete, Items: append([]driver.InventoryItem(nil), m.items...)}, nil
}
func (m *model) Inspect(ctx context.Context, id string) ([]driver.InventoryItem, error) {
	if m.inspect != nil {
		return m.inspect(id)
	}
	var out []driver.InventoryItem
	for _, it := range m.items {
		if it.ID == id {
			out = append(out, it)
		}
	}
	return out, nil
}
func (m *model) CreateBlock(ctx context.Context, r driver.BlockRequest) error {
	if m.onWrite != nil {
		m.onWrite()
	}
	if m.createErr != nil {
		return m.createErr
	}
	m.created++
	if m.order != nil {
		*m.order = append(*m.order, "create")
	}
	iv := r.Interval
	iv.ID = fmt.Sprintf("m-%d", m.created)
	m.items = append(m.items, driver.InventoryItem{Interval: iv, Class: driver.MirrorBooking, Source: r.Source, SourceID: r.SourceID})
	return nil
}
func (m *model) DeleteBlock(ctx context.Context, w driver.InventoryItem) error {
	if m.onWrite != nil {
		m.onWrite()
	}
	for i, it := range m.items {
		if driver.SameBlock(it, w) {
			m.deleted++
			if m.order != nil {
				*m.order = append(*m.order, "delete")
			}
			m.items = append(m.items[:i], m.items[i+1:]...)
			return nil
		}
	}
	return driver.ErrNotOwned
}

var epoch = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

func item(id, c string, h int) driver.InventoryItem {
	return driver.InventoryItem{Interval: driver.Interval{ID: id, Court: c, Start: epoch.Add(time.Duration(h) * time.Hour), End: epoch.Add(time.Duration(h+1) * time.Hour)}, Class: driver.ClubBooking}
}
func mirror(id string, s driver.InventoryItem, src string) driver.InventoryItem {
	m := s
	m.ID = id
	m.Class = driver.MirrorBooking
	m.Source = src
	m.SourceID = s.ID
	return m
}
func fixture(t *testing.T) (*Driver, *model, *model) {
	t.Helper()
	a, b := &model{complete: true, name: "primary"}, &model{complete: true, name: "lunda"}
	locked := false
	check := func() {
		if !locked {
			t.Fatal("write outside shared lock")
		}
	}
	a.onWrite = check
	b.onWrite = check
	d, e := New(Options{PrimaryType: "viva", Primary: a, Mirror: b, Courts: []string{"court-1", "court-2"}, Clock: func() (time.Time, bool) { return epoch, true }, Lock: func(context.Context, string) (func(), error) {
		if locked {
			t.Fatal("nested lock")
		}
		locked = true
		return func() { locked = false }, nil
	}, MaxRemovals: 3, MaxCreates: 50})
	if e != nil {
		t.Fatal(e)
	}
	return d, a, b
}
func run(t *testing.T, d *Driver) {
	t.Helper()
	if e := d.Reconcile(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func alarmIs(t *testing.T, e error, code string) {
	t.Helper()
	if e == nil || !strings.Contains(e.Error(), code) {
		t.Fatalf("want %s, got %v", code, e)
	}
}
func TestMirrorLifecycle(t *testing.T) {
	d, a, b := fixture(t)
	a.items = []driver.InventoryItem{item("e-1", "court-1", 2)}
	run(t, d)
	if b.created != 1 {
		t.Fatal("source not mirrored")
	}
	run(t, d)
	if a.created != 0 || b.created != 1 {
		t.Fatal("mirror loop")
	}
	a.items = nil
	run(t, d)
	if b.deleted != 1 || a.created != 0 {
		t.Fatal("orphan not removed or mirror reflected back")
	}
}
func TestIncompleteAbortsAllWrites(t *testing.T) {
	for _, side := range []int{0, 1} {
		t.Run(fmt.Sprint(side), func(t *testing.T) {
			d, a, b := fixture(t)
			a.items = []driver.InventoryItem{item("e-1", "court-1", 2)}
			b.items = []driver.InventoryItem{mirror("m-1", item("gone", "court-2", 4), "viva")}
			[]*model{a, b}[side].complete = false
			alarmIs(t, d.Reconcile(context.Background()), "MIRROR_INCOMPLETE")
			if a.created+b.created+a.deleted+b.deleted != 0 {
				t.Fatal("write on incomplete read")
			}
		})
	}
}
func TestRemovalFuseBothSides(t *testing.T) {
	d, a, b := fixture(t)
	for i := range 2 {
		a.items = append(a.items, mirror(fmt.Sprint("a", i), item(fmt.Sprint("gone-a", i), "court-1", i+1), "lunda"))
		b.items = append(b.items, mirror(fmt.Sprint("b", i), item(fmt.Sprint("gone-b", i), "court-2", i+5), "viva"))
	}
	a.items = append(a.items, item("new", "court-1", 12))
	alarmIs(t, d.Reconcile(context.Background()), "MIRROR_REMOVAL_FUSE")
	if a.deleted+b.deleted != 0 || b.created != 1 {
		t.Fatal("fuse must stop deletions only")
	}
	d.removals = 4
	run(t, d)
	if a.deleted+b.deleted != 4 {
		t.Fatal("operator override")
	}
}
func TestConflictTouchesNothing(t *testing.T) {
	d, a, b := fixture(t)
	a.items = []driver.InventoryItem{item("a", "court-1", 2)}
	b.items = []driver.InventoryItem{item("b", "court-1", 2), mirror("orphan", item("gone", "court-2", 5), "viva")}
	alarmIs(t, d.Reconcile(context.Background()), "MIRROR_CONFLICT")
	if a.created+b.created+a.deleted+b.deleted != 0 {
		t.Fatal("conflict wrote")
	}
}
func TestFreshTargetUnderLock(t *testing.T) {
	d, a, b := fixture(t)
	a.items = []driver.InventoryItem{item("source", "court-1", 2)}
	b.beforeRead = func() { b.items = append(b.items, item("arrived", "court-1", 2)) }
	alarmIs(t, d.Reconcile(context.Background()), "MIRROR_WRITE_FAILED")
	if b.created != 0 {
		t.Fatal("stale target write")
	}
}
func TestFreshSourceBeforeRemoval(t *testing.T) {
	d, a, b := fixture(t)
	src := item("live", "court-1", 2)
	b.items = []driver.InventoryItem{mirror("m-1", src, "viva")}
	a.inspect = func(string) ([]driver.InventoryItem, error) { return []driver.InventoryItem{src}, nil }
	run(t, d)
	if b.deleted != 0 {
		t.Fatal("deleted live source")
	}
}
func TestSourceChangesDuringPass(t *testing.T) {
	for _, phase := range []string{"create", "delete"} {
		t.Run(phase, func(t *testing.T) {
			d, a, b := fixture(t)
			src := item("live", "court-1", 2)
			if phase == "create" {
				a.items = []driver.InventoryItem{src}
				a.inspect = func(string) ([]driver.InventoryItem, error) { return nil, nil }
			} else {
				b.items = []driver.InventoryItem{mirror("m-1", src, "viva")}
				reads := 0
				a.inspect = func(string) ([]driver.InventoryItem, error) {
					reads++
					if reads == 1 {
						return nil, nil
					}
					return []driver.InventoryItem{src}, nil
				}
			}
			run(t, d)
			if b.deleted+b.created != 0 {
				t.Fatal("stale source decision")
			}
		})
	}
}
func TestMarkerlessNeedsTwoCompletePasses(t *testing.T) {
	d, a, b := fixture(t)
	m := mirror("m-1", item("gone", "court-1", 2), "")
	m.SourceID = ""
	b.items = []driver.InventoryItem{m}
	run(t, d)
	if b.deleted != 0 {
		t.Fatal("first pass deleted")
	}
	a.complete = false
	_ = d.Reconcile(context.Background())
	a.complete = true
	run(t, d)
	if b.deleted != 0 {
		t.Fatal("incomplete did not reset evidence")
	}
	run(t, d)
	if b.deleted != 1 {
		t.Fatal("second complete pass did not delete")
	}
}
func TestBoundaryNeverRemoved(t *testing.T) {
	d, _, b := fixture(t)
	left := mirror("left", item("a", "court-1", -1), "viva")
	left.End = epoch.Add(time.Hour)
	right := mirror("right", item("b", "court-2", 335), "viva")
	right.End = epoch.Add(337 * time.Hour)
	b.items = []driver.InventoryItem{left, right}
	run(t, d)
	if b.deleted != 0 {
		t.Fatal("boundary deletion")
	}
}
func TestMoveCreatesBeforeRemoval(t *testing.T) {
	d, a, b := fixture(t)
	old := item("same", "court-1", 2)
	moved := old
	moved.Court = "court-2"
	moved.Start = moved.Start.Add(3 * time.Hour)
	moved.End = moved.End.Add(3 * time.Hour)
	a.items = []driver.InventoryItem{moved}
	b.items = []driver.InventoryItem{mirror("old", old, "viva")}
	order := []string{}
	b.order = &order
	run(t, d)
	if strings.Join(order, ",") != "create,delete" {
		t.Fatal(order)
	}
}
func TestFailedMoveKeepsOld(t *testing.T) {
	d, a, b := fixture(t)
	old := item("same", "court-1", 2)
	a.items = []driver.InventoryItem{item("same", "court-2", 4)}
	b.items = []driver.InventoryItem{mirror("old", old, "viva")}
	b.createErr = errors.New("refused")
	_ = d.Reconcile(context.Background())
	if b.deleted != 0 {
		t.Fatal("old block removed after failed move")
	}
}
func TestWholeIDsAndIntervals(t *testing.T) {
	for _, change := range []string{"prefix", "case", "court", "time"} {
		t.Run(change, func(t *testing.T) {
			d, a, b := fixture(t)
			src := item("Ab12", "court-1", 2)
			m := mirror("old", src, "viva")
			switch change {
			case "prefix":
				src.ID = "Ab123"
			case "case":
				src.ID = "ab12"
			case "court":
				src.Court = "court-2"
			case "time":
				src.Start = src.Start.Add(time.Hour)
				src.End = src.End.Add(time.Hour)
			}
			a.items = []driver.InventoryItem{src}
			b.items = []driver.InventoryItem{m}
			_ = d.Reconcile(context.Background())
			if len(d.absent) != 1 {
				t.Fatal("changed source counted valid")
			}
		})
	}
}
func TestMultiCourtSource(t *testing.T) {
	d, a, b := fixture(t)
	a.items = []driver.InventoryItem{item("same", "court-1", 2), item("same", "court-2", 4)}
	run(t, d)
	run(t, d)
	if b.created != 2 || b.deleted != 0 {
		t.Fatal("not per interval")
	}
}
func TestDamagedAndUnmappedQuarantined(t *testing.T) {
	d, a, b := fixture(t)
	bad := item("damaged", "court-1", 2)
	bad.Class = driver.DamagedMirror
	a.items = []driver.InventoryItem{bad}
	b.items = []driver.InventoryItem{mirror("outside", item("gone", "outside", 2), "viva")}
	e := d.Reconcile(context.Background())
	alarmIs(t, e, "MIRROR_DAMAGED")
	alarmIs(t, e, "MIRROR_UNMAPPED")
	if a.created+b.created+a.deleted+b.deleted != 0 {
		t.Fatal("quarantined record touched")
	}
}
func TestPartnerHalvesAlarmOnSecondPass(t *testing.T) {
	d, a, b := fixture(t)
	p := item("p", "court-1", 2)
	p.Class = driver.PartnerBooking
	p.Partner = "booker"
	a.items = []driver.InventoryItem{p}
	run(t, d)
	alarmIs(t, d.Reconcile(context.Background()), "MIRROR_HALF_BOOKING")
	if a.created+b.created+a.deleted+b.deleted != 0 {
		t.Fatal("half auto repaired")
	}
	b.items = []driver.InventoryItem{p}
	run(t, d)
}
func TestCreateBound(t *testing.T) {
	d, a, b := fixture(t)
	d.creates = 1
	for i := range 4 {
		a.items = append(a.items, item(fmt.Sprint(i), "court-1", i*2+1))
	}
	run(t, d)
	if b.created != 1 {
		t.Fatal("limit")
	}
	run(t, d)
	alarmIs(t, d.Reconcile(context.Background()), "MIRROR_CREATE_LIMIT")
	if b.created != 3 {
		t.Fatal("limit")
	}
}
func TestCreateRollbackAndAlarm(t *testing.T) {
	d, a, b := fixture(t)
	a.onWrite = nil
	b.onWrite = nil
	order := []string{}
	a.order = &order
	b.order = &order
	b.createErr = errors.New("refused")
	_, e := d.Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: epoch.Add(time.Hour), End: epoch.Add(2 * time.Hour), Partner: "booker"})
	if e == nil || len(a.items) != 0 || a.cancelled != 1 || strings.Join(order, ",") != "primary,lunda" {
		t.Fatal("not compensated", order)
	}
	a.cancelErr = errors.New("rollback refused")
	_, _ = d.Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: epoch.Add(time.Hour), End: epoch.Add(2 * time.Hour)})
	alarmIs(t, d.Ready(context.Background()), "MIRROR_HALF_BOOKING")
}
func TestRollbackOutlivesRequest(t *testing.T) {
	d, a, b := fixture(t)
	a.onWrite = nil
	ctx, cancel := context.WithCancel(context.Background())
	b.onWrite = cancel
	b.createErr = context.Canceled
	_, _ = d.Create(ctx, driver.CreateRequest{Court: "court-1", Start: epoch.Add(time.Hour), End: epoch.Add(2 * time.Hour)})
	if len(a.items) != 0 {
		t.Fatal("rollback inherited cancelled context")
	}
}
func TestUnionAndHorizon(t *testing.T) {
	d, a, b := fixture(t)
	a.items = []driver.InventoryItem{item("a", "court-1", 2)}
	b.items = []driver.InventoryItem{item("b", "court-2", 4)}
	o, e := d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: d.courts, From: epoch, To: epoch.Add(24 * time.Hour)})
	if e != nil || !o.Complete || len(o.Intervals) != 2 {
		t.Fatal(o, e)
	}
	b.complete = false
	o, _ = d.Occupancy(context.Background(), driver.OccupancyQuery{From: epoch, To: epoch.Add(time.Hour)})
	if o.Complete {
		t.Fatal("incomplete union")
	}
	b.complete = true
	o, _ = d.Occupancy(context.Background(), driver.OccupancyQuery{From: epoch, To: epoch.Add(15 * 24 * time.Hour)})
	if o.Complete {
		t.Fatal("beyond horizon")
	}
}

type noInventory struct{ driver.Driver }

func TestPairRequiresInventory(t *testing.T) {
	_, e := New(Options{PrimaryType: "viva", Primary: noInventory{}, Mirror: &model{}, Courts: []string{"court"}, Clock: func() (time.Time, bool) { return epoch, true }, Lock: func(context.Context, string) (func(), error) { return func() {}, nil }, MaxRemovals: 3, MaxCreates: 50})
	if e == nil {
		t.Fatal("driver without inventory accepted")
	}
}
func TestRestartResetsAbsenceEvidence(t *testing.T) {
	d, _, b := fixture(t)
	m := mirror("old", item("source", "court-1", 2), "")
	m.SourceID = ""
	b.items = []driver.InventoryItem{m}
	run(t, d)
	fresh, a2, b2 := fixture(t)
	_ = a2
	b2.items = append([]driver.InventoryItem(nil), b.items...)
	run(t, fresh)
	if b2.deleted != 0 {
		t.Fatal("restart inherited evidence")
	}
	run(t, fresh)
	if b2.deleted != 1 {
		t.Fatal("new complete passes")
	}
}
func TestNoPointReadNeedsTwoPasses(t *testing.T) {
	d, a, b := fixture(t)
	a.inspect = func(string) ([]driver.InventoryItem, error) { return nil, driver.ErrNotSupported }
	b.items = []driver.InventoryItem{mirror("m", item("x-source", "court-1", 2), "viva")}
	run(t, d)
	if b.deleted != 0 {
		t.Fatal("synthetic id deleted in first pass")
	}
	run(t, d)
	if b.deleted != 1 {
		t.Fatal("synthetic id not deleted after two passes")
	}
}
func TestForeignSourceMarkerQuarantined(t *testing.T) {
	d, _, b := fixture(t)
	b.items = []driver.InventoryItem{mirror("m", item("gone", "court-1", 2), "yclients")}
	alarmIs(t, d.Reconcile(context.Background()), "MIRROR_DAMAGED")
	if b.deleted != 0 {
		t.Fatal("wrong source type deleted")
	}
}
func TestUnknownSourceKeepsBlock(t *testing.T) {
	d, a, b := fixture(t)
	b.items = []driver.InventoryItem{mirror("m", item("gone", "court-1", 2), "viva")}
	a.inspect = func(string) ([]driver.InventoryItem, error) { return nil, errors.New("unreadable") }
	alarmIs(t, d.Reconcile(context.Background()), "MIRROR_SOURCE_UNKNOWN")
	if b.deleted != 0 {
		t.Fatal("unreadable source removed")
	}
}

type costModel struct{ *model }

func (costModel) RemovalCount(driver.InventoryItem) int { return 4 }
func TestRemovalCostCountsPhysicalRecords(t *testing.T) {
	d, _, b := fixture(t)
	d.mb = costModel{b}
	b.items = []driver.InventoryItem{mirror("m", item("gone", "court-1", 2), "viva")}
	alarmIs(t, d.Reconcile(context.Background()), "MIRROR_REMOVAL_FUSE")
	if b.deleted != 0 {
		t.Fatal("four records counted as one")
	}
}
func TestLostResponseAndRestartHalf(t *testing.T) {
	d, a, b := fixture(t)
	a.onWrite = nil
	b.onWrite = nil
	// The primary accepted a create, but its response was lost. Restart
	// before the Lunda write; the new loop has no remembered transaction.
	_, e := a.Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: epoch.Add(time.Hour), End: epoch.Add(2 * time.Hour), Partner: "booker"})
	if e != nil {
		t.Fatal(e)
	}
	run(t, d)
	alarmIs(t, d.Reconcile(context.Background()), "MIRROR_HALF_BOOKING")
	if b.created != 0 || a.cancelled != 0 {
		t.Fatal("unknown half auto repaired")
	}
}

func TestInterruptedPassResetsEvidence(t *testing.T) {
	d, a, b := fixture(t)
	m := mirror("m", item("gone", "court-1", 2), "")
	m.SourceID = ""
	b.items = []driver.InventoryItem{m}
	run(t, d)
	original := d.clock
	d.clock = func() (time.Time, bool) { return epoch, false }
	if e := d.Reconcile(context.Background()); !errors.Is(e, driver.ErrClockUnknown) {
		t.Fatal(e)
	}
	d.clock = original
	run(t, d)
	if b.deleted != 0 {
		t.Fatal("unknown clock retained absence evidence")
	}
	d2, a2, b2 := fixture(t)
	_ = a
	_ = b2
	d2.creates = 1
	for i := range 5 {
		a2.items = append(a2.items, item(fmt.Sprint(i), "court-1", i*2+1))
	}
	run(t, d2)
	run(t, d2)
	a2.complete = false
	_ = d2.Reconcile(context.Background())
	a2.complete = true
	run(t, d2)
}
