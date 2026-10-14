package viva

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// markerPrefix starts the ownership marker that begins the comment of
// every record the gateway creates: "[cg:<partner>] <note>".
const markerPrefix = "[cg:"

// maxComment caps a comment, marker included, in characters: well within
// what the CRM keeps, so the marker is never cut off.
const maxComment = 1000

func marker(partner string) string { return markerPrefix + partner + "]" }

// buildComment puts the partner's marker in front of the note, cut to
// maxComment. Ownership is read only from the start of the comment
// (hasMarker), so a marker inside the note proves nothing.
func buildComment(note, partner string) string {
	m := marker(partner)
	if room := maxComment - utf8.RuneCountInString(m) - 1; utf8.RuneCountInString(note) > room {
		note = string([]rune(note)[:max(room, 0)])
	}
	if note == "" {
		return m
	}
	return m + " " + note
}

func hasMarker(text, partner string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), marker(partner))
}

// parseID splits "e-<uuid>" / "b-<uuid>".
func parseID(id string) (kind string, uuid string, ok bool) {
	switch {
	case strings.HasPrefix(id, "e-"):
		kind = "exercises"
	case strings.HasPrefix(id, "b-"):
		kind = "breaks"
	default:
		return "", "", false
	}
	uuid = strings.ToLower(id[2:])
	if !uuidRe.MatchString(uuid) {
		return "", "", false
	}
	return kind, uuid, true
}

type createdResponse struct {
	ID string `json:"id"`
}

// Create implements driver.Driver. With a service configured for the kind
// it creates an exercise (an ordinary booking of that service, without a
// client); otherwise a technical break.
func (d *Driver) Create(ctx context.Context, req driver.CreateRequest) (driver.Booking, error) {
	c, ok := d.byCourt[req.Court]
	if !ok {
		return driver.Booking{}, fmt.Errorf("court %q: %w", req.Court, driver.ErrNotFound)
	}
	comment := buildComment(req.Note, req.Partner)
	var (
		path    string
		payload any
		prefix  string
	)
	if svc := d.services[req.Kind]; svc != nil {
		path, prefix = "/exercises", "e-"
		payload = map[string]any{
			"direction":       svc.Direction,
			"type":            svc.Type,
			"roomId":          c.room,
			"timeFrom":        formatTime(req.Start),
			"timeTo":          formatTime(req.End),
			"maxClientsCount": 1,
			"requirements":    []any{},
			"comment":         comment,
		}
	} else {
		path, prefix = "/breaks", "b-"
		payload = map[string]any{
			"title":    d.breakTitle,
			"timeFrom": formatTime(req.Start),
			"timeTo":   formatTime(req.End),
			"roomId":   c.room,
			"comment":  comment,
			"mergable": false,
		}
	}
	status, data, err := d.c.do(ctx, http.MethodPost, path, nil, payload)
	if err != nil {
		return driver.Booking{}, err
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
	case status == http.StatusConflict:
		return driver.Booking{}, fmt.Errorf("POST %s: %w", path, driver.ErrSlotUnavailable)
	default:
		return driver.Booking{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST %s answered HTTP %d", path, status))
	}
	var cr createdResponse
	if err := json.Unmarshal(data, &cr); err != nil || !uuidRe.MatchString(cr.ID) {
		// The record may exist upstream without us knowing its id. The
		// partner sees an error and must re-read occupancy before retrying.
		return driver.Booking{}, driver.Upstream(driver.UpstreamInvalid, "POST "+path+": response has no valid id")
	}
	d.log.Info("record created", "kind", strings.TrimPrefix(path, "/"), "court", req.Court)
	return driver.Booking{
		ID: prefix + strings.ToLower(cr.ID), Court: req.Court, Start: req.Start, End: req.End, Status: driver.StatusActive,
	}, nil
}

// read fetches one record, decoded and as raw fields (so the caller can
// tell an absent field from an empty one). found=false means the CRM
// answered 404.
func (d *Driver) read(ctx context.Context, kind, uuid string) (s slot, raw map[string]json.RawMessage, found bool, err error) {
	status, data, err := d.c.do(ctx, http.MethodGet, "/"+kind+"/"+uuid, nil, nil)
	if err != nil {
		return s, nil, false, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return s, nil, false, nil
	default:
		return s, nil, false, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s/{id} answered HTTP %d", kind, status))
	}
	if json.Unmarshal(data, &raw) != nil || raw == nil || json.Unmarshal(data, &s) != nil {
		return s, nil, false, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: response is not a record")
	}
	if s.ID != "" && !strings.EqualFold(s.ID, uuid) {
		return s, nil, false, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: record id does not match")
	}
	s.ID = uuid
	return s, raw, true, nil
}

func (d *Driver) Get(ctx context.Context, id string) (driver.Booking, error) {
	kind, uuid, ok := parseID(id)
	if !ok {
		return driver.Booking{}, driver.ErrNotFound
	}
	s, _, found, err := d.read(ctx, kind, uuid)
	if err != nil {
		return driver.Booking{}, err
	}
	if !found {
		return driver.Booking{}, driver.ErrNotFound
	}
	prefix := id[:2]
	rooms := d.byRoom
	if s.Room == nil || s.Room.ID == "" {
		// a break without a room closes its studio: occupancy shows it on
		// every court, and it reads on the first one; another studio's is
		// not this club's
		if s.Studio == nil || !strings.EqualFold(s.Studio.ID, d.studio) {
			return driver.Booking{}, driver.ErrNotFound
		}
		rooms = map[string]string{d.courts[0].room: d.byRoom[d.courts[0].room]}
	}
	ivs, err := d.intervals(kind, prefix, slot{ID: s.ID, TimeFrom: s.TimeFrom, TimeTo: s.TimeTo, Room: s.Room}, rooms)
	if err != nil {
		return driver.Booking{}, err
	}
	if len(ivs) == 0 {
		return driver.Booking{}, d.notACourt(s)
	}
	iv := ivs[0]
	status := driver.StatusActive
	if s.Canceled != nil && *s.Canceled {
		status = driver.StatusCancelled
	}
	return driver.Booking{ID: iv.ID, Court: iv.Court, Start: iv.Start, End: iv.End, Status: status}, nil
}

// Cancel implements driver.Driver: it deletes a record only when, read
// back, its comment starts with the caller's marker (checkOwnership; the
// break title is never evidence) and it has not started. A record that
// is gone or already cancelled counts as cancelled. Viva has no
// conditional delete: a change between the read and the DELETE is not
// seen (docs/hub.md).
func (d *Driver) Cancel(ctx context.Context, id string, partner string) error {
	kind, uuid, ok := parseID(id)
	if !ok {
		return driver.ErrNotFound
	}
	s, raw, found, err := d.read(ctx, kind, uuid)
	if err != nil {
		return err
	}
	if !found {
		// gone, unless the id names a record of the other kind (a wrong
		// prefix): that one is not this booking
		other := map[string]string{"exercises": "breaks", "breaks": "exercises"}[kind]
		if _, _, exists, err := d.read(ctx, other, uuid); err != nil || exists {
			return cmp.Or(err, driver.ErrNotFound)
		}
		return nil
	}
	if s.Canceled != nil && *s.Canceled {
		return nil
	}
	if s.Room == nil {
		return driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: record has no room")
	}
	if _, ours := d.byRoom[strings.ToLower(s.Room.ID)]; !ours {
		return d.notACourt(s)
	}
	if err := d.checkOwnership(kind, raw, partner); err != nil {
		return err
	}
	start, ok := parseTime(s.TimeFrom)
	now, known := d.clock()
	switch {
	case !ok:
		return driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: record has no valid timeFrom")
	case !known:
		return driver.ErrClockUnknown
	case !start.After(now):
		return driver.ErrStarted
	}
	status, _, err := d.c.do(ctx, http.MethodDelete, "/"+kind+"/"+uuid, nil, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted, http.StatusNotFound:
		d.log.Info("record deleted", "kind", kind)
		return nil
	default:
		return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("DELETE /%s/{id} answered HTTP %d", kind, status))
	}
}

