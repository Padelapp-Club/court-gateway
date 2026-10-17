// Package lunda is the gateway driver for Lunda Padel, through the API of
// its club admin (club.lundapadel.ru): the gateway calls exactly what the
// admin client calls, as a member of the club's staff who logged it in by a
// QR code or a code sent to the phone. There is no password: the session
// token (kept in the service's state directory, driver.SessionStore) is the
// only secret, and Lunda renews it by asking for it (refresh-required).
//
// A booking of a partner is an ordinary court booking on the club's own
// service client (crm.service_client, chosen at setup) with a price
// adjustment of minus its price, so it is PAID without any payment: the
// driver never writes a payment to Lunda (no add-ext-payment, ever). Its
// notes start with the ownership marker "[cg:<partner>]".
//
// Lunda's times are the club's local times without a zone; the zone is the
// club's own config. Nothing about the club's clients is read but their
// ids, and nothing of them leaves the driver.
package lunda

import (
	"context"
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

// DefaultAPIURL is the API of Lunda's club admin.
const DefaultAPIURL = "https://club.lundapadel.ru/api"

// ClubCRMKeys are the keys of the crm section that belong to one club:
// none. The session is the club's (profile/switch-club binds it to one
// Lunda club), so the club is the installation's login, not a key a club
// on the hub could set; courts name their Lunda courts by CourtKey, and
// CheckConfig refuses a court that is not in the session's club.
var ClubCRMKeys = []string{}

// CourtKey (of a court) is the Lunda court behind it (courtUid); TariffKey
// the Lunda tariff its prices come from (optional: no prices without it).
const CourtKey, TariffKey = "lunda_court", "lunda_tariff"

// ServiceClientKey is the installation-level crm key holding the club's
// service client (profileUid): every booking of the gateway is made on it.
const ServiceClientKey = "service_client"

// Version is the gateway's version as the User-Agent names it to Lunda
// (the staff member confirming a login sees it); the build may set it
// with -X.
var Version = "dev"

// uidRe is what the driver accepts as a Lunda id (a court, a client, a
// booking, a club): Lunda's are UUIDs as observed, the driver takes any
// short id of URL-safe characters so "l-"+id stays a gateway booking id.
var uidRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func init() {
	driver.Register("lunda", driver.CRM{Driver: New, Account: NewAccount, ClubCRMKeys: ClubCRMKeys, CourtKeys: []string{CourtKey}})
	driver.RegisterSetup("lunda", driver.SetupSpec{
		Name:  "Лунда",
		Needs: "сотрудник клуба с приложением Lunda Padel (вход по QR-коду или по коду из Telegram или пуша)",
		Fields: []driver.SetupField{
			// filled after the login, by the choice of a client (ClientPicker)
			{Key: ServiceClientKey, Flag: "crm-service-client"},
			{Key: "api_url", Flag: "crm-api-url"},
		},
		Interactive: true,
	})
}

type court struct {
	uid    string // Lunda courtUid
	tariff string // Lunda tariff uid, "" when none
}

// Driver implements driver.Driver for one club in Lunda.
type Driver struct {
	c       *client
	service string         // the service client's profileUid ("": no bookings)
	loc     *time.Location // the club's zone: Lunda's local times are read in it
	courts  []court
	byCourt map[string]court
	byUID   map[string]string // courtUid -> court id
	first   string            // the first configured court
	log     *slog.Logger
	clock   driver.Clock
}

var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.Resourcer     = (*Driver)(nil)
	_ driver.ConfigChecker = (*Driver)(nil)
)

// New is the driver.Factory for crm.type "lunda".
func New(cfg driver.Config) (driver.Driver, error) { return NewDriver(cfg) }

// login reads the installation-level keys shared by the account and its
// clubs' drivers, and builds the client (or takes the account's, so the
// installation has one session); timeout applies when cfg has no HTTP
// client.
func login(cfg driver.Config, timeout time.Duration, add func(error)) (*client, string) {
	p := cfg.CRM
	add(p.RejectUnknown("crm", append([]string{ServiceClientKey, "api_url"}, ClubCRMKeys...)...))
	service, err := p.String("crm", ServiceClientKey, false)
	add(err)
	if service != "" && !uidRe.MatchString(service) {
		add(fmt.Errorf("crm.%s: «%s» — ожидается идентификатор клиента Лунды", ServiceClientKey, service))
	}
	apiURL, err := p.String("crm", "api_url", false)
	add(err)
	if apiURL == "" {
		apiURL = DefaultAPIURL
	} else {
		add(core.CheckHTTPSURL("crm.api_url", apiURL))
	}
	if a, ok := cfg.Account.(*Account); ok {
		return a.c, service // a club shares its account's session
	}
	hc := &http.Client{Timeout: timeout}
	if cfg.HTTPClient != nil {
		*hc = *cfg.HTTPClient
	} else if cfg.Proxy != nil {
		hc.Transport = core.ProxyFunc(cfg.Proxy).Transport()
	}
	hc.CheckRedirect = core.RefuseRedirects // the token goes nowhere else
	return newClient(strings.TrimRight(apiURL, "/"), hc, cfg.Sessions, cfg.Logger, cfg.Now), service
}

