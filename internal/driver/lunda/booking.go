package lunda

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// markerPrefix starts the ownership marker that begins the notes of every
// booking the gateway makes: "[cg:<partner>] <note>".
const markerPrefix = "[cg:"

// maxNotes caps the notes, marker included, in characters, so the marker
// is never cut off. [UNVERIFIED] how long notes Lunda keeps.
const maxNotes = 1000

// rollbackTimeout bounds the cancel of a booking whose making failed; it
// runs under its own context, so the end of the partner's request does
// not leave the booking half made.
const rollbackTimeout = 20 * time.Second

// Reasons the gateway gives Lunda for its cancels (the club sees them).
const (
	reasonCancel   = "Отмена брони партнёром через шлюз PadelApp"
	reasonRollback = "Бронь шлюза PadelApp не завершена и отменена шлюзом"
)

func marker(partner string) string { return markerPrefix + partner + "]" }

// buildNotes puts the partner's marker in front of the note, cut to
// maxNotes. Ownership is read only from the start of the notes
// (hasMarker), so a marker inside the note proves nothing.
func buildNotes(note, partner string) string {
	m := marker(partner)
	if room := maxNotes - utf8.RuneCountInString(m) - 1; utf8.RuneCountInString(note) > room {
		note = string([]rune(note)[:max(room, 0)])
	}
	if note == "" {
		return m
	}
	return m + " " + note
}

func hasMarker(notes, partner string) bool {
	return strings.HasPrefix(strings.TrimSpace(notes), marker(partner))
}

// parseID takes the Lunda booking id out of "l-<id>"; the synthetic ids of
// slots of no booking ("ls-") are no booking.
func parseID(id string) (string, bool) {
	uid, ok := strings.CutPrefix(id, "l-")
	return uid, ok && uidRe.MatchString(uid)
}

// lundaBooking is the subset of a booking the driver reads (booking-load,
// booking-create). Nothing about the customer but the id is decoded.
// [UNVERIFIED] customerUid, notes and slots as booking-load answers them
// (observed: id, bookingType, placeholder, price, status, payments).
type lundaBooking struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	Price       json.RawMessage `json:"price"`
	CustomerUID string          `json:"customerUid"`
	Customer    *struct {
		UID        string `json:"uid"`
		ProfileUID string `json:"profileUid"`
	} `json:"customer"`
	Notes    *string         `json:"notes"`
	Payments json.RawMessage `json:"payments"`
	Slots    []struct {
		CourtUID string `json:"courtUid"`
		Begin    string `json:"begin"`
		End      string `json:"end"`
	} `json:"slots"`
}

func (b *lundaBooking) customer() string {
	switch {
	case b.CustomerUID != "":
		return b.CustomerUID
	case b.Customer == nil:
		return ""
	case b.Customer.UID != "":
		return b.Customer.UID
	}
	return b.Customer.ProfileUID
}

func (b *lundaBooking) cancelled() bool { return strings.EqualFold(b.Status, "CANCELLED") }

// noPayments: the booking lists no payment (an empty, null or absent list).
func (b *lundaBooking) noPayments() bool {
	switch string(bytes.Join(bytes.Fields(b.Payments), nil)) {
	case "", "null", "[]":
		return true
	}
	return false
}

// span is the booking's start and end and its configured court: the
// first slot on a configured court gives the court, all slots the span.
func (d *Driver) span(b *lundaBooking) (court string, start, end time.Time, err error) {
	if len(b.Slots) == 0 {
		return "", start, end, driver.Upstream(driver.UpstreamInvalid, "GET /"+opBookingLoad+": the booking has no slots")
	}
	for _, s := range b.Slots {
		st, ok1 := parseLocal(s.Begin, d.loc)
		en, ok2 := parseLocal(s.End, d.loc)
		if !ok1 || !ok2 || !en.After(st) {
			return "", start, end, driver.Upstream(driver.UpstreamInvalid, "GET /"+opBookingLoad+": a slot without a valid begin and end")
		}
		if start.IsZero() || st.Before(start) {
			start = st
		}
		if en.After(end) {
			end = en
		}
		if c, ok := d.byUID[s.CourtUID]; ok && court == "" {
			court = c
		}
	}
	return court, start, end, nil
}

// load reads a booking; found=false when Lunda answers 404. [UNVERIFIED]
// the query parameter (bookingId) and what Lunda answers for an unknown
// booking: anything but 404 is not taken for "gone".
func (d *Driver) load(ctx context.Context, uid string) (b lundaBooking, found bool, err error) {
	status, res, err := d.c.call(ctx, opBookingLoad, url.Values{"bookingId": {uid}}, nil)
	if err != nil {
		return b, false, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return b, false, nil
	default:
		return b, false, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opBookingLoad, status))
	}
	if t := bytes.TrimSpace(res); len(t) == 0 || t[0] != '{' || json.Unmarshal(t, &b) != nil {
		return b, false, driver.Upstream(driver.UpstreamInvalid, "GET /"+opBookingLoad+": the answer is not a booking")
	}
	if b.ID != "" && b.ID != uid {
		return b, false, driver.Upstream(driver.UpstreamInvalid, "GET /"+opBookingLoad+": the booking's id does not match")
	}
	return b, true, nil
}

