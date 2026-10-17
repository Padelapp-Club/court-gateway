package yclients

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

const (
	// markerPrefix starts the ownership marker that begins the comment of
	// every record the gateway creates: "[cg:<partner>] <note>".
	markerPrefix = "[cg:"
	// PaidMark is written into the comment of a paid booking's records
	// (D11: the club's cash desk is not touched).
	PaidMark = "Оплачено в PadelApp"
	// maxComment is the comment cap of D11 (YCLIENTS' own is not measured);
	// the comment is cut so the paid mark still fits after it.
	maxComment = 500
)

// The ids of the API: a booking is "y-<record>[-<record>...]" (the records
// of one booking, in order); a stretch without working time is "yh-...".
var (
	bookingIDRe = regexp.MustCompile(`^y-[0-9]{1,18}(-[0-9]{1,18}){0,7}$`)
	setTagRe    = regexp.MustCompile(`^\[cg-set:([0-9a-f]{8})/([1-8])\]`)
)

// maxRecords is the most records one booking may take: its id names them
// all and stays within the API's 100 characters.
const maxRecords = 8

func marker(partner string) string { return markerPrefix + partner + "]" }

// setTag follows the marker in every record of a booking: a key of its own
// and how many records it has. Occupancy shows the records of one booking
// as one; a create that went wrong finds by it every record it made, named
// in the answer or not.
func setTag(key string, n int) string { return fmt.Sprintf("[cg-set:%s/%d]", key, n) }

// buildComment is the marker, the set tag (if any) and the note, cut so
// that the paid mark fits after it. Ownership is read only from the start
// of the comment, so a marker inside the note proves nothing.
func buildComment(note, partner, tag string) string {
	head := marker(partner)
	if tag != "" {
		head += " " + tag
	}
	room := maxComment - utf8.RuneCountInString(PaidMark) - 1 - utf8.RuneCountInString(head) - 1
	if utf8.RuneCountInString(note) > room {
		note = string([]rune(note)[:max(room, 0)])
	}
	if note == "" {
		return head
	}
	return head + " " + note
}

func hasMarker(comment, partner string) bool {
	return strings.HasPrefix(strings.TrimSpace(comment), marker(partner))
}

// setOf reads the set tag after any partner's marker in a comment.
func setOf(comment *string) (key string, n int, ok bool) {
	if comment == nil {
		return "", 0, false
	}
	c := strings.TrimSpace(*comment)
	if !strings.HasPrefix(c, markerPrefix) {
		return "", 0, false
	}
	end := strings.Index(c, "]")
	if end < 0 {
		return "", 0, false
	}
	m := setTagRe.FindStringSubmatch(strings.TrimSpace(c[end+1:]))
	if m == nil {
		return "", 0, false
	}
	n, _ = strconv.Atoi(m[2])
	return m[1] + "/" + m[2], n, true
}

// recordIDs splits a booking id into its records.
func recordIDs(id string) ([]string, bool) {
	if !bookingIDRe.MatchString(id) {
		return nil, false
	}
	return strings.Split(id[2:], "-"), true
}

// yclientsTime writes an instant in the club's zone with its offset, as
// the D11 adapter does ("2026-10-07T10:00:00+05:00").
func yclientsTime(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02T15:04:05Z07:00")
}

func newSetKey() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// catalog reads the services the court offers at start (book_services
// with datetime lists only the services in effect then: clubs price the
// time of day with services).
func (d *Driver) catalog(ctx context.Context, staff string, at *time.Time) ([]service, error) {
	q := url.Values{"staff_id": {staff}}
	if at != nil {
		q.Set("datetime", yclientsTime(*at, d.loc))
	}
	r, err := d.c.do(ctx, http.MethodGet, "/book_services/"+d.company, q, nil, partnerAuth)
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /book_services: HTTP %d", r.status))
	}
	var data struct {
		Services []service `json:"services"`
	}
	if json.Unmarshal(r.data, &data) != nil || data.Services == nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /book_services: no services list")
	}
	return data.Services, nil
}

type appointmentBody struct {
	ID       int     `json:"id"`
	Services []int64 `json:"services"`
	StaffID  int64   `json:"staff_id"`
	Datetime string  `json:"datetime"`
}

type bookRecordBody struct {
	Phone         string            `json:"phone"`
	Fullname      string            `json:"fullname"`
	Email         string            `json:"email,omitempty"`
	Comment       string            `json:"comment"`
	NotifyBySMS   int               `json:"notify_by_sms"`
	NotifyByEmail int               `json:"notify_by_email"`
	Appointments  []appointmentBody `json:"appointments"`
}

