// Package doctor runs the gateway's self-diagnosis: one line per check,
// each with a stable code an agent can act on, and nothing secret in any
// message.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	// the service may run a newer verified version than this binary
	stateDir := core.StateDir(o.ConfigPath, getenv)
	o.Version = update.RunningVersion(stateDir, o.Version, time.Now())
	r.Version = o.Version
	s, err := core.LoadFile(o.ConfigPath, getenv)
	var acct driver.Account
	if err == nil {
		cfg := s.Driver
		if o.DriverHook != nil {
			o.DriverHook(&cfg)
		}
		acct, err = core.BuildAccount(s, o.Accounts, cfg)
		s.Driver.Account = acct
	}
	if err != nil {
		add(Check{Code: CodeConfig, Status: Fail, Message: err.Error(), Hint: "исправьте gateway.yaml или запустите court-gateway setup"})
		skipAll("конфигурация не загружена")
		r.Checks = append(r.Checks, versionCheck(ctx, o, nil))
		return finish(r)
	}
	add(Check{Code: CodeConfig, Status: OK, Message: fmt.Sprintf("установка с CRM %s; клубы настраиваются на хабе", s.CRMType)})
	add(secretsFileCheck(o.ConfigPath))

	// While the service runs, the CRM checks are the service's own, as its
	// status file reports them: doctor does not log in beside it (a second
	// session of the same CRM user). Only without a running service does
	// doctor log in itself, and then it ends that session.
	svc := serviceStatus(ctx, stateDir, o.Timeout)
	if svc != nil {
		r.Checks = append(r.Checks, serviceCRMChecks(ctx, o, s, svc, &r)...)
		r.Checks = append(r.Checks, versionCheck(ctx, o, s))
		return finish(r)
	}

	// CRM login, the hub and the occupancy each get their own timeout: one
	// slow step does not eat the next one's
	const own = "проверка doctor (служба не запущена): "
	loginOK, login := true, len(r.Checks)
	if err := withTimeout(ctx, o.Timeout, acct.CheckLogin); err != nil {
		loginOK = false
		add(Check{Code: CodeCRMLogin, Status: Fail, Message: own + "вход в CRM не удался: " + core.ReadyReason(err), Hint: crmHint(core.ReadyReason(err))})
	} else {
		add(Check{Code: CodeCRMLogin, Status: OK, Message: own + "вход в CRM выполнен"})
	}

	// the hub (before occupancy: the clubs to read come from the hub; nil
	// when its club config was not received)
	hubChecksOut, remoteClubs := hubChecks(ctx, o, s, &r, nil)

	// occupancy of one club
	switch {
	case !loginOK:
		add(Check{Code: CodeOccupancy, Status: Skip, Message: "нет входа в CRM"})
	case remoteClubs == nil:
		add(Check{Code: CodeOccupancy, Status: Skip, Message: "хаб недоступен или не отдал конфигурацию клубов: какие клубы проверять, неизвестно"})
	case len(remoteClubs) == 0 && len(r.Clubs) > 0:
		add(Check{Code: CodeOccupancy, Status: Skip, Message: "нет клуба, конфигурация которого подходит CRM"})
	case len(remoteClubs) == 0:
		add(Check{Code: CodeOccupancy, Status: Skip, Message: "клубов пока нет: оператор ещё не подключил клубы на хабе"})
	default:
		now, c := time.Now(), remoteClubs[0]
		if err := withTimeout(ctx, o.Timeout, func(ctx context.Context) error {
			_, err := c.drv.Occupancy(ctx, driver.OccupancyQuery{Courts: c.courts, From: now, To: now.Add(24 * time.Hour)})
			return err
		}); err != nil {
			add(Check{Code: CodeOccupancy, Status: Fail, Message: "занятость клуба " + c.id + " не читается: " + core.ReadyReason(err), Hint: crmHint(core.ReadyReason(err))})
		} else {
			add(Check{Code: CodeOccupancy, Status: OK, Message: "занятость клуба " + c.id + " на сутки вперёд читается"})
		}
	}
	// the login check, the clubs' config check and the occupancy read
	// shared one session: end it
	if err := withTimeout(ctx, o.Timeout, func(ctx context.Context) error { return driver.EndSession(ctx, acct) }); err != nil {
		r.Checks[login].Message += "; сеанс doctor в CRM не закрыт, CRM закроет его сама по истечении срока"
	}
	r.Checks = append(r.Checks, hubChecksOut...)
	r.Checks = append(r.Checks, versionCheck(ctx, o, s))
	return finish(r)
}