// Get implements driver.Driver.
func (d *Driver) Get(ctx context.Context, id string) (driver.Booking, error) {
	uid, ok := parseID(id)
	if !ok {
		return driver.Booking{}, driver.ErrNotFound
	}
	b, found, err := d.load(ctx, uid)
	if err != nil {
		return driver.Booking{}, err
	}
	if !found {
		return driver.Booking{}, driver.ErrNotFound
	}
	court, start, end, err := d.span(&b)
	if err != nil {
		return driver.Booking{}, err
	}
	if court == "" {
		return driver.Booking{}, driver.ErrCourtNotConfigured // the session is the club's: its booking, on another court
	}
	status := driver.StatusActive
	if b.cancelled() {
		status = driver.StatusCancelled
	}
	return driver.Booking{ID: id, Court: court, Start: start, End: end, Status: status}, nil
}

// decimalRe is a price as Lunda writes it (rubles, maybe with kopecks).
var decimalRe = regexp.MustCompile(`^[0-9]{1,9}(\.[0-9]{1,2})?$`)

// Create implements driver.Driver, as alex decided on 2026-10-17: an
// ordinary court booking on the club's service client
// (booking-create, placeholder false), its price adjusted by minus the
// price Lunda computed for it (update-courts-price-adjustment), so it is
// PAID without any payment, and its notes starting with the partner's
// marker (booking-notes-update). Read back, it must be PAID, list no
// payment and carry the marker and the service client; if any step after
// the create fails, the booking is cancelled and the partner gets an
// error — never a half-made booking (an unpaid debt of the service client
// in the club's books); a booking that has started meanwhile is not
// cancelled (rollback). A create whose outcome is unknown — no answer, a
// 5xx, an answer that does not read or has no id — cancels nothing: the
// club books its own tournaments on the same service client, so a booking
// found there without the marker may be the club's (unknownOutcome).
// Without a service client nothing is sent.
//
// A tournament is a booking of goal TOURNAMENT: the club's own tournaments
// are such bookings of its service client (seen in its schedule).
// [UNVERIFIED] that booking-create takes goal TOURNAMENT.
func (d *Driver) Create(ctx context.Context, req driver.CreateRequest) (driver.Booking, error) {
	c, ok := d.byCourt[req.Court]
	if !ok {
		return driver.Booking{}, fmt.Errorf("court %q: %w", req.Court, driver.ErrNotFound)
	}
	if d.service == "" {
		return driver.Booking{}, errNoServiceClient()
	}
	goal := "GAME"
	if req.Kind == driver.KindTournament {
		goal = "TOURNAMENT"
	}
	begin, end := formatLocal(req.Start, d.loc), formatLocal(req.End, d.loc)
	status, res, err := d.c.call(ctx, opCreate, nil, map[string]any{
		"customerUid": d.service, "placeholder": false, "goal": goal, "addPromisedPayment": false,
		"slots": []map[string]any{{"courtUid": c.uid, "begin": begin, "end": end, "pinned": false}},
	})
	if err != nil {
		var ue *driver.UpstreamError
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ue) && (ue.Kind == driver.UpstreamUnavailable || ue.Kind == driver.UpstreamInvalid) {
			return driver.Booking{}, d.unknownOutcome(ctx, req, c, err) // the booking may exist
		}
		return driver.Booking{}, err // refused before it was made (or not sent at all)
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
	case status == http.StatusConflict:
		return driver.Booking{}, fmt.Errorf("POST /%s: %w", opCreate, driver.ErrSlotUnavailable)
	default:
		return driver.Booking{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /%s answered HTTP %d", opCreate, status))
	}
	var b lundaBooking
	if json.Unmarshal(res, &b) != nil || !uidRe.MatchString(b.ID) {
		return driver.Booking{}, d.unknownOutcome(ctx, req, c, driver.Upstream(driver.UpstreamInvalid, "POST /"+opCreate+": the answer has no valid booking id"))
	}
	uid := b.ID
	price := strings.TrimSpace(string(b.Price))
	if !decimalRe.MatchString(price) {
		return driver.Booking{}, d.rollback(ctx, uid, req, driver.Upstream(driver.UpstreamInvalid, "POST /"+opCreate+": the answer has no valid price"))
	}
	if strings.Trim(price, "0.") != "" { // a booking of price 0 needs no adjustment
		if err := d.write(ctx, opAdjust, map[string]any{"bookingId": uid, "priceAdjustment": json.Number("-" + price)}); err != nil {
			return driver.Booking{}, d.rollback(ctx, uid, req, err)
		}
	}
	// [UNVERIFIED] the parameter's name: notes
	if err := d.write(ctx, opNotes, map[string]any{"bookingId": uid, "notes": buildNotes(req.Note, req.Partner)}); err != nil {
		return driver.Booking{}, d.rollback(ctx, uid, req, err)
	}
	got, found, err := d.load(ctx, uid)
	switch {
	case err != nil:
		return driver.Booking{}, d.rollback(ctx, uid, req, err)
	case !found:
		return driver.Booking{}, d.rollback(ctx, uid, req, driver.Upstream(driver.UpstreamInvalid, "GET /"+opBookingLoad+": the booking just made is not found"))
	case !strings.EqualFold(got.Status, "PAID") || !got.noPayments():
		return driver.Booking{}, d.rollback(ctx, uid, req, driver.Upstream(driver.UpstreamInvalid, "the booking just made is not PAID without payments"))
	case got.Notes == nil || !hasMarker(*got.Notes, req.Partner) || got.customer() != d.service:
		return driver.Booking{}, d.rollback(ctx, uid, req, driver.Upstream(driver.UpstreamInvalid, "the booking just made does not read back with the gateway's marker and service client"))
	}
	d.log.Info("booking created in Lunda", "booking", "l-"+uid, "court", req.Court)
	return driver.Booking{ID: "l-" + uid, Court: req.Court, Start: req.Start, End: req.End, Status: driver.StatusActive}, nil
}

