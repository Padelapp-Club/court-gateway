// Package viva is the gateway driver for VivaCRM: Keycloak password-grant
// login, list endpoints for exercises and breaks (selected by the studio's
// local dates), read by id, create, delete and price lists. The driver
// keeps no state other than the access token.
package viva

import (
	"cmp"
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

const (
	// DefaultAuthURL is the public VivaCRM Keycloak token endpoint.
	DefaultAuthURL = "https://kc.vivacrm.ru/realms/prod/protocol/openid-connect/token"
	// DefaultAPIURL is the public VivaCRM admin API.
	DefaultAPIURL = "https://api.vivacrm.ru/api/v1"
	// ClientID is the Keycloak client used by the VivaCRM web admin.
	ClientID = "React-auth-dev"
	// DefaultBreakTitle is the neutral title of a technical break created
	// when no service is configured for the booking kind.
	DefaultBreakTitle = "Бронь партнёра"
)

// ClubCRMKeys are the keys of the crm section that belong to one club: a
// club configured on the hub may set these and nothing else (the login and
// the endpoints stay in the installation's gateway.yaml).
var ClubCRMKeys = []string{"studio", "overnight_minutes", "break_lookback_days"}

// Defaults of the club-level occupancy keys (see Occupancy).
const (
	DefaultOvernightMinutes  = 240
	DefaultBreakLookbackDays = 31
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type service struct {
	Direction int
	Type      int
}

type court struct {
	room      string
	priceList string
}

// Driver implements driver.Driver for VivaCRM.
type Driver struct {
	c          *client
	studio     string
	loc        *time.Location // the club's zone (Viva's own days are the studio's: zone)
	overnight  time.Duration  // how far an exercise may run past midnight
	lookback   int            // days of breaks listed before the asked day
	breakTitle string
	courts     []court
	byRoom     map[string]string // room id (lower case) -> court id
	byCourt    map[string]court
	services   map[driver.Kind]*service
	log        *slog.Logger
	now        func() time.Time
}

var _ driver.Driver = (*Driver)(nil)

// New is the driver.Factory for crm.type "viva".
func New(cfg driver.Config) (driver.Driver, error) { return NewDriver(cfg) }

// login reads the installation-level keys (the CRM login) shared by the
// driver of a club and the account, and builds the client; timeout applies
// when cfg has no HTTP client.
func login(cfg driver.Config, timeout time.Duration, add func(error)) *client {
	p := cfg.CRM
	add(p.RejectUnknown("crm", append([]string{"username", "auth_url", "api_url", "break_title"}, ClubCRMKeys...)...))
	username, err := p.String("crm", "username", true)
	add(err)
	endpoint := func(key, def string) string {
		v, err := p.String("crm", key, false)
		add(err)
		if v == "" {
			return def
		}
		add(core.CheckHTTPSURL("crm."+key, v))
		return v
	}
	authURL, apiURL := endpoint("auth_url", DefaultAuthURL), endpoint("api_url", DefaultAPIURL)
	if cfg.Password == "" {
		add(errors.New("CRM_PASSWORD: переменная окружения с паролем администратора Viva не задана"))
	}
	hc := &http.Client{Timeout: timeout}
	if cfg.HTTPClient != nil {
		*hc = *cfg.HTTPClient
	} else if cfg.Proxy != nil {
		hc.Transport = core.ProxyFunc(cfg.Proxy).Transport()
	}
	hc.CheckRedirect = core.RefuseRedirects // the password and the token go nowhere else
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &client{authURL: authURL, apiURL: strings.TrimRight(apiURL, "/"), username: username, password: cfg.Password, http: hc, now: now}
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
	c := login(cfg, 20*time.Second, add)
	if a, ok := cfg.Account.(*Account); ok {
		c = a.c
	}
	p := cfg.CRM
	studio, err := p.String("crm", "studio", true)
	add(err)
	studio = strings.ToLower(studio)
	if studio != "" && !uuidRe.MatchString(studio) {
		add(fmt.Errorf("crm.studio: «%s» — ожидается идентификатор студии Viva (UUID)", studio))
	}
	breakTitle, err := p.String("crm", "break_title", false)
	add(err)
	if breakTitle == "" {
		breakTitle = DefaultBreakTitle
	}
	overnight := optInt(p, "overnight_minutes", DefaultOvernightMinutes, 0, 1440, add)
	lookback := optInt(p, "break_lookback_days", DefaultBreakLookbackDays, 0, 60, add)
	loc := cfg.Timezone
	if loc == nil {
		loc = time.UTC
	}

	d := &Driver{
		c:          c,
		studio:     studio,
		loc:        loc,
		overnight:  time.Duration(overnight) * time.Minute,
		lookback:   lookback,
		breakTitle: breakTitle,
		byRoom:     map[string]string{},
		byCourt:    map[string]court{},
		services:   map[driver.Kind]*service{},
		log:        cfg.Logger,
		now:        c.now,
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}

	if len(cfg.Courts) == 0 {
		add(errors.New("courts: нужен хотя бы один корт"))
	}
	for i, cc := range cfg.Courts {
		path := fmt.Sprintf("courts[%d]", i)
		add(cc.Params.RejectUnknown(path, "viva_room", "price_list"))
		room, err := cc.Params.String(path, "viva_room", true)
		add(err)
		if room != "" && !uuidRe.MatchString(room) {
			add(fmt.Errorf("%s.viva_room: «%s» — ожидается идентификатор комнаты Viva (UUID)", path, room))
		}
		if other, dup := d.byRoom[strings.ToLower(room)]; dup && room != "" {
			add(fmt.Errorf("%s.viva_room: комната уже привязана к корту «%s»", path, other))
		}
		pl, err := cc.Params.String(path, "price_list", false)
		add(err)
		if pl != "" && !uuidRe.MatchString(pl) {
			add(fmt.Errorf("%s.price_list: «%s» — ожидается идентификатор прайс-листа Viva (UUID)", path, pl))
		}
		ct := court{room: strings.ToLower(room), priceList: pl}
		d.courts = append(d.courts, ct)
		d.byRoom[ct.room] = cc.ID
		d.byCourt[cc.ID] = ct
	}
	for name, sp := range cfg.Services {
		path := "services." + name
		add(sp.RejectUnknown(path, "direction", "type"))
		dir, err1 := sp.Int(path, "direction")
		typ, err2 := sp.Int(path, "type")
		add(err1)
		add(err2)
		if err1 == nil && err2 == nil && (dir <= 0 || typ <= 0) {
			add(fmt.Errorf("%s: direction и type — положительные номера услуги Viva", path))
		}
		svc := &service{Direction: dir, Type: typ}
		switch name {
		case "booking":
			d.services[driver.KindGame] = svc
		case "tournament":
			d.services[driver.KindTournament] = svc
		}
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return d, nil
}

// optInt reads an optional whole number of the crm section within [min, max].
func optInt(p driver.Params, key string, def, min, max int, add func(error)) int {
	if _, ok := p[key]; !ok {
		return def
	}
	n, err := p.Int("crm", key)
	if err == nil && (n < min || n > max) {
		err = fmt.Errorf("crm.%s: %d — от %d до %d", key, n, min, max)
	}
	add(err)
	return n
}

// Resource implements driver.Resourcer: the Viva room behind the court.
func (d *Driver) Resource(court string) string {
	return "viva " + d.c.apiURL + " " + d.byCourt[court].room
}

// Studio implements driver.AccountClub.
func (d *Driver) Studio() string { return d.studio }

// studioOf finds the club's studio in the account's studio list (read
// once per evaluation, otherwise at every call: nothing of the CRM is
// kept between requests).
func (d *Driver) studioOf(ctx context.Context) (*vivaStudio, error) {
	list, err := d.c.studioList(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == d.studio {
			return &list[i], nil
		}
	}
	return nil, &driver.MisconfigError{Problems: []string{fmt.Sprintf("crm.studio: студии %s нет в учётной записи Viva", d.studio)}}
}

// zone is the studio's own zone, the one Viva reads its local days and
// price rule times in: its fixed offset from UTC as GET /studios reports
// it, or the club's zone when Viva reports none.
func (d *Driver) zone(st *vivaStudio) *time.Location {
	if st.Offset == nil {
		return d.loc
	}
	return time.FixedZone("", int(*st.Offset*3600))
}

// zoneNow reads the studio's zone for one request.
func (d *Driver) zoneNow(ctx context.Context) (*time.Location, error) {
	st, err := d.studioOf(ctx)
	if err != nil {
		var me *driver.MisconfigError
		if errors.As(err, &me) {
			return nil, driver.Upstream(driver.UpstreamRejected, "the club's studio is not in the account")
		}
		return nil, err
	}
	return d.zone(st), nil
}

// CheckConfig implements driver.ConfigChecker: the studio is in the
// account and every room is the studio's (a *MisconfigError otherwise).
// The club's time zone is what the gateway reports; the driver itself
// reads Viva's days and price times in the studio's zone, so a club zone
// that is not as far from UTC as the studio, now or on any day of the
// booking horizon, does not change what is read: it is a
// TIMEZONE_MISMATCH *driver.ConfigAlert for the operator. The account's
// studio list is shared by its clubs within one evaluation.
func (d *Driver) CheckConfig(ctx context.Context) error {
	st, err := d.studioOf(ctx)
	if err != nil {
		return err
	}
	return d.check(st)
}

func (d *Driver) check(st *vivaStudio) error {
	rooms := map[string]bool{}
	for _, r := range st.Rooms {
		rooms[strings.ToLower(r.ID)] = true
	}
	var problems []string
	for i, c := range d.courts {
		if !rooms[c.room] {
			problems = append(problems, fmt.Sprintf("courts[%d].viva_room: комната %s не из студии %s", i, c.room, d.studio))
		}
	}
	if len(problems) > 0 {
		return &driver.MisconfigError{Problems: problems}
	}
	if at, differ := driver.ZonesDiffer(d.loc, d.zone(st), d.now(), driver.ZoneHorizon); differ {
		want := cmp.Or(zoneForOffset(st.Offset), fmt.Sprintf("UTC%+g", *st.Offset))
		return &driver.ConfigAlert{Code: "TIMEZONE_MISMATCH", Problems: []string{fmt.Sprintf(
			"timezone: часовой пояс клуба %s с %s расходится со студией Viva (UTC%+g): занятость и цены шлюз читает по студии, а время партнёрам показывает в %s — задайте клубу пояс студии (%s) или зону города с тем же смещением круглый год",
			d.loc, at.In(d.loc).Format("2006-01-02"), *st.Offset, d.loc, want)}}
	}
	return nil
}

// Capabilities: VivaCRM has no payment mark the gateway can set; its
// prices are in rubles.
func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Payments: false, Currency: "RUB"}
}
