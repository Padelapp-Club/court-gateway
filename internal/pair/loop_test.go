package pair_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
)

// fakeCRM is a CRM in memory for the loop's own rules: its items, what
// its reads answer and what it was asked to write.
type fakeCRM struct {
	typ string

	mu         sync.Mutex
	items      []driver.MirrorItem
	hidden     map[string]bool // ids the full read leaves out (a stale read)
	incomplete bool
	invErr     error
	lookupErr  error
	noLookup   bool                                              // MirrorLookup is ErrNotSupported
	reread     func(q driver.OccupancyQuery) []driver.MirrorItem // more items a read of one court finds
	round      time.Duration                                     // BlockSpan rounds out to it
	created    []driver.BlockRequest
	deleted    []string
	seq        int
}

func (f *fakeCRM) add(it driver.MirrorItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, it)
}

func (f *fakeCRM) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = slices.DeleteFunc(f.items, func(it driver.MirrorItem) bool { return it.ID == id })
}

func (f *fakeCRM) live(class driver.MirrorClass) []driver.MirrorItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []driver.MirrorItem
	for _, it := range f.items {
		if it.Class == class {
			out = append(out, it)
		}
	}
	return out
}

func (f *fakeCRM) Capabilities() driver.Capabilities { return driver.Capabilities{Currency: "RUB"} }
func (f *fakeCRM) Ready(context.Context) error       { return nil }
func (f *fakeCRM) Occupancy(context.Context, driver.OccupancyQuery) (driver.Occupancy, error) {
	return driver.Occupancy{Complete: true}, nil
}
func (f *fakeCRM) PriceTable(context.Context, string) (driver.PriceTable, error) {
	return driver.PriceTable{}, driver.ErrNotSupported
}
func (f *fakeCRM) Create(context.Context, driver.CreateRequest) (driver.Booking, error) {
	return driver.Booking{}, driver.ErrNotSupported
}
func (f *fakeCRM) Get(context.Context, string) (driver.Booking, error) {
	return driver.Booking{}, driver.ErrNotFound
}
func (f *fakeCRM) Cancel(context.Context, string, string) error   { return nil }
func (f *fakeCRM) MarkPaid(context.Context, string, string) error { return driver.ErrNotSupported }
func (f *fakeCRM) Resource(court string) string                   { return f.typ + " " + court }

func (f *fakeCRM) MirrorInventory(_ context.Context, q driver.OccupancyQuery) (driver.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invErr != nil {
		return driver.Inventory{}, f.invErr
	}
	inv := driver.Inventory{Complete: !f.incomplete}
	for _, it := range f.items {
		if !f.hidden[it.ID] && slices.Contains(q.Courts, it.Court) && it.Start.Before(q.To) && it.End.After(q.From) {
			inv.Items = append(inv.Items, it)
		}
	}
	if f.reread != nil && len(q.Courts) == 1 {
		inv.Items = append(inv.Items, f.reread(q)...)
	}
	return inv, nil
}

func (f *fakeCRM) MirrorLookup(_ context.Context, id string) ([]driver.MirrorItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noLookup {
		return nil, driver.ErrNotSupported
	}
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	var out []driver.MirrorItem
	for _, it := range f.items {
		if it.ID == id {
			out = append(out, it)
		}
	}
	return out, nil
}

func (f *fakeCRM) BlockSpan(start, end time.Time) (time.Time, time.Time) {
	if f.round == 0 {
		return start, end
	}
	s := start.Truncate(f.round)
	e := end.Truncate(f.round)
	if e.Before(end) {
		e = e.Add(f.round)
	}
	return s, e
}

func (f *fakeCRM) CreateBlock(_ context.Context, req driver.BlockRequest) (driver.MirrorItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	s, e := f.BlockSpan(req.Start, req.End)
	src := req.Source
	it := driver.MirrorItem{ID: fmt.Sprintf("%s-m%d", f.typ, f.seq), Court: req.Court, Start: s, End: e, Class: driver.ClassMirror, Source: &src}
	if f.typ == "fitness1c" {
		it.Source = nil // 1C keeps no marker
	}
	f.items = append(f.items, it)
	f.created = append(f.created, req)
	return it, nil
}

