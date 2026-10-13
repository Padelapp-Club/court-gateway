// Package setup builds a first gateway.yaml and .env for a club: it reads
// the club's CRM (courts from the coming four weeks of bookings, price
// lists, opening hours from the price rules), redeems the join code at the
// hub and writes both files. Every value it could only guess is marked
// «проверьте» in the file.
package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Input is everything setup needs. Password and JoinCode are secrets: they
// are never printed or logged.
type Input struct {
	ConfigPath  string
	Force       bool
	HubURL      string
	HubCAFile   string
	JoinCode    string
	ClubID      string // optional: must match the club the join code is for
	ClubName    string // optional: overrides the name from the hub
	CRMType     string // viva (default) or demo
	Username    string
	Password    string
	Studio      string
	AuthURL     string // optional CRM endpoint overrides (test doubles)
	APIURL      string
	Timezone    string
	SlotMinutes int
}

// Deps are injectable for tests.
type Deps struct {
	CRMClient    *http.Client
	HubTransport http.RoundTripper
	Now          func() time.Time
	Version      string
}

// Warning is something the administrator must look at.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Court is one court written to the draft.
type Court struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Room      string `json:"crm_room,omitempty"`
	Bookings  int    `json:"bookings_28d"`
	PriceList string `json:"price_list,omitempty"`
}

// Result is the machine-readable outcome (`setup --json`). It never holds
// a secret.
type Result struct {
	OK         bool              `json:"ok"`
	Error      *Problem          `json:"error,omitempty"`
	Config     string            `json:"config,omitempty"`
	EnvFile    string            `json:"env_file,omitempty"`
	Club       *ClubRef          `json:"club,omitempty"`
	Hub        string            `json:"hub,omitempty"`
	Courts     []Court           `json:"courts,omitempty"`
	PriceLists int               `json:"price_lists"`
	Hours      map[string]string `json:"hours,omitempty"`
	Warnings   []Warning         `json:"warnings"`
}

// ClubRef names the club.
type ClubRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Problem is a failure with a stable code.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p *Problem) Error() string { return p.Code + ": " + p.Message }

func problem(code, format string, a ...any) *Problem {
	return &Problem{Code: code, Message: fmt.Sprintf(format, a...)}
}

// Missing lists required inputs that are empty, as flag names.
func (in Input) Missing() []string {
	var out []string
	if in.HubURL == "" {
		out = append(out, "--hub-url")
	}
	if in.JoinCode == "" {
		out = append(out, "--join-code (или GATEWAY_JOIN_CODE)")
	}
	if in.crmType() == "viva" {
		if in.Username == "" {
			out = append(out, "--crm-username")
		}
		if in.Password == "" {
			out = append(out, "CRM_PASSWORD (или --password-stdin)")
		}
		if in.Studio == "" {
			out = append(out, "--crm-studio")
		}
	}
	return out
}

func (in Input) crmType() string {
	if in.CRMType == "" {
		return "viva"
	}
	return in.CRMType
}