// Create implements driver.Driver: book_record for the club's service
// client with the services that make up the booked length (D11), the
// partner's marker and note in the comment. If YCLIENTS creates fewer
// records than asked, those it named are deleted again and the create
// fails: no half booking stays.
func (d *Driver) Create(ctx context.Context, req driver.CreateRequest) (driver.Booking, error) {
	staff, ok := d.byCourt[req.Court]
	if !ok {
		return driver.Booking{}, fmt.Errorf("court %q: %w", req.Court, driver.ErrNotFound)
	}
	length := req.End.Sub(req.Start)
	if length <= 0 || length%time.Minute != 0 {
		return driver.Booking{}, driver.Upstream(driver.UpstreamRejected, "the booking is not a whole number of minutes")
	}
	services, err := d.catalog(ctx, staff, &req.Start)
	if err != nil {
		return driver.Booking{}, err
	}
	pieces, err := pickServices(int(length/time.Second), services)
	if err != nil {
		return driver.Booking{}, driver.Upstream(driver.UpstreamRejected, err.Error())
	}
	plans := planAppointments(pieces)
	if len(plans) > maxRecords {
		return driver.Booking{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("the booking needs %d records of the court's services, at most %d", len(plans), maxRecords))
	}
	tag := setTag(newSetKey(), len(plans))
	staffN, _ := strconv.ParseInt(staff, 10, 64)
	body := bookRecordBody{
		Phone: d.client.phone, Fullname: d.client.name, Email: d.client.email,
		Comment: buildComment(req.Note, req.Partner, tag),
	}
	for i, p := range plans {
		body.Appointments = append(body.Appointments, appointmentBody{
			ID: i + 1, Services: p.services, StaffID: staffN,
			Datetime: yclientsTime(req.Start.Add(time.Duration(p.offset)*time.Second), d.loc),
		})
	}
	// Once book_record is sent, any answer but a clean success may hide
	// records it made (no answer, a 5xx, an unreadable or refusing 2xx, a
	// 4xx): every such failure is undone before it is returned. Undoing
	// what was never made costs one read.
	r, err := d.c.do(ctx, http.MethodPost, "/book_record/"+d.company, nil, body, partnerAuth)
	if err != nil {
		d.undo(ctx, staff, tag, req, body.Appointments, nil)
		return driver.Booking{}, err
	}
	switch {
	case r.status == http.StatusOK || r.status == http.StatusCreated:
	case r.status == http.StatusUnprocessableEntity:
		// YCLIENTS refuses a busy time with 422 (D11 R-9, probe)
		d.undo(ctx, staff, tag, req, body.Appointments, nil)
		return driver.Booking{}, fmt.Errorf("POST /book_record: %w", driver.ErrSlotUnavailable)
	default:
		d.undo(ctx, staff, tag, req, body.Appointments, nil)
		return driver.Booking{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("POST /book_record: HTTP %d", r.status))
	}
	var created []struct {
		RecordID json.Number `json:"record_id"`
	}
	_ = json.Unmarshal(r.data, &created)
	var ids []string
	seen := map[string]bool{}
	for _, c := range created {
		if id := c.RecordID.String(); digitsRe.MatchString(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) != len(plans) || len(created) != len(plans) { // one id per record, each its own
		d.undo(ctx, staff, tag, req, body.Appointments, ids)
		return driver.Booking{}, driver.Upstream(driver.UpstreamInvalid, fmt.Sprintf("POST /book_record: %d record ids for %d records", len(ids), len(plans)))
	}
	d.log.Info("record created", "court", req.Court, "records", len(ids))
	return driver.Booking{ID: "y-" + strings.Join(ids, "-"), Court: req.Court, Start: req.Start, End: req.End, Status: driver.StatusActive}, nil
}

// undo deletes what a create that went wrong made, and nothing else. The
// candidates are the records the answer named and the live records of the
// court around the booking whose comment carries its set tag (YCLIENTS may
// have made records it did not name, or answered nothing). Each is read
// back by id and deleted only if it is the partner's own (marker and the
// club's service client, as Cancel requires), carries this booking's tag
// and starts where one of the booking's records was to start: a club's copy
// of the record — the comment kept, another client or another time — is not
// touched. YCLIENTS shows a client's phone masked, so another client's may
// read as the service client's. So a record the answer did not name is
// deleted only when every candidate was read and it alone claims its start;
// otherwise only the named ones go, the rest stay (occupied, never freed)
// and are logged. It works
// on after the caller's context ends, within its own limit; what it cannot
// delete it logs.
func (d *Driver) undo(ctx context.Context, staff, tag string, req driver.CreateRequest, plan []appointmentBody, named []string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	key := strings.TrimSuffix(strings.TrimPrefix(tag, "[cg-set:"), "]") // "<key>/<n>", as setOf reads it
	candidates := append([]string(nil), named...)
	records, err := d.fetchRecords(ctx, localDay(req.Start, d.loc).AddDate(0, 0, -1), localDay(req.End, d.loc))
	if err != nil {
		d.log.Error("yclients: a failed create could not look for its records: some may be left", "court", req.Court, "err", err)
	}
	for _, r := range records {
		if k, _, ok := setOf(r.Comment); ok && k == key && !r.Deleted && r.StaffID.String() == staff {
			candidates = append(candidates, r.ID.String())
		}
	}
	starts := map[int64]bool{}
	for _, ap := range plan {
		if t, perr := time.Parse(time.RFC3339, ap.Datetime); perr == nil {
			starts[t.Unix()] = true
		}
	}
	isNamed := map[string]bool{}
	for _, id := range named {
		isNamed[id] = true
	}
	// the records that are this booking's by everything the gateway can read,
	// by the start they claim
	left, done, unread := 0, map[string]bool{}, err != nil
	claims := map[int64][]string{}
	for _, id := range candidates {
		if done[id] {
			continue
		}
		done[id] = true
		rec, found, err := d.read(ctx, id)
		if err != nil {
			left++
			unread = true
			d.log.Error("yclients: a record of a failed create could not be read: it may be left", "record", id, "err", err)
			continue
		}
		if !found || rec.Deleted {
			continue
		}
		start, end, ok := rec.span()
		k, _, tagged := setOf(rec.Comment)
		if !ok || !tagged || k != key || rec.StaffID.String() != staff || !d.owned(rec, req.Partner) ||
			start.Before(req.Start) || end.After(req.End) || !starts[start.Unix()] {
			continue // not this booking's record
		}
		claims[start.Unix()] = append(claims[start.Unix()], id)
	}
	for _, ids := range claims {
		for _, id := range ids {
			if (len(ids) > 1 || unread) && !isNamed[id] {
				left++
				d.log.Error("yclients: records of a failed create cannot be told from a club's: left in the journal, check them by hand", "court", req.Court, "record", id)
				continue
			}
			if err := d.deleteRecord(ctx, id); err != nil {
				left++
				d.log.Error("yclients: a record of a failed create is left", "record", id, "err", err)
			}
		}
	}
	if left == 0 && err == nil {
		d.log.Warn("yclients: a failed create was undone", "court", req.Court)
	}
}

// undoTimeout bounds undo after the request that needed it.
const undoTimeout = 30 * time.Second

// details is GET /record: what reading, cancelling and the payment mark
// need. The client's phone comes masked ("+7900XXXXX00").
type details struct {
	ID           json.Number       `json:"id"`
	StaffID      json.Number       `json:"staff_id"`
	Datetime     string            `json:"datetime"`
	SeanceLength *float64          `json:"seance_length"`
	Length       *float64          `json:"length"`
	Deleted      bool              `json:"deleted"`
	Comment      *string           `json:"comment"`
	Attendance   json.RawMessage   `json:"attendance"`
	Services     []json.RawMessage `json:"services"`
	Client       *struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
	} `json:"client"`
	raw map[string]json.RawMessage
}

