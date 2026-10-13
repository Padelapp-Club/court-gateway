package setup

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

type draft struct {
	HubURL, HubCA   string
	CRMType         string
	Username        string
	AuthURL, APIURL string
	Now             time.Time
}

func q(s string) string { return strconv.Quote(s) }

// render writes gateway.yaml of a hub installation: the hub and the CRM
// login. No club: the clubs come from the hub.
func (d *draft) render(installation, name string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# Конфигурация court-gateway, записана командой setup %s.", d.Now.UTC().Format("2006-01-02"))
	if name != "" && name != installation {
		w("# Установка %s (%s).", installation, name)
	} else {
		w("# Установка %s.", installation)
	}
	w("# Клубы, корты и часы работы здесь не задаются: их настраивает оператор на хабе,")
	w("# и шлюз получает их сам. Секреты (пароль CRM, секрет хаба) — в файле .env рядом.")
	w("")
	w("# Связь только исходящая: шлюз сам подключается к хабу, входящих портов нет.")
	w("hub:")
	w("  url: %s", d.HubURL)
	if d.HubCA != "" {
		w("  ca_file: %s", q(d.HubCA))
	}
	w("")
	w("crm:")
	w("  type: %s", d.CRMType)
	if d.CRMType == "viva" {
		w("  username: %s", q(d.Username))
		if d.AuthURL != "" {
			w("  auth_url: %s", d.AuthURL)
		}
		if d.APIURL != "" {
			w("  api_url: %s", d.APIURL)
		}
	}
	return b.String()
}

// envValue writes a value so that our .env parser and docker compose
// env_file read it back unchanged. A value of plain characters is written
// bare; anything else is single-quoted, which both read literally (no $
// interpolation, no escapes but \'). A value with a single quote or a
// trailing backslash is double-quoted instead, with \, " and $ escaped as
// compose expects (\\, \", $$).
func envValue(v string) string {
	if v != "" && strings.IndexFunc(v, func(r rune) bool {
		return r <= ' ' || strings.ContainsRune("$\\'\"`#", r)
	}) < 0 {
		return v
	}
	if !strings.Contains(v, "'") && !strings.HasSuffix(v, "\\") {
		return "'" + v + "'"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", "$$").Replace(v) + `"`
}

func renderEnv(password, secret, crmType string) string {
	var b strings.Builder
	b.WriteString("# Секреты court-gateway. Не пересылайте этот файл и не публикуйте его.\n")
	if crmType == "viva" {
		b.WriteString(core.PasswordEnv + "=" + envValue(password) + "\n")
	}
	b.WriteString(core.DefaultHubSecretEnv + "=" + envValue(secret) + "\n")
	return b.String()
}
