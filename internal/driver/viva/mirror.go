package viva

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Viva in a pair (driver.Mirrorer): a booking of the other CRM is mirrored
// as a technical break of the court whose comment is the marker and
// nothing else ("[cg-mirror:<src>:<id>]"). The gateway's mirrors are
// breaks with the marker (F4): a marker copied into an exercise, or into
// a break as part of a longer comment, is not one.

// mirrorTitles are the titles of a mirror's break by its source's CRM:
// what the club's admin sees in Viva's schedule.
var mirrorTitles = map[string]string{"lunda": "Бронь в Лунде"}

// defaultMirrorTitle is the title of a mirror of any other CRM.
const defaultMirrorTitle = "Бронь во второй CRM"

var _ driver.Mirrorer = (*Driver)(nil)

// MirrorHorizon implements driver.Mirrorer: Viva lists any dates; the
// horizon of a zone check is far enough.
func (d *Driver) MirrorHorizon() time.Duration { return driver.ZoneHorizon }

// BlockSpan implements driver.Mirrorer: a break takes any span.
func (d *Driver) BlockSpan(start, end time.Time) (time.Time, time.Time) { return start, end }

// commentText reads a record's comment: present is false when the field
// is absent or null.
func commentText(raw json.RawMessage) (text string, present bool) {
	if isEmpty(raw) && strings.TrimSpace(string(raw)) != `""` {
		return "", false
	}
	if json.Unmarshal(raw, &text) != nil {
		return "", true // not a string: no marker
	}
	return text, true
}

// classOf classifies a record of kind by its comment.
func classOf(kind, comment string) (driver.MirrorClass, *driver.MirrorMarker, string) {
	class, m, partner := driver.ClassOfComment(comment)
	if kind == "exercises" && (class == driver.ClassMirror || class == driver.ClassDamaged) {
		return driver.ClassClub, nil, "" // the gateway's mirrors are breaks
	}
	return class, m, partner
}

// comment is a list item's comment; an item listed without one is read by
// id ([UNVERIFIED] whether Viva's lists carry comments: the stub's do).
func (d *Driver) comment(ctx context.Context, kind string, it slot) (string, error) {
	if text, ok := commentText(it.Comment); ok {
		return text, nil
	}
	_, raw, found, err := d.read(ctx, kind, strings.ToLower(it.ID))
	if err != nil || !found {
		return "", err
	}
	text, _ := commentText(raw["comment"])
	return text, nil
}

// MirrorInventory implements driver.Mirrorer: the lists occupancy reads,
// classified. It is complete when every exercise starting from q.From's
// day is listed (Viva leaves out exercises of the CRM's past days) and no
// list may be cut into pages.
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
	now, known := d.clock()
	inv := driver.Inventory{Complete: known && !day(q.From, loc).Before(day(now.Add(crmClockLead), loc))}
	for _, kind := range []string{"exercises", "breaks"} {
		dateFrom, dateTo, _ := d.window(kind, loc, q.From, q.To, now)
		items, whole, err := d.listRange(ctx, kind, dateFrom, dateTo)
		if err != nil {
			return driver.Inventory{}, err
		}
		inv.Complete = inv.Complete && whole
		prefix := map[string]string{"exercises": "e-", "breaks": "b-"}[kind]
		for _, it := range items {
			ivs, err := d.intervals(kind, prefix, it, rooms)
			if err != nil {
				return driver.Inventory{}, err
			}
			var text string
			var read bool
			for _, iv := range ivs {
				if !iv.Start.Before(q.To) || !iv.End.After(q.From) {
					continue
				}
				if !read {
					if text, err = d.comment(ctx, kind, it); err != nil {
						return driver.Inventory{}, err
					}
					read = true
				}
				inv.Items = append(inv.Items, item(kind, iv, text))
			}
		}
	}
	return inv, nil
}

// item is an interval of a record of kind, classified by its comment.
func item(kind string, iv driver.Interval, comment string) driver.MirrorItem {
	class, m, partner := classOf(kind, comment)
	return driver.MirrorItem{ID: iv.ID, Court: iv.Court, Start: iv.Start, End: iv.End, Class: class, Source: m, Partner: partner}
}

