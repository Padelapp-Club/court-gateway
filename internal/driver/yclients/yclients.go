// Package yclients is the gateway driver for YCLIENTS (api.yclients.com):
// occupancy from the branch's records and the courts' schedules, bookings
// through book_record, cancellation and the payment mark on the record.
// It is the D11 adapter of the PadelApp backend moved into the gateway,
// with the same rules; the driver keeps no CRM data, only the user token
// of a login.
//
// A court is a YCLIENTS staff member (the way clubs keep courts in
// YCLIENTS: one "staff" per court, its schedule the court's hours). A club
// is a branch (company). Every request carries the partner token
// ("Authorization: Bearer <partner>"); the branch's methods also a user
// token (", User <user>"). The driver does not care whose keys they are:
// PadelApp's marketplace keys (one partner and one user token for every
// branch that connected the app) or a club's own (its partner registration
// and a user of its staff, by token or by login and password).
package yclients

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

const (
	// DefaultAPIURL is the public YCLIENTS API.
	DefaultAPIURL = "https://api.yclients.com/api/v1"
	// PartnerTokenEnv holds the partner (developer) token: every request
	// needs it.
	PartnerTokenEnv = "YCLIENTS_PARTNER_TOKEN"
	// UserTokenEnv holds a user token. Without it the driver logs in with
	// crm.login and CRM_PASSWORD (POST /auth) and keeps the token it gets.
	UserTokenEnv = "YCLIENTS_USER_TOKEN"
	// DefaultClientName names the client of the gateway's records.
	DefaultClientName = "PadelApp"
)

// ClubCRMKeys are the keys of the crm section that belong to one club: the
// branch and the client the club's bookings are made for. The login and
// the endpoint stay in the installation's gateway.yaml.
var ClubCRMKeys = []string{"company_id", "client_phone", "client_name", "client_email"}

// StudioKey (of the crm section) and RoomKey (of a court) hold the CRM
// resources a club takes: two clubs never share one (driver.CRM, setup.go).
const StudioKey, RoomKey = "company_id", "staff_id"

var digitsRe = regexp.MustCompile(`^[0-9]{1,18}$`)

// Driver implements driver.Driver for one YCLIENTS branch.
type Driver struct {
	c       *client
	company string
	loc     *time.Location // the club's zone: schedule times are read in it
	client  bookingClient
	staff   map[string]string // staff id -> court id
	byCourt map[string]string // court id -> staff id
	courts  []string          // court ids in config order
	log     *slog.Logger
	clock   driver.Clock
}

// bookingClient is who the gateway's records are booked for: the club's
// (or PadelApp's) service client, never a player.
type bookingClient struct {
	phone string // digits only
	name  string
	email string
}

var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.Resourcer     = (*Driver)(nil)
	_ driver.ConfigChecker = (*Driver)(nil)
	_ driver.AccountClub   = (*Driver)(nil)
)

// New is the driver.Factory for crm.type "yclients".
func New(cfg driver.Config) (driver.Driver, error) { return NewDriver(cfg) }

// connect reads the installation-level keys and secrets shared by a club's
// driver and the account and builds the client; with the installation's
// account in cfg a club shares its client (one token, one request queue).
func connect(cfg driver.Config, add func(error)) *client {
	p := cfg.CRM
	add(p.RejectUnknown("crm", append([]string{"api_url", "login"}, ClubCRMKeys...)...))
	apiURL, err := p.String("crm", "api_url", false)
	add(err)
	if apiURL == "" {
		apiURL = DefaultAPIURL
	} else {
		add(core.CheckHTTPSURL("crm.api_url", apiURL))
	}
	login, err := p.String("crm", "login", false)
	add(err)
	sec := cfg.Secrets
	partner := strings.TrimSpace(sec[PartnerTokenEnv])
	user := strings.TrimSpace(sec[UserTokenEnv])
	if partner == "" {
		add(fmt.Errorf("%s: переменная окружения с партнёрским токеном YCLIENTS не задана", PartnerTokenEnv))
	}
	if user == "" && (login == "" || cfg.Password == "") {
		add(fmt.Errorf("%s: нет пользовательского токена YCLIENTS — задайте его или вход сотрудника: crm.login и пароль в CRM_PASSWORD", UserTokenEnv))
	}
	if a, ok := cfg.Account.(*Account); ok {
		return a.c
	}
	hc := &http.Client{Timeout: requestTimeout}
	if cfg.HTTPClient != nil {
		*hc = *cfg.HTTPClient
	} else if cfg.Proxy != nil {
		hc.Transport = core.ProxyFunc(cfg.Proxy).Transport()
	}
	hc.CheckRedirect = core.RefuseRedirects // the tokens go nowhere else
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return newClient(strings.TrimRight(apiURL, "/"), partner, user, login, cfg.Password, hc, now, log)
}

