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
	OK      bool    `json:"ok"`
	Version string  `json:"version"`
	Build   string  `json:"build"`
	Club    string  `json:"club,omitempty"`
	Checks  []Check `json:"checks"`
}

// Options are the inputs of a diagnosis.
type Options struct {
	ConfigPath string
	Getenv     func(string) string
	Factories  map[string]driver.Factory
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
	r := Report{Version: o.Version, Build: tunnel.BuildRelease}
	if o.ReleaseKey == "" {
		r.Build = tunnel.BuildSelf
	}
	add := func(c Check) { r.Checks = append(r.Checks, c) }

	getenv, err := core.EnvWithDotEnv(o.ConfigPath, o.Getenv)
	if err != nil {
		add(Check{Code: CodeConfig, Status: Fail, Message: err.Error(), Hint: "исправьте файл .env рядом с gateway.yaml"})
		return finish(r)
	}
	s, err := core.LoadFile(o.ConfigPath, getenv)
	var drv driver.Driver
	if err == nil {
		if o.DriverHook != nil {
			o.DriverHook(&s.Driver)
		}
		drv, err = core.BuildDriver(s, o.Factories)
	}
	if err != nil {
		add(Check{Code: CodeConfig, Status: Fail, Message: err.Error(), Hint: "исправьте gateway.yaml или запустите court-gateway setup"})
		for _, c := range []string{CodeCRMLogin, CodeOccupancy, CodeHubReach, CodeHubAuth, CodeTunnel} {
			add(Check{Code: c, Status: Skip, Message: "конфигурация не загружена"})
		}
		r.Checks = append(r.Checks, versionCheck(ctx, o, nil))
		return finish(r)
	}
	r.Club = s.ClubID
	add(Check{Code: CodeConfig, Status: OK, Message: fmt.Sprintf("клуб %s, CRM %s, кортов: %d", s.ClubID, s.CRMType, len(s.Courts))})
	add(secretsFileCheck(o.ConfigPath))

	// CRM
	cctx, cancel := context.WithTimeout(ctx, o.Timeout)
	loginOK := true
	if lc, ok := drv.(driver.LoginChecker); ok {
		if err := lc.CheckLogin(cctx); err != nil {
			loginOK = false
			add(Check{Code: CodeCRMLogin, Status: Fail, Message: "вход в CRM не удался: " + core.ReadyReason(err), Hint: crmHint(err)})
		} else {
			add(Check{Code: CodeCRMLogin, Status: OK, Message: "вход в CRM выполнен"})
		}
	} else {
		add(Check{Code: CodeCRMLogin, Status: Skip, Message: "у этого типа CRM нет входа"})
	}
	if !loginOK {
		add(Check{Code: CodeOccupancy, Status: Skip, Message: "нет входа в CRM"})
	} else {
		now := time.Now()
		ids := make([]string, len(s.Courts))
		for i, c := range s.Courts {
			ids[i] = c.ID
		}
		if _, err := drv.Occupancy(cctx, driver.OccupancyQuery{Courts: ids, From: now, To: now.Add(24 * time.Hour)}); err != nil {
			add(Check{Code: CodeOccupancy, Status: Fail, Message: "занятость не читается: " + core.ReadyReason(err), Hint: crmHint(err)})
		} else {
			add(Check{Code: CodeOccupancy, Status: OK, Message: "занятость на сутки вперёд читается"})
		}
	}
	cancel()

	// hub
	if s.Hub == nil {
		for _, c := range []string{CodeHubReach, CodeHubAuth, CodeTunnel} {
			add(Check{Code: c, Status: Skip, Message: "хаб не настроен (раздел hub в gateway.yaml)"})
		}
	} else {
		r.Checks = append(r.Checks, hubChecks(ctx, o, s)...)
	}
	if s.Listen != "" {
		add(Check{Code: CodeLocalPort, Status: Warn, Message: "открыт локальный порт " + s.Listen,
			Hint: "для работы через хаб порт не нужен; не пробрасывайте его наружу"})
	}
	r.Checks = append(r.Checks, versionCheck(ctx, o, s))
	return finish(r)
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

func hubChecks(ctx context.Context, o Options, s *core.Settings) []Check {
	var out []Check
	rt := o.HubTransport
	if rt == nil {
		roots, err := tunnel.LoadRoots(s.Hub.CAFile)
		if err != nil {
			return []Check{
				{Code: CodeHubReach, Status: Fail, Message: err.Error()},
				{Code: CodeHubAuth, Status: Skip, Message: "хаб недоступен"},
				{Code: CodeTunnel, Status: Skip, Message: "хаб недоступен"},
			}
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
		return []Check{
			{Code: CodeHubReach, Status: Fail, Message: msg, Hint: "проверьте интернет и адрес hub.url; входящие порты открывать не нужно"},
			{Code: CodeHubAuth, Status: Skip, Message: "хаб недоступен"},
			{Code: CodeTunnel, Status: Skip, Message: "хаб недоступен"},
		}
	}
	out = append(out, Check{Code: CodeHubReach, Status: OK, Message: "хаб отвечает, сертификат проверен"})
	who, err := tunnel.FetchWhoami(hctx, rt, s.Hub.URL, s.Hub.Secret)
	switch {
	case errors.Is(err, tunnel.ErrUnauthorized):
		return append(out,
			Check{Code: CodeHubAuth, Status: Fail, Message: "хаб не принял секрет подключения", Hint: "попросите новый код подключения и выполните court-gateway setup --force"},
			Check{Code: CodeTunnel, Status: Skip, Message: "нет доступа к хабу"})
	case err != nil:
		return append(out,
			Check{Code: CodeHubAuth, Status: Fail, Message: "хаб ответил ошибкой"},
			Check{Code: CodeTunnel, Status: Skip, Message: "нет доступа к хабу"})
	case who.Club != s.ClubID:
		return append(out,
			Check{Code: CodeHubAuth, Status: Fail, Message: fmt.Sprintf("секрет выдан для клуба %s, а в gateway.yaml клуб %s", who.Club, s.ClubID), Hint: "исправьте club.id или выполните setup заново"},
			Check{Code: CodeTunnel, Status: Skip, Message: "клуб не совпадает"})
	}
	out = append(out, Check{Code: CodeHubAuth, Status: OK, Message: "хаб узнал шлюз клуба " + who.Club})
	if who.Connected {
		out = append(out, Check{Code: CodeTunnel, Status: OK, Message: "служба шлюза на связи с хабом"})
	} else {
		out = append(out, Check{Code: CodeTunnel, Status: Fail, Message: "служба шлюза сейчас не подключена к хабу",
			Hint: "запустите службу (court-gateway install-service) или подождите минуту после запуска"})
	}
	return out
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