func (r *details) span() (time.Time, time.Time, bool) {
	l := r.SeanceLength
	if l == nil {
		l = r.Length
	}
	return record{Datetime: r.Datetime, Length: l}.span()
}

// read is GET /record of one record; found=false: YCLIENTS answered 404.
func (d *Driver) read(ctx context.Context, rid string) (*details, bool, error) {
	r, err := d.c.do(ctx, http.MethodGet, "/record/"+d.company+"/"+rid, nil, nil, userAuth)
	if err != nil {
		return nil, false, err
	}
	switch r.status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /record: HTTP %d", r.status))
	}
	var rec details
	if json.Unmarshal(r.data, &rec) != nil || json.Unmarshal(r.data, &rec.raw) != nil || rec.raw == nil {
		return nil, false, driver.Upstream(driver.UpstreamInvalid, "GET /record: not a record")
	}
	if rec.ID.String() != rid {
		return nil, false, driver.Upstream(driver.UpstreamInvalid, "GET /record: record id does not match")
	}
	return &rec, true, nil
}

// part is one record of a booking as read back.
type part struct {
	id         string
	rec        *details
	start, end time.Time
}

// readParts reads every record of a booking; gone (404) or deleted ones
// are left out, and gone counts them.
func (d *Driver) readParts(ctx context.Context, ids []string) (live []part, gone int, err error) {
	for _, rid := range ids {
		rec, found, err := d.read(ctx, rid)
		if err != nil {
			return nil, 0, err
		}
		if !found || rec.Deleted {
			gone++
			continue
		}
		s, e, ok := rec.span()
		if !ok {
			return nil, 0, driver.Upstream(driver.UpstreamInvalid, "GET /record: no valid datetime and length")
		}
		live = append(live, part{id: rid, rec: rec, start: s, end: e})
	}
	return live, gone, nil
}

