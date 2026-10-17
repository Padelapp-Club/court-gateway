package fitness1c

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The occupancy grid of 1C:Fitness (GET /rental_rooms_times/): for every
// room, the half-hours of a rental service, each either listed free,
// listed with is_busy (a temporary reservation of the rental contour), or
// not listed at all (a confirmed rental, a class, a closed hour: 1C does
// not say which). The rules here are the backend adapter's (D14,
// fitness1c-crm.service.ts normalizeGrid / getBreaksForSlots), checked
// against it on the same bodies (difftest_test.go):
//
//   - a half-hour is busy unless 1C listed it free and never busy;
//   - a body that cannot be trusted (a slot without is_busy, a time in an
//     unexpected form) is refused whole, never read as "free";
//   - a slot of another day in a day's answer is not that day's.

// dateTimeLayout is 1C's local time of a slot: the club's, without a zone.
const dateTimeLayout = "2006-01-02 15:04"

var (
	dateTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$`)
	clockRe    = regexp.MustCompile(`^\d{2}:\d{2}$`)
)

// gridSlot is one listed half-hour. Pointers tell a missing field from a
// zero one; nothing else of the slot (its rental, price or service) is read.
type gridSlot struct {
	Time     *string `json:"time"`
	DateTime *string `json:"date_time"`
	IsBusy   *bool   `json:"is_busy"`
}

// gridRoom is one element of the answer: a room with its slots. 1C repeats
// a room once per day of the asked range.
type gridRoom struct {
	Room *struct {
		ID string `json:"id"`
	} `json:"room"`
	Durations *[]gridSlot `json:"durations"`
}

// roomGrid is one room's occupancy: the half-hours 1C listed free and the
// ones it listed busy, by "YYYY-MM-DD HH:MM".
type roomGrid struct {
	free, busy map[string]bool
}

// grid is the normalized answer for some days.
type grid struct {
	rooms map[string]*roomGrid // by lower-case room id
	empty []string             // days on which no room had a slot
}

// decodeDay decodes one day's data array.
func decodeDay(data json.RawMessage) ([]gridRoom, error) {
	var rooms []gridRoom
	if err := json.Unmarshal(data, &rooms); err != nil || rooms == nil {
		return nil, fmt.Errorf("rental_rooms_times: data is not an array of rooms")
	}
	return rooms, nil
}

// normalizeGrid merges the per-day answers (perDay[i] answers days[i]),
// rooms deduplicated by id. Any slot of an unexpected shape refuses the
// whole grid.
func normalizeGrid(days []string, perDay [][]gridRoom) (*grid, error) {
	g := &grid{rooms: map[string]*roomGrid{}}
	for i, entries := range perDay {
		day := days[i]
		slots := 0
		for _, e := range entries {
			if e.Room == nil || e.Room.ID == "" || e.Durations == nil {
				return nil, fmt.Errorf("rental_rooms_times %s: a room without id or slots", day)
			}
			id := strings.ToLower(e.Room.ID)
			r := g.rooms[id]
			if r == nil {
				r = &roomGrid{free: map[string]bool{}, busy: map[string]bool{}}
				g.rooms[id] = r
			}
			for _, s := range *e.Durations {
				if s.IsBusy == nil || s.DateTime == nil || !dateTimeRe.MatchString(*s.DateTime) || s.Time == nil || !clockRe.MatchString(*s.Time) {
					return nil, fmt.Errorf("rental_rooms_times %s: a slot is incomplete or of an unexpected form", day)
				}
				if !strings.HasPrefix(*s.DateTime, day) {
					continue // another day's slot in this day's answer
				}
				slots++
				if *s.IsBusy {
					r.busy[*s.DateTime] = true
				} else {
					r.free[*s.DateTime] = true
				}
			}
		}
		if slots == 0 {
			g.empty = append(g.empty, day)
		}
	}
	return g, nil
}

// daysBetween lists the local dates of loc from from's day to to's day,
// both included (as the backend adapter asks them).
func daysBetween(from, to time.Time, loc *time.Location) []string {
	start, end := midnight(from, loc), midnight(to, loc)
	var out []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out
}

func midnight(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// half is one half-hour of a day: [Start, End).
type half struct {
	Start, End time.Time
	Key        string // "YYYY-MM-DD HH:MM" in the grid's zone
}

// halves cuts day (a "YYYY-MM-DD" of loc) into step-minute pieces from its
// midnight to the next, as the backend adapter counts them.
func halves(day string, loc *time.Location, step int) ([]half, error) {
	d, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		return nil, err
	}
	next := time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, loc)
	var out []half
	for t := d; t.Before(next); t = t.Add(time.Duration(step) * time.Minute) {
		e := t.Add(time.Duration(step) * time.Minute)
		out = append(out, half{Start: t, End: e, Key: t.In(loc).Format(dateTimeLayout)})
	}
	return out, nil
}

// busy reports whether the half-hour key of room r is busy: anything 1C
// did not list free, or listed busy as well.
func (r *roomGrid) isBusy(key string) bool { return r.busy[key] || !r.free[key] }

// span is a stretch of busy half-hours of one room: [Start, End).
type span struct {
	Start, End time.Time
}

// busySpans returns the room's busy stretches of day, half-hours covered
// by mine left out (the gateway's own rentals, reported by their ids).
func (r *roomGrid) busySpans(day string, loc *time.Location, step int, mine []span) ([]span, error) {
	hs, err := halves(day, loc, step)
	if err != nil {
		return nil, err
	}
	var out []span
	open := false
	for _, h := range hs {
		b := r.isBusy(h.Key) && !covered(h, mine)
		switch {
		case b && open:
			out[len(out)-1].End = h.End
		case b:
			out = append(out, span{Start: h.Start, End: h.End})
			open = true
		default:
			open = false
		}
	}
	return out, nil
}

// covered reports whether mine cover all of h: a half-hour only partly
// covered (a rental moved off the grid by the club) may hold someone else
// in the rest, so it stays busy as the club's.
func covered(h half, mine []span) bool {
	at := h.Start
	for progress := true; progress && at.Before(h.End); {
		progress = false
		for _, m := range mine {
			if !m.Start.After(at) && m.End.After(at) {
				at, progress = m.End, true
			}
		}
	}
	return !at.Before(h.End)
}
