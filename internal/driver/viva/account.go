package viva

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Account is a VivaCRM administrator account. Clubs of a network are
// studios of the account; the studio of each club is the club-level key
// crm.studio.
type Account struct {
	c *client
}

var _ driver.Account = (*Account)(nil)

// NewAccount is the driver.AccountFactory for crm.type "viva". It reads
// the installation-level keys only; crm.studio, when present, is ignored
// here (it belongs to a club).
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

// CheckLogin implements driver.Account: it logs in if needed and makes
// one cheap call (GET /studios), so a cached token the CRM no longer
// accepts, or an unreachable API, is noticed even while no club is
// configured.
func (a *Account) CheckLogin(ctx context.Context) error {
	_, err := listStudios(ctx, a.c)
	return err
}

// Discover implements driver.Account. The studios of the account are
// its clubs: every studio is listed (a studio added to the account later
// shows up here by itself), and for the asked studio (or the only one)
// the draft has every room as a court, the hours and time zone of the
// studio and its price lists.
func (a *Account) Discover(ctx context.Context, q driver.DiscoverQuery) (driver.Discovery, error) {
	var out driver.Discovery
	list, err := listStudios(ctx, a.c)
	if err != nil {
		return out, err
	}
	for _, st := range list {
		out.Studios = append(out.Studios, driver.Studio{ID: st.ID, Name: st.Name})
	}
	if len(list) == 0 {
		out.StudiosReason = "в учётной записи Viva нет ни одной студии"
	}
	want := strings.ToLower(strings.TrimSpace(q.Studio))
	if want == "" {
		if len(list) != 1 {
			out.Notices = append(out.Notices, driver.Notice{Code: "STUDIO_REQUIRED", Message: "в учётной записи несколько студий: укажите studio, чтобы получить черновик клуба"})
			return out, nil
		}
		want = list[0].ID
	}
	var st *vivaStudio
	for i := range list {
		if list[i].ID == want {
			st = &list[i]
		}
	}
	if st == nil {
		return out, driver.Upstream(driver.UpstreamRejected, "no such studio in the account", nil)
	}
	out.Studio, out.ClubName = st.ID, st.Name
	out.CRM = driver.Params{"studio": st.ID}
	if out.Timezone = zoneForOffset(st.Offset); out.Timezone == "" {
		out.Notices = append(out.Notices, driver.Notice{Code: "TIMEZONE_UNKNOWN", Message: "Viva не сообщила целое смещение студии от UTC: часовой пояс подставлен по умолчанию"})
	}
	pl, err := readPriceLists(ctx, a.c, st.ID)
	if err != nil {
		return out, err
	}
	rooms := append([]vivaRoom(nil), st.Rooms...)
	sort.SliceStable(rooms, func(i, j int) bool {
		return naturalLess(strings.TrimSpace(rooms[i].Name), strings.TrimSpace(rooms[j].Name))
	})
	for _, r := range rooms {
		if !uuidRe.MatchString(r.ID) {
			continue
		}
		c := driver.DiscoveredCourt{Name: strings.TrimSpace(r.Name), Params: driver.Params{"viva_room": strings.ToLower(r.ID)}}
		if len(pl.lists) == 1 {
			c.Params["price_list"] = pl.lists[0].ID
		}
		out.Courts = append(out.Courts, c)
	}
	if len(out.Courts) == 0 {
		out.Notices = append(out.Notices, driver.Notice{Code: "NO_COURTS_FOUND", Message: "у студии нет комнат: добавьте корты вручную"})
	}
	out.PriceLists = pl.lists
	if hours, notices, ok := studioHours(st.WorkTime); ok {
		out.Hours = hours
		out.Notices = append(out.Notices, notices...)
	} else {
		out.Hours = pl.hours
		out.Notices = append(out.Notices, driver.Notice{Code: "HOURS_FROM_PRICE_LISTS", Message: "у студии нет режима работы: часы собраны по правилам прайс-листов — проверьте"})
	}
	switch {
	case len(pl.lists) == 0:
		out.Notices = append(out.Notices, driver.Notice{Code: "NO_PRICE_LISTS", Message: "в студии нет прайс-листов: цены партнёрам будут недоступны"})
	case len(pl.lists) > 1:
		out.Notices = append(out.Notices, driver.Notice{Code: "PRICE_LIST_AMBIGUOUS",
			Message: fmt.Sprintf("в студии %d прайс-листов: впишите нужный каждому корту в price_list", len(pl.lists))})
	}
	if pl.incomplete {
		out.Notices = append(out.Notices, driver.Notice{Code: "PRICE_LISTS_INCOMPLETE",
			Message: fmt.Sprintf("в студии больше прайс-листов, чем шлюз читает за раз (%d): список неполный, недостающие впишите вручную", len(pl.lists))})
	}
	if pl.failed > 0 {
		out.Notices = append(out.Notices, driver.Notice{Code: "PRICE_LIST_UNREADABLE",
			Message: fmt.Sprintf("не удалось разобрать прайс-листов: %d", pl.failed)})
	}
	return out, nil
}
