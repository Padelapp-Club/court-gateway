// Package viva is the gateway driver for VivaCRM: Keycloak password-grant
// login, list endpoints for exercises and breaks (selected by the studio's
// local dates), read by id, create, delete and price lists. The driver
// keeps no state other than the access token.
package viva

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
	DefaultBreakLookbackDays = 7
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type service struct {
	Direction int
	Type      int
}

type court struct {
	id        string
	room      string
	priceList string
}

// Driver implements driver.Driver for VivaCRM.
type Driver struct {
	c          *client
	studio     string
	loc        *time.Location // the studio's zone: lists select by its dates
	overnight  time.Duration  // how far an exercise may run past midnight
	lookback   int            // days of breaks listed before the asked day
	breakTitle string
	slot       int
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
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &client{authURL: authURL, apiURL: strings.TrimRight(apiURL, "/"), username: username, password: cfg.Password, http: hc, now: now}
}

// NewDriver validates the driver-specific config and builds the driver.
func NewDriver(cfg driver.Config) (*Driver, error) {
	var errs []string
	add := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	c := login(cfg, 20*time.Second, add)
	p := cfg.CRM
	studio, err := p.String("crm", "studio", true)
	add(err)
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
		slot:       cfg.SlotMinutes,
		byRoom:     map[string]string{},
		byCourt:    map[string]court{},
		services:   map[driver.Kind]*service{},
		log:        cfg.Logger,
		now:        c.now,
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if d.slot <= 0 {
		d.slot = 30
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
		ct := court{id: cc.ID, room: strings.ToLower(room), priceList: pl}
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

// Capabilities: VivaCRM has no payment mark the gateway can set.
func (d *Driver) Capabilities() driver.Capabilities { return driver.Capabilities{Payments: false} }

// CheckLogin implements driver.LoginChecker.
func (d *Driver) CheckLogin(ctx context.Context) error {
	_, err := d.c.accessToken(ctx)
	return err
}
