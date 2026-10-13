package core

import (
	"errors"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

var (
	errNoPrice    = errors.New("no price rule covers part of the interval")
	errNotAligned = errors.New("interval is not aligned to the slot grid")
)

func isoWeekday(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// priceAt returns the hourly price in kopecks for the local stretch
// [from, to) minutes of an ISO weekday: the first base rule that fully
// contains the stretch, plus every adjustment that fully contains it.
func priceAt(t driver.PriceTable, day, from, to int) (int64, bool) {
	var price int64
	found := false
	for _, r := range t.Base {
		if r.Days.Has(day) && from >= r.From && to <= r.To {
			price, found = r.PerHour, true
			break
		}
	}
	if !found {
		return 0, false
	}
	for _, a := range t.Adjustments {
		if a.Days.Has(day) && from >= a.From && to <= a.To {
			price += a.PerHourDelta
		}
	}
	return price, true
}

// quote sums the price of every slot in [start, end) and rounds the total
// to whole rubles (half up).
func quote(t driver.PriceTable, loc *time.Location, slot int, start, end time.Time) (int64, error) {
	if !aligned(start, loc, slot) || !aligned(end, loc, slot) || !end.After(start) {
		return 0, errNotAligned
	}
	step := time.Duration(slot) * time.Minute
	var kopeckMinutes int64
	for c := start; c.Before(end); c = c.Add(step) {
		lt := c.In(loc)
		m := lt.Hour()*60 + lt.Minute()
		p, ok := priceAt(t, isoWeekday(lt), m, m+slot)
		if !ok {
			return 0, errNoPrice
		}
		kopeckMinutes += p * int64(slot)
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
}

// dayPeriods lists the effective hourly prices of a local date, merging
// consecutive slots with the same price. Slots without a price are omitted.
func dayPeriods(t driver.PriceTable, slot int, day int) []pricePeriod {
	var out []pricePeriod
	for m := 0; m+slot <= 1440; m += slot {
		p, ok := priceAt(t, day, m, m+slot)
		if !ok {
			continue
		}
		if n := len(out); n > 0 && out[n-1].To == m && out[n-1].PerHour == p {
			out[n-1].To = m + slot
			continue
		}
		out = append(out, pricePeriod{From: m, To: m + slot, PerHour: p})
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
