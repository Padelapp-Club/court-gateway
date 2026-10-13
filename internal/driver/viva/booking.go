package viva

import (
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
// maxComment. A marker-like sequence inside the note is defused so a
// partner cannot forge another partner's ownership.
func buildComment(note, partner string) string {
	m := marker(partner)
	note = strings.ReplaceAll(note, markerPrefix, "[cg ")
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
		return driver.Booking{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST %s answered HTTP %d", path, status), nil)
	}
	var cr createdResponse
	if err := json.Unmarshal(data, &cr); err != nil || !uuidRe.MatchString(cr.ID) {
		// The record may exist upstream without us knowing its id. The
		// partner sees an error and must re-read occupancy before retrying.
		return driver.Booking{}, driver.Upstream(driver.UpstreamInvalid, "POST "+path+": response has no valid id", nil)
	}
	d.log.Info("record created", "kind", strings.TrimPrefix(path, "/"), "court", req.Court)
	return driver.Booking{
		ID: prefix + strings.ToLower(cr.ID), Court: req.Court, Start: req.Start, End: req.End, Status: driver.StatusActive,
	}, nil
}

// read fetches one record as raw fields, so the caller can tell an absent
// field from an empty one. found=false means the CRM answered 404.
func (d *Driver) read(ctx context.Context, kind, uuid string) (map[string]json.RawMessage, bool, error) {
	status, data, err := d.c.do(ctx, http.MethodGet, "/"+kind+"/"+uuid, nil, nil)
	if err != nil {
		return nil, false, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s/{id} answered HTTP %d", kind, status), nil)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return nil, false, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: response is not a JSON object", nil)
	}
	return raw, true, nil
}

func (d *Driver) decodeSlot(kind, uuid string, raw map[string]json.RawMessage) (slot, error) {
	b, _ := json.Marshal(raw)
	var s slot
	if err := json.Unmarshal(b, &s); err != nil {
		return slot{}, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: malformed record", nil)
	}
	if s.ID != "" && !strings.EqualFold(s.ID, uuid) {
		return slot{}, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: record id does not match", nil)
	}
	s.ID = uuid
	return s, nil
}

// Get implements driver.Driver.
func (d *Driver) Get(ctx context.Context, id string) (driver.Booking, error) {
	kind, uuid, ok := parseID(id)
	if !ok {
		return driver.Booking{}, driver.ErrNotFound
	}
	raw, found, err := d.read(ctx, kind, uuid)
	if err != nil {
		return driver.Booking{}, err
	}
	if !found {
		return driver.Booking{}, driver.ErrNotFound
	}
	s, err := d.decodeSlot(kind, uuid, raw)
	if err != nil {
		return driver.Booking{}, err
	}
	prefix := id[:2]
	iv, _, err := d.interval(kind, prefix, slot{ID: s.ID, TimeFrom: s.TimeFrom, TimeTo: s.TimeTo, Room: s.Room}, d.byRoom)
	if err != nil {
		return driver.Booking{}, err
	}
	if iv.Court == "" {
		return driver.Booking{}, driver.ErrNotFound // a room that is not a configured court
	}
	status := driver.StatusActive
	if s.Canceled != nil && *s.Canceled {
		status = driver.StatusCancelled
	}
	return driver.Booking{ID: iv.ID, Court: iv.Court, Start: iv.Start, End: iv.End, Status: status}, nil
}

// Cancel implements driver.Driver. The record is read first, and the
// gateway cancels it only when it can see that the record is the
// caller's: its comment, as read back, must start with the caller's
// marker ("[cg:<partner>]", written by Create), and only before it starts
// (ErrStarted). Anything else is refused
// (NOT_OWNED) — a comment without the marker or with another partner's,
// a record read without a comment, an exercise with a client booking or
// clients. The break title is never evidence: it is one text for every
// partner and anyone can type it. A record that is gone (404) or already
// cancelled counts as cancelled.
func (d *Driver) Cancel(ctx context.Context, id string, partner string) error {
	kind, uuid, ok := parseID(id)
	if !ok {
		return driver.ErrNotFound
	}
	raw, found, err := d.read(ctx, kind, uuid)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	s, err := d.decodeSlot(kind, uuid, raw)
	if err != nil {
		return err
	}
	if s.Canceled != nil && *s.Canceled {
		return nil
	}
	if s.Room == nil {
		return driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: record has no room", nil)
	}
	if _, ours := d.byRoom[strings.ToLower(s.Room.ID)]; !ours {
		return driver.ErrNotOwned
	}
	if err := d.checkOwnership(kind, raw, partner); err != nil {
		return err
	}
	if start, ok := parseTime(s.TimeFrom); !ok {
		return driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: record has no valid timeFrom", nil)
	} else if !start.After(d.now()) {
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
		return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("DELETE /%s/{id} answered HTTP %d", kind, status), nil)
	}
}

func isNull(r json.RawMessage) bool { return len(r) == 0 || string(r) == "null" }

func (d *Driver) checkOwnership(kind string, raw map[string]json.RawMessage, partner string) error {
	invalid := func(what string) error {
		return driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+"/{id}: "+what, nil)
	}
	if kind == "exercises" {
		if msb, ok := raw["masterServiceBooking"]; ok && !isNull(msb) {
			return driver.ErrNotOwned
		}
		if cc, ok := raw["clientsCount"]; ok && !isNull(cc) {
			var n float64
			if err := json.Unmarshal(cc, &n); err != nil {
				return invalid("clientsCount is not a number")
			}
			if n > 0 {
				return driver.ErrNotOwned
			}
		}
	}
	r, ok := raw["comment"]
	if !ok || isNull(r) {
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
