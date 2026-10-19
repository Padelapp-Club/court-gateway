package yclients

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// YCLIENTS in a pair (driver.Mirrorer): a booking of the other CRM is
// mirrored as records of the club's service client (book: the court's
// services that make up its length), whose comment is the marker and the
// set tag of its records, nothing else: "[cg-mirror:<src>:<id>]
// [cg-set:<key>/<n>]". A record is the gateway's mirror only with both the
// marker and the service client: a marker copied onto another
// client's record is the club's. YCLIENTS has no block of its own the
// driver may write (a court's schedule is the club's working time), so a
// record of the service client is its block, as a partner's booking is.

// mirrorCommentRe splits a mirror's comment into its marker and set tag.
var mirrorCommentRe = regexp.MustCompile(`^(\S+)\s+\[cg-set:[0-9a-f]{8}/[1-8]\]$`)

var _ driver.Mirrorer = (*Driver)(nil)

// MirrorHorizon implements driver.Mirrorer: records are listed for any
// dates; the horizon of a zone check is far enough.
func (d *Driver) MirrorHorizon(context.Context) (time.Duration, error) {
	return driver.ZoneHorizon, nil
}

// maxCover is how far past its source's end a mirror may reach so that the
// courts' services make it up.
const maxCover = 30 * time.Minute

// BlockSpan implements driver.Mirrorer: records take the span when the
// courts' services make up its length; otherwise the shortest length up
// to maxCover longer that they make up on every court the last
// MirrorInventory read (a 45-minute booking of the other CRM, with
// services of 30, 60, 90 and 120 minutes, is blocked for an hour: over,
// never under). The start stays: records start at any minute, so only the
// length has to fit, and a block never reaches back before its source
// (into the past or another booking). No such length: the span itself,
// which CreateBlock cannot make (MIRROR_CREATE_FAILED).
func (d *Driver) BlockSpan(start, end time.Time) (time.Time, time.Time) {
	return start, start.Add(d.tiles.cover(end.Sub(start)))
}

// tiles are the services of each court (staff) as MirrorInventory last
// read them — book_services without a time: every service the court has,
// read again by every inventory, so a pass rounds by what it has just
// read — and the lengths BlockSpan has rounded with them.
type tiles struct {
	mu      sync.Mutex
	byStaff map[string][]service
	covers  map[time.Duration]time.Duration
}

func (t *tiles) set(read map[string][]service) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byStaff == nil {
		t.byStaff = map[string][]service{}
	}
	for staff, s := range read {
		t.byStaff[staff] = s
	}
	t.covers = map[time.Duration]time.Duration{}
}

// cover is the length a block of length l takes (see BlockSpan).
func (t *tiles) cover(l time.Duration) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if l <= 0 || l%time.Minute != 0 || len(t.byStaff) == 0 {
		return l
	}
	if c, ok := t.covers[l]; ok {
		return c
	}
	c := l
	for x := l; x <= l+maxCover; x += time.Minute {
		if t.fits(x) {
			c = x
			break
		}
	}
	t.covers[l] = c
	return c
}

// fits: book makes up a booking of length l on every court read.
func (t *tiles) fits(l time.Duration) bool {
	for _, s := range t.byStaff {
		pieces, err := pickServices(int(l/time.Second), s)
		if err != nil || len(planAppointments(pieces)) > maxRecords {
			return false
		}
	}
	return true
}

// mirrorText is a mirror's comment without its set tag.
func mirrorText(comment string) string {
	t := strings.TrimSpace(comment)
	if m := mirrorCommentRe.FindStringSubmatch(t); m != nil {
		return m[1]
	}
	return t
}

// classOf classifies a record by its comment and its client's phone (as
// YCLIENTS shows it, maybe masked).
func (d *Driver) classOf(comment *string, phone string) (driver.MirrorClass, *driver.MirrorMarker, string) {
	text := ""
	if comment != nil {
		text = *comment
	}
	if !phoneFits(phone, d.client.phone) {
		return driver.ClassClub, nil, "" // another client's: the club's, whatever its comment
	}
	if strings.HasPrefix(strings.TrimSpace(text), "[cg-mirror") {
		text = mirrorText(text)
	}
	return driver.ClassOfComment(text)
}

