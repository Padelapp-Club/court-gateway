package setup

import (
	"bytes"
	"cmp"
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
)

// SetPassword replaces CRM_PASSWORD in the .env next to cfgPath, for when
// the CRM administrator's password changed: no join code is spent and
// nothing else in .env changes. The new password is first checked with a
// CRM login through the installation's own config (getenv supplies the
// rest of the environment, as serve sees it). The file is written as setup
// writes it: same quoting, mode 0600, and on Windows the folder and .env
// closed and verified (deps.ProtectSecrets) before and after. The running
// service takes the new password from .env by itself.
func SetPassword(ctx context.Context, cfgPath, password string, getenv func(string) string, deps Deps) error {
	if password == "" {
		return problem("MISSING_INPUT", "не хватает: CRM_PASSWORD (или --password-stdin)")
	}
	envPath := filepath.Join(filepath.Dir(cfgPath), core.EnvFileName)
	old, err := os.ReadFile(envPath)
	if err != nil {
		return problem("CONFIG_INVALID", "не удалось прочитать %s: %v — сначала выполните court-gateway setup", envPath, err)
	}
	dotenv, err := core.EnvWithDotEnv(cfgPath, getenv)
	if err != nil {
		return problem("CONFIG_INVALID", "%v", err)
	}
	env := func(k string) string {
		if k == core.PasswordEnv {
			return password
		}
		return dotenv(k)
	}
	s, err := core.LoadFile(cfgPath, env)
	if err != nil {
		return problem("CONFIG_INVALID", "%v", err)
	}
	cfg := s.Driver
	cfg.HTTPClient = cmp.Or(deps.CRMClient, cfg.HTTPClient)
	if deps.Now != nil {
		cfg.Now = deps.Now
	}
	if deps.Accounts == nil {
		deps.Accounts = drivers.Accounts
	}
	acct, err := core.BuildAccount(s, deps.Accounts, cfg)
	if err != nil {
		return problem("CONFIG_INVALID", "%v", err)
	}
	if p := checkLogin(ctx, acct, deps.Note); p != nil {
		return p
	}
	protect := func() error {
		if deps.ProtectSecrets == nil {
			return nil
		}
		for _, p := range []string{filepath.Dir(envPath), envPath} {
			if err := deps.ProtectSecrets(p); err != nil {
				return problem("FOLDER_NOT_RESTRICTED", "не удалось закрыть %s от других пользователей (запустите от имени администратора): %v", p, err)
			}
		}
		return nil
	}
	if err := protect(); err != nil {
		return err
	}
	if err := core.WriteFileAtomic(envPath, replaceEnv(old, core.PasswordEnv, envValue(password)), 0o600); err != nil {
		return problem("WRITE_FAILED", "не удалось записать %s: %v", envPath, err)
	}
	return protect()
}

// replaceEnv sets key=value in .env data: the line(s) of key are replaced
// in place (the first; later ones dropped), every other line is kept as
// is, and the key is appended when absent.
func replaceEnv(data []byte, key, value string) []byte {
	lines := bytes.SplitAfter(data, []byte("\n"))
	var out bytes.Buffer
	done := false
	for _, l := range lines {
		k, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(string(l)), "export "), "=")
		if !ok || strings.TrimSpace(k) != key {
			out.Write(l)
			continue
		}
		if !done {
			out.WriteString(key + "=" + value + "\n")
			done = true
		}
	}
	if !done {
		if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			out.WriteString("\n")
		}
		out.WriteString(key + "=" + value + "\n")
	}
	return out.Bytes()
}