func (f *fakeCRM) DeleteBlock(_ context.Context, it driver.MirrorItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.items, func(x driver.MirrorItem) bool { return x.ID == it.ID })
	if i < 0 {
		return nil
	}
	if f.items[i].Class != driver.ClassMirror {
		return driver.ErrNotOwned
	}
	f.items = slices.Delete(f.items, i, i+1)
	f.deleted = append(f.deleted, it.ID)
	return nil
}

func (f *fakeCRM) MirrorHorizon() time.Duration { return 14 * 24 * time.Hour }

// loop is a pair of fake CRMs at a fixed time.
type loop struct {
	a, b *fakeCRM
	p    *pair.Pair
	now  time.Time
	loc  *time.Location
	// clockUnknown: the hub's time is not known (a pass is skipped)
	clockUnknown bool
}

func newLoop(t *testing.T, opts ...func(*pair.Options)) *loop {
	t.Helper()
	loc := time.FixedZone("MSK", 3*3600)
	l := &loop{a: &fakeCRM{typ: "viva"}, b: &fakeCRM{typ: "lunda"}, loc: loc,
		now: time.Date(2026, 10, 20, 9, 0, 0, 0, loc)}
	o := pair.Options{PrimaryType: "viva", MirrorType: "lunda", Courts: []string{"court-1", "court-2"}, Location: loc,
		Clock: func() (time.Time, bool) { return l.now, !l.clockUnknown }}
	for i := range 7 {
		o.Hours[i] = &[2]int{7 * 60, 23 * 60}
	}
	for _, f := range opts {
		f(&o)
	}
	p, err := pair.New(l.a, l.b, o)
	if err != nil {
		t.Fatal(err)
	}
	l.p = p
	return l
}

func (l *loop) at(day, hh, mm int) time.Time {
	return time.Date(l.now.Year(), l.now.Month(), l.now.Day()+day, hh, mm, 0, 0, l.loc)
}

func (l *loop) pass() pair.Report { return l.p.Reconcile(context.Background()) }

func club(id, court string, start, end time.Time) driver.MirrorItem {
	return driver.MirrorItem{ID: id, Court: court, Start: start, End: end, Class: driver.ClassClub}
}

func mirrorOf(id, court string, start, end time.Time, src, srcID string) driver.MirrorItem {
	return driver.MirrorItem{ID: id, Court: court, Start: start, End: end, Class: driver.ClassMirror, Source: &driver.MirrorMarker{Src: src, ID: srcID}}
}

// The validity rule: a mirror is its source's only for the very id of its
// marker, the same court and the span. A prefix of an id, another case,
// another court, another span: not valid, so the mirror is removed and a
// new one made.
func TestValidityRuleIsExact(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mirror driver.MirrorItem
		keep   bool
	}{
		{"exact", mirrorOf("b-1", "court-1", time.Time{}, time.Time{}, "lunda", "l-123"), true},
		{"id prefix", mirrorOf("b-1", "court-1", time.Time{}, time.Time{}, "lunda", "l-12"), false},
		{"id longer", mirrorOf("b-1", "court-1", time.Time{}, time.Time{}, "lunda", "l-1234"), false},
		{"id case", mirrorOf("b-1", "court-1", time.Time{}, time.Time{}, "lunda", "L-123"), false},
		{"another court", mirrorOf("b-1", "court-2", time.Time{}, time.Time{}, "lunda", "l-123"), false},
		{"another span", mirrorOf("b-1", "court-1", time.Time{}, time.Time{}, "lunda", "l-123"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLoop(t)
			s, e := l.at(1, 10, 0), l.at(1, 11, 0)
			l.b.add(club("l-123", "court-1", s, e))
			m := tc.mirror
			m.Start, m.End = s, e
			if tc.name == "another span" {
				m.End = e.Add(30 * time.Minute)
			}
			l.a.add(m)
			rep := l.pass()
			kept := !slices.Contains(l.a.deleted, "b-1")
			if kept != tc.keep {
				t.Fatalf("mirror kept: %v, want %v (%+v)", kept, tc.keep, rep)
			}
			if !tc.keep && len(l.a.created) != 1 {
				t.Fatalf("no new mirror for the source: %+v", rep)
			}
			if tc.keep && len(l.a.created) != 0 {
				t.Fatalf("a valid mirror was made again: %+v", rep)
			}
		})
	}
}