// isMirror: a record read by id is a mirror of marker: the service
// client's, its comment the marker and a set tag.
func (d *Driver) isMirror(rec *details, marker string) bool {
	return rec.Comment != nil && strings.HasPrefix(strings.TrimSpace(*rec.Comment), "[cg-mirror") && mirrorText(*rec.Comment) == marker &&
		rec.Client != nil && phoneFits(rec.Client.Phone, d.client.phone)
}

// recordItems classifies the records of the wanted courts (staff ->
// court). The records of one booking or mirror of the gateway (a set tag)
// are one item when all of them are there, back to back; otherwise each
// is its own.
func (d *Driver) recordItems(records []record, want map[string]string) ([]driver.MirrorItem, error) {
	type set struct {
		n     int
		items []driver.MirrorItem
		ids   []string
	}
	sets := map[string]*set{}
	var keys []string
	var out []driver.MirrorItem
	for _, r := range records {
		if r.Deleted {
			continue
		}
		court, ok := want[r.StaffID.String()]
		if !ok {
			continue
		}
		start, end, ok := r.span()
		if !ok || !digitsRe.MatchString(r.ID.String()) {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /records: a record in an unexpected form")
		}
		class, src, partner := d.classOf(r.Comment, phoneOf(r.Client))
		it := driver.MirrorItem{ID: "y-" + r.ID.String(), Court: court, Start: start, End: end, Class: class, Source: src, Partner: partner}
		key, n, tagged := setOf(r.Comment)
		if !tagged || class == driver.ClassClub {
			out = append(out, it)
			continue
		}
		key = r.StaffID.String() + " " + key
		if sets[key] == nil {
			sets[key] = &set{n: n}
			keys = append(keys, key)
		}
		sets[key].items = append(sets[key].items, it)
		sets[key].ids = append(sets[key].ids, r.ID.String())
	}
	for _, k := range keys {
		st := sets[k]
		sort.Slice(st.items, func(i, j int) bool { return st.items[i].Start.Before(st.items[j].Start) })
		whole := len(st.items) == st.n
		for i := 1; whole && i < len(st.items); i++ {
			whole = st.items[i].Start.Equal(st.items[i-1].End) && st.items[i].Class == st.items[0].Class && st.items[i].Partner == st.items[0].Partner &&
				(st.items[i].Source == nil) == (st.items[0].Source == nil) && (st.items[i].Source == nil || *st.items[i].Source == *st.items[0].Source)
		}
		if !whole {
			out = append(out, st.items...)
			continue
		}
		ids := make([]string, len(st.items))
		for i, it := range st.items {
			ids[i] = strings.TrimPrefix(it.ID, "y-")
		}
		one := st.items[0]
		one.ID, one.End = "y-"+strings.Join(ids, "-"), st.items[len(st.items)-1].End
		out = append(out, one)
	}
	return out, nil
}

// MirrorInventory implements driver.Mirrorer: the records occupancy reads,
// classified, and the courts' time out of work as the club's busy time
// the CRM does not explain (opaque: mirrored within the club's hours).
func (d *Driver) MirrorInventory(ctx context.Context, q driver.OccupancyQuery) (driver.Inventory, error) {
	want := map[string]string{}
	for _, cid := range q.Courts {
		staff, ok := d.byCourt[cid]
		if !ok {
			return driver.Inventory{}, fmt.Errorf("court %q: %w", cid, driver.ErrNotFound)
		}
		want[staff] = cid
	}
	inv := driver.Inventory{Complete: true}
	ds := days(q.From, q.To, d.loc)
	if len(ds) == 0 {
		return inv, nil
	}
	first, last := ds[0], ds[len(ds)-1]
	records, err := d.fetchRecords(ctx, first.AddDate(0, 0, -1), last)
	if err != nil {
		return driver.Inventory{}, err
	}
	items, err := d.recordItems(records, want)
	if err != nil {
		return driver.Inventory{}, err
	}
	windowFrom, windowTo := first, time.Date(last.Year(), last.Month(), last.Day()+1, 0, 0, 0, 0, d.loc)
	staffs := make([]string, 0, len(want))
	for s := range want {
		staffs = append(staffs, s)
	}
	sort.Strings(staffs)
	read := map[string][]service{}
	for _, staff := range staffs {
		w, err := d.working(ctx, staff, ds)
		if err != nil {
			return driver.Inventory{}, err
		}
		// the court's services, for BlockSpan: a read that fails fails the
		// inventory (no pass rounds by nothing and takes its mirrors for
		// invalid)
		if read[staff], err = d.catalog(ctx, staff, nil); err != nil {
			return driver.Inventory{}, err
		}
		for _, h := range complement(windowFrom, windowTo, w) {
			items = append(items, driver.MirrorItem{ID: holeID(staff, h[0]), Court: want[staff], Start: h[0], End: h[1], Class: driver.ClassClub, Opaque: true})
		}
	}
	d.tiles.set(read)
	for _, it := range items {
		if it.Start.Before(q.To) && it.End.After(q.From) {
			inv.Items = append(inv.Items, it)
		}
	}
	return inv, nil
}

