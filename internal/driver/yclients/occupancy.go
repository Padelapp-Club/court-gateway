package yclients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

const (
	recordsPageSize = 200
	// recordsMaxPages: 50 x 200 records in a window is far more than a
	// club has; more is taken as an answer the driver does not understand.
	recordsMaxPages = 50
)

var (
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	clockRe = regexp.MustCompile(`^\d{2}:\d{2}$`)
)

// record is the part of a YCLIENTS record the driver reads. The client
// and every other field are never decoded; the comment only to recognize
// the gateway's own records (marker, set tag).
type record struct {
	ID       json.Number `json:"id"`
	StaffID  json.Number `json:"staff_id"`
	Datetime string      `json:"datetime"`
	Length   *float64    `json:"length"`
	Deleted  bool        `json:"deleted"`
	Comment  *string     `json:"comment"`
}

// span is a record's interval, ok=false when it is not readable.
func (r record) span() (start, end time.Time, ok bool) {
	t, err := time.Parse(time.RFC3339, r.Datetime)
	if err != nil || r.Length == nil || *r.Length <= 0 || *r.Length != float64(int64(*r.Length)) || *r.Length > 7*24*3600 {
		return time.Time{}, time.Time{}, false
	}
	return t, t.Add(time.Duration(*r.Length) * time.Second), true
}

type scheduleDay struct {
	Date      string          `json:"date"`
	IsWorking json.RawMessage `json:"is_working"`
	Slots     []struct {
		From *string `json:"from"`
		To   *string `json:"to"`
	} `json:"slots"`
}

func (d scheduleDay) working() bool {
	s := strings.Trim(strings.TrimSpace(string(d.IsWorking)), `"`)
	return s != "" && s != "0" && s != "false" && s != "null"
}

// localDay is midnight of t's day in loc.
func localDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// days are the club's days that [from, to) touches.
func days(from, to time.Time, loc *time.Location) []time.Time {
	var out []time.Time
	last := localDay(to.Add(-time.Nanosecond), loc)
	for d := localDay(from, loc); !d.After(last); d = time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, loc) {
		out = append(out, d)
	}
	return out
}

func ymd(t time.Time) string { return t.Format("2006-01-02") }

// fetchRecords reads the branch's records whose start is on the days
// [first, last], every page. A page short of the total the first page
// promised, or an empty page before it, is an incomplete answer: refused,
// since it would show as free what a missing record occupies.
func (d *Driver) fetchRecords(ctx context.Context, first, last time.Time) ([]record, error) {
	var all []record
	declared := -1
	for page := 1; ; page++ {
		if page > recordsMaxPages {
			return nil, driver.Upstream(driver.UpstreamInvalid, fmt.Sprintf("GET /records: more than %d pages", recordsMaxPages))
		}
		q := url.Values{"start_date": {ymd(first)}, "end_date": {ymd(last)}, "count": {strconv.Itoa(recordsPageSize)}, "page": {strconv.Itoa(page)}}
		r, err := d.c.do(ctx, http.MethodGet, "/records/"+d.company, q, nil, userAuth)
		if err != nil {
			return nil, err
		}
		if r.status != http.StatusOK {
			return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /records: HTTP %d", r.status))
		}
		var data []record
		if len(r.data) == 0 || r.data[0] != '[' || json.Unmarshal(r.data, &data) != nil {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /records: data is not an array of records")
		}
		all = append(all, data...)
		var meta struct {
			Total *int `json:"total_count"`
		}
		if json.Unmarshal(r.meta, &meta) == nil && meta.Total != nil && *meta.Total > declared {
			declared = *meta.Total // the largest promise stands: a page without meta does not erase it
		}
		if len(data) == 0 {
			if declared >= 0 && len(all) < declared {
				return nil, driver.Upstream(driver.UpstreamInvalid, fmt.Sprintf("GET /records: %d of %d records", len(all), declared))
			}
			return all, nil
		}
		if declared >= 0 && len(all) >= declared || declared < 0 && len(data) < recordsPageSize {
			return all, nil
		}
	}
}

