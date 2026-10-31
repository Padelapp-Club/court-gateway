package lunda

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
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
// booking-create). Nothing about the customer but the id is decoded. Seen
// live (Kosmos, 2026-10-17): a gateway booking read back with its status,
// payments, notes and service client, and its cancel read its slots.
type lundaBooking struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	CustomerUID string `json:"customerUid"`
	Customer    *struct {
		UID        string `json:"uid"`
		ProfileUID string `json:"profileUid"`
	} `json:"customer"`
	Notes    *string         `json:"notes"`
	Payments json.RawMessage `json:"payments"`
	// Courts carry the price of each court of the booking (rubles): Lunda
	// answers no top-level price. A price adjustment adds a further entry,
	// the discount: no courtUid, description "Court discount" and a
	// POSITIVE price, the discount's size (seen live at Kosmos, 2026-10-17
	// and 2026-10-18: court 2000 + discount 2000 is PAID).
	Courts []struct {
		CourtUID    string          `json:"courtUid"`
		Description string          `json:"description"`
		Price       json.RawMessage `json:"price"`
	} `json:"courts"`
	Slots []struct {
		CourtUID string `json:"courtUid"`
		Begin    string `json:"begin"`
		End      string `json:"end"`
	} `json:"slots"`
}

// discountRow is the description of the entry a price adjustment adds to
// a booking's courts[].
const discountRow = "Court discount"

