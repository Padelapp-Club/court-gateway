package setup

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
)

type draft struct {
	ClubID, ClubName string
	HubURL, HubCA    string
	CRMType          string
	Username, Studio string
	AuthURL, APIURL  string
	Timezone         string
	Slot             int
	Hours            [7]*[2]int
	Courts           []Court
	PriceLists       []viva.PriceListInfo
	Warnings         []Warning
	Now              time.Time
}

func (d *draft) warn(code, msg string) {
	d.Warnings = append(d.Warnings, Warning{Code: code, Message: msg})
}

var dayKeys = [7]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

func clockText(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

// roundHours widens the hours to the slot grid; true if anything changed.
func (d *draft) roundHours() bool {
	changed := false
	for _, h := range d.Hours {
		if h == nil {
			continue
		}
		o := h[0] / d.Slot * d.Slot
		c := (h[1] + d.Slot - 1) / d.Slot * d.Slot
		if c > 1440 {
			c = 1440
		}
		if o != h[0] || c != h[1] {
			h[0], h[1], changed = o, c, true
		}
	}
	return changed
}

func (d *draft) dayText(i int) string {
	h := d.Hours[i]
	if h == nil || h[0] >= h[1] {
		return "closed"
	}
	return clockText(h[0]) + "-" + clockText(h[1])
}

// hoursMap groups consecutive days with the same hours: mon-fri, sat-sun.
func (d *draft) hoursMap() map[string]string {
	out := map[string]string{}
	for _, g := range d.hourGroups() {
		out[g[0]] = g[1]
	}
	return out
}

func (d *draft) hourGroups() [][2]string {
	var out [][2]string
	for i := 0; i < 7; {
		j := i
		for j+1 < 7 && d.dayText(j+1) == d.dayText(i) {
			j++
		}
		key := dayKeys[i]
		if j > i {
			key += "-" + dayKeys[j]
		}
		out = append(out, [2]string{key, d.dayText(i)})
		i = j + 1
	}
	return out
}

func q(s string) string { return strconv.Quote(s) }

func (d *draft) render() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# Конфигурация court-gateway. Черновик собран командой setup %s.", d.Now.UTC().Format("2006-01-02"))
	w("# Строки с пометкой «проверьте» сверьте с CRM. Секреты (пароль CRM, секрет хаба) — в файле .env рядом.")
	w("# После правок: court-gateway doctor")
	w("")
	w("club:")
	w("  id: %s", d.ClubID)
	w("  name: %s", q(d.ClubName))
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
		w("  studio: %s", d.Studio)
		if d.AuthURL != "" {
			w("  auth_url: %s", d.AuthURL)
		}
		if d.APIURL != "" {
			w("  api_url: %s", d.APIURL)
		}
	}
	w("")
	w("venue:")
	w("  timezone: %s   # проверьте", d.Timezone)
	w("  slot_minutes: %d   # проверьте: шаг сетки бронирования, минут", d.Slot)
	if d.CRMType == "viva" {
		w("  hours:   # проверьте: собрано по границам правил прайс-листов; выходной — closed")
	} else {
		w("  hours:   # проверьте; выходной — closed")
	}
	for _, g := range d.hourGroups() {
		w("    %s: %s", g[0], q(g[1]))
	}
	w("")
	if d.CRMType == "viva" {
		w("# Корты — комнаты Viva, в которых за 28 дней вперёд были брони. Корт без броней за это время")
		w("# сюда НЕ попал: добавьте его вручную (id, name, viva_room). Проверьте названия.")
		if len(d.PriceLists) > 1 {
			w("# Прайс-листы студии (впишите нужный корту в price_list):")
			for _, p := range d.PriceLists {
				w("#   %s  %s", p.ID, q(p.Name))
			}
		}
	}
	w("courts:")
	for _, c := range d.Courts {
		w("  - id: %s", c.ID)
		w("    name: %s   # проверьте", q(c.Name))
		if c.Room != "" {
			w("    viva_room: %s   # броней за 28 дней: %d", c.Room, c.Bookings)
		}
		if c.PriceList != "" {
			w("    price_list: %s   # проверьте", c.PriceList)
		}
	}
	if d.CRMType == "viva" {
		w("")
		w("# Услуга Viva, которой создаётся бронь партнёра. Без этого раздела бронь создаётся")
		w("# технической блокировкой корта. Номера direction и type возьмите в настройках услуг Viva.")
		w("# services:")
		w("#   booking: { direction: 1, type: 1 }")
		w("#   tournament: { direction: 1, type: 2 }")
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
