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

// parseTime accepts RFC3339 and, as an assumption, a zone-less ISO
// timestamp, which is taken as UTC (what the API returned to the previous
// integration, whose host ran in UTC).
func parseTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.UTC); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// formatTime writes UTC with milliseconds, like JavaScript toISOString().
func formatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// listWindow computes the whole-UTC-day window to request for [from, to),
// and whether the answer can be complete.
//
// The list endpoints filter by whole UTC days, so a booking that starts on
// the previous UTC day may still overlap the range: we ask one extra day on
// each side and cut the overlap ourselves. But the endpoints return an
// EMPTY list for past dates. Two consequences:
//   - a range that starts before today (UTC) is reported incomplete;
//   - for a range that starts today or later, the margin day is never moved
//     into the past: asking with a past dateFrom could empty the whole answer
//     (only fully past ranges were observed), and an empty answer reads as
//     "everything is free". The price is a documented residual: a booking that
//     began yesterday (UTC) and runs past midnight is not seen by a query
//     starting today.
func listWindow(from, to, now time.Time) (dateFrom, dateTo time.Time, complete bool) {
	today := utcDay(now)
	complete = !from.Before(today)
	dateFrom = utcDay(from).AddDate(0, 0, -1)
	if complete && dateFrom.Before(today) {
		dateFrom = today
	}
	dateTo = utcDay(to).AddDate(0, 0, 1)
	return dateFrom, dateTo, complete
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
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", kind, status), nil)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+": response is not a JSON array", nil)
	}
	var out []slot
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+": malformed item", nil)
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
	dateFrom, dateTo, complete := listWindow(q.From, q.To, d.now())

	occ := driver.Occupancy{Complete: complete}
	for _, kind := range []string{"exercises", "breaks"} {
		items, err := d.list(ctx, kind, roomFilter, dateFrom, dateTo)
		if err != nil {
			return driver.Occupancy{}, err
		}
		prefix := "e-"
		if kind == "breaks" {
			prefix = "b-"
		}
		for _, it := range items {
			iv, keep, err := d.interval(kind, prefix, it, rooms)
			if err != nil {
				return driver.Occupancy{}, err
			}
			if keep && iv.Start.Before(q.To) && iv.End.After(q.From) {
				occ.Intervals = append(occ.Intervals, iv)
			}
		}
	}
	return occ, nil
}

// interval validates one list item. Anything structurally wrong makes the
// whole answer invalid: silently dropping an item would show a busy court
// as free.
func (d *Driver) interval(kind, prefix string, it slot, rooms map[string]string) (driver.Interval, bool, error) {
	bad := func(what string) (driver.Interval, bool, error) {
		return driver.Interval{}, false, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+": item "+what, nil)
	}
	if !uuidRe.MatchString(it.ID) {
		return bad("has no valid id")
	}
	if it.Room == nil || it.Room.ID == "" {
		return bad("has no room")
	}
	start, ok1 := parseTime(it.TimeFrom)
	end, ok2 := parseTime(it.TimeTo)
	if !ok1 || !ok2 {
		return bad("has unparseable timeFrom/timeTo")
	}
	if !end.After(start) {
		return bad("ends before it starts")
	}
	if it.Canceled != nil && *it.Canceled {
		return driver.Interval{}, false, nil
	}
	court, ok := rooms[strings.ToLower(it.Room.ID)]
	if !ok {
		return driver.Interval{}, false, nil // a room that is not a configured court
	}
	return driver.Interval{ID: prefix + strings.ToLower(it.ID), Court: court, Start: start, End: end}, true, nil
}

// Ready logs in (or reuses the cached token) and makes a narrow occupancy
// read on the first court.
func (d *Driver) Ready(ctx context.Context) error {
	if _, err := d.c.accessToken(ctx); err != nil {
		return err
	}
	now := d.now()
	_, err := d.Occupancy(ctx, driver.OccupancyQuery{
		Courts: []string{d.courts[0].id},
		From:   now,
		To:     now.Add(time.Duration(d.slot) * time.Minute),
	})
	return err
}
