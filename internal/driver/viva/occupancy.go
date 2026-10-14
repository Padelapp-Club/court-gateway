package viva

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// slot is the subset of an exercise/break the driver reads. Unknown fields
// (including any client data) are never decoded.
type slot struct {
	ID       string `json:"id"`
	TimeFrom string `json:"timeFrom"`
	TimeTo   string `json:"timeTo"`
	Room     *struct {
		ID string `json:"id"`
	} `json:"room"`
	Canceled *bool `json:"canceled"`
}

// parseTime accepts RFC3339 only. A timestamp without a zone is refused
// (the answer is invalid): nothing observed says which zone it would be in.
func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// formatTime writes UTC with milliseconds, like JavaScript toISOString().
func formatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// day is local midnight of t in the studio's zone loc.
func day(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// clockSkew is how far the CRM's clock may run ahead of the gateway's:
// near midnight the CRM's "today" may already be the gateway's tomorrow.
const clockSkew = 5 * time.Minute

// listDays is the longest date range asked in one list request: a club's
// month of exercises in one answer would be large, so long ranges are
// asked a week at a time.
const listDays = 7

// window computes the dates to list for [from, to) in the studio's zone
// loc and whether the answer can be complete. Viva selects list records by
// their start date in the studio's day and leaves out exercises of past
// days (docs/hub.md, completeness bounds). So exercises are listed from
// the day before the range; a range before the CRM's today (clockSkew
// ahead of ours) plus overnight_minutes is incomplete; breaks are listed
// break_lookback_days back.
func (d *Driver) window(kind string, loc *time.Location, from, to, now time.Time) (dateFrom, dateTo time.Time, complete bool) {
	dateTo = day(to, loc) // a record that starts after to's day cannot overlap
	if kind == "breaks" {
		return day(from, loc).AddDate(0, 0, -d.lookback), dateTo, true
	}
	today := day(now.Add(clockSkew), loc)
	return day(from, loc).AddDate(0, 0, -1), dateTo, !from.Before(today.Add(d.overnight))
}

// pageSizes are list lengths a CRM that cut its lists into pages would
// answer with. [ASSUMPTION] Viva's lists are not paged: they are a bare
// JSON array (a paging envelope is not one, and is refused); a list of
// exactly one of these lengths may still be a first page, so the answer
// is taken as incomplete.
var pageSizes = map[int]bool{50: true, 100: true, 200: true, 250: true, 500: true, 1000: true}

// list reads one kind for the whole studio; whole is false when the list
// may be one page of a longer one (pageSizes). Rooms are filtered here
// (intervals), never upstream: a break without a room closes the whole
// studio, and what a room filter on the CRM side leaves out is not known.
func (d *Driver) list(ctx context.Context, kind string, dateFrom, dateTo time.Time) (items []slot, whole bool, err error) {
	q := url.Values{
		"studioId":        {d.studio},
		"dateFrom":        {dateFrom.Format("2006-01-02")},
		"dateTo":          {dateTo.Format("2006-01-02")},
		"includeCanceled": {"false"},
	}
	status, data, err := d.c.do(ctx, http.MethodGet, "/"+kind, q, nil)
	if err != nil {
		return nil, false, err
	}
	if status != http.StatusOK {
		return nil, false, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", kind, status))
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+": response is not a JSON array")
	}
	var out []slot
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, false, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+": malformed item")
	}
	return out, !pageSizes[len(out)], nil
}

func (d *Driver) Occupancy(ctx context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	rooms := map[string]string{}
	for _, cid := range q.Courts {
		c, ok := d.byCourt[cid]
		if !ok {
			return driver.Occupancy{}, fmt.Errorf("court %q: %w", cid, driver.ErrNotFound)
		}
		rooms[c.room] = cid
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return driver.Occupancy{}, err
	}
	// without the hub's time the CRM's "today" is not known: incomplete
	now, known := d.clock()
	occ := driver.Occupancy{Complete: known}
	for _, kind := range []string{"exercises", "breaks"} {
		dateFrom, dateTo, complete := d.window(kind, loc, q.From, q.To, now)
		occ.Complete = occ.Complete && complete
		step := listDays
		if kind == "breaks" {
			step = 1 << 20 // breaks are few: one request, however long the look-back
		}
		var items []slot
		for f := dateFrom; !f.After(dateTo); f = f.AddDate(0, 0, step) {
			part, whole, err := d.list(ctx, kind, f, minTime(f.AddDate(0, 0, step-1), dateTo))
			if err != nil {
				return driver.Occupancy{}, err
			}
			occ.Complete = occ.Complete && whole
			items = append(items, part...)
		}
		prefix := "e-"
		if kind == "breaks" {
			prefix = "b-"
		}
		for _, it := range items {
			ivs, err := d.intervals(kind, prefix, it, rooms)
			if err != nil {
				return driver.Occupancy{}, err
			}
			for _, iv := range ivs {
				if iv.Start.Before(q.To) && iv.End.After(q.From) {
					occ.Intervals = append(occ.Intervals, iv)
				}
			}
		}
	}
	return occ, nil
}

// intervals validates one list item and returns the courts it occupies
// among rooms (room id -> court). Anything structurally wrong makes the
// whole answer invalid: silently dropping an item would show a busy court
// as free. A break without a room closes the whole studio, so it occupies
// every court asked about; an exercise without one is invalid.
func (d *Driver) intervals(kind, prefix string, it slot, rooms map[string]string) ([]driver.Interval, error) {
	bad := func(what string) ([]driver.Interval, error) {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+": item "+what)
	}
	if !uuidRe.MatchString(it.ID) {
		return bad("has no valid id")
	}
	roomless := it.Room == nil || it.Room.ID == ""
	if roomless && kind != "breaks" {
		return bad("has no room")
	}
	start, ok1 := parseTime(it.TimeFrom)
	end, ok2 := parseTime(it.TimeTo)
	if !ok1 || !ok2 {
		return bad("has unparseable or zone-less timeFrom/timeTo")
	}
	if !end.After(start) {
		return bad("ends before it starts")
	}
	if it.Canceled != nil && *it.Canceled {
		return nil, nil
	}
	iv := driver.Interval{ID: prefix + strings.ToLower(it.ID), Start: start, End: end}
	var out []driver.Interval
	for room, court := range rooms {
		if roomless || room == strings.ToLower(it.Room.ID) {
			iv.Court = court
			out = append(out, iv)
		}
	}
	return out, nil // a room that is not a configured court occupies none
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Ready checks the club's config against the account (CheckConfig, which
// logs in) and reads today's exercises and breaks of the first court,
// validating every item as occupancy does: an answer occupancy would
// refuse is not ready. A config alert is returned only when the reads
// pass.
func (d *Driver) Ready(ctx context.Context) error {
	st, err := d.studioOf(ctx)
	if err != nil {
		return err
	}
	alert := d.check(st)
	var ca *driver.ConfigAlert
	if alert != nil && !errors.As(alert, &ca) {
		return alert
	}
	now, _ := d.clock()
	today := day(now, d.zone(st))
	rooms := map[string]string{d.courts[0].room: ""}
	for _, kind := range []string{"exercises", "breaks"} {
		items, _, err := d.list(ctx, kind, today, today)
		if err != nil {
			return err
		}
		for _, it := range items {
			if _, err := d.intervals(kind, "", it, rooms); err != nil {
				return err
			}
		}
	}
	return alert
}
