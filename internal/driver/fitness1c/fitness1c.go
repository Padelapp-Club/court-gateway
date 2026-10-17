// Package fitness1c is the gateway driver for 1C:Fitness club (the client
// API v3 of the club's own 1C instance): occupancy from the rental grid,
// bookings as rentals of a free service of the club ("Аренда корта
// PadelApp") made by a technical client, prices of a rental service. It is
// the backend's adapter (D14, fitness1c-crm.service.ts) moved into the
// gateway with the same rules; the driver keeps no state other than the
// technical client's token.
package fitness1c

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// APIKeyEnv holds the application key of the external system in the
// club's 1C (the card of the external system «PadelApp»: «ключ
// приложения»). The club's instance issues it, so it is the club's.
const APIKeyEnv = "FITNESS1C_APIKEY"

// ClubCRMKeys are the keys of the crm section that belong to one club: the
// club's id in 1C and, optionally, the rental service its grid is read by
// and the one its prices come from.
var ClubCRMKeys = []string{"studio", "occupancy_service", "price_service"}

// StudioKey (of the crm section) and RoomKey (of a court) hold the CRM
// resources a club takes (drivers.ResourceKeys).
const StudioKey, RoomKey = "studio", "fitness1c_room"

// installKeys are the installation-level keys of the crm section.
var installKeys = []string{"base_url", "username", "client_phone", "partner"}

// seance is the only grid step proven by a run (D14 SUPPORTED_SEANCE_LENGTH_MIN):
// a club whose smallest rental service has another step is not served.
const seance = 30

// dayConcurrency is how many days of the grid are asked at once.
const dayConcurrency = 5

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	phoneRe = regexp.MustCompile(`^\+?[0-9]{10,15}$`)
)

func init() {
	driver.Register("fitness1c", driver.CRM{Driver: New, Account: NewAccount, ClubCRMKeys: ClubCRMKeys,
		StudioKeys: []string{StudioKey}, CourtKeys: []string{RoomKey}})
	driver.RegisterSetup("fitness1c", driver.SetupSpec{
		Name:  "1С:Фитнес клуб",
		Needs: "адрес 1С клуба, пользователь и пароль 1С для внешнего приложения, ключ приложения из карточки внешней системы",
		Fields: []driver.SetupField{
			{Key: "base_url", Flag: "crm-base-url", Prompt: "Адрес 1С клуба (как в карточке внешней системы, без /hs/api/v3)", Required: true},
			{Key: "username", Flag: "crm-username", Env: "CRM_USERNAME", Prompt: "Пользователь 1С (роли «Удаленный доступ внешнее приложение» и «Запуск внешнего соединения»)", Required: true},
			{Key: "client_phone", Flag: "crm-client-phone", Prompt: "Телефон технического клиента в 1С, на которого ставятся брони партнёра (пусто — только чтение)"},
			{Key: "partner", Flag: "crm-partner", Prompt: "Партнёр хаба, чьи брони ставит этот клиент (например, padelapp)"},
		},
		Secrets: []driver.SetupSecret{
			{Env: "CRM_PASSWORD", Prompt: "Пароль пользователя 1С", Required: true},
			{Env: APIKeyEnv, Prompt: "Ключ приложения (карточка внешней системы в 1С клуба)", Required: true},
		},
	})
}

type court struct {
	room string // lower-case room id
}

// Driver implements driver.Driver for 1C:Fitness.
type Driver struct {
	c                *client
	studio           string // the club's id in 1C (club_id)
	occupancyService string // "" = the rental service with the smallest step
	priceService     string // "" = the occupancy service
	loc              *time.Location
	courts           []court
	byRoom           map[string]string // room id -> court id
	byCourt          map[string]court
	services         map[driver.Kind]string // rental service of our bookings
	log              *slog.Logger
	clock            driver.Clock
}

var _ driver.Driver = (*Driver)(nil)

// New is the driver.Factory for crm.type "fitness1c".
func New(cfg driver.Config) (driver.Driver, error) { return NewDriver(cfg) }

