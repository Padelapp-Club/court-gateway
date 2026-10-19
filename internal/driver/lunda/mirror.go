package lunda

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Lunda in a pair (driver.Mirrorer): a booking of the other CRM is
// mirrored as a booking of the gateway's own kind — on the club's service
// client, adjusted to a net price of 0, PAID without a payment (book) —
// whose notes are the marker and nothing else. A booking of the service
// client is the gateway's by its notes: "[cg:<partner>]" a partner's,
// "[cg-mirror:…]" a mirror's; without either it is the club's own (the
// club books its tournaments on the same client).

// mirrorDays bounds the days ahead the reconciliation reads Lunda: the
// partners' sale and the pair's horizon are two weeks; a longer
// booking depth is not read, so each pass reads at most 15 days.
const mirrorDays = 14

// reasonMirror is the reason Lunda shows for a mirror the loop removes.
const reasonMirror = "Отражение брони из второй CRM снято шлюзом PadelApp: исходной брони больше нет"

var _ driver.Mirrorer = (*Driver)(nil)

// MirrorHorizon implements driver.Mirrorer: the club's booking depth
// (club/settings bookingDepth, read every pass — a club that books 7 days
// ahead answers every later day "beyond the booking depth", which would
// leave every read incomplete), at most mirrorDays (14 when the settings
// name none), less an hour, so that a day past it is never asked around
// midnight when the clocks of the gateway and of Lunda disagree by a
// little. [UNVERIFIED] live: that bookingDepth counts whole days after
// today, as the stub has it.
func (d *Driver) MirrorHorizon(ctx context.Context) (time.Duration, error) {
	cs, err := readClubSettings(ctx, d.c)
	if err != nil {
		return 0, err
	}
	days := min(cs.depthDays(mirrorDays), mirrorDays)
	return time.Duration(days)*24*time.Hour - time.Hour, nil
}

// BlockSpan implements driver.Mirrorer: a booking takes any span.
func (d *Driver) BlockSpan(start, end time.Time) (time.Time, time.Time) { return start, end }

// classify sets the class of a slot's item: a hold (DRAFT) is transient,
// a booking of the service client is classified by its notes, any other
// slot is the club's. slots-load answers an abstract of the notes
// ([UNVERIFIED] whether it is ever cut short): an abstract that reads as
// no marker, a partner's or a whole mirror's is taken as it reads (a
// mirror is removed only after DeleteBlock reads the booking whole); one
// that starts like a marker but does not read, or none at all, makes the
// notes be read from the booking (needNotes).
func (d *Driver) classify(it *slotItem, s *lundaSlot) {
	switch {
	case s.Type == "DRAFT":
		it.Class = driver.ClassTransient
	case s.BookingID != "" && d.service != "" && s.ClientUID == d.service:
		text := s.Notes
		if text == nil {
			text = s.NotesAbstract
		}
		if text == nil {
			it.needNotes = true
			return
		}
		it.Class, it.Source, it.Partner = driver.ClassOfComment(*text)
		it.needNotes = it.Class == driver.ClassDamaged && s.Notes == nil
	default:
		it.Class = driver.ClassClub
	}
}

