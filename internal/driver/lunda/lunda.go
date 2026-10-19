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
// Lunda's times are the club's local times without a zone, read in the
// club's own zone (its config); Lunda's own (club/settings timeZoneId) is
// checked against it. Nothing about the club's clients is read but their
// ids, and nothing of them leaves the driver.
//
// The gateway presents itself as the admin's browser does (userAgent: Lunda
// binds a session's token to it) and, while it has a session, registers a
// WebSocket ticket and keeps the WebSocket open as the admin does
// (keepalive.go: without a ticket Lunda refused sessions ~20 s after the
// login; which of the two keeps one is not separated).
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

// CourtKey (of a court) is the Lunda court behind it (courtUid). Its
// prices are its court category's tariff, found through the court.
const CourtKey = "lunda_court"

// ServiceClientKey is the installation-level crm key holding the club's
// service client (profileUid): every booking of the gateway is made on it.
const ServiceClientKey = "service_client"

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
	uid string // Lunda courtUid
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
	own     bool // the client is the driver's own, not its account's
}

var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.Resourcer     = (*Driver)(nil)
	_ driver.ConfigChecker = (*Driver)(nil)
)

// New is the driver.Factory for crm.type "lunda".
func New(cfg driver.Config) (driver.Driver, error) { return NewDriver(cfg) }

// login reads the installation-level keys shared by the account and its
// clubs' drivers and returns how to build the client (or take the
// account's, so the installation has one session); timeout applies when
// cfg has no HTTP client. The client is built only once the whole config
// is valid (build): nothing — no saved token above all — may go anywhere
// from a constructor that returns an error.
func login(cfg driver.Config, timeout time.Duration, add func(error)) (build func() *client, service string, own bool) {
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
		return func() *client { return a.c }, service, false // a club shares its account's session
	}
	return func() *client {
		hc := &http.Client{Timeout: timeout}
		if cfg.HTTPClient != nil {
			*hc = *cfg.HTTPClient
		} else if cfg.Proxy != nil {
			hc.Transport = core.ProxyFunc(cfg.Proxy).Transport()
		}
		hc.CheckRedirect = core.RefuseRedirects // the token goes nowhere else
		u := strings.TrimRight(apiURL, "/")
		return newClient(u, wsURLOf(u), hc, cfg.Sessions, cfg.Logger, cfg.Now)
	}, service, true
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
	build, service, own := login(cfg, 20*time.Second, add)
	d := &Driver{
		own:     own,
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
		add(cc.Params.RejectUnknown(path, CourtKey))
		uid, err := cc.Params.String(path, CourtKey, true)
		add(err)
		if uid != "" && !uidRe.MatchString(uid) {
			add(fmt.Errorf("%s.%s: «%s» — ожидается идентификатор корта Лунды", path, CourtKey, uid))
		}
		if other, dup := d.byUID[uid]; dup && uid != "" {
			add(fmt.Errorf("%s.%s: корт Лунды уже привязан к корту «%s»", path, CourtKey, other))
		}
		ct := court{uid: uid}
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
	d.c = build() // valid: only now is a client (and its keep-alive) made
	return d, nil
}

// Close stops what the driver runs on its own (the keep-alive of a session
// it does not share with an account; the account's stops with
// Account.EndSession).
func (d *Driver) Close() {
	if d.own {
		_ = d.c.stopKeepAliveCtx(context.Background())
	}
}

// Resource implements driver.Resourcer: the Lunda court behind the court.
func (d *Driver) Resource(court string) string {
	return "lunda " + d.c.apiURL + " " + d.byCourt[court].uid
}

// Capabilities: the gateway never writes a payment to Lunda (a payment
// there is a line in the club's till);
// Lunda's prices are rubles.
func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Payments: false, Currency: "RUB"}
}

// serviceClientAlert is the readiness alert of an installation whose
// service client is not chosen: it reads, but does not book (bookings
// are made on the service client).
func serviceClientAlert() *driver.ConfigAlert {
	return &driver.ConfigAlert{Code: "SERVICE_CLIENT_MISSING", Problems: []string{
		"crm.service_client: служебный клиент клуба в Лунде не выбран — шлюз показывает расписание и цены, но брони в Лунду не ставит; выберите клиента при настройке шлюза (court-gateway setup)",
	}}
}

// errNoServiceClient refuses a booking without a service client: no call
// is made. It is a NOT_SUPPORTED to the partner and carries the readiness
// alert's code.
func errNoServiceClient() error {
	return fmt.Errorf("%w (%w)", serviceClientAlert(), driver.ErrBookingNotConfigured)
}

// CheckConfig implements driver.ConfigChecker: every configured court is a
// court of the session's club (a *MisconfigError otherwise). The club is
// served, with a *driver.ConfigAlert, when its zone is not as far from UTC
// as Lunda's own for the club (club/settings timeZoneId), now or on any day
// of the booking horizon (TIMEZONE_MISMATCH: Lunda's local times are read
// in the club's zone), and without a service client (SERVICE_CLIENT_MISSING:
// it reads, but does not book).
func (d *Driver) CheckConfig(ctx context.Context) error {
	list, err := courtList(ctx, d.c)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range list {
		have[c.CourtUID] = true
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
	cs, err := readClubSettings(ctx, d.c)
	if err != nil {
		return err
	}
	var alert *driver.ConfigAlert
	if zone, ok := cs.zone(); ok {
		now, _ := d.clock()
		if at, differ := driver.ZonesDiffer(d.loc, zone, now, driver.ZoneHorizon); differ {
			alert = &driver.ConfigAlert{Code: "TIMEZONE_MISMATCH", Problems: []string{fmt.Sprintf(
				"timezone: часовой пояс клуба %s с %s расходится с поясом клуба в Лунде (%s): время Лунды шлюз читает в поясе клуба — задайте клубу пояс %s",
				d.loc, at.In(d.loc).Format("2006-01-02"), zone, zone)}}
		}
	}
	if d.service == "" {
		sc := serviceClientAlert()
		if alert == nil {
			return sc
		}
		alert.Problems = append(alert.Problems, sc.Problems...)
	}
	if alert != nil {
		return alert
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
	ds, err := readSlots(ctx, d.c, localDay(now, d.loc))
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