// login reads the installation-level keys and builds the client; a club
// with the installation's account in cfg shares the account's client.
func login(cfg driver.Config, timeout time.Duration, add func(error)) *client {
	p := cfg.CRM
	add(p.RejectUnknown("crm", append(append([]string(nil), installKeys...), ClubCRMKeys...)...))
	base, err := p.String("crm", "base_url", true)
	add(err)
	if base != "" {
		add(core.CheckHTTPSURL("crm.base_url", base))
	}
	base = strings.TrimSuffix(strings.TrimRight(base, "/"), apiPrefix)
	username, err := p.String("crm", "username", true)
	add(err)
	phone, err := p.String("crm", "client_phone", false)
	add(err)
	phone = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(phone)
	if phone != "" && !phoneRe.MatchString(phone) {
		add(errors.New("crm.client_phone: ожидается номер телефона технического клиента 1С, например +79990000000"))
	}
	partner, err := p.String("crm", "partner", false)
	add(err)
	if (phone == "") != (partner == "") {
		add(errors.New("crm.client_phone и crm.partner задаются вместе: телефон технического клиента 1С и партнёр хаба, чьи брони он ставит"))
	}
	if cfg.Password == "" {
		add(errors.New("CRM_PASSWORD: переменная окружения с паролем пользователя 1С не задана"))
	}
	apiKey := strings.TrimSpace(cfg.Secrets[APIKeyEnv])
	if apiKey == "" {
		add(errors.New(APIKeyEnv + ": переменная окружения с ключом приложения 1С не задана"))
	}
	if a, ok := cfg.Account.(*Account); ok {
		return a.c
	}
	hc := &http.Client{Timeout: timeout}
	if cfg.HTTPClient != nil {
		*hc = *cfg.HTTPClient
	} else if cfg.Proxy != nil {
		hc.Transport = core.ProxyFunc(cfg.Proxy).Transport()
	}
	hc.CheckRedirect = core.RefuseRedirects // Basic and the key go nowhere else
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &client{
		base: base + apiPrefix, username: username, password: cfg.Password, apiKey: apiKey,
		phone: phone, partner: partner, http: hc,
		log: cmp.Or(cfg.Logger, slog.New(slog.DiscardHandler)), now: now,
	}
}

// NewDriver validates the driver-specific config and builds the driver.
func NewDriver(cfg driver.Config) (*Driver, error) {
	var errs []string
	add := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	// 15 s per request, as the backend adapter: bounds the worst snapshot
	c := login(cfg, 15*time.Second, add)
	p := cfg.CRM
	id := func(key string, required bool) string {
		v, err := p.String("crm", key, required)
		add(err)
		if v != "" && !uuidRe.MatchString(v) {
			add(fmt.Errorf("crm.%s: «%s» — ожидается идентификатор 1С (UUID)", key, v))
		}
		return strings.ToLower(v)
	}
	d := &Driver{
		c:                c,
		studio:           id("studio", true),
		occupancyService: id("occupancy_service", false),
		priceService:     id("price_service", false),
		loc:              cmp.Or(cfg.Timezone, time.UTC),
		byRoom:           map[string]string{},
		byCourt:          map[string]court{},
		services:         map[driver.Kind]string{},
		log:              cmp.Or(cfg.Logger, slog.New(slog.DiscardHandler)),
		clock:            cfg.DecisionClock(),
	}
	if len(cfg.Courts) == 0 {
		add(errors.New("courts: нужен хотя бы один корт"))
	}
	for i, cc := range cfg.Courts {
		path := fmt.Sprintf("courts[%d]", i)
		add(cc.Params.RejectUnknown(path, RoomKey))
		room, err := cc.Params.String(path, RoomKey, true)
		add(err)
		room = strings.ToLower(room)
		if room != "" && !uuidRe.MatchString(room) {
			add(fmt.Errorf("%s.%s: «%s» — ожидается идентификатор помещения 1С (UUID)", path, RoomKey, room))
		}
		if other, dup := d.byRoom[room]; dup && room != "" {
			add(fmt.Errorf("%s.%s: помещение уже привязано к корту «%s»", path, RoomKey, other))
		}
		d.courts = append(d.courts, court{room: room})
		d.byRoom[room] = cc.ID
		d.byCourt[cc.ID] = court{room: room}
	}
	for name, sp := range cfg.Services {
		path := "services." + name
		add(sp.RejectUnknown(path, "service"))
		svc, err := sp.String(path, "service", true)
		add(err)
		if svc != "" && !uuidRe.MatchString(svc) {
			add(fmt.Errorf("%s.service: «%s» — ожидается идентификатор услуги аренды 1С (UUID)", path, svc))
		}
		switch name {
		case "booking":
			d.services[driver.KindGame] = strings.ToLower(svc)
		case "tournament":
			d.services[driver.KindTournament] = strings.ToLower(svc)
		default:
			add(fmt.Errorf("%s: неизвестная услуга (допустимы booking, tournament)", path))
		}
	}
	// a tournament is booked as a game unless it has its own service
	if _, ok := d.services[driver.KindTournament]; !ok && d.services[driver.KindGame] != "" {
		d.services[driver.KindTournament] = d.services[driver.KindGame]
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return d, nil
}

// Resource implements driver.Resourcer: the 1C room behind the court.
func (d *Driver) Resource(court string) string {
	return "fitness1c " + d.c.base + " " + d.byCourt[court].room
}

// Studio implements driver.AccountClub.
func (d *Driver) Studio() string { return d.studio }

// Capabilities: no payment mark (our rentals are of a free service: there
// is nothing to pay in 1C); prices are in rubles.
func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Payments: false, Currency: "RUB"}
}

