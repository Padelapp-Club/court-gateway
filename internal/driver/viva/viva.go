// Package viva is the gateway driver for VivaCRM.
//
// Everything the driver knows about VivaCRM is what an existing production
// integration already relied on: Keycloak password-grant login, list
// endpoints for exercises and breaks filtered by whole UTC days, read by id,
// create, delete and price lists. No webhooks exist, and the driver keeps no
// state other than the access token.
package viva

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

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

// NewDriver validates the driver-specific config and builds the driver.
func NewDriver(cfg driver.Config) (*Driver, error) {
	var errs []string
	add := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	p := cfg.CRM
	add(p.RejectUnknown("crm", "username", "studio", "auth_url", "api_url", "break_title"))
	username, err := p.String("crm", "username", true)
	add(err)
	studio, err := p.String("crm", "studio", true)
	add(err)
	if studio != "" && !uuidRe.MatchString(studio) {
		add(fmt.Errorf("crm.studio: «%s» — ожидается идентификатор студии Viva (UUID)", studio))
	}
	authURL, err := p.String("crm", "auth_url", false)
	add(err)
	if authURL == "" {
		authURL = DefaultAuthURL
	} else {
		add(checkURL("crm.auth_url", authURL))
	}
	apiURL, err := p.String("crm", "api_url", false)
	add(err)
	if apiURL == "" {
		apiURL = DefaultAPIURL
	} else {
		add(checkURL("crm.api_url", apiURL))
	}
	breakTitle, err := p.String("crm", "break_title", false)
	add(err)
	if breakTitle == "" {
		breakTitle = DefaultBreakTitle
	}
	if cfg.Password == "" {
		add(errors.New("CRM_PASSWORD: переменная окружения с паролем администратора Viva не задана"))
	}

	d := &Driver{
		studio:     studio,
		breakTitle: breakTitle,
		slot:       cfg.SlotMinutes,
		byRoom:     map[string]string{},
		byCourt:    map[string]court{},
		services:   map[driver.Kind]*service{},
		log:        cfg.Logger,
		now:        cfg.Now,
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if d.now == nil {
		d.now = time.Now
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
		c := court{id: cc.ID, room: strings.ToLower(room), priceList: pl}
		d.courts = append(d.courts, c)
		d.byRoom[c.room] = cc.ID
		d.byCourt[cc.ID] = c
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
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	d.c = &client{
		authURL:  authURL,
		apiURL:   strings.TrimRight(apiURL, "/"),
		username: username,
		password: cfg.Password,
		http:     hc,
		now:      d.now,
	}
	return d, nil
}

// checkURL accepts https URLs, and plain http only for loopback hosts
// (local test doubles).
func checkURL(path, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: «%s» — неверный адрес", path, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("%s: «%s» — адрес должен начинаться с https://", path, raw)
	default:
		return fmt.Errorf("%s: «%s» — адрес должен начинаться с https://", path, raw)
	}
}

// Capabilities: VivaCRM has no payment mark the gateway can set.
func (d *Driver) Capabilities() driver.Capabilities { return driver.Capabilities{Payments: false} }

// CheckLogin implements driver.LoginChecker.
func (d *Driver) CheckLogin(ctx context.Context) error {
	_, err := d.c.accessToken(ctx)
	return err
}
