package fitness1c

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Account is the installation's login to a club's 1C instance: the clubs
// of the instance's external system are the account's clubs, each a
// club_id (crm.studio).
type Account struct {
	c *client
}

var (
	_ driver.Account        = (*Account)(nil)
	_ driver.PasswordSetter = (*Account)(nil)
)

// NewAccount is the driver.AccountFactory for crm.type "fitness1c".
func NewAccount(cfg driver.Config) (driver.Account, error) {
	var errs []string
	c := login(cfg, 30*time.Second, func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	})
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return &Account{c: c}, nil
}

// SetPassword implements driver.PasswordSetter: the pause after a refused
// login ends at once.
func (a *Account) SetPassword(p string) { a.c.setPassword(p) }

// PasswordRejected implements driver.PasswordSetter.
func (a *Account) PasswordRejected() bool { _, stop := a.c.stopped(); return stop }

// NextLogin implements driver.PasswordSetter.
func (a *Account) NextLogin() time.Time {
	if until, stop := a.c.stopped(); stop {
		return until
	}
	return time.Time{}
}

// CheckLogin implements driver.Account: one cheap call (GET /clubs/) with
// the login and the key.
func (a *Account) CheckLogin(ctx context.Context) error {
	_, err := listClubs(ctx, a.c)
	return err
}

// discoverDays is how many days of the grid the hours are drafted from.
const discoverDays = 7

// Discover implements driver.Account: the clubs of the external system;
// for the asked one (or the only one) its rooms as courts, its zone, and
// hours drafted from a week of its grid (1C's work_time is not the grid's,
// D14 §15.1). The services a booking can be made by are named in notices.
func (a *Account) Discover(ctx context.Context, q driver.DiscoverQuery) (driver.Discovery, error) {
	var out driver.Discovery
	list, err := listClubs(ctx, a.c)
	if err != nil {
		return out, err
	}
	for _, cl := range list {
		out.Studios = append(out.Studios, driver.Studio{ID: strings.ToLower(cl.ID), Name: cl.Title})
	}
	if len(list) == 0 {
		out.StudiosReason = "у внешней системы в 1С не сопоставлено ни одного клуба"
	}
	want := strings.ToLower(strings.TrimSpace(q.Studio))
	if want == "" {
		if len(list) != 1 {
			out.Notices = append(out.Notices, driver.Notice{Code: "STUDIO_REQUIRED", Message: "у внешней системы несколько клубов: укажите studio, чтобы получить черновик клуба"})
			return out, nil
		}
		want = strings.ToLower(list[0].ID)
	}
	var cl *club1C
	for i := range list {
		if strings.EqualFold(list[i].ID, want) {
			cl = &list[i]
		}
	}
	if cl == nil {
		return out, driver.Upstream(driver.UpstreamRejected, "no such club in 1C's external system")
	}
	out.Studio, out.ClubName = want, cl.Title
	out.CRM = driver.Params{"studio": want}
	loc, err := zoneOf(*cl)
	if err != nil {
		return out, err
	}
	out.Timezone = loc.String()
	services, err := listServices(ctx, a.c, want)
	if err != nil {
		return out, err
	}
	svc, err := pickGridService(services, "")
	if err != nil {
		out.Notices = append(out.Notices, driver.Notice{Code: "GRID_UNSUPPORTED", Message: err.Error()})
		return out, nil
	}
	rooms, err := listRooms(ctx, a.c, want, svc.id())
	if err != nil {
		return out, err
	}
	sort.SliceStable(rooms, func(i, j int) bool { return strings.TrimSpace(rooms[i].Title) < strings.TrimSpace(rooms[j].Title) })
	for _, r := range rooms {
		if uuidRe.MatchString(r.ID) {
			out.Courts = append(out.Courts, driver.DiscoveredCourt{Name: strings.TrimSpace(r.Title), Params: driver.Params{RoomKey: strings.ToLower(r.ID)}})
		}
	}
	if len(out.Courts) == 0 {
		out.Notices = append(out.Notices, driver.Notice{Code: "NO_COURTS_FOUND", Message: "у клуба в 1С нет помещений аренды: добавьте корты вручную"})
	}
	var free []string
	for _, s := range services {
		if s.PaymentRequired != nil && !*s.PaymentRequired && s.step() == seance {
			free = append(free, fmt.Sprintf("«%s» (%s)", s.Title, s.id()))
		}
	}
	if len(free) == 0 {
		out.Notices = append(out.Notices, driver.Notice{Code: "NO_BOOKING_SERVICE", Message: "в 1С нет услуги аренды без основания оплаты (цена 0, шаг 30 мин): пока клуб не заведёт «Аренда корта PadelApp», шлюз только читает занятость"})
	} else {
		out.Notices = append(out.Notices, driver.Notice{Code: "BOOKING_SERVICE", Message: "услуга для броней партнёров — services.booking.service: " + strings.Join(free, ", ")})
	}
	d := &Driver{c: a.c, studio: want, log: a.c.log}
	start := midnight(a.c.now(), loc).AddDate(0, 0, 1)
	days := daysBetween(start, start.AddDate(0, 0, discoverDays-1), loc)
	g, err := d.readGrid(ctx, svc, loc, days)
	if err != nil {
		out.Notices = append(out.Notices, driver.Notice{Code: "HOURS_UNKNOWN", Message: "сетку 1С прочитать не удалось: часы работы впишите вручную"})
		return out, nil
	}
	out.Hours = hoursOf(g, days, loc)
	out.Notices = append(out.Notices, driver.Notice{Code: "HOURS_FROM_GRID", Message: "часы собраны по сетке 1С на ближайшую неделю — проверьте"})
	return out, nil
}

// hoursOf drafts the hours of every weekday from the grid: from the
// earliest to the end of the latest half-hour 1C listed on any room.
func hoursOf(g *grid, days []string, loc *time.Location) [7]*[2]int {
	var out [7]*[2]int
	for _, day := range days {
		t, err := time.ParseInLocation("2006-01-02", day, loc)
		if err != nil {
			continue
		}
		wd := (int(t.Weekday()) + 6) % 7
		for _, r := range g.rooms {
			for _, set := range []map[string]bool{r.free, r.busy} {
				for key := range set {
					if !strings.HasPrefix(key, day) {
						continue
					}
					k, err := time.ParseInLocation(dateTimeLayout, key, loc)
					if err != nil {
						continue
					}
					m := k.Hour()*60 + k.Minute()
					if out[wd] == nil {
						out[wd] = &[2]int{m, m + seance}
					}
					out[wd][0], out[wd][1] = min(out[wd][0], m), max(out[wd][1], m+seance)
				}
			}
		}
	}
	return out
}