// serviceStatus is the running service's status file, or nil when no
// service runs: no file, or one older than healthcheck allows. A service
// that has not finished its first CRM check yet (it starts at once) is
// waited for within timeout.
func serviceStatus(ctx context.Context, dir string, timeout time.Duration) *core.HealthStatus {
	deadline := time.Now().Add(timeout)
	for {
		st, err := core.ReadHealthStatus(dir)
		if err != nil || time.Since(st.Time) > 3*core.HealthStatusInterval {
			return nil
		}
		if st.CRM != "" || !time.Now().Before(deadline) {
			return &st
		}
		select {
		case <-ctx.Done():
			return &st
		case <-time.After(time.Second):
		}
	}
}

// loginReasons are the reasons of a CRM self-check that are about logging
// in to the CRM or reaching it at all, whichever club's check met them.
var loginReasons = map[string]bool{"CRM_AUTH_FAILED": true, "CRM_AUTH_ERROR": true, "CRM_UNREACHABLE": true, "CRM_TIMEOUT": true, "CRM_ERROR": true}

// serviceCRMChecks are CRM_LOGIN, CRM_OCCUPANCY and the hub's checks as
// the running service's last CRM self-check (svc) reports them: the
// account's login, or each served club's config and today's occupancy.
// No CRM call is made.
func serviceCRMChecks(ctx context.Context, o Options, s *core.Settings, svc *core.HealthStatus, r *Report) []Check {
	var out []Check
	from := "по данным службы, проверка " + svc.CRMCheckedAt.Local().Format("15:04") + ": "
	loginFailed := svc.CRM != "ok" && (loginReasons[svc.CRM] || len(svc.CRMClubs) == 0)
	switch {
	case svc.CRM == "":
		out = append(out, Check{Code: CodeCRMLogin, Status: Warn, Message: "служба запущена, но ещё не сообщила, входит ли она в CRM",
			Hint: "повторите doctor через минуту"})
	case loginFailed && svc.CRM == "CRM_AUTH_FAILED":
		out = append(out, Check{Code: CodeCRMLogin, Status: Fail, Message: from + "вход в CRM не удался: " + svc.CRM,
			Hint: "служба не может войти в Viva: проверьте пароль, затем court-gateway set-password"})
	case loginFailed:
		out = append(out, Check{Code: CodeCRMLogin, Status: Fail, Message: from + "вход в CRM не удался: " + svc.CRM, Hint: crmHint(svc.CRM)})
	default:
		out = append(out, Check{Code: CodeCRMLogin, Status: OK, Message: from + "вход в CRM выполнен"})
	}

	hubOut, remoteClubs := hubChecks(ctx, o, s, r, svc)
	clubs := strings.Join(svc.CRMClubs, ", ")
	switch {
	case svc.CRM == "":
		out = append(out, Check{Code: CodeOccupancy, Status: Skip, Message: "служба ещё не проверила CRM"})
	case loginFailed:
		out = append(out, Check{Code: CodeOccupancy, Status: Skip, Message: "у службы нет входа в CRM"})
	case remoteClubs == nil:
		out = append(out, Check{Code: CodeOccupancy, Status: Skip, Message: "хаб недоступен или не отдал конфигурацию клубов: какие клубы проверять, неизвестно"})
	case len(remoteClubs) == 0 && len(r.Clubs) > 0:
		out = append(out, Check{Code: CodeOccupancy, Status: Skip, Message: "нет клуба, конфигурация которого подходит CRM"})
	case len(remoteClubs) == 0:
		out = append(out, Check{Code: CodeOccupancy, Status: Skip, Message: "клубов пока нет: оператор ещё не подключил клубы на хабе"})
	case svc.CRM == "ok" || !strings.HasPrefix(svc.CRM, "CRM_"):
		// a config alert comes only after the club's reads passed; a
		// config mismatch is HUB_CONFIG's
		out = append(out, Check{Code: CodeOccupancy, Status: OK, Message: from + "занятость клубов на сегодня читается"})
	default:
		out = append(out, Check{Code: CodeOccupancy, Status: Fail, Message: from + "занятость клуба " + clubs + " не читается: " + svc.CRM, Hint: crmHint(svc.CRM)})
	}
	return append(out, hubOut...)
}