// ours reports whether a rental service is one the gateway books by.
func (d *Driver) ours(service string) bool {
	service = strings.ToLower(service)
	for _, s := range d.services {
		if s != "" && s == service {
			return true
		}
	}
	return false
}

// club1C is an element of GET /clubs/.
type club1C struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	TimeZone *string `json:"time_zone"`
}

func listClubs(ctx context.Context, c *client) ([]club1C, error) {
	var list []club1C
	if _, err := c.get(ctx, "/clubs/", nil, "", &list); err != nil {
		return nil, err
	}
	return list, nil
}

// clubZone finds the club in GET /clubs/ and its zone: 1C's time_zone is
// the zone of every time 1C writes for the club (the backend adapter's
// rule: older than the club's zone in our config).
func (d *Driver) clubZone(ctx context.Context) (*time.Location, error) {
	list, err := listClubs(ctx, d.c)
	if err != nil {
		return nil, err
	}
	for _, cl := range list {
		if strings.EqualFold(cl.ID, d.studio) {
			return zoneOf(cl)
		}
	}
	return nil, &driver.MisconfigError{Problems: []string{fmt.Sprintf("crm.studio: клуба %s нет среди клубов внешней системы в 1С", d.studio)}}
}

func zoneOf(cl club1C) (*time.Location, error) {
	if cl.TimeZone == nil || *cl.TimeZone == "" {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /clubs/: the club has no time_zone")
	}
	loc, err := time.LoadLocation(*cl.TimeZone)
	if err != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /clubs/: the club's time_zone is not a zone")
	}
	return loc, nil
}

// zoneNow is clubZone for a request: a club missing from 1C is the CRM's
// refusal there, not a config error.
func (d *Driver) zoneNow(ctx context.Context) (*time.Location, error) {
	loc, err := d.clubZone(ctx)
	var me *driver.MisconfigError
	if errors.As(err, &me) {
		return nil, driver.Upstream(driver.UpstreamRejected, "the club is not among 1C's clubs of the external system")
	}
	return loc, err
}

// rentalService is an element of GET /rental_services/.
type rentalService struct {
	ID              any    `json:"id"`
	Title           string `json:"title"`
	SeanceLength    any    `json:"seance_length"`
	Price           any    `json:"price"` // a string in 1C's answers
	PaymentRequired *bool  `json:"payment_required"`
}

func (s rentalService) id() string { v, _ := s.ID.(string); return strings.ToLower(v) }

// step is the service's seance length in minutes, 0 when it is not a
// positive whole number.
func (s rentalService) step() int {
	n, ok := s.SeanceLength.(float64)
	if !ok || n <= 0 || n != float64(int(n)) {
		return 0
	}
	return int(n)
}

func listServices(ctx context.Context, c *client, studio string) ([]rentalService, error) {
	var list []rentalService
	if _, err := c.get(ctx, "/rental_services/", url.Values{"club_id": {studio}}, "", &list); err != nil {
		return nil, err
	}
	return list, nil
}

// pickGridService chooses the service the grid is read by: the configured
// one or the one with the smallest step. Every service must have an id and
// a step; the smallest step must be the proven one, and a configured
// service may not be coarser (its grid would block a free half-hour next
// to a busy one). The backend adapter's resolveOccupancyService.
func pickGridService(list []rentalService, preferred string) (rentalService, error) {
	if len(list) == 0 {
		return rentalService{}, errors.New("у клуба в 1С нет услуг аренды")
	}
	for _, s := range list {
		if s.id() == "" || s.step() == 0 {
			return rentalService{}, errors.New("у услуг аренды в 1С нет действительных id или seance_length")
		}
	}
	minimal := list[0]
	for _, s := range list[1:] {
		if s.step() < minimal.step() {
			minimal = s
		}
	}
	if minimal.step() != seance {
		return rentalService{}, fmt.Errorf("наименьший шаг сетки клуба в 1С — %d мин, поддержан только %d: клуб не обслуживается, пока шаг не проверен прогоном", minimal.step(), seance)
	}
	if preferred == "" {
		return minimal, nil
	}
	for _, s := range list {
		if s.id() == preferred {
			if s.step() != minimal.step() {
				return rentalService{}, fmt.Errorf("crm.occupancy_service: услуга %s (%d мин) крупнее наименьшего шага (%d мин): сетка читалась бы неверно", preferred, s.step(), minimal.step())
			}
			return s, nil
		}
	}
	return rentalService{}, fmt.Errorf("crm.occupancy_service: услуги %s нет среди услуг аренды клуба в 1С", preferred)
}

// gridService reads the services and picks the grid's for a request.
func (d *Driver) gridService(ctx context.Context) (rentalService, error) {
	list, err := listServices(ctx, d.c, d.studio)
	if err != nil {
		return rentalService{}, err
	}
	s, err := pickGridService(list, d.occupancyService)
	if err != nil {
		return rentalService{}, driver.Upstream(driver.UpstreamInvalid, "rental services: "+err.Error())
	}
	return s, nil
}