func (d *Driver) Get(ctx context.Context, id string) (driver.Booking, error) {
	ids, ok := recordIDs(id)
	if !ok {
		return driver.Booking{}, driver.ErrNotFound
	}
	live, gone, err := d.readParts(ctx, ids)
	if err != nil {
		return driver.Booking{}, err
	}
	b := driver.Booking{ID: id, Status: driver.StatusActive}
	if len(live) == 0 {
		// every record deleted: the booking reads as cancelled, with the
		// first record's time (YCLIENTS keeps a deleted record readable)
		rec, found, err := d.read(ctx, ids[0])
		if err != nil {
			return driver.Booking{}, err
		}
		if !found || gone == 0 {
			return driver.Booking{}, driver.ErrNotFound
		}
		s, e, ok := rec.span()
		if !ok {
			return driver.Booking{}, driver.ErrNotFound
		}
		live, b.Status = []part{{id: ids[0], rec: rec, start: s, end: e}}, driver.StatusCancelled
	}
	court, ok := d.staff[live[0].rec.StaffID.String()]
	if !ok {
		return driver.Booking{}, driver.ErrCourtNotConfigured
	}
	b.Court, b.Start, b.End = court, live[0].start, live[0].end
	for _, p := range live[1:] {
		if p.start.Before(b.Start) {
			b.Start = p.start
		}
		if p.end.After(b.End) {
			b.End = p.end
		}
	}
	return b, nil
}

// owned: the record's comment starts with the partner's marker and its
// client is the club's service client (the masked phone fits ours). Both,
// or the gateway does not touch it.
func (d *Driver) owned(rec *details, partner string) bool {
	if rec.Comment == nil || !hasMarker(*rec.Comment, partner) || rec.Client == nil {
		return false
	}
	return phoneFits(rec.Client.Phone, d.client.phone)
}

// phoneFits: a phone as YCLIENTS shows it (digits, X or * for hidden
// ones) is ours digit for digit where it is not hidden.
func phoneFits(shown, ours string) bool {
	var v []rune
	for _, r := range shown {
		if r >= '0' && r <= '9' || r == 'X' || r == 'x' || r == '*' {
			v = append(v, r)
		}
	}
	if len(v) == 0 || len(v) != len(ours) {
		return false
	}
	for i, r := range v {
		if r != 'X' && r != 'x' && r != '*' && byte(r) != ours[i] {
			return false
		}
	}
	return true
}

// Cancel implements driver.Driver: every record of the booking is read
// first; it is deleted only if each one is on a configured court, the
// caller's (owned) and the booking has not started. Records already gone
// count as cancelled. YCLIENTS has no conditional delete: a change between
// the read and the DELETE is not seen.
func (d *Driver) Cancel(ctx context.Context, id, partner string) error {
	if strings.HasPrefix(id, "yh-") {
		return driver.ErrNotOwned // the court's own closed time
	}
	ids, ok := recordIDs(id)
	if !ok {
		return driver.ErrNotFound
	}
	live, _, err := d.readParts(ctx, ids)
	if err != nil || len(live) == 0 {
		return err
	}
	start := live[0].start
	for _, p := range live {
		if _, ok := d.staff[p.rec.StaffID.String()]; !ok {
			return driver.ErrCourtNotConfigured
		}
		if !d.owned(p.rec, partner) {
			return driver.ErrNotOwned
		}
		if p.start.Before(start) {
			start = p.start
		}
	}
	switch now, known := d.clock(); {
	case !known:
		return driver.ErrClockUnknown
	case !start.After(now):
		return driver.ErrStarted
	}
	for _, p := range live {
		if err := d.deleteRecord(ctx, p.id); err != nil {
			return err
		}
	}
	d.log.Info("record deleted", "records", len(live))
	return nil
}

