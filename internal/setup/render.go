package setup

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

type draft struct {
	HubURL, HubCA string
	HubKey        string
	CRMType       string
	// CRM are the installation's crm keys and Secrets the CRM secrets
	// (environment variable, value), in the order the CRM type declares.
	CRM, Secrets [][2]string
	// Mirror is the second CRM's type ("" for none), MirrorCRM its keys.
	Mirror    string
	MirrorCRM [][2]string
	Now       time.Time
}

func q(s string) string { return strconv.Quote(s) }

// render writes gateway.yaml of a hub installation: the hub and the CRM
// login. No club: the clubs come from the hub.
func (d *draft) render(installation, name string) string {
	// the name goes into a comment: no line break may end it
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
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
	w("  installation: %s", installation)
	w("  # открытый ключ хаба: шлюз выполняет только подписанные им запросы")
	w("  key: %s", q(d.HubKey))
	if d.HubCA != "" {
		w("  ca_file: %s", q(d.HubCA))
	}
	w("")
	w("crm:")
	w("  type: %s", d.CRMType)
	for _, kv := range d.CRM {
		w("  %s: %s", kv[0], q(kv[1]))
	}
	if d.Mirror != "" {
		w("")
		w("# Вторая CRM тех же клубов: шлюз сверяет брони между обеими. Работает у клубов,")
		w("# которым оператор на хабе включил раздел mirror.")
		w("mirror:")
		w("  type: %s", d.Mirror)
		for _, kv := range d.MirrorCRM {
			w("  %s: %s", kv[0], q(kv[1]))
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

func renderEnv(crmSecrets [][2]string, secret string) string {
	var b strings.Builder
	b.WriteString("# Секреты court-gateway. Не пересылайте этот файл и не публикуйте его.\n")
	for _, kv := range crmSecrets {
		b.WriteString(kv[0] + "=" + envValue(kv[1]) + "\n")
	}
	b.WriteString(core.DefaultHubSecretEnv + "=" + envValue(secret) + "\n")
	return b.String()
}
