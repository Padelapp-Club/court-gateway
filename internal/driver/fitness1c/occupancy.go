package fitness1c

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Booking ids: "f-<rental id>" is a rental the gateway made (1C's own id);
// "x-<room>-<start>-<end>" is a busy stretch of the grid, which carries no
// id (1C does not say what occupies it), start and end in UTC as
// YYYYMMDDHHMM.
const (
	ownPrefix     = "f-"
	foreignPrefix = "x-"
	stampLayout   = "200601021504"
)

func ownID(rental string) string { return ownPrefix + strings.ToLower(rental) }

func parseOwnID(id string) (string, bool) {
	r, ok := strings.CutPrefix(id, ownPrefix)
	return strings.ToLower(r), ok && uuidRe.MatchString(r)
}

func foreignID(room string, s span) string {
	return foreignPrefix + room + "-" + s.Start.UTC().Format(stampLayout) + "-" + s.End.UTC().Format(stampLayout)
}

func parseForeignID(id string) (room string, s span, ok bool) {
	rest, found := strings.CutPrefix(id, foreignPrefix)
	if !found || len(rest) != 36+1+12+1+12 {
		return "", span{}, false
	}
	room = strings.ToLower(rest[:36])
	start, err1 := time.Parse(stampLayout, rest[37:49])
	end, err2 := time.Parse(stampLayout, rest[50:])
	if rest[36] != '-' || rest[49] != '-' || !uuidRe.MatchString(room) || err1 != nil || err2 != nil || !end.After(start) {
		return "", span{}, false
	}
	return room, span{Start: start, End: end}, true
}

// readGrid reads the grid of days by the service svc, one request per day
// (an answer for N days grows as N², D14 §15.1), dayConcurrency at a time.
// Any day that fails fails the whole grid: a partial grid would read the
// missing days as free.
func (d *Driver) readGrid(ctx context.Context, svc rentalService, loc *time.Location, days []string) (*grid, error) {
	perDay := make([][]gridRoom, len(days))
	errs := make([]error, len(days))
	var wg sync.WaitGroup
	next := make(chan int)
	for range min(dayConcurrency, len(days)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				day, _ := time.ParseInLocation("2006-01-02", days[i], loc)
				q := url.Values{
					"club_id":          {d.studio},
					"service_id":       {svc.id()},
					"show_unavailable": {"true"},
					"start_date":       {days[i] + " 00:00"},
					"end_date":         {day.AddDate(0, 0, 1).Format("2006-01-02") + " 00:00"},
				}
				env, err := d.c.call(ctx, http.MethodGet, "/rental_rooms_times/", q, nil, "", false)
				if err == nil {
					perDay[i], err = decodeDay(env.Data)
					if err != nil {
						err = driver.Upstream(driver.UpstreamInvalid, err.Error())
					}
				}
				errs[i] = err
			}
		}()
	}
	for i := range days {
		if ctx.Err() != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, e := range errs {
		if e != nil {
			return nil, e // the first failed day's, classified
		}
	}
	g, err := normalizeGrid(days, perDay)
	if err != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, err.Error())
	}
	for _, day := range g.empty {
		// the club is closed that day, or 1C answered nothing: the whole
		// day is busy, the safe side
		d.log.Warn("1C: no slot in any room on a day", "day", day)
	}
	return g, nil
}

// appointment is an element of GET /appointments/ (the technical client's
// appointments). Nothing personal is decoded.
type appointment struct {
	ClubID  string `json:"club_id"`
	ID      string `json:"appointment_id"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Service *struct {
		ID string `json:"id"`
	} `json:"service"`
	Room *struct {
		ID string `json:"id"`
	} `json:"room"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
}

// rental is one of the gateway's rentals as read back.
type rental struct {
	id, room string
	span
	started bool // 1C reports it under way
}

// appointmentsPage is 1C's page size limit for /appointments/.
const appointmentsPage = 30

// maxAppointmentPages bounds one listing: more is not read, and the list
// counts as incomplete.
const maxAppointmentPages = 40

// ownRentals lists the technical client's planned and running rentals of
// the gateway's services in this club. complete is false when 1C reports
// more appointments than were read. The appointment id of a rental is its
// rental id (observed: GET /rental/ answers both equal, D14 probe).
func (d *Driver) ownRentals(ctx context.Context, loc *time.Location) (out []rental, complete bool, err error) {
	if d.c.phone == "" || len(d.services) == 0 {
		return nil, true, nil // the gateway makes no rentals here
	}
	return d.rentalsOf(ctx, loc, gatewayClient, d.ours)
}

