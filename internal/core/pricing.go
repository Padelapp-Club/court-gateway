package core

import (
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

// quote integrates the price over [start, end), split at local midnights
// and at every price boundary, and rounds the total to whole rubles (half
// up). start and end must be on the slot grid (the API's rule for
// bookings); the price itself does not depend on the grid.
func quote(t driver.PriceTable, loc *time.Location, slot int, start, end time.Time) (int64, error) {
	if !aligned(start, loc, slot) || !aligned(end, loc, slot) || !end.After(start) {
		return 0, errNotAligned
	}
	var kopeckMinutes int64
	for c := start.In(loc); c.Before(end); {
		next := time.Date(c.Year(), c.Month(), c.Day()+1, 0, 0, 0, 0, loc)
		to := 1440
		if end.Before(next) {
			e := end.In(loc)
			next, to = end, e.Hour()*60+e.Minute()
		}
		for _, p := range segments(t, isoWeekday(c), c.Hour()*60+c.Minute(), to) {
			if !p.ok {
				return 0, errNoPrice
			}
			kopeckMinutes += p.PerHour * int64(p.To-p.From)
		}
		c = next.In(loc)
	}
	if kopeckMinutes < 0 {
		return 0, errNoPrice
	}
	// kopeck-minutes -> rubles: divide by 60 (per hour) and by 100.
	return (kopeckMinutes + 3000) / 6000, nil
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