// NewDriver validates the club's config and builds the driver.
func NewDriver(cfg driver.Config) (*Driver, error) {
	var errs []string
	add := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	c := connect(cfg, add)
	p := cfg.CRM
	company, err := idParam(p, "crm", "company_id")
	add(err)
	if company != "" && !digitsRe.MatchString(company) {
		add(fmt.Errorf("crm.company_id: «%s» — ожидается номер филиала YCLIENTS (цифры)", company))
	}
	phone, err := p.String("crm", "client_phone", true)
	add(err)
	digits := onlyDigits(phone)
	if phone != "" && (len(digits) < 10 || len(digits) > 15) {
		add(errors.New("crm.client_phone: телефон клиента для броней шлюза — от 10 до 15 цифр"))
	}
	name, err := p.String("crm", "client_name", false)
	add(err)
	if name == "" {
		name = DefaultClientName
	}
	email, err := p.String("crm", "client_email", false)
	add(err)
	loc := cfg.Timezone
	if loc == nil {
		loc = time.UTC
	}
	d := &Driver{
		c: c, company: company, loc: loc,
		client:  bookingClient{phone: digits, name: name, email: email},
		staff:   map[string]string{},
		byCourt: map[string]string{},
		log:     cfg.Logger,
		clock:   cfg.DecisionClock(),
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if len(cfg.Courts) == 0 {
		add(errors.New("courts: нужен хотя бы один корт"))
	}
	for i, cc := range cfg.Courts {
		path := fmt.Sprintf("courts[%d]", i)
		add(cc.Params.RejectUnknown(path, RoomKey))
		staff, err := idParam(cc.Params, path, RoomKey)
		add(err)
		if staff != "" && !digitsRe.MatchString(staff) {
			add(fmt.Errorf("%s.%s: «%s» — ожидается номер сотрудника-корта YCLIENTS (цифры)", path, RoomKey, staff))
		}
		if other, dup := d.staff[staff]; dup && staff != "" {
			add(fmt.Errorf("%s.%s: сотрудник %s уже привязан к корту «%s»", path, RoomKey, staff, other))
		}
		d.staff[staff] = cc.ID
		d.byCourt[cc.ID] = staff
		d.courts = append(d.courts, cc.ID)
	}
	for name, sp := range cfg.Services {
		// YCLIENTS books services the court offers for the booked length
		// (book_services): nothing to configure per kind
		add(sp.RejectUnknown("services." + name))
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return d, nil
}

// idParam reads a required YCLIENTS number, written as a string or, as
// YAML and JSON decode a bare number, an integer.
func idParam(p driver.Params, path, key string) (string, error) {
	switch v := p[key].(type) {
	case int, int64, uint64:
		n, err := p.Int(path, key)
		if err != nil {
			return "", err
		}
		return fmt.Sprint(n), nil
	case float64: // JSON
		if v != float64(int64(v)) || v < 0 || v > 1e15 {
			return "", fmt.Errorf("%s.%s: должно быть целое число", path, key)
		}
		return fmt.Sprint(int64(v)), nil
	}
	return p.String(path, key, true)
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Resource implements driver.Resourcer: the staff member behind the court.
func (d *Driver) Resource(court string) string {
	return "yclients " + d.c.apiURL + " " + d.company + " " + d.byCourt[court]
}

// Studio implements driver.AccountClub: the club's branch.
func (d *Driver) Studio() string { return d.company }

// Capabilities: the payment mark goes into the record's comment (D11);
// no prices (see PriceTable); the CRM's currency is the ruble.
func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Payments: true, NoPrices: true, Currency: "RUB"}
}