// MirrorInventory implements driver.Mirrorer: the schedule of every day
// occupancy reads, classified. It is incomplete, as occupancy is, from the
// first day that cannot be read or is beyond the club's booking depth.
func (d *Driver) MirrorInventory(ctx context.Context, q driver.OccupancyQuery) (driver.Inventory, error) {
	want := map[string]string{}
	for _, cid := range q.Courts {
		c, ok := d.byCourt[cid]
		if !ok {
			return driver.Inventory{}, fmt.Errorf("court %q: %w", cid, driver.ErrNotFound)
		}
		want[c.uid] = cid
	}
	first := localDay(q.From, d.loc).AddDate(0, 0, -1)
	last := localDay(q.To.Add(-time.Nanosecond), d.loc)
	inv := driver.Inventory{Complete: true}
	var all, unmapped []slotItem
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		ds, err := readSlots(ctx, d.c, day)
		var ue *driver.UpstreamError
		if err != nil {
			if errors.As(err, &ue) && (ue.Kind == driver.UpstreamUnavailable || ue.Kind == driver.UpstreamRejected) {
				inv.Complete = false
				break
			}
			return driver.Inventory{}, err
		}
		items, err := d.dayItems(ds, want)
		if err != nil {
			return driver.Inventory{}, err
		}
		all = append(all, items...)
		unmapped = append(unmapped, d.unmappedMirrors(ds)...)
		if ds.BeyondBookingDepth {
			inv.Complete = false
			break
		}
	}
	notes := map[string]string{} // booking id -> notes read from the booking
	for i := range all {
		it := &all[i]
		if !it.needNotes {
			continue
		}
		text, ok := notes[it.ID]
		if !ok {
			uid, _ := parseID(it.ID)
			b, found, err := d.load(ctx, uid)
			if err != nil {
				return driver.Inventory{}, err
			}
			if found && b.Notes != nil {
				text = *b.Notes
			}
			notes[it.ID] = text
		}
		it.Class, it.Source, it.Partner = driver.ClassOfComment(text)
	}
	for _, it := range mergeItems(all) {
		if it.Start.Before(q.To) && it.End.After(q.From) {
			inv.Items = append(inv.Items, it)
		}
	}
	for _, it := range mergeItems(unmapped) {
		if it.Start.Before(q.To) && it.End.After(q.From) {
			inv.Unmapped = append(inv.Unmapped, it)
		}
	}
	return inv, nil
}

// unmappedMirrors are the gateway's mirrors on the day's courts that no
// court of the config maps (a court mapping changed under them):
// bookings of the service client
// whose notes, as the schedule gives them, read as a mirror's marker. One
// listed without its notes is not read again: it is not the loop's to
// judge, only to report when it can tell.
func (d *Driver) unmappedMirrors(ds daySchedule) []slotItem {
	var out []slotItem
	for _, ct := range *ds.Courts {
		if _, mapped := d.byUID[ct.CourtUID]; mapped || ct.Slots == nil || d.service == "" {
			continue
		}
		for _, s := range *ct.Slots {
			if s == nil || s.Type == "DRAFT" || s.BookingID == "" || s.ClientUID != d.service {
				continue
			}
			text := s.Notes
			if text == nil {
				text = s.NotesAbstract
			}
			if text == nil {
				continue
			}
			m, st := driver.ParseMirrorMarker(*text)
			start, ok1 := parseLocal(s.Begin, d.loc)
			end, ok2 := parseLocal(s.End, d.loc)
			id, ok3 := slotID(s, ct.CourtUID)
			if st != driver.MarkerValid || !ok1 || !ok2 || !ok3 || !end.After(start) {
				continue
			}
			out = append(out, slotItem{MirrorItem: driver.MirrorItem{ID: id, Court: ct.CourtUID, Start: start, End: end, Class: driver.ClassMirror, Source: &m}})
		}
	}
	return out
}

// mergeItems joins the overlapping or touching items of one id on one
// court (a booking listed slot by slot, or under two days), as merge does
// intervals; the first slot's class stands.
func mergeItems(items []slotItem) []driver.MirrorItem {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Court != b.Court {
			return a.Court < b.Court
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Start.Before(b.Start)
	})
	var out []driver.MirrorItem
	for _, it := range items {
		if n := len(out); n > 0 && out[n-1].ID == it.ID && out[n-1].Court == it.Court && !it.Start.After(out[n-1].End) {
			if it.End.After(out[n-1].End) {
				out[n-1].End = it.End
			}
			continue
		}
		out = append(out, it.MirrorItem)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// bookingItems are a booking's intervals on the configured courts (one
// per court), classified by its client and notes.
func (d *Driver) bookingItems(id string, b *lundaBooking) ([]driver.MirrorItem, error) {
	class, source, partner := driver.ClassClub, (*driver.MirrorMarker)(nil), ""
	if d.service != "" && b.customer() == d.service {
		text := ""
		if b.Notes != nil {
			text = *b.Notes
		}
		class, source, partner = driver.ClassOfComment(text)
	}
	var items []slotItem
	for _, s := range b.Slots {
		court, ok := d.byUID[s.CourtUID]
		if !ok {
			continue
		}
		st, ok1 := parseLocal(s.Begin, d.loc)
		en, ok2 := parseLocal(s.End, d.loc)
		if !ok1 || !ok2 || !en.After(st) {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opBookingLoad+": a slot without a valid begin and end")
		}
		items = append(items, slotItem{MirrorItem: driver.MirrorItem{ID: id, Court: court, Start: st, End: en, Class: class, Source: source, Partner: partner}})
	}
	return mergeItems(items), nil
}

