// Package setup connects a gateway installation to the hub: it checks the
// CRM login, redeems the join code at the hub and writes gateway.yaml and
// .env. That is all the club side ever does: the clubs of the account,
// their courts and hours are configured by the operator on the hub, and
// the running gateway pulls them from there.
package setup

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Input is everything setup needs. Password and JoinCode are secrets: they
// are never printed or logged.
type Input struct {
	ConfigPath string
	Force      bool
	HubURL     string
	HubCAFile  string
	JoinCode   string
	// HubKey is the hub's public key embedded in a release for its own hub
	// (base64), HubKeyFingerprint the key's fingerprint given by the
	// operator (core.HubKeyFingerprint): one of them is required, and the
	// key the hub answers with must match every one given. So the key the
	// gateway pins is authenticated apart from the TLS of the enrollment.
	HubKey            string
	HubKeyFingerprint string
	CRMType           string // drivers.DefaultCRM when empty, or another of Deps.Accounts
	// CRM holds the installation's crm keys the CRM type asks for
	// (driver.SetupSpec.Fields), by key.
	CRM map[string]string
	// Password is CRM_PASSWORD; Secrets the CRM type's other secrets
	// (driver.SetupSpec.Secrets), by environment variable.
	Password string
	Secrets  map[string]string
}

// Spec is the setup of the input's CRM type (empty for an unknown type).
func (in Input) Spec() driver.SetupSpec {
	s, _ := driver.SetupFor(in.crmType())
	return s
}

// Secret returns the value of a secret environment variable of the input.
func (in Input) Secret(env string) string {
	if env == core.PasswordEnv {
		return in.Password
	}
	return in.Secrets[env]
}

// Deps are injectable for tests.
type Deps struct {
	CRMClient    *http.Client
	HubTransport http.RoundTripper
	Now          func() time.Time
	Version      string
	// Accounts default to the built-in drivers.
	Accounts map[string]driver.AccountFactory
	// ProtectSecrets, when set, closes a path to other users and verifies
	// it (Windows: service.ProtectSecrets). It runs on the config folder
	// (and an existing .env) before the join code is redeemed, and on the
	// new .env after writing; any failure aborts setup, as does a folder
	// it closed so that this user can no longer write in it.
	ProtectSecrets func(path string) error
	// Note, when set, takes a remark that does not change the outcome
	// (the CRM session of the login check was not ended).
	Note func(string)
	// Login logs an interactive account in with a person (QR or code;
	// LoginInteractively in a terminal). Without it setup of such a CRM
	// fails: nobody can log the gateway in.
	Login func(context.Context, driver.InteractiveLogin) error
	// PickClient has the person choose the club's service client of a
	// ClientPicker account; it returns the client's id. Without it, or
	// with "" chosen, the gateway reads and does not book.
	PickClient func(context.Context, driver.ClientPicker) (string, error)
	// Sessions keeps the session of an interactive login; default: the
	// state directory of the config being written.
	Sessions driver.SessionStore
}

// Warning is something the administrator must look at.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result is the machine-readable outcome (`setup --json`). It never holds
// a secret.
type Result struct {
	OK           bool             `json:"ok"`
	Error        *Problem         `json:"error,omitempty"`
	Config       string           `json:"config,omitempty"`
	EnvFile      string           `json:"env_file,omitempty"`
	Installation *InstallationRef `json:"installation,omitempty"`
	Hub          string           `json:"hub,omitempty"`
	Warnings     []Warning        `json:"warnings"`
}

// InstallationRef names the installation the hub assigned.
type InstallationRef struct {
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
	if in.HubKey == "" && in.HubKeyFingerprint == "" {
		out = append(out, "--hub-key-fingerprint (или GATEWAY_HUB_KEY_FINGERPRINT; отпечаток ключа хаба приходит вместе с кодом подключения)")
	}
	spec := in.Spec()
	for _, f := range spec.Fields {
		if f.Required && strings.TrimSpace(in.CRM[f.Key]) == "" {
			out = append(out, "--"+f.Flag)
		}
	}
	for _, x := range spec.Secrets {
		if x.Required && in.Secret(x.Env) == "" {
			if x.Env == core.PasswordEnv {
				out = append(out, core.PasswordEnv+" (или --password-stdin)")
			} else {
				out = append(out, x.Env)
			}
		}
	}
	return out
}