// The fuse: more removals in the plan than MaxRemovals — both CRMs
// counted together — removes none and raises MIRROR_FUSE; creations go on.
func TestFuseCountsBothSides(t *testing.T) {
	l := newLoop(t)
	for i := range 2 {
		s := l.at(1, 10+i, 0)
		l.a.add(mirrorOf(fmt.Sprintf("b-%d", i), "court-1", s, s.Add(time.Hour), "lunda", fmt.Sprintf("l-gone-%d", i)))
		l.b.add(mirrorOf(fmt.Sprintf("l-m%d", i), "court-2", s, s.Add(time.Hour), "viva", fmt.Sprintf("e-gone-%d", i)))
	}
	l.b.add(club("l-new", "court-1", l.at(2, 10, 0), l.at(2, 11, 0)))
	rep := l.pass()
	if !alarmed(rep, pair.AlarmFuse) || len(rep.Removed) != 0 || len(l.a.deleted)+len(l.b.deleted) != 0 {
		t.Fatalf("4 removals against a fuse of 3: %+v", rep)
	}
	if len(l.a.created) != 1 {
		t.Fatalf("creations stopped by the fuse: %+v", rep)
	}
	// the operator raises it for one pass
	p, _ := pair.New(l.a, l.b, pair.Options{PrimaryType: "viva", MirrorType: "lunda", Courts: []string{"court-1", "court-2"},
		Location: l.loc, MaxRemovals: 4, Clock: func() (time.Time, bool) { return l.now, true }})
	if rep := p.Reconcile(context.Background()); len(rep.Removed) != 4 || alarmed(rep, pair.AlarmFuse) {
		t.Fatalf("with the fuse raised to 4: %+v", rep)
	}
}

// An incomplete read or a failed read of either CRM aborts the pass before
// any write: no creation, no removal, an alarm.
func TestIncompleteReadAbortsPass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(l *loop)
	}{
		{"primary incomplete", func(l *loop) { l.a.incomplete = true }},
		{"mirror incomplete", func(l *loop) { l.b.incomplete = true }},
		{"primary fails", func(l *loop) { l.a.invErr = driver.Upstream(driver.UpstreamUnavailable, "down") }},
		{"mirror fails", func(l *loop) { l.b.invErr = driver.Upstream(driver.UpstreamAuth, "no session") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLoop(t)
			l.b.add(club("l-1", "court-1", l.at(1, 10, 0), l.at(1, 11, 0)))
			l.a.add(mirrorOf("b-old", "court-2", l.at(1, 12, 0), l.at(1, 13, 0), "lunda", "l-gone"))
			tc.break_(l)
			rep := l.pass()
			if !rep.Aborted || !alarmed(rep, pair.AlarmIncomplete) || len(l.a.created)+len(l.a.deleted)+len(l.b.created)+len(l.b.deleted) > 0 {
				t.Fatalf("pass: %+v", rep)
			}
		})
	}
}

// F11: a mirror whose source the full read missed is removed only after a
// point read of the source says it is gone; a point read that fails keeps
// it.
func TestPointReadBeforeRemoval(t *testing.T) {
	l := newLoop(t)
	s, e := l.at(1, 10, 0), l.at(1, 11, 0)
	l.b.add(club("l-1", "court-1", s, e))
	l.a.add(mirrorOf("b-1", "court-1", s, e, "lunda", "l-1"))
	l.b.hidden = map[string]bool{"l-1": true} // a stale full read
	rep := l.pass()
	if slices.Contains(l.a.deleted, "b-1") {
		t.Fatalf("a mirror of a source still there was removed: %+v", rep)
	}
	l.b.lookupErr = driver.Upstream(driver.UpstreamUnavailable, "down")
	l.b.remove("l-1")
	l.b.hidden = nil
	if l.pass(); slices.Contains(l.a.deleted, "b-1") {
		t.Fatal("a mirror was removed without a point read of its source")
	}
	l.b.lookupErr = nil
	if l.pass(); !slices.Contains(l.a.deleted, "b-1") {
		t.Fatal("the mirror of a gone source stays")
	}
}

