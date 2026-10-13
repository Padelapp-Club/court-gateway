package viva

import (
	"bytes"
	"context"
	"encoding/json"
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

// day is local midnight of t in the studio's zone.
func (d *Driver) day(t time.Time) time.Time {
	t = t.In(d.loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, d.loc)
}

// clockSkew is how far the CRM's clock may run ahead of the gateway's:
// near midnight the CRM's "today" may already be the gateway's tomorrow.
const clockSkew = 5 * time.Minute

// listDays is the longest date range asked in one list request: a club's
// month of exercises in one answer would be large, so long ranges are
// asked a week at a time.
const listDays = 7

// window computes the dates to list for [from, to) and whether the answer
// can be complete. Observed on a live account: the list endpoints select
// records by the START date in the studio's local day (dateFrom..dateTo,
// both included); the exercise list leaves out every exercise of a day
// that is already past for the CRM (it keeps today and later), the break
// list leaves out nothing. So:
//
//   - exercises are listed from the day before the range (one that starts
//     the evening before may still run into it). One that started on a
//     past day is not returned: a range that starts before the CRM's
//     today plus overnight_minutes may overlap such an exercise and is
//     reported incomplete (and cannot be booked). The CRM's today is taken
//     clockSkew ahead of ours, so a CRM clock slightly ahead cannot make an
//     incomplete answer look complete;
//   - breaks are listed from break_lookback_days before the range, so a
//     multi-day break (a renovation) that started earlier is seen. A break
//     that started even earlier is not: the look-back bounds what is
//     assumed (the partner API documents the limit).
func (d *Driver) window(kind string, from, to, now time.Time) (dateFrom, dateTo time.Time, complete bool) {
	dateTo = d.day(to).AddDate(0, 0, 1)
	if kind == "breaks" {
		return d.day(from).AddDate(0, 0, -d.lookback), dateTo, true
	}
	today := d.day(now.Add(clockSkew))
	return d.day(from).AddDate(0, 0, -1), dateTo, !from.Before(today.Add(d.overnight))
}

func (d *Driver) list(ctx context.Context, kind string, room string, dateFrom, dateTo time.Time) ([]slot, error) {
	q := url.Values{
		"studioId":        {d.studio},
		"dateFrom":        {dateFrom.Format("2006-01-02")},
		"dateTo":          {dateTo.Format("2006-01-02")},
		"includeCanceled": {"false"},
	}
	if room != "" {
		q.Set("roomId", room)
	}
	status, data, err := d.c.do(ctx, http.MethodGet, "/"+kind, q, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", kind, status))
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+": response is not a JSON array")
	}
	var out []slot
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+": malformed item")
	}
	return out, nil
}

// Occupancy implements driver.Driver.
func (d *Driver) Occupancy(ctx context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	rooms := map[string]string{}
	for _, cid := range q.Courts {
		c, ok := d.byCourt[cid]
		if !ok {
			return driver.Occupancy{}, fmt.Errorf("court %q: %w", cid, driver.ErrNotFound)
		}
		rooms[c.room] = cid
	}
	roomFilter := ""
	if len(q.Courts) == 1 {
		roomFilter = d.byCourt[q.Courts[0]].room
	}
	occ := driver.Occupancy{Complete: true}
	for _, kind := range []string{"exercises", "breaks"} {
		dateFrom, dateTo, complete := d.window(kind, q.From, q.To, d.now())
		occ.Complete = occ.Complete && complete
		step := listDays
		if kind == "breaks" {
			step = 1 << 20 // breaks are few: one request, however long the look-back
		}
		var items []slot
		for f := dateFrom; !f.After(dateTo); f = f.AddDate(0, 0, step) {
			part, err := d.list(ctx, kind, roomFilter, f, minTime(f.AddDate(0, 0, step-1), dateTo))
			if err != nil {
				return driver.Occupancy{}, err
			}
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
// refuse is not ready.
func (d *Driver) Ready(ctx context.Context) error {
	if err := d.CheckConfig(ctx); err != nil {
		return err
	}
	today := d.day(d.now())
	rooms := map[string]string{d.courts[0].room: ""}
	for _, kind := range []string{"exercises", "breaks"} {
		items, err := d.list(ctx, kind, d.courts[0].room, today, today)
		if err != nil {
			return err
		}
		for _, it := range items {
			if _, err := d.intervals(kind, "", it, rooms); err != nil {
				return err
			}
		}
	}
	return nil
}
