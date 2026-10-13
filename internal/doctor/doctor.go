// Package doctor runs the gateway's self-diagnosis: one line per check,
// each with a stable code an agent can act on, and nothing secret in any
// message.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/remote"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
	"github.com/Padelapp-Club/court-gateway/internal/update"
	"github.com/Padelapp-Club/court-gateway/internal/version"
)

// Check codes. They are part of the interface: do not rename.
const (
	CodeConfig     = "CONFIG_VALID"
	CodeCRMLogin   = "CRM_LOGIN"
	CodeOccupancy  = "CRM_OCCUPANCY"
	CodeHubReach   = "HUB_REACHABLE"
	CodeHubAuth    = "HUB_AUTH"
	CodeTunnel     = "TUNNEL_CONNECTED"
	CodeVersion    = "VERSION_CURRENT"
	CodeLocalPort  = "LOCAL_PORT"
	CodeSecretFile = "SECRETS_FILE"
	// CodeHubConfig: the installation's club config on the hub loads with
	// this gateway's CRM driver, and the running service applied it.
	CodeHubConfig = "HUB_CONFIG"
)

// Status of a check.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one diagnosis line.
type Check struct {
	Code    string `json:"code"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	// Hint says what to do, for a person or an agent.
	Hint string `json:"hint,omitempty"`
}

// Report is the whole diagnosis.
type Report struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	Build   string `json:"build"`
	// Club is the club written in gateway.yaml, if any.
	Club string `json:"club,omitempty"`
	// Installation, ConfigVersion and Clubs are the hub's view: the
	// installation id, its club config version and its clubs. ClubsServed
	// and AppliedConfigVersion are what the running service last reported.
	Installation         string   `json:"installation,omitempty"`
	ConfigVersion        int64    `json:"config_version"`
	Clubs                []string `json:"clubs"`
	AppliedConfigVersion int64    `json:"applied_config_version"`
	ClubsServed          []string `json:"clubs_served"`
	Checks               []Check  `json:"checks"`
}

// Options are the inputs of a diagnosis.
type Options struct {
	ConfigPath string
	Getenv     func(string) string
	Factories  map[string]driver.Factory
	Accounts   map[string]driver.AccountFactory
	Version    string
	ReleaseKey string // base64; empty for a self-built binary
	// HubTransport overrides the transport built from the config (tests).
	HubTransport http.RoundTripper
	// UpdateClient fetches the release manifest (tests).
	UpdateClient *http.Client
	// DriverHook adjusts the driver config before building (tests).
	DriverHook func(*driver.Config)
	Timeout    time.Duration
}

// Run performs every check it can; a failed prerequisite turns the
// dependent checks into "skip".
func Run(ctx context.Context, o Options) Report {
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Second
	}
	r := Report{Version: o.Version, Build: tunnel.BuildRelease, Clubs: []string{}, ClubsServed: []string{}}
	if o.ReleaseKey == "" {
		r.Build = tunnel.BuildSelf
	}
	add := func(c Check) { r.Checks = append(r.Checks, c) }
	skipAll := func(msg string) {
		for _, c := range []string{CodeCRMLogin, CodeOccupancy, CodeHubReach, CodeHubAuth, CodeTunnel, CodeHubConfig} {
			add(Check{Code: c, Status: Skip, Message: msg})
		}
	}

	getenv, err := core.EnvWithDotEnv(o.ConfigPath, o.Getenv)
	if err != nil {
		add(Check{Code: CodeConfig, Status: Fail, Message: err.Error(), Hint: "исправьте файл .env рядом с gateway.yaml"})
		return finish(r)
	}
	s, err := core.LoadFile(o.ConfigPath, getenv)
	var local driver.Driver
	var acct driver.Account
	if err == nil && s.Hub == nil {
		if o.DriverHook != nil {
			o.DriverHook(&s.Driver)
		}
		local, err = core.BuildDriver(s, o.Factories)
	}
	if err == nil {
		if f, ok := o.Accounts[s.CRMType]; ok {
			cfg := s.Driver
			if o.DriverHook != nil && s.Hub != nil {
				o.DriverHook(&cfg)
			}
			if acct, err = f(cfg); err != nil {
				err = &core.ConfigError{Problems: strings.Split(err.Error(), "\n")}
			}
		} else if s.Hub != nil {
			err = &core.ConfigError{Problems: []string{fmt.Sprintf("crm.type: неизвестный тип CRM «%s»", s.CRMType)}}
		}
	}
	if err != nil {
		add(Check{Code: CodeConfig, Status: Fail, Message: err.Error(), Hint: "исправьте gateway.yaml или запустите court-gateway setup"})
		skipAll("конфигурация не загружена")
		r.Checks = append(r.Checks, versionCheck(ctx, o, nil))
		return finish(r)
	}
	r.Club = s.ClubID
	if s.Hub == nil {
		add(Check{Code: CodeConfig, Status: OK, Message: fmt.Sprintf("клуб %s, CRM %s, кортов: %d", s.ClubID, s.CRMType, len(s.Courts))})
	} else {
		add(Check{Code: CodeConfig, Status: OK, Message: fmt.Sprintf("установка с CRM %s; клубы настраиваются на хабе", s.CRMType)})
	}
	add(secretsFileCheck(o.ConfigPath))

	// CRM login
	cctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	loginOK := true
	var login func(context.Context) error
	if acct != nil {
		login = acct.CheckLogin
	} else if lc, ok := local.(driver.LoginChecker); ok {
		login = lc.CheckLogin
	}
	if login == nil {
		add(Check{Code: CodeCRMLogin, Status: Skip, Message: "у этого типа CRM нет входа"})
	} else if err := login(cctx); err != nil {
		loginOK = false
		add(Check{Code: CodeCRMLogin, Status: Fail, Message: "вход в CRM не удался: " + core.ReadyReason(err), Hint: crmHint(err)})
	} else {
		add(Check{Code: CodeCRMLogin, Status: OK, Message: "вход в CRM выполнен"})
	}

	// hub (before occupancy: the clubs to read may come from the hub)
	var hubChecksOut []Check
	var remoteClubs []clubDriver
	if s.Hub == nil {
		for _, c := range []string{CodeHubReach, CodeHubAuth, CodeTunnel, CodeHubConfig} {
			hubChecksOut = append(hubChecksOut, Check{Code: c, Status: Skip, Message: "хаб не настроен (раздел hub в gateway.yaml)"})
		}
	} else {
		hubChecksOut, remoteClubs = hubChecks(ctx, o, s, &r)
	}

	// occupancy of one club
	target, targetName := local, s.ClubID
	if target == nil && len(remoteClubs) > 0 {
		target, targetName = remoteClubs[0].drv, remoteClubs[0].id
	}
	switch {
	case !loginOK:
		add(Check{Code: CodeOccupancy, Status: Skip, Message: "нет входа в CRM"})
	case target == nil:
		add(Check{Code: CodeOccupancy, Status: Skip, Message: "клубов пока нет: оператор ещё не подключил клубы на хабе"})
	default:
		now := time.Now()
		var ids []string
		if target == local {
			for _, c := range s.Courts {
				ids = append(ids, c.ID)
			}
		} else {
			ids = remoteClubs[0].courts
		}
		if _, err := target.Occupancy(cctx, driver.OccupancyQuery{Courts: ids, From: now, To: now.Add(24 * time.Hour)}); err != nil {
			add(Check{Code: CodeOccupancy, Status: Fail, Message: "занятость клуба " + targetName + " не читается: " + core.ReadyReason(err), Hint: crmHint(err)})
		} else {
			add(Check{Code: CodeOccupancy, Status: OK, Message: "занятость клуба " + targetName + " на сутки вперёд читается"})
		}
	}
	r.Checks = append(r.Checks, hubChecksOut...)
	if s.Listen != "" {
		add(Check{Code: CodeLocalPort, Status: Warn, Message: "открыт локальный порт " + s.Listen,
			Hint: "для работы через хаб порт не нужен; не пробрасывайте его наружу"})
	}
	r.Checks = append(r.Checks, versionCheck(ctx, o, s))
	return finish(r)
}

type clubDriver struct {
	id     string
	drv    driver.Driver
	courts []string
}

func finish(r Report) Report {
	r.OK = true
	for _, c := range r.Checks {
		if c.Status == Fail {
			r.OK = false
		}
	}
	return r
}

func crmHint(err error) string {
	switch core.ReadyReason(err) {
	case "CRM_AUTH_FAILED":
		return "проверьте логин в gateway.yaml и пароль CRM_PASSWORD в .env"
	case "CRM_REJECTED":
		return "проверьте идентификатор студии и комнат в gateway.yaml"
	default:
		return "проверьте интернет на этом компьютере; если CRM недоступна у всех — подождите"
	}
}

func hubChecks(ctx context.Context, o Options, s *core.Settings, r *Report) ([]Check, []clubDriver) {
	var out []Check
	skipRest := func(from int, msg string) []Check {
		codes := []string{CodeHubReach, CodeHubAuth, CodeTunnel, CodeHubConfig}
		for _, c := range codes[from:] {
			out = append(out, Check{Code: c, Status: Skip, Message: msg})
		}
		return out
	}
	rt := o.HubTransport
	if rt == nil {
		roots, err := tunnel.LoadRoots(s.Hub.CAFile)
		if err != nil {
			out = append(out, Check{Code: CodeHubReach, Status: Fail, Message: err.Error()})
			return skipRest(1, "хаб недоступен"), nil
		}
		rt = tunnel.NewTransport(roots)
	}
	hctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(hctx, http.MethodGet, strings.TrimRight(s.Hub.URL, "/")+"/health/live", nil)
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if err != nil || resp.StatusCode != http.StatusOK {
		msg := "хаб не отвечает"
		if err != nil && strings.Contains(err.Error(), "certificate") {
			msg = "сертификат хаба не прошёл проверку"
		}
		out = append(out, Check{Code: CodeHubReach, Status: Fail, Message: msg, Hint: "проверьте интернет и адрес hub.url; входящие порты открывать не нужно"})
		return skipRest(1, "хаб недоступен"), nil
	}
	out = append(out, Check{Code: CodeHubReach, Status: OK, Message: "хаб отвечает, сертификат проверен"})
	who, err := tunnel.FetchWhoami(hctx, rt, s.Hub.URL, s.Hub.Secret)
	switch {
	case errors.Is(err, tunnel.ErrUnauthorized):
		out = append(out, Check{Code: CodeHubAuth, Status: Fail, Message: "хаб не принял секрет подключения", Hint: "попросите новый код подключения и выполните court-gateway setup --force"})
		return skipRest(2, "нет доступа к хабу"), nil
	case err != nil:
		out = append(out, Check{Code: CodeHubAuth, Status: Fail, Message: "хаб ответил ошибкой"})
		return skipRest(2, "нет доступа к хабу"), nil
	}
	r.Installation, r.ConfigVersion, r.AppliedConfigVersion = who.Installation, who.ConfigVersion, who.AppliedConfigVersion
	r.Clubs, r.ClubsServed = append(r.Clubs[:0], who.Clubs...), append(r.ClubsServed[:0], who.ClubsServed...)
	out = append(out, Check{Code: CodeHubAuth, Status: OK, Message: "хаб узнал шлюз установки " + who.Installation})
	if who.Connected {
		out = append(out, Check{Code: CodeTunnel, Status: OK, Message: "служба шлюза на связи с хабом"})
	} else {
		out = append(out, Check{Code: CodeTunnel, Status: Fail, Message: "служба шлюза сейчас не подключена к хабу",
			Hint: "запустите службу (court-gateway install-service) или подождите минуту после запуска"})
	}

	// the club config on the hub, loaded with this gateway's driver
	c, err := tunnel.NewClient(tunnel.ClientConfig{HubURL: s.Hub.URL, Secret: s.Hub.Secret, Handler: http.NotFoundHandler(), Transport: rt})
	if err != nil {
		return append(out, Check{Code: CodeHubConfig, Status: Fail, Message: err.Error()}), nil
	}
	rc, err := c.FetchConfig(hctx)
	if err != nil {
		return append(out, Check{Code: CodeHubConfig, Status: Fail, Message: "конфигурация клубов с хаба не получена"}), nil
	}
	servers, problems := remote.Build(s, o.Factories, o.DriverHook, rc)
	var clubs []clubDriver
	ids := make([]string, len(servers))
	for i, srv := range servers {
		cd := clubDriver{id: srv.Settings().ClubID, drv: srv.Driver()}
		for _, ct := range srv.Settings().Courts {
			cd.courts = append(cd.courts, ct.ID)
		}
		clubs, ids[i] = append(clubs, cd), cd.id
	}
	switch {
	case len(problems) > 0:
		out = append(out, Check{Code: CodeHubConfig, Status: Fail, Message: fmt.Sprintf("конфигурация клубов на хабе (версия %d) не подходит этому шлюзу: %s", rc.Version, strings.Join(problems, "; ")),
			Hint: "сообщите оператору хаба: служба продолжает работать на предыдущей конфигурации"})
	case len(rc.Clubs) == 0:
		out = append(out, Check{Code: CodeHubConfig, Status: OK, Message: "на хабе пока нет клубов этой установки: оператор подключит их сам, на этом компьютере ничего делать не нужно"})
	case who.Connected && who.AppliedConfigVersion != rc.Version:
		msg := fmt.Sprintf("конфигурация клубов: версия %d на хабе, служба работает на версии %d", rc.Version, who.AppliedConfigVersion)
		if who.ConfigError != "" {
			msg += "; служба отклонила новую: " + who.ConfigError
		}
		out = append(out, Check{Code: CodeHubConfig, Status: Warn, Message: msg, Hint: "служба применяет новую конфигурацию в течение минуты; если не применила — сообщите оператору"})
	default:
		out = append(out, Check{Code: CodeHubConfig, Status: OK, Message: fmt.Sprintf("конфигурация клубов: версия %d, клубы: %s", rc.Version, strings.Join(ids, ", "))})
	}
	return out, clubs
}

func versionCheck(ctx context.Context, o Options, s *core.Settings) Check {
	if o.ReleaseKey == "" {
		return Check{Code: CodeVersion, Status: Warn, Message: "версия " + o.Version + " собрана самостоятельно: автообновление выключено",
			Hint: "обновляйте вручную или поставьте официальный выпуск"}
	}
	pub, err := update.ParsePublicKey(o.ReleaseKey)
	if err != nil {
		return Check{Code: CodeVersion, Status: Fail, Message: "встроенный ключ выпусков повреждён"}
	}
	u := &update.Updater{PublicKey: pub, Current: o.Version, Client: o.UpdateClient}
	if s != nil {
		u.ManifestURL = s.Update.ManifestURL
	}
	if u.Client == nil {
		u.Client = &http.Client{Timeout: 20 * time.Second}
	}
	vctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	m, err := u.Fetch(vctx)
	if err != nil {
		return Check{Code: CodeVersion, Status: Warn, Message: "не удалось проверить последнюю версию: " + shortErr(err)}
	}
	if version.Compare(o.Version, m.Version) < 0 {
		msg := fmt.Sprintf("версия %s, доступна %s", o.Version, m.Version)
		if s != nil && !s.Update.Enabled {
			return Check{Code: CodeVersion, Status: Warn, Message: msg + "; автообновление выключено в gateway.yaml"}
		}
		return Check{Code: CodeVersion, Status: Warn, Message: msg + "; служба обновится сама в течение суток"}
	}
	return Check{Code: CodeVersion, Status: OK, Message: "версия " + o.Version + " — последняя"}
}

func shortErr(err error) string {
	if errors.Is(err, update.ErrBadSignature) {
		return "подпись манифеста не сошлась"
	}
	return "сервер выпусков недоступен"
}

func secretsFileCheck(cfgPath string) Check {
	return secretsFileCheckOS(cfgPath)
}