func (in Input) crmType() string {
	return cmp.Or(in.CRMType, drivers.DefaultCRM)
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
	if deps.Accounts == nil {
		deps.Accounts = drivers.Accounts
	}
	if m := in.Missing(); len(m) > 0 {
		return fail(problem("MISSING_INPUT", "не хватает: %s", strings.Join(m, ", ")))
	}
	if err := core.CheckHTTPSURL("--hub-url", in.HubURL); err != nil {
		return fail(problem("INVALID_INPUT", "%s", err.Error()))
	}
	var wantKey ed25519.PublicKey
	if in.HubKey != "" {
		var err error
		if wantKey, err = core.ParseHubKey(in.HubKey); err != nil {
			return fail(problem("INVALID_INPUT", "встроенный в программу ключ хаба повреждён — скачайте выпуск заново"))
		}
	}
	wantPrint := core.NormalizeFingerprint(in.HubKeyFingerprint)
	if in.HubKeyFingerprint != "" && wantPrint == "" {
		return fail(problem("INVALID_INPUT", "--hub-key-fingerprint: ожидается отпечаток вида XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XX (буквы A–Z и цифры 2–7)"))
	}
	factory, ok := deps.Accounts[in.crmType()]
	if !ok {
		return fail(problem("INVALID_INPUT", "тип CRM «%s» не поддерживается (%s)", in.CRMType, strings.Join(slices.Sorted(maps.Keys(deps.Accounts)), ", ")))
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
	d := &draft{HubURL: strings.TrimRight(in.HubURL, "/"), HubCA: in.HubCAFile, CRMType: in.crmType(), Now: deps.Now()}

	// 1. the CRM login, before the join code is spent: the keys and the
	// secrets the CRM type declares, and nothing else
	spec := in.Spec()
	crm := driver.Params{}
	for _, f := range spec.Fields {
		if v := strings.TrimSpace(in.CRM[f.Key]); v != "" {
			crm[f.Key] = v
			d.CRM = append(d.CRM, [2]string{f.Key, v})
		}
	}
	secrets := map[string]string{}
	for _, x := range spec.Secrets {
		if v := in.Secret(x.Env); v != "" {
			secrets[x.Env] = v
			d.Secrets = append(d.Secrets, [2]string{x.Env, v})
		}
	}
	sessions := deps.Sessions
	if sessions == nil && spec.Interactive {
		sessions = core.SessionStore(core.StateDir(cfgPath, nil))
	}
	acct, err := factory(driver.Config{CRM: crm, Password: in.Password, Secrets: secrets, HTTPClient: deps.CRMClient, Now: deps.Now, Sessions: sessions})
	if err != nil {
		return fail(problem("INVALID_INPUT", "%v", err))
	}
	if il, ok := acct.(driver.InteractiveLogin); ok {
		// a person logs the gateway in, every setup: the session is the
		// one the service will use (saved in its state directory)
		if deps.Login == nil {
			return fail(problem("LOGIN_REQUIRED", "в CRM %s входит сотрудник клуба (QR-код или код из сообщения): запустите setup без --non-interactive", cmp.Or(spec.Name, d.CRMType)))
		}
		if err := deps.Login(ctx, il); err != nil {
			return fail(loginProblem(err))
		}
	}
	if p := checkLogin(ctx, acct, deps.Note); p != nil {
		return fail(p)
	}
	if cp, ok := acct.(driver.ClientPicker); ok && crm[cp.ClientKey()] == nil {
		id := ""
		if deps.PickClient != nil {
			if id, err = deps.PickClient(ctx, cp); err != nil {
				return fail(problem("CLIENT_NOT_CHOSEN", "служебный клиент не выбран: %v", err))
			}
		}
		if id = strings.TrimSpace(id); id != "" {
			crm[cp.ClientKey()] = id
			d.CRM = append(d.CRM, [2]string{cp.ClientKey(), id})
		} else {
			res.Warnings = append(res.Warnings, Warning{Code: "NO_SERVICE_CLIENT",
				Message: "служебный клиент клуба не выбран: шлюз будет только показывать занятость, брони PadelApp не встанут — выберите его, запустив setup ещё раз"})
		}
	}
	d.HubKey = placeholderKey
	if _, p := validate(d, crm, "pending", "", in.Password, "placeholder-secret"); p != nil {
		return fail(p)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fail(problem("WRITE_FAILED", "%v", err))
	}
	notProtected := func(err error) (Result, error) {
		return fail(problem("FOLDER_NOT_RESTRICTED", "не удалось закрыть папку %s от других пользователей (запустите setup от имени администратора): %v", filepath.Dir(cfgPath), err))
	}
	if deps.ProtectSecrets != nil {
		// before the join code is spent and before any secret is written
		for _, p := range []string{filepath.Dir(cfgPath), envPath} {
			if _, err := os.Stat(p); err != nil && p == envPath {
				continue // no .env yet: it inherits the folder's entries
			}
			if err := deps.ProtectSecrets(p); err != nil {
				return notProtected(err)
			}
		}
		// a folder closed so that this user cannot write in it would
		// take no .env after the join code is spent
		f, err := os.CreateTemp(filepath.Dir(cfgPath), ".setup-*")
		if err == nil {
			f.Close()
			err = os.Remove(f.Name())
		}
		if err != nil {
			return notProtected(err)
		}
	}

	// 2. the hub
	rt := deps.HubTransport
	if rt == nil {
		roots, err := tunnel.LoadRoots(in.HubCAFile)
		if err != nil {
			return fail(problem("INVALID_INPUT", "%v", err))
		}
		rt = tunnel.NewTransport(roots, nil)
	}
	// the hub's key, checked out of band before the join code is spent:
	// the embedded key or the operator's fingerprint (an interceptor of the
	// TLS could put in its own)
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key, err := tunnel.FetchHubKey(rctx, rt, d.HubURL)
	if err == nil && ((wantKey != nil && !key.Equal(wantKey)) || (wantPrint != "" && core.NormalizeFingerprint(core.HubKeyFingerprint(key)) != wantPrint)) {
		return fail(problem("HUB_KEY_MISMATCH", "хаб ответил ключом %s, а ожидался другой (встроенный в выпуск или отпечаток от оператора): соединение с хабом перехвачено или отпечаток неверный — ничего не записано, код подключения не израсходован; сверьте отпечаток с оператором",
			core.HubKeyFingerprint(key)))
	}
	var reg tunnel.RegisterResponse
	if err == nil {
		reg, err = tunnel.Register(rctx, rt, d.HubURL, in.JoinCode, deps.Version)
	}
	switch {
	case errors.Is(err, tunnel.ErrRateLimited):
		return fail(problem("HUB_RATE_LIMITED", "хаб временно не принимает коды подключения с этого адреса — повторите позже, через минуту"))
	case errors.Is(err, tunnel.ErrJoinCodeRejected):
		return fail(problem("JOIN_CODE_REJECTED", "хаб не принял код подключения: он неверный, уже использован или истёк — попросите новый"))
	case errors.Is(err, tunnel.ErrRedirected):
		return fail(problem("HUB_REDIRECTED", "адрес хаба перенаправляет на другой адрес, а перенаправлениям шлюз не следует — укажите точный адрес хаба"))
	case err != nil:
		return fail(problem("HUB_UNREACHABLE", "хаб недоступен: %v", err))
	}

	// 3. the whole answer, which that key must have signed for this join
	// code, before any field of it is used; then write, pinning the key
	// here and again at every new join code. The CRM settings written are
	// this computer's inputs, never the hub's (validate): the hub must not
	// decide where the CRM password goes.
	if reg.Verify(key, strings.TrimSpace(in.JoinCode)) != nil {
		return fail(problem("HUB_KEY_MISMATCH", "ответ хаба на код подключения не подписан его ключом: соединение с хабом перехвачено — ничего не записано; код подключения израсходован, попросите у оператора новый"))
	}
	d.HubKey = core.EncodeHubKey(key)
	text, p := validate(d, crm, reg.Installation, reg.Name, in.Password, reg.Secret)
	if p != nil {
		return fail(p)
	}
	if err := core.WriteFileAtomic(envPath, []byte(renderEnv(d.Secrets, reg.Secret)), 0o600); err != nil {
		return fail(problem("WRITE_FAILED", "не удалось записать %s: %v", envPath, err))
	}
	if deps.ProtectSecrets != nil {
		if err := deps.ProtectSecrets(envPath); err != nil {
			_ = os.Remove(envPath)
			return notProtected(err)
		}
	}
	if err := core.WriteFileAtomic(cfgPath, []byte(text), 0o644); err != nil {
		return fail(problem("WRITE_FAILED", "не удалось записать %s: %v", cfgPath, err))
	}
	res.OK = true
	res.Config, res.EnvFile, res.Hub = cfgPath, envPath, d.HubURL
	res.Installation = &InstallationRef{ID: reg.Installation, Name: cmp.Or(strings.TrimSpace(reg.Name), reg.Installation)}
	return res, nil
}

// checkLogin logs in to the CRM and names the failure. Then it ends the
// CRM session of that login: the installation's one session is the
// service's. Not ending it changes nothing else (the session expires by
// itself); note hears of it.
func checkLogin(ctx context.Context, acct driver.Account, note func(string)) *Problem {
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	err := acct.CheckLogin(lctx)
	ectx, ecancel := context.WithTimeout(ctx, 10*time.Second)
	defer ecancel()
	if eerr := driver.EndSession(ectx, acct); eerr != nil && note != nil {
		note("сеанс проверки входа в CRM не закрыт, CRM закроет его сама по истечении срока: " + eerr.Error())
	}
	var ue *driver.UpstreamError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ue) && ue.Kind == driver.UpstreamAuth:
		return problem("CRM_AUTH_FAILED", "CRM не приняла логин или пароль администратора")
	case errors.As(err, &ue) && ue.Kind == driver.UpstreamInvalid:
		return problem("CRM_INVALID_RESPONSE", "CRM ответила в непонятном формате")
	default:
		return problem("CRM_UNREACHABLE", "CRM недоступна, попробуйте позже")
	}
}

