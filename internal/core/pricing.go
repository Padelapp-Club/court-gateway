package core

import (
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
// forward) not at all. start and end must be on the slot grid (the API's
// rule for bookings); the price itself does not depend on the grid.
func quote(ctx context.Context, t driver.PriceTable, loc *time.Location, slot int, start, end time.Time) (int64, error) {
	if !aligned(start, loc, slot) || !aligned(end, loc, slot) || !end.After(start) {
		return 0, errNotAligned
	}
	var kopeckMinutes int64
	for c := start; c.Before(end); {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		next := nextBoundary(c, loc)
		if end.Before(next) {
			next = end
		}
		lc := c.In(loc)
		from := lc.Hour()*60 + lc.Minute()
		for _, p := range segments(t, isoWeekday(lc), from, from+int(next.Sub(c)/time.Minute)) {
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

// dayPeriods lists the effective hourly prices of a local date, merging
// adjacent stretches with the same price. Stretches without a price are
// omitted.
func dayPeriods(t driver.PriceTable, day int) []pricePeriod {
	var out []pricePeriod
	for _, p := range segments(t, day, 0, 1440) {
		if !p.ok {
			continue
		}
		if n := len(out); n > 0 && out[n-1].To == p.From && out[n-1].PerHour == p.PerHour {
			out[n-1].To = p.To
			continue
		}
		out = append(out, p)
	}
	return out
}

// rubles rounds kopecks to whole rubles, half away from zero.
func rubles(kop int64) int64 {
	if kop < 0 {
		return -((-kop + 50) / 100)
	}
	return (kop + 50) / 100
}