// Without a point read (1C's mirrors carry no marker; a source of no id):
// a mirror is removed only when it is invalid in two passes in a row.
func TestTwoPassesWithoutPointRead(t *testing.T) {
	t.Run("no marker", func(t *testing.T) {
		l := newLoop(t)
		m := mirrorOf("f-1", "court-1", l.at(1, 10, 0), l.at(1, 11, 0), "", "")
		m.Source = nil
		l.a.add(m)
		if l.pass(); len(l.a.deleted) != 0 {
			t.Fatal("removed at the first sight")
		}
		if l.pass(); len(l.a.deleted) != 1 {
			t.Fatal("not removed at the second")
		}
	})
	t.Run("no point read", func(t *testing.T) {
		l := newLoop(t)
		l.b.noLookup = true
		l.a.add(mirrorOf("b-1", "court-1", l.at(1, 10, 0), l.at(1, 11, 0), "lunda", "ls-abc"))
		if l.pass(); len(l.a.deleted) != 0 {
			t.Fatal("removed at the first sight")
		}
		if l.pass(); len(l.a.deleted) != 1 {
			t.Fatal("not removed at the second")
		}
	})
	t.Run("seen valid between", func(t *testing.T) {
		l := newLoop(t)
		s, e := l.at(1, 10, 0), l.at(1, 11, 0)
		m := mirrorOf("f-1", "court-1", s, e, "", "")
		m.Source = nil
		l.a.add(m)
		l.pass()
		l.b.add(club("l-1", "court-1", s, e)) // valid again
		l.pass()
		l.b.remove("l-1")
		if l.pass(); len(l.a.deleted) != 0 {
			t.Fatal("removed although valid in the pass before")
		}
	})
}

// "Two passes in a row" are two COMPLETE passes in a row (F11, D16): a pass
// aborted on an incomplete or failed read, or skipped without the hub's
// time, between them starts the count again.
func TestTwoPassesResetByAbortedPass(t *testing.T) {
	for _, c := range []struct {
		name  string
		spoil func(l *loop) (restore func())
	}{
		{"incomplete read", func(l *loop) func() {
			l.a.incomplete = true
			return func() { l.a.incomplete = false }
		}},
		{"failed read", func(l *loop) func() {
			l.b.invErr = errors.New("down")
			return func() { l.b.invErr = nil }
		}},
		{"clock unknown", func(l *loop) func() {
			l.clockUnknown = true
			return func() { l.clockUnknown = false }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLoop(t)
			m := mirrorOf("f-1", "court-1", l.at(1, 10, 0), l.at(1, 11, 0), "", "")
			m.Source = nil
			l.a.add(m)
			l.pass() // invalid: first sight
			restore := c.spoil(l)
			if rep := l.pass(); !rep.Aborted && !rep.Skipped {
				t.Fatalf("the spoilt pass ran: %+v", rep)
			}
			restore()
			if l.pass(); len(l.a.deleted) != 0 {
				t.Fatal("removed on evidence from before a pass that did not read both CRMs whole")
			}
			if l.pass(); len(l.a.deleted) != 1 {
				t.Fatal("not removed after two complete passes in a row")
			}
		})
	}
}

// Nothing but a club's item is ever mirrored: not a partner's booking, a
// mirror, a hold, a damaged marker.
func TestOnlyClubItemsAreMirrored(t *testing.T) {
	l := newLoop(t)
	s := l.at(1, 10, 0)
	l.b.add(driver.MirrorItem{ID: "l-own", Court: "court-1", Start: s, End: s.Add(time.Hour), Class: driver.ClassOwn, Partner: "padelapp"})
	l.a.add(driver.MirrorItem{ID: "e-own", Court: "court-1", Start: s, End: s.Add(time.Hour), Class: driver.ClassOwn, Partner: "padelapp"})
	l.b.add(driver.MirrorItem{ID: "ls-draft", Court: "court-2", Start: s, End: s.Add(time.Hour), Class: driver.ClassTransient})
	l.b.add(driver.MirrorItem{ID: "l-dmg", Court: "court-2", Start: s.Add(2 * time.Hour), End: s.Add(3 * time.Hour), Class: driver.ClassDamaged})
	l.b.add(mirrorOf("l-mir", "court-1", s.Add(4*time.Hour), s.Add(5*time.Hour), "viva", "e-x"))
	l.a.add(club("e-x", "court-1", s.Add(4*time.Hour), s.Add(5*time.Hour)))
	for range 3 {
		rep := l.pass()
		if len(l.a.created)+len(l.b.created)+len(l.a.deleted)+len(l.b.deleted) > 0 {
			t.Fatalf("pass wrote: %+v; a %+v b %+v", rep, l.a.created, l.b.created)
		}
	}
	if !alarmed(l.pass(), pair.AlarmDamaged) {
		t.Fatal("no alarm for the damaged marker")
	}
}