// NewDriver validates the driver-specific config and builds the driver.
// With the installation's account in cfg it shares the account's session.
func NewDriver(cfg driver.Config) (*Driver, error) {
	var errs []string
	add := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	c, service := login(cfg, 20*time.Second, add)
	d := &Driver{
		c:       c,
		service: service,
		loc:     cfg.Timezone,
		byCourt: map[string]court{},
		byUID:   map[string]string{},
		log:     cfg.Logger,
		clock:   cfg.DecisionClock(),
	}
	if d.loc == nil {
		d.loc = time.UTC
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if len(cfg.Courts) == 0 {
		add(errors.New("courts: нужен хотя бы один корт"))
	}
	for i, cc := range cfg.Courts {
		path := fmt.Sprintf("courts[%d]", i)
		add(cc.Params.RejectUnknown(path, CourtKey, TariffKey))
		uid, err := cc.Params.String(path, CourtKey, true)
		add(err)
		if uid != "" && !uidRe.MatchString(uid) {
			add(fmt.Errorf("%s.%s: «%s» — ожидается идентификатор корта Лунды", path, CourtKey, uid))
		}
		if other, dup := d.byUID[uid]; dup && uid != "" {
			add(fmt.Errorf("%s.%s: корт Лунды уже привязан к корту «%s»", path, CourtKey, other))
		}
		tariff, err := cc.Params.String(path, TariffKey, false)
		add(err)
		if tariff != "" && !uidRe.MatchString(tariff) {
			add(fmt.Errorf("%s.%s: «%s» — ожидается идентификатор тарифа Лунды", path, TariffKey, tariff))
		}
		ct := court{uid: uid, tariff: tariff}
		d.courts = append(d.courts, ct)
		d.byCourt[cc.ID] = ct
		d.byUID[uid] = cc.ID
		if i == 0 {
			d.first = cc.ID
		}
	}
	if len(cfg.Services) > 0 {
		add(errors.New("services: у Лунды нет услуг для броней — уберите раздел services (бронь партнёра — обычная бронь корта)"))
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return d, nil
}

// Resource implements driver.Resourcer: the Lunda court behind the court.
func (d *Driver) Resource(court string) string {
	return "lunda " + d.c.apiURL + " " + d.byCourt[court].uid
}

// Capabilities: the gateway never writes a payment to Lunda (REQ-394);
// Lunda's prices are rubles.
func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Payments: false, Currency: "RUB"}
}

// serviceClientAlert is the readiness alert of an installation whose
// service client is not chosen: it reads, but does not book (REQ-396).
func serviceClientAlert() *driver.ConfigAlert {
	return &driver.ConfigAlert{Code: "SERVICE_CLIENT_MISSING", Problems: []string{
		"crm.service_client: служебный клиент клуба в Лунде не выбран — шлюз показывает расписание и цены, но брони в Лунду не ставит; выберите клиента при настройке шлюза (court-gateway setup)",
	}}
}

// errNoServiceClient refuses a booking without a service client: no call
// is made. It is a NOT_SUPPORTED to the partner and carries the readiness
// alert's code.
func errNoServiceClient() error {
	return fmt.Errorf("%w (%w)", serviceClientAlert(), driver.ErrNotSupported)
}

// CheckConfig implements driver.ConfigChecker: every configured court is a
// court of the session's club (a *MisconfigError otherwise); without a
// service client the club is served for reading, with a
// SERVICE_CLIENT_MISSING *driver.ConfigAlert.
func (d *Driver) CheckConfig(ctx context.Context) error {
	list, err := courtList(ctx, d.c)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range list {
		have[c.uid()] = true
	}
	var problems []string
	for i, c := range d.courts {
		if !have[c.uid] {
			problems = append(problems, fmt.Sprintf("courts[%d].%s: корта %s нет в клубе Лунды, в который выполнен вход", i, CourtKey, c.uid))
		}
	}
	if len(problems) > 0 {
		return &driver.MisconfigError{Problems: problems}
	}
	if d.service == "" {
		return serviceClientAlert()
	}
	return nil
}

// Ready implements driver.Driver: the session works (profile/current, the
// check the admin makes after a login), the config matches the club
// (CheckConfig) and today's schedule reads as occupancy would read it. A
// config alert is returned only when the reads pass.
func (d *Driver) Ready(ctx context.Context) error {
	if err := profileCurrent(ctx, d.c); err != nil {
		return err
	}
	alert := d.CheckConfig(ctx)
	var ca *driver.ConfigAlert
	if alert != nil && !errors.As(alert, &ca) {
		return alert
	}
	now, _ := d.clock()
	ds, err := d.readDay(ctx, localDay(now, d.loc))
	if err != nil {
		return err
	}
	if _, err := d.dayIntervals(ds, map[string]string{d.byCourt[d.first].uid: d.first}); err != nil {
		return err
	}
	return alert
}

// localDay is local midnight of t in loc.
func localDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}
