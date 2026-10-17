package core

import (
	"cmp"
	"context"
	"errors"
	"sort"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

var (
	errNoPrice    = errors.New("no price rule covers part of the interval")
	errNotAligned = errors.New("interval is not aligned to the slot grid")
)

func isoWeekday(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// onDate is the table that prices the local date of lt (in the rules'
// zone): its Dates entry if it has one, else the weekly table.
func onDate(t driver.PriceTable, lt time.Time) driver.PriceTable {
	rules, ok := t.Dates[lt.Format("2006-01-02")]
	if !ok {
		return t
	}
	out := driver.PriceTable{Zone: t.Zone}
	for _, r := range rules {
		r.Days = driver.AllWeekdays
		out.Base = append(out.Base, r)
	}
	return out
}

// segments splits the local stretch [from, to) minutes of an ISO weekday
// at every boundary of a rule or adjustment that applies to that day, and
// prices each piece: the first base rule that contains it plus every
// adjustment that contains it. A piece no base rule covers has ok=false.
// Prices are therefore exact to the minute whatever the booking grid is.
func segments(t driver.PriceTable, day, from, to int) []pricePeriod {
	cuts := []int{from, to}
	add := func(rf, rt int) {
		for _, m := range [2]int{rf, rt} {
			if m > from && m < to {
				cuts = append(cuts, m)
			}
		}
	}
	for _, r := range t.Base {
		if r.Days.Has(day) {
			add(r.From, r.To)
		}
	}
	for _, a := range t.Adjustments {
		if a.Days.Has(day) {
			add(a.From, a.To)
		}
	}
	sort.Ints(cuts)
	var out []pricePeriod
	for i := 0; i+1 < len(cuts); i++ {
		a, b := cuts[i], cuts[i+1]
		if a == b {
			continue
		}
		p := pricePeriod{From: a, To: b}
		for _, r := range t.Base {
			if r.Days.Has(day) && a >= r.From && b <= r.To {
				p.PerHour, p.ok = r.PerHour, true
				break
			}
		}
		if p.ok {
			for _, adj := range t.Adjustments {
				if adj.Days.Has(day) && a >= adj.From && b <= adj.To {
					p.PerHour += adj.PerHourDelta
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// quote integrates the price over [start, end), split at local midnights,
// at UTC offset changes and at every price boundary, and rounds the total
// to whole rubles (half up). Each piece lies within one local day and one
// offset, so its local clock minutes are its elapsed minutes: a repeated
// hour (clocks go back) is charged twice and a skipped one (clocks go
// forward) not at all. start and end must be on the slot grid of the
// club's zone loc (the API's rule for bookings); the price itself does not
// depend on the grid. The rules are read in their own zone (t.Zone).
func quote(ctx context.Context, t driver.PriceTable, loc *time.Location, slot int, start, end time.Time) (int64, error) {
	if !aligned(start, loc, slot) || !aligned(end, loc, slot) || !end.After(start) {
		return 0, errNotAligned
	}
	rz := cmp.Or(t.Zone, loc)
	var kopeckMinutes int64
	for c := start; c.Before(end); {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		next := nextBoundary(c, rz)
		if end.Before(next) {
			next = end
		}
		lc := c.In(rz)
		from := lc.Hour()*60 + lc.Minute()
		for _, p := range segments(onDate(t, lc), isoWeekday(lc), from, from+int(next.Sub(c)/time.Minute)) {
			if !p.ok {
				return 0, errNoPrice
			}
			kopeckMinutes += p.PerHour * int64(p.To-p.From)
		}
		c = next
	}
	if kopeckMinutes < 0 {
		return 0, errNoPrice
	}
	// kopeck-minutes -> rubles: divide by 60 (per hour) and by 100.
	return (kopeckMinutes + 3000) / 6000, nil
}

// nextBoundary is the first instant strictly after c at which the local
// date or the UTC offset changes. Midnight is computed in c's own offset,
// where clock arithmetic is exact, and the offset's end is taken if it
// comes first. So a midnight the clocks skip (they jump from 23:59 to
// 01:00) or repeat (they go back from 00:00 to 23:00) still yields a
// boundary after c, and the caller always makes progress.
func nextBoundary(c time.Time, loc *time.Location) time.Time {
	lc := c.In(loc)
	name, offset := lc.Zone()
	next := time.Date(lc.Year(), lc.Month(), lc.Day()+1, 0, 0, 0, 0, time.FixedZone(name, offset))
	if _, zoneEnd := lc.ZoneBounds(); !zoneEnd.IsZero() && zoneEnd.Before(next) {
		next = zoneEnd
	}
	return next
}

// aligned reports whether t falls on the slot grid of its local day.
func aligned(t time.Time, loc *time.Location, slot int) bool {
	lt := t.In(loc)
	if lt.Second() != 0 || lt.Nanosecond() != 0 {
		return false
	}
	return (lt.Hour()*60+lt.Minute())%slot == 0
}

type pricePeriod struct {
	From, To int // local minutes
	PerHour  int64
	ok       bool // a base rule covers the stretch
}

// datePeriod is a stretch of a date with one hourly price.
type datePeriod struct {
	Start, End time.Time
	PerHour    int64
}

// datePeriods lists the prices of the club's civil date (date's year,
// month and day in loc) from its first instant to the next date's: a
// repeated hour is listed twice, a skipped one not at all. The day is
// walked in pieces of one date and offset of the rules' zone, as quote
// does; adjacent stretches with the same price are joined and stretches
// without one omitted.
func datePeriods(t driver.PriceTable, loc *time.Location, date time.Time) []datePeriod {
	rz := cmp.Or(t.Zone, loc)
	end := dayStart(date.Year(), date.Month(), date.Day()+1, loc)
	var out []datePeriod
	for c := dayStart(date.Year(), date.Month(), date.Day(), loc); c.Before(end); {
		next := nextBoundary(c, rz)
		if end.Before(next) {
			next = end
		}
		rc := c.In(rz)
		from := rc.Hour()*60 + rc.Minute()
		at := func(m int) time.Time { return c.Add(time.Duration(m-from) * time.Minute) }
		for _, p := range segments(onDate(t, rc), isoWeekday(rc), from, from+int(next.Sub(c)/time.Minute)) {
			switch n := len(out); {
			case !p.ok:
			case n > 0 && out[n-1].End.Equal(at(p.From)) && out[n-1].PerHour == p.PerHour:
				out[n-1].End = at(p.To)
			default:
				out = append(out, datePeriod{at(p.From), at(p.To), p.PerHour})
			}
		}
		c = next
	}
	return out
}

// dayStart is the first instant of the civil date y-m-d in loc. time.Date
// alone may miss it: it puts a skipped midnight on the day before (the
// date then starts when the clocks jump) and may take the later of a
// repeated midnight.
func dayStart(y int, m time.Month, d int, loc *time.Location) time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, loc)
	if t.Day() != time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Day() {
		_, t = t.ZoneBounds()
	}
	if start, _ := t.ZoneBounds(); !start.IsZero() {
		_, off := start.Add(-time.Nanosecond).Zone()
		e := time.Date(y, m, d, 0, 0, 0, 0, time.FixedZone("", off)).In(loc)
		if _, eo := e.Zone(); eo == off && e.Before(t) {
			t = e
		}
	}
	return t
}

// rubles rounds kopecks to whole rubles, half away from zero.
func rubles(kop int64) int64 {
	if kop < 0 {
		return -((-kop + 50) / 100)
	}
	return (kop + 50) / 100
}
