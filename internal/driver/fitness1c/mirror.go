package fitness1c

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// 1C:Fitness in a pair (driver.Mirrorer). A rental of 1C has no comment,
// so a mirror cannot carry a marker: it is a rental of the mirror service
// made by a technical client of its own, the mirror client
// (crm.mirror_client_phone) — never the client of the partners' bookings
// (crm.client_phone), or the two would mix. Which booking of the
// other CRM a mirror is for is told by its court and span alone (the
// validity rule); 1C books whole seances, so a mirror takes its source's
// span rounded out to the grid (BlockSpan).
//
// A rental of the mirror client is a mirror whatever its service: the
// client alone tells the class, so a mirror whose service the club
// has edited is still the gateway's — not club time to mirror back, not a
// rental to leave behind. The mirror service only says what a new
// mirror is rented by.
//
// The club's own busy time is the grid's: 1C does not say what a busy
// half-hour holds (a rental, a class, a closed hour), so it is opaque —
// mirrored within the club's hours only — and has no id of its own: its
// id names its room and span ("x-…"), and its point read is the grid read
// again.

var _ driver.Mirrorer = (*Driver)(nil)

// MirrorHorizon implements driver.Mirrorer: the grid is read for any
// dates; the horizon of a zone check is far enough.
func (d *Driver) MirrorHorizon(context.Context) (time.Duration, error) {
	return driver.ZoneHorizon, nil
}

// BlockSpan implements driver.Mirrorer: the span rounded out to whole
// seances of the club's day.
func (d *Driver) BlockSpan(start, end time.Time) (time.Time, time.Time) {
	floor := func(t time.Time) time.Time {
		t = t.In(d.loc)
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()/seance*seance, 0, 0, d.loc)
	}
	s, e := floor(start), floor(end)
	if e.Before(end) {
		e = e.Add(seance * time.Minute)
	}
	return s, e
}

// mirrorService is the rental service of the mirrors: crm.mirror_service,
// else the service of the partners' games.
func (d *Driver) mirrorService() string {
	if d.mirrorSvc != "" {
		return d.mirrorSvc
	}
	return d.services[driver.KindGame]
}

