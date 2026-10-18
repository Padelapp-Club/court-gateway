package yclients

import (
	"fmt"
	"sort"
	"strings"
)

// service is a court's service as book_services lists it (with staff_id:
// seance_length is null without one).
type service struct {
	ID           int64    `json:"id"`
	SeanceLength *float64 `json:"seance_length"`
	PriceMin     float64  `json:"price_min"`
	Active       *int     `json:"active"`
}

func (s service) usable() bool {
	return (s.Active == nil || *s.Active == 1) && s.SeanceLength != nil && *s.SeanceLength > 0 && *s.SeanceLength == float64(int(*s.SeanceLength))
}

// piece is one service of a booking and where it starts in it.
type piece struct {
	id     int64
	length int // seconds
	offset int
}

// tileSpan covers span seconds exactly with consecutive services, each
// starting and ending on a boundary (or the span's end): fewest pieces,
// then the cheapest, then longer pieces first; nil when no cover exists.
// It is the backend's tileSpan (service-tiling.util.ts), the same choices.
func tileSpan(span int, services []service, boundaries []int) []piece {
	if span <= 0 {
		return nil
	}
	// one service per length: the cheapest, then the smaller id as text
	byLength := map[int]service{}
	for _, s := range services {
		if !s.usable() {
			continue
		}
		l := int(*s.SeanceLength)
		if l > span {
			continue
		}
		kept, ok := byLength[l]
		if !ok || s.PriceMin < kept.PriceMin || s.PriceMin == kept.PriceMin && fmt.Sprint(s.ID) < fmt.Sprint(kept.ID) {
			byLength[l] = s
		}
	}
	if len(byLength) == 0 {
		return nil
	}
	lengths := make([]int, 0, len(byLength))
	for l := range byLength {
		lengths = append(lengths, l)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(lengths)))
	isBoundary := map[int]bool{}
	for _, b := range boundaries {
		if b > 0 && b < span {
			isBoundary[b] = true
		}
	}
	starts := []int{0}
	for b := range isBoundary {
		starts = append(starts, b)
	}
	sort.Ints(starts)
	type optimum struct {
		pieces int
		price  float64
	}
	best := map[int]optimum{span: {}}
	for i := len(starts) - 1; i >= 0; i-- {
		off := starts[i]
		var o *optimum
		for _, l := range lengths {
			end := off + l
			if end != span && !isBoundary[end] {
				continue
			}
			tail, ok := best[end]
			if !ok {
				continue
			}
			c := optimum{tail.pieces + 1, tail.price + byLength[l].PriceMin}
			if o == nil || c.pieces < o.pieces || c.pieces == o.pieces && c.price < o.price {
				o = &c
			}
		}
		if o != nil {
			best[off] = *o
		}
	}
	if _, ok := best[0]; !ok {
		return nil
	}
	var out []piece
	for off := 0; off != span; {
		here := best[off]
		advanced := false
		for _, l := range lengths {
			end := off + l
			if end != span && !isBoundary[end] {
				continue
			}
			tail, ok := best[end]
			if ok && tail.pieces+1 == here.pieces && tail.price+byLength[l].PriceMin == here.price {
				out = append(out, piece{id: byLength[l].ID, length: l, offset: off})
				off, advanced = end, true
				break
			}
		}
		if !advanced {
			return nil
		}
	}
	return out
}

// pickServices picks the services of a booking of seconds (any whole
// minute may be a boundary: all services of one record are one record).
// The error is for the partner: the length the court's services cannot
// make, and which they can.
func pickServices(seconds int, services []service) ([]piece, error) {
	var boundaries []int
	for b := 60; b < seconds; b += 60 {
		boundaries = append(boundaries, b)
	}
	if p := tileSpan(seconds, services, boundaries); p != nil {
		return p, nil
	}
	seen := map[int]bool{}
	var avail []int
	for _, s := range services {
		if s.usable() && !seen[int(*s.SeanceLength)/60] {
			seen[int(*s.SeanceLength)/60] = true
			avail = append(avail, int(*s.SeanceLength)/60)
		}
	}
	sort.Ints(avail)
	if len(avail) == 0 {
		return nil, fmt.Errorf("бронь на %d минут не выражается услугами корта (услуг с длительностью нет)", seconds/60)
	}
	parts := make([]string, len(avail))
	for i, a := range avail {
		parts[i] = fmt.Sprint(a)
	}
	return nil, fmt.Errorf("бронь на %d минут не выражается услугами корта (доступны: %s минут)", seconds/60, strings.Join(parts, ", "))
}

// appointment is one record of a booking: its services and its offset
// from the booking's start.
type appointment struct {
	services []int64
	offset   int
}

// planAppointments lays the services out in records. YCLIENTS adds up
// different services of one appointment but folds the same service twice
// into one (live probe, D11): a repeated service opens the next record,
// back to back.
func planAppointments(pieces []piece) []appointment {
	var out []appointment
	off := 0
	for _, p := range pieces {
		n := len(out)
		if n == 0 || contains(out[n-1].services, p.id) {
			out = append(out, appointment{services: []int64{p.id}, offset: off})
		} else {
			out[n-1].services = append(out[n-1].services, p.id)
		}
		off += p.length
	}
	return out
}

func contains(xs []int64, x int64) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