// MirrorLookup implements driver.Mirrorer: a booking read by id
// (booking-load); none when it is gone or cancelled. A slot of no booking
// ("ls-") has no point read: driver.ErrNotSupported.
func (d *Driver) MirrorLookup(ctx context.Context, id string) ([]driver.MirrorItem, error) {
	if strings.HasPrefix(id, "ls-") {
		return nil, driver.ErrNotSupported
	}
	uid, ok := parseID(id)
	if !ok {
		return nil, nil
	}
	b, found, err := d.load(ctx, uid)
	if err != nil || !found || b.cancelled() {
		return nil, err
	}
	return d.bookingItems(id, &b)
}

// CreateBlock implements driver.Mirrorer: a booking of the gateway's kind
// (book) whose notes are the marker alone. As a partner's, a block that
// cannot be made whole is cancelled again; one whose making has an
// unknown outcome is not (the next pass reads it by its marker).
func (d *Driver) CreateBlock(ctx context.Context, req driver.BlockRequest) (driver.MirrorItem, error) {
	marker := req.Source.String()
	uid, err := d.book(ctx, driver.CreateRequest{Court: req.Court, Start: req.Start, End: req.End, Kind: driver.KindGame},
		marker, func(notes string) bool { return strings.TrimSpace(notes) == marker })
	if err != nil {
		return driver.MirrorItem{}, err
	}
	d.log.Info("mirror booking created in Lunda", "booking", "l-"+uid, "court", req.Court)
	src := req.Source
	return driver.MirrorItem{ID: "l-" + uid, Court: req.Court, Start: req.Start, End: req.End, Class: driver.ClassMirror, Source: &src}, nil
}

// isBlock establishes that a booking is still the mirror it was read as:
// on the service client, its notes exactly the same marker, on the same
// court with the same span, not started. ErrNotOwned (or ErrStarted,
// ErrClockUnknown) otherwise.
func (d *Driver) isBlock(b *lundaBooking, it driver.MirrorItem) error {
	court, start, end, err := d.span(b)
	if err != nil {
		return err
	}
	notes := ""
	if b.Notes != nil {
		notes = *b.Notes
	}
	m, st := driver.ParseMirrorMarker(notes)
	switch {
	case d.service == "" || b.customer() != d.service, st != driver.MarkerValid || it.Source == nil || m != *it.Source,
		court != it.Court || !start.Equal(it.Start) || !end.Equal(it.End):
		return driver.ErrNotOwned
	}
	now, known := d.clock()
	switch {
	case !known:
		return driver.ErrClockUnknown
	case !start.After(now):
		return driver.ErrStarted
	}
	return nil
}

// DeleteBlock implements driver.Mirrorer: the booking is read again and
// cancelled only while it is still the mirror read before (isBlock); a
// cancel sent again with a renewed session checks that once more.
func (d *Driver) DeleteBlock(ctx context.Context, it driver.MirrorItem) error {
	uid, ok := parseID(it.ID)
	if !ok {
		return driver.ErrNotOwned
	}
	still := func(ctx context.Context) error {
		b, found, err := d.load(ctx, uid)
		switch {
		case err != nil:
			return err
		case !found || b.cancelled():
			return errGone
		}
		return d.isBlock(&b, it)
	}
	switch err := still(ctx); {
	case errors.Is(err, errGone):
		return nil
	case err != nil:
		return err
	}
	status, _, err := d.c.callGuarded(ctx, opCancel, nil, map[string]any{"bookingId": uid, "bookingType": "COURT", "forced": false, "reason": reasonMirror}, still)
	switch {
	case errors.Is(err, errGone):
		return nil
	case err != nil:
		return err
	case status >= 200 && status <= 299, status == http.StatusNotFound:
		d.log.Info("mirror booking cancelled in Lunda", "booking", it.ID, "court", it.Court)
		return nil
	}
	return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("DELETE /%s answered HTTP %d", opCancel, status))
}