// write sends a change of a booking: anything but a 2xx is an error.
func (d *Driver) write(ctx context.Context, op string, params map[string]any) error {
	status, _, err := d.c.call(ctx, op, nil, params)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("%s /%s answered HTTP %d", endpoints[op].method, op, status))
	}
	return nil
}

// errGone: the booking is gone or cancelled already — nothing to cancel.
var errGone = errors.New("the booking is gone or cancelled")

// notStarted reads the booking again and establishes, by the decision
// clock, that its CURRENT start (the club may have moved it) is ahead:
// driver.ErrStarted or driver.ErrClockUnknown otherwise, errGone when it is
// gone or cancelled, any other error when it cannot be read.
func (d *Driver) notStarted(ctx context.Context, uid string) error {
	b, found, err := d.load(ctx, uid)
	switch {
	case err != nil:
		return err
	case !found || b.cancelled():
		return errGone
	}
	_, start, _, err := d.span(&b)
	if err != nil {
		return err
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

// cancel cancels a booking (booking-cancel, not forced). A booking Lunda
// does not know is gone: no error. A cancel refused with the session and
// sent again with another one first re-establishes that the booking has
// not started (notStarted): if that fails, it is not sent again.
// [UNVERIFIED] whether a cancel inside the club's cancellation deadline
// (24 h before the start) needs forced: true.
func (d *Driver) cancel(ctx context.Context, uid, reason string) error {
	status, _, err := d.c.callGuarded(ctx, opCancel, nil, map[string]any{"bookingId": uid, "bookingType": "COURT", "forced": false, "reason": reason},
		func(ctx context.Context) error { return d.notStarted(ctx, uid) })
	if errors.Is(err, errGone) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status <= 299, status == http.StatusNotFound:
		return nil
	}
	return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("DELETE /%s answered HTTP %d", opCancel, status))
}

// rollback cancels the booking a failed Create made and returns cause, or,
// when it cannot be cancelled, an error that says the booking stays in
// Lunda (logged at ERROR: the core reports it). Like Cancel, it goes by
// the decision clock and the booking as it is now, read again: one whose
// current start has passed (the club may have moved it), whose start or
// the time is not known, or that cannot be read, is not cancelled; nor is
// anything sent on a lost session.
func (d *Driver) rollback(ctx context.Context, uid string, req driver.CreateRequest, cause error) error {
	stays := func(why error) error {
		d.log.Error("a booking the gateway made in Lunda could not be cancelled after a failed step: it stays in Lunda, unpaid, until the club cancels it",
			"booking", "l-"+uid, "court", req.Court, "error", why)
		return fmt.Errorf("%w; the booking l-%s it made STAYS IN LUNDA: it could not be cancelled (%v)", cause, uid, why)
	}
	rctx, stop := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer stop()
	switch err := d.notStarted(rctx, uid); {
	case errors.Is(err, errGone):
		return cause // nothing left to undo
	case err != nil:
		return stays(err)
	}
	if err := d.cancel(rctx, uid, reasonRollback); err != nil {
		return stays(err)
	}
	d.log.Warn("a booking the gateway made in Lunda was cancelled after a failed step", "booking", "l-"+uid, "court", req.Court, "error", cause)
	return cause
}