// F9: under the court's lock the target is read again; something there
// that the plan did not see stops the creation (and alarms).
func TestReadAgainUnderLock(t *testing.T) {
	l := newLoop(t)
	s, e := l.at(1, 10, 0), l.at(1, 11, 0)
	l.b.add(club("l-1", "court-1", s, e))
	var once sync.Once
	l.a.reread = func(q driver.OccupancyQuery) []driver.MirrorItem {
		var out []driver.MirrorItem
		once.Do(func() { out = []driver.MirrorItem{club("e-late", "court-1", s, e)} }) // booked meanwhile
		return out
	}
	rep := l.pass()
	if len(l.a.created) != 0 || !alarmed(rep, pair.AlarmOverlap) {
		t.Fatalf("a mirror was made over a booking the plan did not see: %+v", rep)
	}
}

// The loop's writes wait for the court's lock, the one a partner's create
// holds from its occupancy check to its write.
func TestLoopWritesUnderCourtLock(t *testing.T) {
	var held sync.Mutex
	held.Lock()
	locked := func(ctx context.Context, keys []string) (func(), error) {
		ok := make(chan struct{})
		go func() { held.Lock(); close(ok) }()
		select {
		case <-ok:
			return held.Unlock, nil
		case <-ctx.Done():
			go func() { <-ok; held.Unlock() }()
			return nil, ctx.Err()
		}
	}
	l := newLoop(t, func(o *pair.Options) { o.Lock, o.LockWait = locked, 50*time.Millisecond })
	l.b.add(club("l-1", "court-1", l.at(1, 10, 0), l.at(1, 11, 0)))
	if l.pass(); len(l.a.created) != 0 {
		t.Fatal("a mirror was made while a partner's create held the court")
	}
	held.Unlock()
	if l.pass(); len(l.a.created) != 1 {
		t.Fatal("no mirror once the court was free")
	}
}

// The window: a club's booking starting within Lead or ending past the
// horizon is not mirrored yet; a mirror not wholly within it is not
// removed.
func TestWindow(t *testing.T) {
	l := newLoop(t)
	l.b.add(club("l-soon", "court-1", l.now.Add(2*time.Minute), l.now.Add(62*time.Minute)))
	l.b.add(club("l-far", "court-2", l.at(14, 8, 0), l.at(14, 10, 0)))
	l.a.add(mirrorOf("b-started", "court-2", l.now.Add(-30*time.Minute), l.now.Add(30*time.Minute), "lunda", "l-gone"))
	l.pass()
	if len(l.a.created) != 0 || len(l.a.deleted) != 0 {
		t.Fatalf("created %+v, deleted %v", l.a.created, l.a.deleted)
	}
}

// Busy time a CRM does not explain (opaque) is mirrored within the club's
// hours only; a CRM that books whole seances blocks the span rounded out,
// and that rounded mirror is valid.
func TestOpaqueClippedAndRounded(t *testing.T) {
	l := newLoop(t)
	l.a.typ = "fitness1c"
	l.p, _ = pair.New(l.a, l.b, pair.Options{PrimaryType: "fitness1c", MirrorType: "lunda", Courts: []string{"court-1", "court-2"}, Location: l.loc,
		Hours: [7]*[2]int{{420, 1380}, {420, 1380}, {420, 1380}, {420, 1380}, {420, 1380}, {420, 1380}, {420, 1380}}, Clock: func() (time.Time, bool) { return l.now, true }})
	l.a.round = 30 * time.Minute
	op := club("x-1", "court-1", l.at(1, 0, 0), l.at(1, 8, 0)) // closed hours and a booking 7-8
	op.Opaque = true
	l.a.add(op)
	l.b.add(club("l-odd", "court-2", l.at(1, 10, 15), l.at(1, 11, 15)))
	l.pass()
	if len(l.b.created) != 1 || !l.b.created[0].Start.Equal(l.at(1, 7, 0)) || !l.b.created[0].End.Equal(l.at(1, 8, 0)) {
		t.Fatalf("the opaque busy time: %+v", l.b.created)
	}
	if len(l.a.created) != 1 {
		t.Fatalf("the odd booking: %+v", l.a.created)
	}
	if m := l.a.live(driver.ClassMirror); len(m) != 1 || !m[0].Start.Equal(l.at(1, 10, 0)) || !m[0].End.Equal(l.at(1, 11, 30)) {
		t.Fatalf("the rounded mirror: %+v", m)
	}
	// the next passes keep both: valid as they are
	for range 2 {
		if rep := l.pass(); len(rep.Created)+len(rep.Removed) > 0 {
			t.Fatalf("a valid mirror was made again or removed: %+v", rep)
		}
	}
}