// courtsNet is the booking's price in rubles: the prices of its entries
// on court less those of its discount entries (no courtUid, description
// discountRow), which count only when discounts is true. ok is false when
// there is no entry on court, a price does not read (a negative one
// included: never taken for 0), or an entry is neither court's nor, with
// discounts, a discount.
func (b *lundaBooking) courtsNet(court string, discounts bool) (net *big.Rat, ok bool) {
	net = new(big.Rat)
	for _, c := range b.Courts {
		p := strings.TrimSpace(string(c.Price))
		if !decimalRe.MatchString(p) {
			return nil, false
		}
		r, _ := new(big.Rat).SetString(p)
		switch {
		case c.CourtUID == court:
			net.Add(net, r)
			ok = true
		case discounts && c.CourtUID == "" && c.Description == discountRow:
			net.Sub(net, r)
		default:
			return nil, false
		}
	}
	return net, ok
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

// netZero: the booking's price on court less its discounts is 0 —
// neither under- nor over-adjusted. Entries that do not read, or none on
// court, are not 0.
func (b *lundaBooking) netZero(court string) bool {
	net, ok := b.courtsNet(court, true)
	return ok && net.Sign() == 0
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
// what Lunda answers for an unknown booking: anything but 404 is not taken
// for "gone".
func (d *Driver) load(ctx context.Context, uid string) (b lundaBooking, found bool, err error) {
	// bookingType is required: without it Lunda answers 400 (seen live)
	status, res, err := d.c.call(ctx, opBookingLoad, url.Values{"bookingId": {uid}, "bookingType": {"COURT"}}, nil)
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

// decimalRe is a price as Lunda writes it (rubles, maybe with kopecks;
// never negative, a discount entry's neither).
var decimalRe = regexp.MustCompile(`^[0-9]{1,9}(\.[0-9]{1,2})?$`)

// bookingGoal is the goal of every booking the gateway makes in Lunda, a
// game or a tournament block alike: GENERAL ("other" in Lunda's own
// labels, club/global-form-config), since a GAME or TOURNAMENT may surface
// in Lunda's app to players, and nothing the gateway books may ever reach
// them there.
const bookingGoal = "GENERAL"

// Create implements driver.Driver: an
// ordinary court booking of goal bookingGoal on the club's service client
// (booking-create, placeholder false), adjusted by minus the booked court's
// price (update-courts-price-adjustment) so it is PAID without any payment,
// its notes starting with the partner's marker (booking-notes-update). Read
// back, it must be PAID, list no payment, and carry the marker and the
// service client; a price on the booked court other than its discount
// entries' (net 0) is logged at WARN, not rolled back (book). If a step
// after the create fails, the booking is cancelled unless it has started
// (rollback) and the partner gets an error: never a half-made booking, an
// unpaid debt of the service client.
// A create of unknown outcome (no answer, a 5xx, no readable id) cancels
// nothing: the club books its own tournaments on the same service client
// (unknownOutcome). Without a service client nothing is sent.
func (d *Driver) Create(ctx context.Context, req driver.CreateRequest) (driver.Booking, error) {
	uid, err := d.book(ctx, req, buildNotes(req.Note, req.Partner), func(notes string) bool { return hasMarker(notes, req.Partner) })
	if err != nil {
		return driver.Booking{}, err
	}
	d.log.Info("booking created in Lunda", "booking", "l-"+uid, "court", req.Court)
	return driver.Booking{ID: "l-" + uid, Court: req.Court, Start: req.Start, End: req.End, Status: driver.StatusActive}, nil
}

// book makes a booking of the gateway as Create describes, with notes,
// and returns its id: read back, its notes must be marked (they carry the
// marker they were written with). A mirror's block is made by it too
// (CreateBlock), the same kind of booking with a mirror's marker.
func (d *Driver) book(ctx context.Context, req driver.CreateRequest, notes string, marked func(notes string) bool) (string, error) {
	c, ok := d.byCourt[req.Court]
	if !ok {
		return "", fmt.Errorf("court %q: %w", req.Court, driver.ErrNotFound)
	}
	if d.service == "" {
		return "", errNoServiceClient()
	}
	begin, end := formatLocal(req.Start, d.loc), formatLocal(req.End, d.loc)
	status, res, err := d.c.call(ctx, opCreate, nil, map[string]any{
		"customerUid": d.service, "placeholder": false, "goal": bookingGoal, "addPromisedPayment": false,
		"slots": []map[string]any{{"courtUid": c.uid, "begin": begin, "end": end, "pinned": false}},
	})
	if err != nil {
		var ue *driver.UpstreamError
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ue) && (ue.Kind == driver.UpstreamUnavailable || ue.Kind == driver.UpstreamInvalid) {
			return "", d.unknownOutcome(ctx, req, c, err) // the booking may exist
		}
		return "", err // refused before it was made (or not sent at all)
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
	case status == http.StatusConflict:
		return "", fmt.Errorf("POST /%s: %w", opCreate, driver.ErrSlotUnavailable)
	case status >= 200 && status < 300: // accepted, but not as observed: it may exist
		return "", d.unknownOutcome(ctx, req, c, driver.Upstream(driver.UpstreamInvalid, fmt.Sprintf("POST /%s answered HTTP %d", opCreate, status)))
	default:
		return "", driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /%s answered HTTP %d", opCreate, status))
	}
	var b lundaBooking
	if json.Unmarshal(res, &b) != nil || !uidRe.MatchString(b.ID) {
		return "", d.unknownOutcome(ctx, req, c, driver.Upstream(driver.UpstreamInvalid, "POST /"+opCreate+": the answer has no valid booking id"))
	}
	uid := b.ID
	// the price is that of the booked court alone: an entry on another
	// court or a discount already, or none on ours, is a price that does
	// not read
	price, ok := b.courtsNet(c.uid, false)
	if !ok {
		return "", d.rollback(ctx, uid, req, driver.Upstream(driver.UpstreamInvalid, "POST /"+opCreate+": the answer has no valid price of the court"))
	}
	if price.Sign() != 0 { // a booking of price 0 needs no adjustment
		minus := strings.TrimRight(strings.TrimRight(price.FloatString(2), "0"), ".")
		if err := d.write(ctx, opAdjust, map[string]any{"bookingId": uid, "priceAdjustment": json.Number("-" + minus)}); err != nil {
			return "", d.rollback(ctx, uid, req, err)
		}
	}
	if err := d.write(ctx, opNotes, map[string]any{"bookingId": uid, "bookingType": "COURT", "notes": notes}); err != nil {
		return "", d.rollback(ctx, uid, req, err)
	}
	got, found, err := d.load(ctx, uid)
	switch {
	case err != nil:
		return "", d.rollback(ctx, uid, req, err)
	case !found:
		return "", d.rollback(ctx, uid, req, driver.Upstream(driver.UpstreamInvalid, "GET /"+opBookingLoad+": the booking just made is not found"))
	case !strings.EqualFold(got.Status, "PAID") || !got.noPayments():
		return "", d.rollback(ctx, uid, req, driver.Upstream(driver.UpstreamInvalid, "the booking just made is not PAID without payments"))
	case got.Notes == nil || !marked(*got.Notes) || got.customer() != d.service:
		return "", d.rollback(ctx, uid, req, driver.Upstream(driver.UpstreamInvalid, "the booking just made does not read back with the gateway's marker and service client"))
	}
	// Lunda calling it PAID with no payment already rules out a debt of the
	// service client; the net of its price lines is only what the gateway
	// expects to see. Seen live at Amur Padel (2026-10-31): bookings PAID
	// without payments whose lines did not read as net 0 by the shape seen
	// at Kosmos, every one of them rolled back. They are accepted and their
	// lines logged: [UNVERIFIED] what shape Amur's lines have; the check can
	// be tightened to what the log shows.
	if !got.netZero(c.uid) {
		d.log.Warn("a booking the gateway made in Lunda reads back PAID without payments, but its price lines do not net to 0: accepted",
			"booking", "l-"+uid, "court", req.Court, "lines", got.priceLines(c.uid))
	}
	return uid, nil
}

// priceLines describes the booking's courts[] for the log, entry by
// entry: its kind ("booked" on court, "other" on another court, "none"
// without a courtUid), its raw price cut to 20 characters and its
// description cut to 40. Neither a court uid nor anything about the
// customer is in it.
func (b *lundaBooking) priceLines(court string) string {
	lines := make([]string, len(b.Courts))
	for i, c := range b.Courts {
		kind := "other"
		switch c.CourtUID {
		case court:
			kind = "booked"
		case "":
			kind = "none"
		}
		lines[i] = fmt.Sprintf("%s price=%s description=%q", kind, truncate(strings.TrimSpace(string(c.Price)), 20), truncate(c.Description, 40))
	}
	return strings.Join(lines, "; ")
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

// notStarted reads the booking again and establishes that it is still the
// gateway's to cancel (ownedAndAhead, as it is NOW: the club may have moved
// it, changed its client or its notes): errGone when it is gone or
// cancelled, any other error when it cannot be read or is not.
func (d *Driver) notStarted(ctx context.Context, uid, partner string) error {
	b, found, err := d.load(ctx, uid)
	switch {
	case err != nil:
		return err
	case !found || b.cancelled():
		return errGone
	}
	return d.ownedAndAhead(&b, partner)
}

// ownedAndAhead: the booking is on a configured court, on the service
// client, carries partner's marker (unless partner is "": a rollback of
// the gateway's own fresh booking, whose notes may not be written yet),
// and its start is ahead by the decision clock. driver.ErrCourtNotConfigured,
// ErrNotOwned, ErrClockUnknown or ErrStarted otherwise.
func (d *Driver) ownedAndAhead(b *lundaBooking, partner string) error {
	court, start, _, err := d.span(b)
	if err != nil {
		return err
	}
	if court == "" {
		return driver.ErrCourtNotConfigured
	}
	if d.service == "" || b.customer() != d.service || partner != "" && (b.Notes == nil || !hasMarker(*b.Notes, partner)) {
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

// cancel cancels a booking (booking-cancel, not forced). A booking Lunda
// does not know is gone: no error. A cancel refused with the session and
// sent again with another one first re-establishes that the booking has
// not started (notStarted): if that fails, it is not sent again.
// [UNVERIFIED] whether a cancel inside the club's cancellation deadline
// (24 h before the start) needs forced: true.
func (d *Driver) cancel(ctx context.Context, uid, reason, partner string) error {
	status, _, err := d.c.callGuarded(ctx, opCancel, nil, map[string]any{"bookingId": uid, "bookingType": "COURT", "forced": false, "reason": reason},
		func(ctx context.Context) error { return d.notStarted(ctx, uid, partner) })
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
	switch err := d.notStarted(rctx, uid, ""); {
	case errors.Is(err, errGone):
		return cause // nothing left to undo
	case err != nil:
		return stays(err)
	}
	if err := d.cancel(rctx, uid, reasonRollback, ""); err != nil {
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
	if err := d.ownedAndAhead(&b, partner); err != nil {
		return err
	}
	court, _, _, _ := d.span(&b)
	if err := d.cancel(ctx, uid, reasonCancel, partner); err != nil {
		return err
	}
	d.log.Info("booking cancelled in Lunda", "booking", id, "court", court)
	return nil
}

// MarkPaid implements driver.Driver: the gateway never writes a payment to
// Lunda — a payment there is a line in the club's till.
func (d *Driver) MarkPaid(context.Context, string, string) error { return driver.ErrNotSupported }