// MirrorLookup implements driver.Mirrorer: the records of a booking read
// by id; none when every one is deleted or gone. Time out of work has no
// point read (driver.ErrNotSupported).
func (d *Driver) MirrorLookup(ctx context.Context, id string) ([]driver.MirrorItem, error) {
	if strings.HasPrefix(id, "yh-") {
		return nil, driver.ErrNotSupported
	}
	ids, ok := recordIDs(id)
	if !ok {
		return nil, nil
	}
	live, _, err := d.readParts(ctx, ids)
	if err != nil || len(live) == 0 {
		return nil, err
	}
	sort.Slice(live, func(i, j int) bool { return live[i].start.Before(live[j].start) })
	var out []driver.MirrorItem
	for _, p := range live {
		court, ok := d.staff[p.rec.StaffID.String()]
		if !ok {
			continue
		}
		phone := ""
		if p.rec.Client != nil {
			phone = p.rec.Client.Phone
		}
		class, src, partner := d.classOf(p.rec.Comment, phone)
		it := driver.MirrorItem{ID: id, Court: court, Start: p.start, End: p.end, Class: class, Source: src, Partner: partner}
		if n := len(out); n > 0 && out[n-1].Court == court && out[n-1].End.Equal(p.start) {
			out[n-1].End = p.end // the records of one booking, back to back
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// CreateBlock implements driver.Mirrorer: records of the service client
// over the span (book) as BlockSpan takes it, their comment the marker and
// their set tag. A create that goes wrong is undone as a partner's is.
func (d *Driver) CreateBlock(ctx context.Context, req driver.BlockRequest) (driver.MirrorItem, error) {
	marker := req.Source.String()
	req.Start, req.End = d.BlockSpan(req.Start, req.End)
	ids, err := d.book(ctx, driver.CreateRequest{Court: req.Court, Start: req.Start, End: req.End, Kind: driver.KindGame},
		func(tag string) string { return marker + " " + tag }, func(rec *details) bool { return d.isMirror(rec, marker) })
	if err != nil {
		return driver.MirrorItem{}, err
	}
	d.log.Info("mirror records created", "court", req.Court, "records", len(ids))
	src := req.Source
	return driver.MirrorItem{ID: "y-" + strings.Join(ids, "-"), Court: req.Court, Start: req.Start, End: req.End, Class: driver.ClassMirror, Source: &src}, nil
}

// DeleteBlock implements driver.Mirrorer: every record of the block is
// read again and the block deleted only while each is still the mirror
// read before — the court's, the service client's, the same marker — and
// together they take the same span, not started.
func (d *Driver) DeleteBlock(ctx context.Context, it driver.MirrorItem) error {
	ids, ok := recordIDs(it.ID)
	staff, configured := d.byCourt[it.Court]
	if !ok || !configured || it.Source == nil {
		return driver.ErrNotOwned
	}
	live, _, err := d.readParts(ctx, ids)
	if err != nil || len(live) == 0 {
		return err
	}
	sort.Slice(live, func(i, j int) bool { return live[i].start.Before(live[j].start) })
	for i, p := range live {
		if p.rec.StaffID.String() != staff || !d.isMirror(p.rec, it.Source.String()) || i > 0 && !p.start.Equal(live[i-1].end) {
			return driver.ErrNotOwned
		}
	}
	if !live[0].start.Equal(it.Start) || !live[len(live)-1].end.Equal(it.End) {
		return driver.ErrNotOwned
	}
	switch now, known := d.clock(); {
	case !known:
		return driver.ErrClockUnknown
	case !it.Start.After(now):
		return driver.ErrStarted
	}
	for _, p := range live {
		if err := d.deleteRecord(ctx, p.id); err != nil {
			return err
		}
	}
	d.log.Info("mirror records deleted", "court", it.Court, "records", len(live))
	return nil
}