func withTimeout(ctx context.Context, d time.Duration, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(ctx)
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

func crmHint(reason string) string {
	switch reason {
	case "CRM_AUTH_FAILED":
		return "проверьте логин в gateway.yaml и пароль CRM_PASSWORD в .env"
	case "CRM_REJECTED":
		return "сообщите оператору хаба: CRM не принимает настройки клуба"
	default:
		return "проверьте интернет на этом компьютере; если CRM недоступна у всех — подождите"
	}
}

// hubChecks are the hub's checks; svc, when the service runs, stands for
// the CRM check of the clubs' config (no CRM call is made then).
func hubChecks(ctx context.Context, o Options, s *core.Settings, r *Report, svc *core.HealthStatus) ([]Check, []clubDriver) {
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
		rt = tunnel.NewTransport(roots, s.Hub.Proxy)
	}
	hctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(hctx, http.MethodGet, strings.TrimRight(s.Hub.URL, "/")+"/health/live", nil)
	resp, err := (&http.Client{Transport: rt, CheckRedirect: core.RefuseRedirects}).Do(req)
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
	reach := "хаб отвечает, сертификат проверен"
	if strings.HasPrefix(s.Hub.URL, "http://") {
		reach = "хаб отвечает (loopback без TLS)"
	}
	out = append(out, Check{Code: CodeHubReach, Status: OK, Message: reach})
	c, err := tunnel.NewClient(tunnel.ClientConfig{HubURL: s.Hub.URL, Secret: s.Hub.Secret, HubKey: s.Hub.Key, Installation: s.Hub.Installation,
		Handler: http.NotFoundHandler(), Transport: rt})
	if err != nil {
		out = append(out, Check{Code: CodeHubAuth, Status: Fail, Message: err.Error()})
		return skipRest(2, "нет доступа к хабу"), nil
	}
	who, err := c.Whoami(hctx) // and the hub's time, which the club config is checked against
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
	rc, err := c.FetchConfig(hctx)
	switch {
	case errors.Is(err, tunnel.ErrBadSignature):
		return append(out, Check{Code: CodeHubConfig, Status: Fail, Message: "конфигурация клубов не подписана ключом хаба, записанным при подключении",
			Hint: "проверьте часы компьютера; если хаб сменил ключ — новый код подключения и court-gateway setup --force"}), nil
	case err != nil:
		return append(out, Check{Code: CodeHubConfig, Status: Fail, Message: "конфигурация клубов с хаба не получена"}), nil
	}
	servers, refused, _ := remote.Build(s, o.Factories, o.DriverHook, rc)
	var problems, unchecked, alerts []string
	for _, r := range refused {
		problems = append(problems, r.Problems...)
	}
	// the CRM check the service makes before serving a club, each bounded
	ectx := driver.WithEvaluation(ctx)
	clubs := []clubDriver{}
	ids := make([]string, 0, len(servers))
	for _, srv := range servers {
		cd := clubDriver{id: srv.Settings().ClubID, drv: srv.Driver()}
		for _, ct := range srv.Settings().Courts {
			cd.courts = append(cd.courts, ct.ID)
		}
		ids = append(ids, cd.id)
		if cc, ok := cd.drv.(driver.ConfigChecker); ok && svc == nil {
			err := withTimeout(ectx, o.Timeout, cc.CheckConfig)
			var me *driver.MisconfigError
			var ca *driver.ConfigAlert
			switch {
			case errors.As(err, &me):
				for _, p := range me.Problems {
					problems = append(problems, "клуб «"+cd.id+"»: "+p)
				}
				continue
			case errors.As(err, &ca):
				for _, p := range ca.Problems {
					alerts = append(alerts, "клуб «"+cd.id+"»: "+p)
				}
			case err != nil:
				unchecked = append(unchecked, cd.id+" ("+core.ReadyReason(err)+")")
			}
		}
		clubs = append(clubs, cd)
	}
	if svc != nil && svc.CRM != "ok" && len(svc.CRMClubs) > 0 && !loginReasons[svc.CRM] {
		for _, id := range svc.CRMClubs {
			switch {
			case svc.CRM == "CONFIG_MISMATCH":
				problems = append(problems, "клуб «"+id+"»: по данным службы, не совпадает с CRM")
			case !strings.HasPrefix(svc.CRM, "CRM_"):
				alerts = append(alerts, "клуб «"+id+"»: по данным службы, "+svc.CRM)
			}
		}
	}
	var unserved []string
	for _, id := range who.Clubs {
		if !slices.Contains(who.ClubsServed, id) {
			unserved = append(unserved, id)
		}
	}
	hint := "сообщите оператору хаба: остальные клубы работают, у этих служба оставляет прежнюю конфигурацию, если она была"
	switch {
	case len(problems) > 0:
		out = append(out, Check{Code: CodeHubConfig, Status: Fail, Message: fmt.Sprintf("конфигурация клубов на хабе (версия %d) не подходит этому шлюзу: %s", rc.Version, strings.Join(problems, "; ")), Hint: hint})
	case who.Connected && who.ConfigError != "":
		out = append(out, Check{Code: CodeHubConfig, Status: Fail, Message: fmt.Sprintf("служба (версия конфигурации %d) не приняла клубы: %s", who.AppliedConfigVersion, who.ConfigError), Hint: hint})
	case len(rc.Clubs) == 0:
		out = append(out, Check{Code: CodeHubConfig, Status: OK, Message: "на хабе пока нет клубов этой установки: оператор подключит их сам, на этом компьютере ничего делать не нужно"})
	case who.Connected && who.AppliedConfigVersion != rc.Version:
		out = append(out, Check{Code: CodeHubConfig, Status: Warn, Message: fmt.Sprintf("конфигурация клубов: версия %d на хабе, служба работает на версии %d", rc.Version, who.AppliedConfigVersion),
			Hint: "служба применяет новую конфигурацию в течение минуты; если не применила — сообщите оператору"})
	case who.Connected && len(unserved) > 0:
		out = append(out, Check{Code: CodeHubConfig, Status: Fail, Message: "служба не обслуживает клубы: " + strings.Join(unserved, ", "), Hint: hint})
	case len(alerts) > 0:
		out = append(out, Check{Code: CodeHubConfig, Status: Warn, Message: "клубы обслуживаются, но их конфигурация расходится с CRM: " + strings.Join(alerts, "; "),
			Hint: "сообщите оператору хаба: это исправляется в конфигурации клуба на хабе"})
	case len(unchecked) > 0:
		out = append(out, Check{Code: CodeHubConfig, Status: Warn, Message: "конфигурацию клубов не удалось сверить с CRM: " + strings.Join(unchecked, ", "),
			Hint: "повторите doctor через минуту; служба сверяет клубы сама"})
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
		if s != nil {
			u.Client.Transport = s.Hub.Proxy.Transport()
		}
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

func secretsFileCheck(cfgPath string) Check {
	p := filepath.Join(filepath.Dir(cfgPath), core.EnvFileName)
	if _, err := os.Stat(p); err != nil {
		return Check{Code: CodeSecretFile, Status: Skip, Message: "файла .env нет: секреты берутся из окружения"}
	}
	st, msg, hint := secretsExposure(cfgPath, p)
	return Check{Code: CodeSecretFile, Status: st, Message: msg, Hint: hint}
}

func shortErr(err error) string {
	if errors.Is(err, update.ErrBadSignature) {
		return "подпись манифеста не сошлась"
	}
	return "сервер выпусков недоступен"
}
