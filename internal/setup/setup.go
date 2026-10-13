// Package setup connects a gateway installation to the hub: it checks the
// CRM login, redeems the join code at the hub and writes gateway.yaml and
// .env. That is all the club side ever does: the clubs of the account,
// their courts and hours are configured by the operator on the hub, and
// the running gateway pulls them from there.
package setup

import (
	"cmp"
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
	CRMType    string // viva (default) or demo
	Username   string
	Password   string
	AuthURL    string // optional CRM endpoint overrides (test doubles)
	APIURL     string
}

// Deps are injectable for tests.
type Deps struct {
	CRMClient    *http.Client
	HubTransport http.RoundTripper
	Now          func() time.Time
	Version      string
	// Accounts default to the built-in drivers.
	Accounts map[string]driver.AccountFactory
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
	if in.crmType() == "viva" {
		if in.Username == "" {
			out = append(out, "--crm-username")
		}
		if in.Password == "" {
			out = append(out, "CRM_PASSWORD (или --password-stdin)")
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
	if deps.Accounts == nil {
		deps.Accounts = drivers.Accounts
	}
	if m := in.Missing(); len(m) > 0 {
		return fail(problem("MISSING_INPUT", "не хватает: %s", strings.Join(m, ", ")))
	}
	if err := core.CheckHTTPSURL("--hub-url", in.HubURL); err != nil {
		return fail(problem("INVALID_INPUT", "%s", err.Error()))
	}
	factory, ok := deps.Accounts[in.crmType()]
	if !ok {
		return fail(problem("INVALID_INPUT", "тип CRM «%s» не поддерживается (viva или demo)", in.CRMType))
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
	d := &draft{HubURL: strings.TrimRight(in.HubURL, "/"), HubCA: in.HubCAFile, CRMType: in.crmType(),
		Username: in.Username, AuthURL: in.AuthURL, APIURL: in.APIURL, Now: deps.Now()}

	// 1. the CRM login, before the join code is spent
	crm := driver.Params{}
	if d.CRMType == "viva" {
		crm["username"] = in.Username
		if in.AuthURL != "" {
			crm["auth_url"] = in.AuthURL
		}
		if in.APIURL != "" {
			crm["api_url"] = in.APIURL
		}
	}
	acct, err := factory(driver.Config{CRM: crm, Password: in.Password, HTTPClient: deps.CRMClient, Now: deps.Now})
	if err != nil {
		return fail(problem("INVALID_INPUT", "%v", err))
	}
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	err = acct.CheckLogin(lctx)
	cancel()
	if err != nil {
		var ue *driver.UpstreamError
		switch {
		case errors.As(err, &ue) && ue.Kind == driver.UpstreamAuth:
			return fail(problem("CRM_AUTH_FAILED", "CRM не приняла логин или пароль администратора"))
		case errors.As(err, &ue) && ue.Kind == driver.UpstreamInvalid:
			return fail(problem("CRM_INVALID_RESPONSE", "CRM ответила в непонятном формате"))
		default:
			return fail(problem("CRM_UNREACHABLE", "CRM недоступна, попробуйте позже"))
		}
	}
	if p := validate(d, in.Password, "placeholder-secret"); p != nil {
		return fail(p)
	}

	// 2. the hub
	rt := deps.HubTransport
	if rt == nil {
		roots, err := tunnel.LoadRoots(in.HubCAFile)
		if err != nil {
			return fail(problem("INVALID_INPUT", "%v", err))
		}
		rt = tunnel.NewTransport(roots)
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	reg, err := tunnel.Register(rctx, rt, d.HubURL, in.JoinCode, deps.Version)
	cancel()
	switch {
	case errors.Is(err, tunnel.ErrJoinCodeRejected):
		return fail(problem("JOIN_CODE_REJECTED", "хаб не принял код подключения: он неверный, уже использован или истёк — попросите новый"))
	case err != nil:
		return fail(problem("HUB_UNREACHABLE", "хаб недоступен: %v", err))
	}

	// 3. write
	if p := validate(d, in.Password, reg.Secret); p != nil {
		return fail(p)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fail(problem("WRITE_FAILED", "%v", err))
	}
	if err := core.WriteFileAtomic(envPath, []byte(renderEnv(in.Password, reg.Secret, d.CRMType)), 0o600); err != nil {
		return fail(problem("WRITE_FAILED", "не удалось записать %s: %v", envPath, err))
	}
	if err := core.WriteFileAtomic(cfgPath, []byte(d.render(reg.Installation, reg.Name)), 0o644); err != nil {
		return fail(problem("WRITE_FAILED", "не удалось записать %s: %v", cfgPath, err))
	}
	res.OK = true
	res.Config, res.EnvFile, res.Hub = cfgPath, envPath, d.HubURL
	res.Installation = &InstallationRef{ID: reg.Installation, Name: cmp.Or(strings.TrimSpace(reg.Name), reg.Installation)}
	return res, nil
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

// validate checks that the config about to be written loads.
func validate(d *draft, password, secret string) *Problem {
	if password == "" {
		password = "placeholder"
	}
	if _, err := core.Load([]byte(d.render("pending", "")), envFor(password, secret)); err != nil {
		return problem("INTERNAL", "собранная конфигурация не прошла проверку: %v", err)
	}
	return nil
}