// MaxCreates bounds a pass; cut off three passes in a row, an alarm.
func TestCreatesBounded(t *testing.T) {
	l := newLoop(t, func(o *pair.Options) { o.MaxCreates = 2 })
	for i := range 9 {
		s := l.at(1+i, 10, 0)
		l.b.add(club(fmt.Sprintf("l-%d", i), "court-1", s, s.Add(time.Hour)))
	}
	for i := range 3 {
		rep := l.pass()
		if len(rep.Created) != 2 {
			t.Fatalf("pass %d made %d", i, len(rep.Created))
		}
		if alarmed(rep, pair.AlarmBacklog) != (i == 2) {
			t.Fatalf("pass %d: backlog alarm %v", i, rep.Alarms)
		}
	}
}

// An alarm reaches the hub as the club's readiness: a config alert of the
// first alarm's code; it clears when a pass finds nothing.
func TestAlarmsInReadiness(t *testing.T) {
	l := newLoop(t)
	s, e := l.at(1, 10, 0), l.at(1, 11, 0)
	l.a.add(club("e-1", "court-1", s, e))
	l.b.add(club("l-1", "court-1", s, e))
	l.pass()
	var ca *driver.ConfigAlert
	if err := l.p.Ready(context.Background()); !errors.As(err, &ca) || ca.Code != pair.AlarmOverlap {
		t.Fatalf("readiness: %v", err)
	}
	l.a.remove("e-1")
	l.pass()
	if err := l.p.Ready(context.Background()); err != nil {
		t.Fatalf("readiness after the overlap is gone: %v", err)
	}
}

// Two club bookings overlapping across the CRMs are an alarm even when
// each was mirrored before the other was made (a CRM that does not refuse
// an overlap): nothing is created or removed for them.
func TestOverlapOfMirroredClubBookings(t *testing.T) {
	l := newLoop(t)
	s := l.at(1, 10, 0)
	l.a.add(club("e-1", "court-1", s, s.Add(time.Hour)))
	l.b.add(mirrorOf("l-m1", "court-1", s, s.Add(time.Hour), "viva", "e-1"))
	l.b.add(club("l-1", "court-1", s.Add(30*time.Minute), s.Add(90*time.Minute)))
	l.a.add(mirrorOf("b-m1", "court-1", s.Add(30*time.Minute), s.Add(90*time.Minute), "lunda", "l-1"))
	rep := l.pass()
	if !alarmed(rep, pair.AlarmOverlap) || len(rep.Created)+len(rep.Removed) > 0 {
		t.Fatalf("pass: %+v", rep)
	}
}

// A pair is not made of a driver that cannot keep mirrors (F13).
func TestPairNeedsMirrorers(t *testing.T) {
	if _, err := pair.New(plainDriver{}, &fakeCRM{typ: "lunda"}, pair.Options{PrimaryType: "demo", MirrorType: "lunda"}); err == nil {
		t.Fatal("a pair with a driver that cannot keep mirrors")
	}
}

type plainDriver struct{}

func (plainDriver) Capabilities() driver.Capabilities { return driver.Capabilities{} }
func (plainDriver) Ready(context.Context) error       { return nil }
func (plainDriver) Occupancy(context.Context, driver.OccupancyQuery) (driver.Occupancy, error) {
	return driver.Occupancy{}, nil
}
func (plainDriver) PriceTable(context.Context, string) (driver.PriceTable, error) {
	return driver.PriceTable{}, nil
}
func (plainDriver) Create(context.Context, driver.CreateRequest) (driver.Booking, error) {
	return driver.Booking{}, nil
}
func (plainDriver) Get(context.Context, string) (driver.Booking, error) { return driver.Booking{}, nil }
func (plainDriver) Cancel(context.Context, string, string) error        { return nil }
func (plainDriver) MarkPaid(context.Context, string, string) error      { return nil }
