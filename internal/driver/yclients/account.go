package yclients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Account is the YCLIENTS access of an installation: a partner token and a
// user (token, or login and password). Its clubs are the branches the user
// may manage; each club's branch is the club-level key crm.company_id.
type Account struct {
	c *client
}

var (
	_ driver.Account        = (*Account)(nil)
	_ driver.PasswordSetter = (*Account)(nil)
)

// NewAccount is the driver.AccountFactory for crm.type "yclients".
func NewAccount(cfg driver.Config) (driver.Account, error) {
	var errs []string
	c := connect(cfg, func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	})
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return &Account{c: c}, nil
}

// SetPassword implements driver.PasswordSetter: the next call logs in with
// it, at once.
func (a *Account) SetPassword(p string) { a.c.setPassword(p) }

// PasswordRejected implements driver.PasswordSetter.
func (a *Account) PasswordRejected() bool { r, _ := a.c.loginRefused(); return r }

// NextLogin implements driver.PasswordSetter.
func (a *Account) NextLogin() time.Time { _, t := a.c.loginRefused(); return t }

type companyItem struct {
	ID    json.Number `json:"id"`
	Title string      `json:"title"`
}

// companies lists the branches the user may manage (GET /companies?my=1,
// user-authorized). [UNVERIFIED] read from YCLIENTS' documentation, not
// yet seen in a live answer (D11 probed one known branch).
func (a *Account) companies(ctx context.Context) ([]companyItem, error) {
	r, err := a.c.do(ctx, http.MethodGet, "/companies", url.Values{"my": {"1"}}, nil, userAuth)
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /companies: HTTP %d", r.status))
	}
	var list []companyItem
	if len(r.data) == 0 || r.data[0] != '[' || json.Unmarshal(r.data, &list) != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /companies: not a list")
	}
	return list, nil
}

// CheckLogin implements driver.Account: one user-authorized read (a login
// first, without a user token), so both tokens are checked.
func (a *Account) CheckLogin(ctx context.Context) error {
	_, err := a.companies(ctx)
	return err
}

// Discover implements driver.Account: the user's branches; for the asked
// branch (or the only one) its staff as courts, its name and time zone.
// YCLIENTS' hours are per court and per day, so none are drafted.
func (a *Account) Discover(ctx context.Context, q driver.DiscoverQuery) (driver.Discovery, error) {
	var out driver.Discovery
	list, err := a.companies(ctx)
	if err != nil {
		return out, err
	}
	for _, c := range list {
		out.Studios = append(out.Studios, driver.Studio{ID: c.ID.String(), Name: strings.TrimSpace(c.Title)})
	}
	if len(list) == 0 {
		out.StudiosReason = "пользователю YCLIENTS не доступен ни один филиал"
	}
	want := strings.TrimSpace(q.Studio)
	if want == "" {
		if len(list) != 1 {
			out.Notices = append(out.Notices, driver.Notice{Code: "STUDIO_REQUIRED", Message: "пользователю доступно несколько филиалов: укажите company_id, чтобы получить черновик клуба"})
			return out, nil
		}
		want = list[0].ID.String()
	}
	if !digitsRe.MatchString(want) {
		return out, driver.Upstream(driver.UpstreamRejected, "no such branch")
	}
	d := &Driver{c: a.c, company: want, loc: time.UTC}
	co, err := d.readCompany(ctx)
	if err != nil {
		return out, err
	}
	staff, err := d.readStaff(ctx)
	if err != nil {
		return out, err
	}
	out.Studio, out.ClubName = want, strings.TrimSpace(co.Title)
	out.CRM = driver.Params{"company_id": want}
	if _, err := time.LoadLocation(co.TimezoneName); co.TimezoneName != "" && err == nil {
		out.Timezone = co.TimezoneName
	} else {
		out.Notices = append(out.Notices, driver.Notice{Code: "TIMEZONE_UNKNOWN", Message: "YCLIENTS не сообщил пояс филиала: часовой пояс подставлен по умолчанию"})
	}
	sort.SliceStable(staff, func(i, j int) bool { return strings.TrimSpace(staff[i].Name) < strings.TrimSpace(staff[j].Name) })
	for _, s := range staff {
		if (s.IsFired != nil && *s.IsFired) || truthy(s.Fired) || (s.IsDeleted != nil && *s.IsDeleted) || !digitsRe.MatchString(s.ID.String()) {
			continue
		}
		out.Courts = append(out.Courts, driver.DiscoveredCourt{Name: strings.TrimSpace(s.Name), Params: driver.Params{RoomKey: s.ID.String()}})
	}
	out.Notices = append(out.Notices,
		driver.Notice{Code: "COURTS_ARE_STAFF", Message: "корты YCLIENTS — сотрудники филиала: в черновике все сотрудники, уберите тренеров и прочих"},
		driver.Notice{Code: "HOURS_UNKNOWN", Message: "часы работы в YCLIENTS — расписание каждого корта: впишите часы клуба вручную"},
		driver.Notice{Code: "CLIENT_PHONE_REQUIRED", Message: "впишите client_phone — телефон служебного клиента, на которого шлюз ставит брони"})
	return out, nil
}
