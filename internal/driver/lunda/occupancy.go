package lunda

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// The schedule of one local day, as slots-load answers it (observed under
// a staff login): {timeFrameSize, beyondBookingDepth, availableHours,
// courts: [{courtUid, courtName, categoryName, slots: [...]}]}. A slot is
// {type, bookingId, begin, end, goal, payed, clientUid, contact, notes...}
// with begin and end in the club's local time without a zone. Only the
// type, the booking id and the times are decoded: nothing about clients.

// Slot types seen. Every slot occupies its court, whatever its type: a
// DRAFT is someone's hold of a few minutes, a PLACEHOLDER a block, BLOCKED
// a closure; a type not seen yet occupies it too.
var knownTypes = map[string]bool{"BOOKING": true, "PLACEHOLDER": true, "DRAFT": true, "BLOCKED": true}

type lundaSlot struct {
	Type      string `json:"type"`
	BookingID string `json:"bookingId"`
	Begin     string `json:"begin"`
	End       string `json:"end"`
}

type daySchedule struct {
	day                time.Time // the local day asked
	BeyondBookingDepth bool      `json:"beyondBookingDepth"`
	AvailableHours     []int     `json:"availableHours"`
	Courts             *[]struct {
		CourtUID string       `json:"courtUid"`
		Slots    []*lundaSlot `json:"slots"`
	} `json:"courts"`
}

// localLayout is how Lunda writes a local time.
const localLayout = "2006-01-02T15:04:05"

// parseLocal reads Lunda's local time in loc; a time with a zone (not seen,
// but unambiguous) is taken as it says.
func parseLocal(s string, loc *time.Location) (time.Time, bool) {
	if t, err := time.ParseInLocation(localLayout, s, loc); err == nil {
		return t, true
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// formatLocal writes t as a local time of loc, as Lunda takes it.
func formatLocal(t time.Time, loc *time.Location) string { return t.In(loc).Format(localLayout) }

// readSlots reads the schedule of one local day. An answer without the
// list of courts is invalid: an empty schedule must say so.
func readSlots(ctx context.Context, c *client, day time.Time) (daySchedule, error) {
	ds := daySchedule{day: day}
	status, res, err := c.call(ctx, opSlots, url.Values{"date": {day.Format("2006-01-02")}}, nil)
	if err != nil {
		return ds, err
	}
	if status != http.StatusOK {
		return ds, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opSlots, status))
	}
	if t := bytes.TrimSpace(res); len(t) == 0 || t[0] != '{' || json.Unmarshal(t, &ds) != nil || ds.Courts == nil {
		return ds, driver.Upstream(driver.UpstreamInvalid, "GET /"+opSlots+": the answer is not a day's schedule")
	}
	ds.day = day
	return ds, nil
}

func (d *Driver) readDay(ctx context.Context, day time.Time) (daySchedule, error) {
	return readSlots(ctx, d.c, day)
}

// slotID is the interval id of a slot: "l-<bookingId>" for a slot of a
// booking (a placeholder is one too), the same id as Create returns;
// a slot of no booking (a draft, a closure) gets "ls-" and a hash of what
// it is, stable while it does not change, and never a booking's.
func slotID(s *lundaSlot, courtUID string) (string, bool) {
	if s.BookingID != "" {
		if !uidRe.MatchString(s.BookingID) {
			return "", false
		}
		return "l-" + s.BookingID, true
	}
	return syntheticID(s.Type, courtUID, s.Begin, s.End), true
}

func syntheticID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "ls-" + hex.EncodeToString(h.Sum(nil)[:8])
}