// deleteRecord: DELETE /record, user-authorized; 404 is already deleted.
func (d *Driver) deleteRecord(ctx context.Context, rid string) error {
	r, err := d.c.do(ctx, http.MethodDelete, "/record/"+d.company+"/"+rid, nil, nil, userAuth)
	if err != nil {
		return err
	}
	switch {
	case r.status >= 200 && r.status < 300, r.status == http.StatusNotFound:
		return nil
	default:
		return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("DELETE /record: HTTP %d", r.status))
	}
}

// withPaidMark appends the paid mark to a comment, once.
func withPaidMark(comment string) string {
	if strings.Contains(comment, PaidMark) {
		return comment
	}
	if comment == "" {
		return PaidMark
	}
	return comment + "\n" + PaidMark
}

// MarkPaid implements driver.Driver: the paid mark in the comment of each
// record (D11). PUT /record takes the whole record, so it is built from a
// fresh read with only the comment changed — attendance as it was, no
// SMS, never over another booking — and read back: an answer of 2xx
// proves nothing (D11 probe: 200 and the change dropped).
func (d *Driver) MarkPaid(ctx context.Context, id, partner string) error {
	ids, ok := recordIDs(id)
	if !ok {
		return driver.ErrNotFound
	}
	live, gone, err := d.readParts(ctx, ids)
	if err != nil {
		return err
	}
	switch {
	case len(live) == 0:
		return driver.ErrNotFound
	case gone > 0:
		// D11: a deleted record has nothing to mark, and the payment must
		// not read as marked
		return driver.Upstream(driver.UpstreamRejected, "a record of the booking is deleted in the journal: the paid mark is not written")
	}
	for _, p := range live {
		if !d.owned(p.rec, partner) {
			return driver.ErrNotOwned
		}
	}
	for _, p := range live {
		if err := d.markRecord(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) markRecord(ctx context.Context, p part) error {
	comment := *p.rec.Comment
	if strings.Contains(comment, PaidMark) {
		return nil
	}
	body, err := d.updateBody(p.rec, withPaidMark(comment))
	if err != nil {
		return err
	}
	path := "/record/" + d.company + "/" + p.id
	r, err := d.c.do(ctx, http.MethodPut, path, nil, body, userAuth)
	if err != nil {
		return err
	}
	if r.status < 200 || r.status >= 300 {
		return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("PUT /record: HTTP %d", r.status))
	}
	after, found, err := d.read(ctx, p.id)
	switch {
	case err != nil:
		return err
	case !found || after.Comment == nil || !strings.Contains(*after.Comment, PaidMark):
		return driver.Upstream(driver.UpstreamInvalid, "PUT /record passed but the paid mark is not in the comment")
	case string(after.Attendance) != string(p.rec.Attendance):
		return driver.Upstream(driver.UpstreamInvalid, "PUT /record changed the attendance")
	}
	d.log.Info("paid mark written and read back")
	return nil
}

// updateBody is the whole PUT /record body from a read: every field as it
// was, the client's phone ours (it reads masked), the comment new.
func (d *Driver) updateBody(rec *details, comment string) (map[string]any, error) {
	services := make([]map[string]json.RawMessage, 0, len(rec.Services))
	for _, s := range rec.Services {
		var m map[string]json.RawMessage
		if json.Unmarshal(s, &m) != nil {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /record: a service in an unexpected form")
		}
		keep := map[string]json.RawMessage{}
		for _, k := range []string{"id", "cost", "first_cost", "discount"} {
			if v, ok := m[k]; ok {
				keep[k] = v
			}
		}
		services = append(services, keep)
	}
	for _, k := range []string{"staff_id", "datetime", "seance_length", "attendance"} {
		if _, ok := rec.raw[k]; !ok {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /record: no "+k+": the whole record cannot be written back")
		}
	}
	return map[string]any{
		"staff_id":      rec.raw["staff_id"],
		"services":      services,
		"client":        map[string]string{"phone": d.client.phone, "name": rec.Client.Name},
		"datetime":      rec.Datetime,
		"seance_length": rec.raw["seance_length"],
		"save_if_busy":  0,
		"send_sms":      0,
		"comment":       comment,
		"attendance":    rec.raw["attendance"],
	}, nil
}