// unknownOutcome is the error of a create whose outcome is unknown. It
// cancels nothing — a booking of the service client on that court and
// time may as well be the club's own, made meanwhile — but looks for one
// and names it: the club must check it (it may stay in Lunda, unpaid). The
// lookup is logged at ERROR, the alarm; neither names a client.
func (d *Driver) unknownOutcome(ctx context.Context, req driver.CreateRequest, c court, cause error) error {
	rctx, stop := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer stop()
	at := req.Start.UTC().Format(time.RFC3339)
	ids, err := d.findOwnSlot(rctx, req, c)
	switch {
	case err != nil:
		d.log.Error("the outcome of a create in Lunda is unknown and the court could not be checked: a booking may stay in Lunda, unpaid; the club must check the court",
			"court", req.Court, "start", at, "error", cause, "lookup", err)
		return fmt.Errorf("%w; the outcome of the booking is unknown and the court could not be checked (%v): the club must check it in Lunda", cause, err)
	case len(ids) == 0:
		d.log.Error("the outcome of a create in Lunda is unknown; no booking of the service client was found on the court",
			"court", req.Court, "start", at, "error", cause)
		return fmt.Errorf("%w; the outcome of the booking is unknown: no booking of the service client was found on the court", cause)
	}
	named := "l-" + strings.Join(ids, ", l-")
	d.log.Error("the outcome of a create in Lunda is unknown: a booking of the service client on that court and time may stay in Lunda, unpaid; the club must check it",
		"court", req.Court, "start", at, "bookings", named, "error", cause)
	return fmt.Errorf("%w; the outcome of the booking is unknown: %s on the court may stay in Lunda unpaid, the club must check it", cause, named)
}

// slotOwner is the client of a slot, read only to find the gateway's own.
type slotOwner struct {
	Type      string `json:"type"`
	BookingID string `json:"bookingId"`
	Begin     string `json:"begin"`
	End       string `json:"end"`
	ClientUID string `json:"clientUid"`
}

// findOwnSlot lists the bookings of the service client exactly on the
// court and interval of req: candidates to name, never to cancel.
func (d *Driver) findOwnSlot(ctx context.Context, req driver.CreateRequest, c court) ([]string, error) {
	day := localDay(req.Start, d.loc)
	status, res, err := d.c.call(ctx, opSlots, url.Values{"date": {day.Format("2006-01-02")}}, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opSlots, status))
	}
	var ds struct {
		Courts []struct {
			CourtUID string      `json:"courtUid"`
			Slots    []slotOwner `json:"slots"`
		} `json:"courts"`
	}
	if json.Unmarshal(res, &ds) != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opSlots+": the answer is not a day's schedule")
	}
	var out []string
	for _, ct := range ds.Courts {
		if ct.CourtUID != c.uid {
			continue
		}
		for _, s := range ct.Slots {
			st, ok1 := parseLocal(s.Begin, d.loc)
			en, ok2 := parseLocal(s.End, d.loc)
			if ok1 && ok2 && st.Equal(req.Start) && en.Equal(req.End) && s.ClientUID == d.service && uidRe.MatchString(s.BookingID) {
				out = append(out, s.BookingID)
			}
		}
	}
	return out, nil
}

// Cancel implements driver.Driver: a booking is cancelled only when, read
// back, its notes start with the caller's marker AND its customer is the
// service client (the club's own bookings of its service client — its
// tournaments — carry no marker), it is on a configured court and it has
// not started. A booking that is gone or already cancelled counts as
// cancelled. Lunda has no conditional cancel: a change between the read
// and the cancel is not seen.
func (d *Driver) Cancel(ctx context.Context, id string, partner string) error {
	uid, ok := parseID(id)
	if !ok {
		return driver.ErrNotFound
	}
	b, found, err := d.load(ctx, uid)
	if err != nil {
		return err
	}
	if !found || b.cancelled() {
		return nil
	}
	court, start, _, err := d.span(&b)
	if err != nil {
		return err
	}
	if court == "" {
		return driver.ErrCourtNotConfigured
	}
	if d.service == "" || b.customer() != d.service || b.Notes == nil || !hasMarker(*b.Notes, partner) {
		return driver.ErrNotOwned
	}
	now, known := d.clock()
	switch {
	case !known:
		return driver.ErrClockUnknown
	case !start.After(now):
		return driver.ErrStarted
	}
	if err := d.cancel(ctx, uid, reasonCancel); err != nil {
		return err
	}
	d.log.Info("booking cancelled in Lunda", "booking", id, "court", court)
	return nil
}

// MarkPaid implements driver.Driver: the gateway never writes a payment to
// Lunda (REQ-394) — a payment there is a line in the club's till.
func (d *Driver) MarkPaid(context.Context, string, string) error { return driver.ErrNotSupported }