// dayIntervals turns one day's schedule into the intervals of the courts
// in want (courtUid -> court). Anything structurally wrong makes the whole
// answer invalid — dropping a slot would show a busy court as free. A
// court of want that the day does not list at all (Lunda leaves out a
// court blocked for the day) is occupied the whole day.
func (d *Driver) dayIntervals(ds daySchedule, want map[string]string) ([]driver.Interval, error) {
	bad := func(what string) ([]driver.Interval, error) {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opSlots+": "+what)
	}
	var out []driver.Interval
	listed := map[string]bool{}
	for _, ct := range *ds.Courts {
		if ct.CourtUID == "" {
			return bad("a court without courtUid")
		}
		listed[ct.CourtUID] = true
		court, ok := want[ct.CourtUID]
		if !ok {
			continue
		}
		for _, s := range ct.Slots {
			if s == nil || s.Type == "" {
				return bad("a slot without a type")
			}
			start, ok1 := parseLocal(s.Begin, d.loc)
			end, ok2 := parseLocal(s.End, d.loc)
			if !ok1 || !ok2 || !end.After(start) {
				return bad("a slot without a valid begin and end")
			}
			id, ok := slotID(s, ct.CourtUID)
			if !ok {
				return bad("a slot with a malformed bookingId")
			}
			if !knownTypes[s.Type] {
				d.log.Warn("a Lunda slot of an unknown type: it occupies its court", "type", truncate(s.Type, 32))
			}
			out = append(out, driver.Interval{ID: id, Court: court, Start: start, End: end})
		}
	}
	for uid, court := range want {
		if !listed[uid] {
			day := ds.day
			out = append(out, driver.Interval{ID: syntheticID("closed", uid, day.Format("2006-01-02")), Court: court,
				Start: day, End: day.AddDate(0, 0, 1)})
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// crossMidnight: a range starting this early in its local day may meet a
// booking of the day before running past midnight, so that day is read
// too (a slot is listed under the day it begins on).
const crossMidnight = 6 * time.Hour

// Occupancy implements driver.Driver: the schedule of every local day the
// range touches (and the day before, early in the morning). The answer is
// incomplete from the first day beyond the club's booking depth on (Lunda
// may not show it all), and from the first day that cannot be read; an
// answer that does not parse, or a lost session, is an error. A booking
// cut into several slots of one court is one interval.
func (d *Driver) Occupancy(ctx context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	want := map[string]string{}
	for _, cid := range q.Courts {
		c, ok := d.byCourt[cid]
		if !ok {
			return driver.Occupancy{}, fmt.Errorf("court %q: %w", cid, driver.ErrNotFound)
		}
		want[c.uid] = cid
	}
	first := localDay(q.From, d.loc)
	if q.From.Sub(first) < crossMidnight {
		first = first.AddDate(0, 0, -1)
	}
	last := localDay(q.To.Add(-time.Nanosecond), d.loc)
	occ := driver.Occupancy{Complete: true}
	var all []driver.Interval
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		ds, err := d.readDay(ctx, day)
		var ue *driver.UpstreamError
		if err != nil {
			if errors.As(err, &ue) && (ue.Kind == driver.UpstreamUnavailable || ue.Kind == driver.UpstreamRejected) {
				d.log.Warn("a day of the Lunda schedule could not be read: occupancy is incomplete", "error", err)
				occ.Complete = false
				break
			}
			return driver.Occupancy{}, err
		}
		ivs, err := d.dayIntervals(ds, want)
		if err != nil {
			return driver.Occupancy{}, err
		}
		all = append(all, ivs...)
		if ds.BeyondBookingDepth {
			occ.Complete = false // every later day is beyond it too
			break
		}
	}
	for _, iv := range merge(all) {
		if iv.Start.Before(q.To) && iv.End.After(q.From) {
			occ.Intervals = append(occ.Intervals, iv)
		}
	}
	return occ, nil
}

// merge joins the overlapping or touching intervals of one id on one court
// (a booking listed slot by slot, or under two days).
func merge(ivs []driver.Interval) []driver.Interval {
	sort.SliceStable(ivs, func(i, j int) bool {
		a, b := ivs[i], ivs[j]
		if a.Court != b.Court {
			return a.Court < b.Court
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Start.Before(b.Start)
	})
	var out []driver.Interval
	for _, iv := range ivs {
		if n := len(out); n > 0 && out[n-1].ID == iv.ID && out[n-1].Court == iv.Court && !iv.Start.After(out[n-1].End) {
			if iv.End.After(out[n-1].End) {
				out[n-1].End = iv.End
			}
			continue
		}
		out = append(out, iv)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}