// MirrorLookup implements driver.Mirrorer: the record read by id; none
// when it is gone, cancelled or of another studio.
func (d *Driver) MirrorLookup(ctx context.Context, id string) ([]driver.MirrorItem, error) {
	kind, uuid, ok := parseID(id)
	if !ok {
		return nil, nil
	}
	s, raw, found, err := d.read(ctx, kind, uuid)
	if err != nil || !found || s.Canceled != nil && *s.Canceled {
		return nil, err
	}
	if (s.Room == nil || s.Room.ID == "") && (s.Studio == nil || !strings.EqualFold(s.Studio.ID, d.studio)) {
		return nil, nil // a roomless break of another studio
	}
	ivs, err := d.intervals(kind, id[:2], s, d.byRoom)
	if err != nil {
		return nil, err
	}
	text, _ := commentText(raw["comment"])
	var out []driver.MirrorItem
	for _, iv := range ivs {
		out = append(out, item(kind, iv, text))
	}
	return out, nil
}

// CreateBlock implements driver.Mirrorer: a break of the court over the
// span, its comment the marker alone.
func (d *Driver) CreateBlock(ctx context.Context, req driver.BlockRequest) (driver.MirrorItem, error) {
	c, ok := d.byCourt[req.Court]
	if !ok {
		return driver.MirrorItem{}, fmt.Errorf("court %q: %w", req.Court, driver.ErrNotFound)
	}
	title, ok := mirrorTitles[req.Source.Src]
	if !ok {
		title = defaultMirrorTitle
	}
	status, data, err := d.c.do(ctx, http.MethodPost, "/breaks", nil, map[string]any{
		"title":    title,
		"timeFrom": formatTime(req.Start),
		"timeTo":   formatTime(req.End),
		"roomId":   c.room,
		"comment":  req.Source.String(),
		"mergable": false,
	})
	if err != nil {
		return driver.MirrorItem{}, err
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
	case status == http.StatusConflict:
		return driver.MirrorItem{}, fmt.Errorf("POST /breaks: %w", driver.ErrSlotUnavailable)
	default:
		return driver.MirrorItem{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /breaks answered HTTP %d", status))
	}
	var cr createdResponse
	if err := json.Unmarshal(data, &cr); err != nil || !uuidRe.MatchString(cr.ID) {
		// the break may exist: the next pass reads it by its marker
		return driver.MirrorItem{}, driver.Upstream(driver.UpstreamInvalid, "POST /breaks: response has no valid id")
	}
	src := req.Source
	d.log.Info("mirror break created", "court", req.Court)
	return driver.MirrorItem{ID: "b-" + strings.ToLower(cr.ID), Court: req.Court, Start: req.Start, End: req.End,
		Class: driver.ClassMirror, Source: &src}, nil
}

// DeleteBlock implements driver.Mirrorer: the break is read again and
// deleted only while it is still the mirror read before — a break of the
// court's room, the same span, its comment the same marker alone — and
// has not started.
func (d *Driver) DeleteBlock(ctx context.Context, it driver.MirrorItem) error {
	kind, uuid, ok := parseID(it.ID)
	c, configured := d.byCourt[it.Court]
	if !ok || kind != "breaks" || it.Source == nil || !configured {
		return driver.ErrNotOwned
	}
	s, raw, found, err := d.read(ctx, kind, uuid)
	if err != nil {
		return err
	}
	if !found || s.Canceled != nil && *s.Canceled {
		return nil
	}
	start, ok1 := parseTime(s.TimeFrom)
	end, ok2 := parseTime(s.TimeTo)
	text, _ := commentText(raw["comment"])
	m, st := driver.ParseMirrorMarker(text)
	switch {
	case s.Room == nil || strings.ToLower(s.Room.ID) != c.room, !ok1 || !ok2, !start.Equal(it.Start) || !end.Equal(it.End),
		st != driver.MarkerValid || m != *it.Source:
		return driver.ErrNotOwned
	}
	now, known := d.clock()
	switch {
	case !known:
		return driver.ErrClockUnknown
	case !start.After(now):
		return driver.ErrStarted
	}
	status, _, err := d.c.do(ctx, http.MethodDelete, "/breaks/"+uuid, nil, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted, http.StatusNotFound:
		d.log.Info("mirror break deleted", "court", it.Court)
		return nil
	}
	return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("DELETE /breaks/{id} answered HTTP %d", status))
}
