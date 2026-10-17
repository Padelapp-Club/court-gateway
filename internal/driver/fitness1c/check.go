package fitness1c

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// room1C is an element of GET /rental_rooms/.
type room1C struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func listRooms(ctx context.Context, c *client, studio, service string) ([]room1C, error) {
	q := url.Values{"club_id": {studio}}
	if service != "" {
		q.Set("service_id", service)
	}
	var list []room1C
	if _, err := c.get(ctx, "/rental_rooms/", q, "", &list); err != nil {
		return nil, err
	}
	return list, nil
}

// CheckConfig implements driver.ConfigChecker: the club is among 1C's
// clubs of the external system with a valid zone, its rental services give
// a grid of the proven step, the booking services exist and need no
// payment basis (otherwise 1C makes a 15-minute reservation, not a
// booking), and every room is the club's. A club zone that is not as far
// from UTC as 1C's (1C's is the one the driver reads and writes times in)
// is a TIMEZONE_MISMATCH alert.
func (d *Driver) CheckConfig(ctx context.Context) error {
	loc, err := d.clubZone(ctx)
	if err != nil {
		return err
	}
	list, err := listServices(ctx, d.c, d.studio)
	if err != nil {
		return err
	}
	var problems []string
	grid, err := pickGridService(list, d.occupancyService)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if _, err := d.priceServiceOf(list); err != nil && d.priceService != "" {
		problems = append(problems, err.Error())
	}
	byID := map[string]rentalService{}
	for _, s := range list {
		byID[s.id()] = s
	}
	for _, k := range []driver.Kind{driver.KindGame, driver.KindTournament} {
		svc := d.services[k]
		if svc == "" {
			continue
		}
		s, ok := byID[svc]
		name := map[driver.Kind]string{driver.KindGame: "booking", driver.KindTournament: "tournament"}[k]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("services.%s.service: услуги %s нет среди услуг аренды клуба в 1С", name, svc))
		case s.PaymentRequired == nil || *s.PaymentRequired:
			problems = append(problems, fmt.Sprintf("services.%s.service: услуга «%s» требует основания оплаты — 1С сделала бы 15-минутный резерв, а не бронь; нужна услуга аренды с ценой 0 без основания", name, s.Title))
		case !free(s.Price):
			problems = append(problems, fmt.Sprintf("services.%s.service: у услуги «%s» цена не 0 — в 1С по брони партнёра остался бы долг; нужна услуга аренды с ценой 0", name, s.Title))
		case s.step() != seance:
			problems = append(problems, fmt.Sprintf("services.%s.service: шаг услуги «%s» — %d мин, поддержан только %d", name, s.Title, s.step(), seance))
		}
	}
	rooms, err := listRooms(ctx, d.c, d.studio, grid.id())
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, r := range rooms {
		have[strings.ToLower(r.ID)] = true
	}
	for i, c := range d.courts {
		if !have[c.room] {
			problems = append(problems, fmt.Sprintf("courts[%d].%s: помещения %s нет среди помещений аренды клуба в 1С", i, RoomKey, c.room))
		}
	}
	if len(problems) > 0 {
		return &driver.MisconfigError{Problems: problems}
	}
	now, _ := d.clock()
	if at, differ := driver.ZonesDiffer(d.loc, loc, now, driver.ZoneHorizon); differ {
		return &driver.ConfigAlert{Code: "TIMEZONE_MISMATCH", Problems: []string{fmt.Sprintf(
			"timezone: часовой пояс клуба %s с %s расходится с поясом клуба в 1С (%s): занятость и брони шлюз читает и пишет по 1С, а время партнёрам показывает в %s — задайте клубу пояс %s",
			d.loc, at.In(d.loc).Format("2006-01-02"), loc, d.loc, loc)}}
	}
	return nil
}

// Ready implements driver.Driver: the config matches 1C (CheckConfig) and
// today's grid reads, every configured room in it. A config alert is
// returned only when the reads pass.
func (d *Driver) Ready(ctx context.Context) error {
	alert := d.CheckConfig(ctx)
	var ca *driver.ConfigAlert
	if alert != nil && !errors.As(alert, &ca) {
		return alert
	}
	now, _ := d.clock()
	courts := make([]string, 0, len(d.courts))
	for _, c := range d.courts {
		courts = append(courts, d.byRoom[c.room])
	}
	if _, err := d.Occupancy(ctx, driver.OccupancyQuery{Courts: courts, From: now, To: now.Add(1)}); err != nil {
		return err
	}
	return alert
}

// free reports whether a service's price is zero.
func free(price any) bool {
	k, ok := kopecks(price)
	return ok && k == 0
}