// loginProblem names the failure of an interactive login.
func loginProblem(err error) *Problem {
	var ue *driver.UpstreamError
	switch {
	case errors.Is(err, driver.ErrLoginExpired):
		return problem("CRM_LOGIN_EXPIRED", "время входа истекло — запустите ещё раз")
	case errors.Is(err, driver.ErrLoginDeclined):
		return problem("CRM_LOGIN_DECLINED", "вход отклонён в приложении CRM или код неверный")
	case errors.Is(err, context.Canceled):
		return problem("CRM_LOGIN_ABORTED", "вход прерван")
	case errors.As(err, &ue) && ue.Kind == driver.UpstreamInvalid:
		return problem("CRM_INVALID_RESPONSE", "CRM ответила в непонятном формате")
	case errors.As(err, &ue) && ue.Kind == driver.UpstreamUnavailable:
		return problem("CRM_UNREACHABLE", "CRM недоступна, попробуйте позже")
	default:
		return problem("CRM_LOGIN_FAILED", "вход в CRM не удался: %v", err)
	}
}

// envFor is the environment a written config will see.
func envFor(password, secret string) func(string) string {
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

// placeholderKey stands for the hub's key until the hub has answered.
var placeholderKey = core.EncodeHubKey(make([]byte, 32))

// validate renders the config about to be written and checks that it
// loads with exactly the CRM settings of this computer's inputs (crm).
func validate(d *draft, crm driver.Params, installation, name, password, secret string) (string, *Problem) {
	if password == "" {
		password = "placeholder"
	}
	text := d.render(installation, name)
	s, err := core.Load([]byte(text), envFor(password, secret))
	if err != nil {
		return "", problem("INTERNAL", "собранная конфигурация не прошла проверку: %v", err)
	}
	if s.CRMType != d.CRMType || !reflect.DeepEqual(map[string]any(s.Driver.CRM), map[string]any(crm)) {
		return "", problem("INTERNAL", "в собранную конфигурацию попали настройки CRM не из установки — ничего не записано")
	}
	return text, nil
}