// working reads a court's schedule for the days and returns its working
// intervals. Schedule times carry no zone: they are the club's. An end of
// "00:00", or one not after the start, is on the next day. A day missing
// from the answer or not working has no working time: the whole day is
// occupied, as YCLIENTS does not sell it either.
func (d *Driver) working(ctx context.Context, staff string, ds []time.Time) ([][2]time.Time, error) {
	first, last := ds[0], ds[len(ds)-1]
	path := "/schedule/" + d.company + "/" + staff + "/" + ymd(first) + "/" + ymd(last)
	r, err := d.c.do(ctx, http.MethodGet, path, nil, nil, userAuth)
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /schedule: HTTP %d", r.status))
	}
	var sched []scheduleDay
	if len(r.data) == 0 || r.data[0] != '[' || json.Unmarshal(r.data, &sched) != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /schedule: data is not an array of days")
	}
	byDate := map[string]scheduleDay{}
	for _, day := range sched {
		if !dateRe.MatchString(day.Date) || day.Slots == nil {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /schedule: a day in an unexpected form")
		}
		byDate[day.Date] = day
	}
	var missing []string
	var out [][2]time.Time
	for _, date := range ds {
		day, ok := byDate[ymd(date)]
		if !ok {
			missing = append(missing, ymd(date))
			continue
		}
		if !day.working() {
			continue
		}
		for _, s := range day.Slots {
			if s.From == nil || s.To == nil || !clockRe.MatchString(*s.From) || !clockRe.MatchString(*s.To) {
				return nil, driver.Upstream(driver.UpstreamInvalid, "GET /schedule: an interval in an unexpected form")
			}
			fh, fm := clockOf(*s.From)
			th, tm := clockOf(*s.To)
			if fh > 23 || fm > 59 || th > 24 || tm > 59 || th == 24 && tm != 0 {
				return nil, driver.Upstream(driver.UpstreamInvalid, "GET /schedule: an interval that is no time of day")
			}
			from := time.Date(date.Year(), date.Month(), date.Day(), fh, fm, 0, 0, d.loc)
			to := time.Date(date.Year(), date.Month(), date.Day(), th, tm, 0, 0, d.loc)
			if !to.After(from) {
				to = time.Date(date.Year(), date.Month(), date.Day()+1, th, tm, 0, 0, d.loc)
			}
			out = append(out, [2]time.Time{from, to})
		}
	}
	if len(missing) > 0 {
		d.log.Warn("yclients schedule lacks days: they count as occupied", "staff", staff, "days", strings.Join(missing, ","))
	}
	return out, nil
}

func clockOf(s string) (int, int) {
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h, m
}

