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

// envValue writes a value that our .env parser, docker compose env_file
// and docker run --env-file all read back unchanged whenever possible:
// quotes are added only when a value cannot be written bare.
func envValue(v string) string {
	needs := v == "" || v != strings.TrimSpace(v) || v[0] == '"' || v[0] == '\'' || strings.Contains(v, " #")
	if !needs {
		return v
	}
	if !strings.Contains(v, "\"") {
		return "\"" + v + "\""
	}
	return "'" + v + "'"
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