// Run performs setup. The returned Result is always filled; err is a
// *Problem when setup failed.
func Run(ctx context.Context, in Input, deps Deps) (Result, error) {
	res := Result{Warnings: []Warning{}}
	fail := func(p *Problem) (Result, error) {
		res.OK, res.Error = false, p
		return res, p
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if in.Timezone == "" {
		in.Timezone = "Europe/Moscow"
	}
	if in.SlotMinutes == 0 {
		in.SlotMinutes = 30
	}
	if m := in.Missing(); len(m) > 0 {
		return fail(problem("MISSING_INPUT", "не хватает: %s", strings.Join(m, ", ")))
	}
	if err := core.CheckHTTPSURL("--hub-url", in.HubURL); err != nil {
		return fail(problem("INVALID_INPUT", "%s", err.Error()))
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return fail(problem("INVALID_INPUT", "неизвестный часовой пояс «%s»", in.Timezone))
	}
	if in.SlotMinutes < 5 || in.SlotMinutes > 240 || 1440%in.SlotMinutes != 0 {
		return fail(problem("INVALID_INPUT", "шаг сетки %d мин: от 5 до 240, сутки должны делиться без остатка", in.SlotMinutes))
	}
	if in.ConfigPath == "" {
		in.ConfigPath = "gateway.yaml"
	}
	cfgPath, err := filepath.Abs(in.ConfigPath)
	if err != nil {
		return fail(problem("INVALID_INPUT", "путь к конфигурации: %v", err))
	}
	envPath := filepath.Join(filepath.Dir(cfgPath), core.EnvFileName)
	if !in.Force {
		for _, p := range []string{cfgPath, envPath} {
			if _, err := os.Stat(p); err == nil {
				return fail(problem("CONFIG_EXISTS", "файл %s уже есть; чтобы перезаписать, добавьте --force", p))
			}
		}
	}

	// 1. the CRM
	draft := &draft{Timezone: in.Timezone, Slot: in.SlotMinutes, HubURL: strings.TrimRight(in.HubURL, "/"), HubCA: in.HubCAFile, Now: deps.Now()}
	switch in.crmType() {
	case "viva":
		if p := discoverViva(ctx, in, deps, draft); p != nil {
			return fail(p)
		}
	case "demo":
		draft.CRMType = "demo"
		draft.Courts = []Court{{ID: "court-1", Name: "Корт 1"}, {ID: "court-2", Name: "Корт 2"}}
		draft.Hours = defaultHours()
		draft.warn("DEMO_CRM", "учебный тип CRM demo: брони хранятся в памяти до перезапуска; для клуба выберите viva")
	default:
		return fail(problem("INVALID_INPUT", "тип CRM «%s» не поддерживается командой setup (viva или demo)", in.CRMType))
	}

	// 2. render with a placeholder secret and check that the result loads,
	// before the join code is spent
	clubID := in.ClubID
	if clubID == "" {
		clubID = "pending-club"
	}
	draft.ClubID, draft.ClubName = clubID, firstNonEmpty(in.ClubName, clubID)
	if p := validate(draft, in.Password); p != nil {
		return fail(p)
	}

	// 3. the hub
	rt := deps.HubTransport
	if rt == nil {
		roots, err := tunnel.LoadRoots(in.HubCAFile)
		if err != nil {
			return fail(problem("INVALID_INPUT", "%v", err))
		}
		rt = tunnel.NewTransport(roots)
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	reg, err := tunnel.Register(rctx, rt, draft.HubURL, in.JoinCode, deps.Version)
	cancel()
	switch {
	case errors.Is(err, tunnel.ErrJoinCodeRejected):
		return fail(problem("JOIN_CODE_REJECTED", "хаб не принял код подключения: он неверный, уже использован или истёк — попросите новый"))
	case err != nil:
		return fail(problem("HUB_UNREACHABLE", "хаб недоступен: %v", err))
	}
	if in.ClubID != "" && in.ClubID != reg.Club {
		return fail(problem("CLUB_MISMATCH", "код подключения выдан для клуба %s, а указан %s; секрет не сохранён — попросите новый код", reg.Club, in.ClubID))
	}
	draft.ClubID, draft.ClubName = reg.Club, firstNonEmpty(in.ClubName, reg.ClubName, reg.Club)

	// 4. write
	cfgText := draft.render()
	if _, err := core.Load([]byte(cfgText), envFor(draft, in.Password, reg.Secret)); err != nil {
		return fail(problem("INTERNAL", "собранная конфигурация не прошла проверку: %v", err))
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fail(problem("WRITE_FAILED", "%v", err))
	}
	if err := core.WriteFileAtomic(envPath, []byte(renderEnv(in.Password, reg.Secret, draft.CRMType)), 0o600); err != nil {
		return fail(problem("WRITE_FAILED", "не удалось записать %s: %v", envPath, err))
	}
	if err := core.WriteFileAtomic(cfgPath, []byte(cfgText), 0o644); err != nil {
		return fail(problem("WRITE_FAILED", "не удалось записать %s: %v", cfgPath, err))
	}
	res.OK = true
	res.Config, res.EnvFile, res.Hub = cfgPath, envPath, draft.HubURL
	res.Club = &ClubRef{ID: draft.ClubID, Name: draft.ClubName}
	res.Courts = draft.Courts
	res.PriceLists = len(draft.PriceLists)
	res.Hours = draft.hoursMap()
	res.Warnings = append(res.Warnings, draft.Warnings...)
	return res, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func discoverViva(ctx context.Context, in Input, deps Deps, d *draft) *Problem {
	d.CRMType, d.Username, d.Studio, d.AuthURL, d.APIURL = "viva", in.Username, strings.ToLower(in.Studio), in.AuthURL, in.APIURL
	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	disc, err := viva.Discover(dctx, viva.DiscoverConfig{
		AuthURL: in.AuthURL, APIURL: in.APIURL, Username: in.Username, Password: in.Password,
		Studio: in.Studio, HTTPClient: deps.CRMClient, Now: deps.Now,
	})
	if err != nil {
		var ue *driver.UpstreamError
		switch {
		case errors.As(err, &ue) && ue.Kind == driver.UpstreamAuth:
			return problem("CRM_AUTH_FAILED", "Viva не приняла логин или пароль администратора")
		case errors.As(err, &ue) && ue.Kind == driver.UpstreamInvalid:
			return problem("CRM_INVALID_RESPONSE", "Viva ответила в непонятном формате")
		case errors.As(err, &ue) && ue.Kind == driver.UpstreamRejected:
			return problem("CRM_REJECTED", "Viva отклонила запрос — проверьте идентификатор студии")
		default:
			return problem("CRM_UNREACHABLE", "Viva недоступна, попробуйте позже")
		}
	}
	if len(disc.Rooms) == 0 {
		return problem("NO_COURTS_FOUND", "за 28 дней вперёд в студии нет ни одной брони, поэтому корты не найдены — проверьте идентификатор студии или впишите корты в gateway.yaml вручную")
	}
	for i, r := range disc.Rooms {
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("Корт %d", i+1)
		}
		d.Courts = append(d.Courts, Court{ID: fmt.Sprintf("court-%d", i+1), Name: name, Room: r.ID, Bookings: r.Bookings})
	}
	d.warn("COURTS_FROM_BOOKINGS", "корты найдены по броням за 28 дней вперёд: корт, на котором броней не было, в черновик не попал — добавьте его вручную")
	d.PriceLists = disc.PriceLists
	switch len(disc.PriceLists) {
	case 0:
		d.warn("NO_PRICE_LISTS", "в студии нет прайс-листов: цены партнёрам будут недоступны")
	case 1:
		for i := range d.Courts {
			d.Courts[i].PriceList = disc.PriceLists[0].ID
		}
	default:
		d.warn("PRICE_LIST_AMBIGUOUS", fmt.Sprintf("в студии %d прайс-листов: впишите нужный каждому корту в price_list (список — в комментарии в gateway.yaml)", len(disc.PriceLists)))
	}
	if len(disc.PriceListErrors) > 0 {
		d.warn("PRICE_LIST_UNREADABLE", fmt.Sprintf("не удалось разобрать прайс-листов: %d — часы работы по ним не учтены", len(disc.PriceListErrors)))
	}
	d.Hours = disc.Hours
	empty := true
	for _, h := range d.Hours {
		if h != nil {
			empty = false
		}
	}
	if empty {
		d.Hours = defaultHours()
		d.warn("HOURS_DEFAULTED", "часы работы не удалось вывести из прайс-листов: подставлено 07:00–23:00 ежедневно — проверьте")
	} else if d.roundHours() {
		d.warn("HOURS_ROUNDED", fmt.Sprintf("часы работы округлены до сетки %d мин — проверьте", d.Slot))
	}
	return nil
}

func defaultHours() [7]*[2]int {
	var h [7]*[2]int
	for i := range h {
		h[i] = &[2]int{7 * 60, 23 * 60}
	}
	return h
}

// envFor is the environment a written config will see.
func envFor(d *draft, password, secret string) func(string) string {
	return func(k string) string {
		switch k {
		case core.PasswordEnv:
			return password
		case core.DefaultHubSecretEnv:
			return secret
		}
		return ""
	}
}

func validate(d *draft, password string) *Problem {
	if password == "" {
		password = "placeholder"
	}
	s, err := core.Load([]byte(d.render()), envFor(d, password, "placeholder-secret"))
	if err != nil {
		return problem("INTERNAL", "собранная конфигурация не прошла проверку: %v", err)
	}
	if d.CRMType == "viva" {
		if _, err := viva.New(s.Driver); err != nil {
			return problem("INTERNAL", "собранная конфигурация не прошла проверку драйвера: %v", err)
		}
	}
	return nil
}