// MirrorCheck tells pair.New what a 1C of a pair needs more: the mirror
// client and a service to rent by.
func (d *Driver) MirrorCheck() error {
	var problems []string
	if d.c.mirrorPhone == "" {
		problems = append(problems, "crm.mirror_client_phone: у 1С в паре со второй CRM нужен отдельный технический клиент для отражений — у аренды 1С нет комментария, отражение узнаётся по клиенту")
	}
	if d.mirrorService() == "" {
		problems = append(problems, "crm.mirror_service: нужна бесплатная услуга аренды для отражений (или services.booking)")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// MirrorInventory implements driver.Mirrorer: the grid of every day the
// range touches, the partners' rentals (the technical client's) and the
// mirrors (the mirror client's) by their ids, and the rest of the busy
// time as the club's, opaque. Incomplete when either client's rentals are
// not all read or the hub's time is not known.
func (d *Driver) MirrorInventory(ctx context.Context, q driver.OccupancyQuery) (driver.Inventory, error) {
	rooms := map[string]string{}
	for _, cid := range q.Courts {
		c, ok := d.byCourt[cid]
		if !ok {
			return driver.Inventory{}, fmt.Errorf("court %q: %w", cid, driver.ErrNotFound)
		}
		rooms[c.room] = cid
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return driver.Inventory{}, err
	}
	svc, err := d.gridService(ctx)
	if err != nil {
		return driver.Inventory{}, err
	}
	days := daysBetween(q.From, q.To, loc)
	g, err := d.readGrid(ctx, svc, loc, days)
	if err != nil {
		return driver.Inventory{}, err
	}
	own, ownComplete, err := d.ownRentals(ctx, loc)
	if err != nil {
		return driver.Inventory{}, err
	}
	var mirrors []rental
	mirComplete := true
	if d.c.mirrorPhone != "" {
		if mirrors, mirComplete, err = d.rentalsOf(ctx, loc, mirrorClient, anyService); err != nil {
			return driver.Inventory{}, err
		}
	}
	_, known := d.clock()
	inv := driver.Inventory{Complete: known && ownComplete && mirComplete}
	var items []driver.MirrorItem
	for room, court := range rooms {
		rg := g.rooms[room]
		if rg == nil {
			return driver.Inventory{}, driver.Upstream(driver.UpstreamInvalid, fmt.Sprintf("rental_rooms_times: the room of court %s is not in the grid", court))
		}
		var mine []span
		for _, set := range []struct {
			list  []rental
			class driver.MirrorClass
		}{{own, driver.ClassOwn}, {mirrors, driver.ClassMirror}} {
			for _, r := range set.list {
				if r.room != room {
					continue
				}
				mine = append(mine, r.span)
				it := driver.MirrorItem{ID: ownID(r.id), Court: court, Start: r.Start, End: r.End, Class: set.class, Started: r.started}
				if set.class == driver.ClassOwn {
					it.Partner = d.c.partner
				}
				items = append(items, it)
			}
		}
		for _, day := range days {
			spans, err := rg.busySpans(day, loc, svc.step(), mine)
			if err != nil {
				return driver.Inventory{}, err
			}
			for _, s := range spans {
				items = append(items, driver.MirrorItem{ID: foreignID(room, s), Court: court, Start: s.Start, End: s.End, Class: driver.ClassClub, Opaque: true})
			}
		}
	}
	for _, it := range items {
		if it.Start.Before(q.To) && it.End.After(q.From) {
			inv.Items = append(inv.Items, it)
		}
	}
	// the mirror client's rentals in rooms no court maps: reported, never
	// touched
	for _, r := range mirrors {
		if _, mapped := d.byRoom[r.room]; !mapped && r.Start.Before(q.To) && r.End.After(q.From) {
			inv.Unmapped = append(inv.Unmapped, driver.MirrorItem{ID: ownID(r.id), Court: r.room, Start: r.Start, End: r.End, Class: driver.ClassMirror, Started: r.started})
		}
	}
	return inv, nil
}

// MirrorLookup implements driver.Mirrorer: a busy stretch is the grid of
// its span read again (and the rentals of both clients): the stretch as it
// is now, if it is still exactly there. A rental is never a source of a
// mirror: no point read (driver.ErrNotSupported).
func (d *Driver) MirrorLookup(ctx context.Context, id string) ([]driver.MirrorItem, error) {
	room, s, ok := parseForeignID(id)
	if !ok {
		if _, own := parseOwnID(id); own {
			return nil, driver.ErrNotSupported
		}
		return nil, nil
	}
	court, ok := d.byRoom[room]
	if !ok {
		return nil, nil
	}
	inv, err := d.MirrorInventory(ctx, driver.OccupancyQuery{Courts: []string{court}, From: s.Start, To: s.End})
	if err != nil {
		return nil, err
	}
	if !inv.Complete {
		return nil, driver.Upstream(driver.UpstreamInvalid, "the technical clients' rentals were not all read: the stretch cannot be told")
	}
	var out []driver.MirrorItem
	for _, it := range inv.Items {
		if it.ID == id {
			out = append(out, it)
		}
	}
	return out, nil
}

// CreateBlock implements driver.Mirrorer: a rental of the mirror service
// by the mirror client over the span rounded out to the grid; like a
// partner's (Create), it is a block only when 1C confirms it at once,
// otherwise it is removed again.
func (d *Driver) CreateBlock(ctx context.Context, req driver.BlockRequest) (driver.MirrorItem, error) {
	c, ok := d.byCourt[req.Court]
	if !ok {
		return driver.MirrorItem{}, fmt.Errorf("court %q: %w", req.Court, driver.ErrNotFound)
	}
	svc := d.mirrorService()
	if svc == "" || d.c.mirrorPhone == "" {
		return driver.MirrorItem{}, fmt.Errorf("no mirror client or service in 1C: %w", driver.ErrBookingNotConfigured)
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return driver.MirrorItem{}, err
	}
	if err := d.bookingServiceNow(ctx, svc); err != nil {
		return driver.MirrorItem{}, err
	}
	start, end := d.BlockSpan(req.Start, req.End)
	body := map[string]any{
		"date_time":  start.In(loc).Format(dateTimeLayout),
		"service_id": svc,
		"room_id":    c.room,
		"count":      int(end.Sub(start)/time.Minute) / seance,
	}
	var out created1C
	err = d.c.as(ctx, mirrorClient, func(tok string) error {
		env, err := d.c.call(ctx, http.MethodPost, "/rental/", nil, body, tok, true)
		if err != nil {
			return err
		}
		if json.Unmarshal(env.Data, &out) != nil {
			return driver.Upstream(driver.UpstreamInvalid, "POST /rental/: data of an unexpected form")
		}
		return nil
	})
	if codeOf(err) == codeRoomBusy {
		return driver.MirrorItem{}, fmt.Errorf("POST /rental/: %w", driver.ErrSlotUnavailable)
	}
	if err != nil {
		return driver.MirrorItem{}, err
	}
	if out.Rental == nil || !uuidRe.MatchString(out.Rental.ID) {
		// the rental may exist: the next pass reads it among the mirror client's
		return driver.MirrorItem{}, driver.Upstream(driver.UpstreamInvalid, "POST /rental/: answer has no valid rental id")
	}
	id := strings.ToLower(out.Rental.ID)
	if out.TemporarilyReserved == nil || *out.TemporarilyReserved || out.Status == nil || *out.Status != statusConfirmed {
		d.compensate(ctx, mirrorClient, id, "a mirror not confirmed without payment")
		return driver.MirrorItem{}, driver.Upstream(driver.UpstreamRejected,
			"POST /rental/: 1C made a temporary reservation, not a block: the mirror service must not require a payment basis")
	}
	d.log.Info("mirror rental created", "court", req.Court)
	return driver.MirrorItem{ID: ownID(id), Court: req.Court, Start: start, End: end, Class: driver.ClassMirror}, nil
}

// DeleteBlock implements driver.Mirrorer: the rental is cancelled only
// while it is among the mirror client's planned rentals (of any service),
// in the court's room, with the same span, not started.
func (d *Driver) DeleteBlock(ctx context.Context, it driver.MirrorItem) error {
	rid, ok := parseOwnID(it.ID)
	c, configured := d.byCourt[it.Court]
	if !ok || !configured || it.Source != nil || d.c.mirrorPhone == "" {
		return driver.ErrNotOwned
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return err
	}
	mirrors, _, err := d.rentalsOf(ctx, loc, mirrorClient, anyService)
	if err != nil {
		return err
	}
	for _, r := range mirrors {
		if r.id != rid {
			continue
		}
		if r.room != c.room || !r.Start.Equal(it.Start) || !r.End.Equal(it.End) {
			return driver.ErrNotOwned
		}
		now, known := d.clock()
		switch {
		case !known:
			return driver.ErrClockUnknown
		case r.started || !r.Start.After(now):
			return driver.ErrStarted
		}
		if err := d.deleteRental(ctx, mirrorClient, rid); err != nil {
			return err
		}
		d.log.Info("mirror rental cancelled", "court", it.Court)
		return nil
	}
	r, _, err := d.readRental(ctx, rid, loc)
	if err != nil {
		return err
	}
	if *r.Canceled {
		return nil
	}
	return driver.ErrNotOwned
}