// complement is what of [from, to) the working intervals leave out.
func complement(from, to time.Time, working [][2]time.Time) [][2]time.Time {
	sort.Slice(working, func(i, j int) bool { return working[i][0].Before(working[j][0]) })
	var holes [][2]time.Time
	cursor := from
	for _, w := range working {
		if w[0].After(cursor) {
			holes = append(holes, [2]time.Time{cursor, minTime(w[0], to)})
		}
		if w[1].After(cursor) {
			cursor = w[1]
		}
		if !cursor.Before(to) {
			break
		}
	}
	if cursor.Before(to) {
		holes = append(holes, [2]time.Time{cursor, to})
	}
	out := holes[:0]
	for _, h := range holes {
		if h[1].After(h[0]) {
			out = append(out, h)
		}
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// holeID names an interval of no working time: no record, no booking.
func holeID(staff string, start time.Time) string {
	return "yh-" + staff + "-" + strconv.FormatInt(start.Unix(), 10)
}

// Occupancy implements driver.Driver: the branch's records on the courts
// plus every stretch of the window that is not the court's working time
// (breaks, closed days): YCLIENTS keeps breaks in the schedule, not in
// records, and records alone would show a break as free. Records are read
// from the day before the window, so one running past midnight is seen.
// Any failed or malformed answer fails the whole read: an occupancy with
// a hole would sell what is taken.
func (d *Driver) Occupancy(ctx context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	want := map[string]string{}
	for _, cid := range q.Courts {
		staff, ok := d.byCourt[cid]
		if !ok {
			return driver.Occupancy{}, fmt.Errorf("court %q: %w", cid, driver.ErrNotFound)
		}
		want[staff] = cid
	}
	ds := days(q.From, q.To, d.loc)
	if len(ds) == 0 {
		return driver.Occupancy{Complete: true, Intervals: []driver.Interval{}}, nil
	}
	first, last := ds[0], ds[len(ds)-1]
	records, err := d.fetchRecords(ctx, first.AddDate(0, 0, -1), last)
	if err != nil {
		return driver.Occupancy{}, err
	}
	ivs, err := d.recordIntervals(records, want)
	if err != nil {
		return driver.Occupancy{}, err
	}
	windowFrom, windowTo := first, time.Date(last.Year(), last.Month(), last.Day()+1, 0, 0, 0, 0, d.loc)
	staffs := make([]string, 0, len(want))
	for s := range want {
		staffs = append(staffs, s)
	}
	sort.Strings(staffs)
	for _, staff := range staffs {
		w, err := d.working(ctx, staff, ds)
		if err != nil {
			return driver.Occupancy{}, err
		}
		for _, h := range complement(windowFrom, windowTo, w) {
			ivs = append(ivs, driver.Interval{ID: holeID(staff, h[0]), Court: want[staff], Start: h[0], End: h[1]})
		}
	}
	occ := driver.Occupancy{Complete: true, Intervals: []driver.Interval{}}
	for _, iv := range ivs {
		if iv.Start.Before(q.To) && iv.End.After(q.From) {
			occ.Intervals = append(occ.Intervals, iv)
		}
	}
	return occ, nil
}

// recordIntervals turns the records on the wanted courts (staff id ->
// court) into intervals. The records of one booking of the gateway made
// of several (a repeated service, see setTag) are one interval under the
// booking's id when all of them are there, back to back on one court;
// otherwise each is its own. A record of a wanted court that cannot be
// read fails the answer; others' (a coach's) are not looked at.
func (d *Driver) recordIntervals(records []record, want map[string]string) ([]driver.Interval, error) {
	type piece struct {
		id         string
		start, end time.Time
	}
	type set struct {
		court  string
		n      int
		pieces []piece
	}
	sets := map[string]*set{}
	var keys []string
	var out []driver.Interval
	for _, r := range records {
		if r.Deleted {
			continue
		}
		court, ok := want[r.StaffID.String()]
		if !ok {
			continue
		}
		start, end, ok := r.span()
		if !ok || !digitsRe.MatchString(r.ID.String()) {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /records: a record in an unexpected form")
		}
		key, n, tagged := setOf(r.Comment)
		if !tagged {
			out = append(out, driver.Interval{ID: "y-" + r.ID.String(), Court: court, Start: start, End: end})
			continue
		}
		key = r.StaffID.String() + " " + key
		if sets[key] == nil {
			sets[key] = &set{court: court, n: n}
			keys = append(keys, key)
		}
		sets[key].pieces = append(sets[key].pieces, piece{r.ID.String(), start, end})
	}
	for _, k := range keys {
		st := sets[k]
		ps := st.pieces
		sort.Slice(ps, func(i, j int) bool { return ps[i].start.Before(ps[j].start) })
		whole := len(ps) == st.n
		for i := 1; whole && i < len(ps); i++ {
			whole = ps[i].start.Equal(ps[i-1].end)
		}
		if !whole { // a part is gone, moved or one too many: each stands alone
			for _, p := range ps {
				out = append(out, driver.Interval{ID: "y-" + p.id, Court: st.court, Start: p.start, End: p.end})
			}
			continue
		}
		ids := make([]string, len(ps))
		for i, p := range ps {
			ids[i] = p.id
		}
		out = append(out, driver.Interval{ID: "y-" + strings.Join(ids, "-"), Court: st.court, Start: ps[0].start, End: ps[len(ps)-1].end})
	}
	return out, nil
}

// Ready implements driver.Driver: the config check (which reads the
// branch and its staff) and one day's records and first court's schedule,
// validated as occupancy validates them. A config alert is returned only
// when the reads pass.
func (d *Driver) Ready(ctx context.Context) error {
	alert := d.CheckConfig(ctx)
	if alert != nil && !isAlert(alert) {
		return alert
	}
	now, _ := d.clock()
	today := localDay(now, d.loc)
	records, err := d.fetchRecords(ctx, today, today)
	if err != nil {
		return err
	}
	if _, err := d.recordIntervals(records, d.staff); err != nil {
		return err
	}
	if _, err := d.working(ctx, d.byCourt[d.courts[0]], []time.Time{today}); err != nil {
		return err
	}
	return alert
}