// anyService is rentalsOf's keep of a client whose every rental counts
// (the mirror client).
var anyService func(service string) bool

// rentalsOf lists the planned and running rentals of a technical client in
// this club whose service keep takes (anyService: every one), as
// ownRentals does.
func (d *Driver) rentalsOf(ctx context.Context, loc *time.Location, who techClient, keep func(service string) bool) (out []rental, complete bool, err error) {
	seen := map[string]bool{}
	read, total := 0, -1
	err = d.c.as(ctx, who, func(tok string) error {
		out, seen, read, total = nil, map[string]bool{}, 0, -1
		for page := 0; page < maxAppointmentPages; page++ {
			q := url.Values{
				"type":             {"rental"},
				"statuses":         {`["planned","passes"]`},
				"requested_offset": {strconv.Itoa(read)},
				"page_size":        {strconv.Itoa(appointmentsPage)},
			}
			var list []appointment
			env, err := d.c.get(ctx, "/appointments/", q, tok, &list)
			if err != nil {
				return err
			}
			if env.Pagination != nil && env.Pagination.Total != nil {
				total = *env.Pagination.Total
			}
			read += len(list)
			for _, a := range list {
				r, kept, err := d.ownRental(a, loc, keep)
				if err != nil {
					return err
				}
				if kept && !seen[r.id] {
					seen[r.id] = true
					out = append(out, r)
				}
			}
			if len(list) < appointmentsPage || (total >= 0 && read >= total) {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	// without a total, a short last page is all there is
	complete = total < 0 || read >= total
	if !complete {
		d.log.Warn("1C: not every appointment of the technical client was read", "read", read, "total", total)
	}
	return out, complete, nil
}

// ownRental validates one appointment and keeps it when it is a rental of
// a gateway service in this club's configured rooms.
func (d *Driver) ownRental(a appointment, loc *time.Location, keep func(service string) bool) (rental, bool, error) {
	if a.Type != "" && a.Type != "rental" {
		return rental{}, false, nil
	}
	if a.ClubID != "" && !strings.EqualFold(a.ClubID, d.studio) {
		return rental{}, false, nil // another club of the instance
	}
	if keep != nil && (a.Service == nil || !keep(a.Service.ID)) {
		return rental{}, false, nil
	}
	bad := func(what string) (rental, bool, error) {
		return rental{}, false, driver.Upstream(driver.UpstreamInvalid, "GET /appointments/: a rental "+what)
	}
	if !uuidRe.MatchString(a.ID) {
		return bad("has no valid id")
	}
	if a.Room == nil || !uuidRe.MatchString(a.Room.ID) {
		return bad("has no room")
	}
	s, ok := parseSpan(a.StartDate, a.EndDate, loc)
	if !ok {
		return bad("has no valid start_date/end_date")
	}
	switch a.Status {
	case "planned", "passes":
	default:
		return rental{}, false, nil
	}
	return rental{id: strings.ToLower(a.ID), room: strings.ToLower(a.Room.ID), span: s, started: a.Status == "passes"}, true, nil
}

// parseSpan reads 1C's "YYYY-MM-DD HH:MM" pair in the club's zone.
func parseSpan(from, to string, loc *time.Location) (span, bool) {
	s, err1 := time.ParseInLocation(dateTimeLayout, from, loc)
	e, err2 := time.ParseInLocation(dateTimeLayout, to, loc)
	if err1 != nil || err2 != nil || !e.After(s) {
		return span{}, false
	}
	return span{Start: s, End: e}, true
}

// Occupancy implements driver.Driver. Every day of the range is read from
// the grid; a configured room missing from it fails the call (a missing
// room is not a free one). The gateway's own rentals are reported by their
// ids, everything else busy as stretches.
func (d *Driver) Occupancy(ctx context.Context, q driver.OccupancyQuery) (driver.Occupancy, error) {
	rooms := map[string]string{}
	for _, cid := range q.Courts {
		c, ok := d.byCourt[cid]
		if !ok {
			return driver.Occupancy{}, fmt.Errorf("court %q: %w", cid, driver.ErrNotFound)
		}
		rooms[c.room] = cid
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return driver.Occupancy{}, err
	}
	svc, err := d.gridService(ctx)
	if err != nil {
		return driver.Occupancy{}, err
	}
	days := daysBetween(q.From, q.To, loc)
	g, err := d.readGrid(ctx, svc, loc, days)
	if err != nil {
		return driver.Occupancy{}, err
	}
	own, complete, err := d.ownRentals(ctx, loc)
	if err != nil {
		return driver.Occupancy{}, err
	}
	_, known := d.clock()
	occ := driver.Occupancy{Complete: known && complete, Intervals: []driver.Interval{}}
	ivs, err := d.intervals(g, rooms, days, loc, svc.step(), own)
	if err != nil {
		return driver.Occupancy{}, err
	}
	for _, iv := range ivs {
		if iv.Start.Before(q.To) && iv.End.After(q.From) {
			occ.Intervals = append(occ.Intervals, iv)
		}
	}
	return occ, nil
}

// intervals turns the grid of days and the own rentals into intervals of
// rooms (room id -> court).
func (d *Driver) intervals(g *grid, rooms map[string]string, days []string, loc *time.Location, step int, own []rental) ([]driver.Interval, error) {
	var out []driver.Interval
	for room, courtID := range rooms {
		rg := g.rooms[room]
		if rg == nil {
			return nil, driver.Upstream(driver.UpstreamInvalid, fmt.Sprintf("rental_rooms_times: the room of court %s is not in the grid", courtID))
		}
		var mine []span
		for _, r := range own {
			if r.room == room {
				mine = append(mine, r.span)
				out = append(out, driver.Interval{ID: ownID(r.id), Court: courtID, Start: r.Start, End: r.End})
			}
		}
		for _, day := range days {
			spans, err := rg.busySpans(day, loc, step, mine)
			if err != nil {
				return nil, err
			}
			for _, s := range spans {
				out = append(out, driver.Interval{ID: foreignID(room, s), Court: courtID, Start: s.Start, End: s.End})
			}
		}
	}
	return out, nil
}

// rental1C is GET /rental/: the parts the driver reads.
type rental1C struct {
	RentalID  string `json:"rental_id"`
	Canceled  *bool  `json:"canceled"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	Room      *struct {
		ID string `json:"id"`
	} `json:"room"`
	Service *struct {
		ID string `json:"id"`
	} `json:"service"`
}

// readRental reads one rental (no client token needed). 1C's refusals are
// returned as they are and never become "not found": a booking reported
// gone is cancelled by the partner with a refund.
func (d *Driver) readRental(ctx context.Context, id string, loc *time.Location) (rental1C, span, error) {
	var r rental1C
	if _, err := d.c.get(ctx, "/rental/", url.Values{"rental_id": {id}}, "", &r); err != nil {
		return r, span{}, err
	}
	bad := func(what string) (rental1C, span, error) {
		return r, span{}, driver.Upstream(driver.UpstreamInvalid, "GET /rental/: "+what)
	}
	if r.RentalID != "" && !strings.EqualFold(r.RentalID, id) {
		return bad("another rental's answer")
	}
	if r.Canceled == nil || r.Room == nil || r.Room.ID == "" {
		return bad("the rental is of an unexpected form")
	}
	s, ok := parseSpan(r.StartDate, r.EndDate, loc)
	if !ok {
		return bad("the rental has no valid start_date/end_date")
	}
	return r, s, nil
}

// Get implements driver.Driver.
func (d *Driver) Get(ctx context.Context, id string) (driver.Booking, error) {
	if room, s, ok := parseForeignID(id); ok {
		return d.getForeign(ctx, id, room, s)
	}
	rid, ok := parseOwnID(id)
	if !ok {
		return driver.Booking{}, driver.ErrNotFound
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return driver.Booking{}, err
	}
	r, s, err := d.readRental(ctx, rid, loc)
	if err != nil {
		return driver.Booking{}, err
	}
	courtID, ok := d.byRoom[strings.ToLower(r.Room.ID)]
	if !ok {
		// the club may have moved the gateway's rental to another room and
		// service: still not gone
		return driver.Booking{}, driver.ErrCourtNotConfigured
	}
	status := driver.StatusActive
	if *r.Canceled {
		status = driver.StatusCancelled
	}
	return driver.Booking{ID: ownID(rid), Court: courtID, Start: s.Start, End: s.End, Status: status}, nil
}

// getForeign: a busy stretch is read again; it is the booking while the
// grid still shows exactly that stretch.
func (d *Driver) getForeign(ctx context.Context, id, room string, s span) (driver.Booking, error) {
	courtID, ok := d.byRoom[room]
	if !ok {
		return driver.Booking{}, driver.ErrNotFound
	}
	occ, err := d.Occupancy(ctx, driver.OccupancyQuery{Courts: []string{courtID}, From: s.Start, To: s.End})
	if err != nil {
		return driver.Booking{}, err
	}
	for _, iv := range occ.Intervals {
		if iv.ID == id {
			return driver.Booking{ID: id, Court: courtID, Start: iv.Start, End: iv.End, Status: driver.StatusActive}, nil
		}
	}
	return driver.Booking{}, driver.ErrNotFound
}