// notACourt: a record in no configured court's room is on a court removed
// from this club's config, or another studio's: no booking of this club.
func (d *Driver) notACourt(s slot) error {
	if s.Studio != nil && strings.EqualFold(s.Studio.ID, d.studio) {
		return driver.ErrCourtNotConfigured
	}
	return driver.ErrNotFound
}

// clientKeys are the fields of an exercise that may list its clients. A
// value other than null, "", [] or {} under any of them means the
// exercise has a client: Cancel refuses it.
var clientKeys = []string{"clients", "guests", "participants", "bookings"}

func isEmpty(r json.RawMessage) bool {
	switch strings.Join(strings.Fields(string(r)), "") {
	case "", "null", `""`, "[]", "{}":
		return true
	}
	return false
}

// checkOwnership fails closed: an exercise is deleted only when it shows
// no client at all — clientsCount present and 0, no masterServiceBooking,
// nothing under clientKeys — and its comment starts with the caller's
// marker.
func (d *Driver) checkOwnership(kind string, raw map[string]json.RawMessage, partner string) error {
	invalid := func(what string) error {
		return driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: "+what)
	}
	if kind == "exercises" {
		for _, k := range clientKeys {
			if !isEmpty(raw[k]) {
				return driver.ErrNotOwned
			}
		}
		if m := raw["masterServiceBooking"]; len(m) > 0 && string(m) != "null" { // even {}: stricter than isEmpty
			return driver.ErrNotOwned
		}
		var n float64
		if cc := raw["clientsCount"]; isEmpty(cc) {
			return invalid("clientsCount is missing: the record may have clients")
		} else if err := json.Unmarshal(cc, &n); err != nil {
			return invalid("clientsCount is not a number")
		}
		if n != 0 {
			return driver.ErrNotOwned
		}
	}
	r, ok := raw["comment"]
	if !ok || isEmpty(r) {
		return driver.ErrNotOwned
	}
	var comment string
	if err := json.Unmarshal(r, &comment); err != nil {
		return invalid("comment is not a string")
	}
	if !hasMarker(comment, partner) {
		return driver.ErrNotOwned
	}
	return nil
}

// MarkPaid implements driver.Driver: VivaCRM offers no payment mark.
func (d *Driver) MarkPaid(context.Context, string, string) error { return driver.ErrNotSupported }
