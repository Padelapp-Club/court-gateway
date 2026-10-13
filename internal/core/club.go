package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// ClubSpec is the configuration of one club as the hub stores it per
// installation (docs/hub.md shows the JSON). Driver-specific keys (the
// club's part of the crm section, court entries, services) are raw maps
// handed to the driver.
type ClubSpec struct {
	ID          string                    `json:"id,omitempty"`
	Name        string                    `json:"name"`
	Timezone    string                    `json:"timezone"`
	SlotMinutes int                       `json:"slot_minutes"`
	Hours       map[string]string         `json:"hours"`
	CRM         map[string]any            `json:"crm,omitempty"`
	Courts      []CourtSpec               `json:"courts"`
	Services    map[string]map[string]any `json:"services,omitempty"`
}

// CourtSpec is one court; in JSON its driver keys sit next to id and name.
type CourtSpec struct {
	ID     string
	Name   string
	Params map[string]any
}

// MarshalJSON writes the court flat: {"id", "name", driver keys...}.
func (c CourtSpec) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(c.Params)+2)
	for k, v := range c.Params {
		m[k] = v
	}
	m["id"], m["name"] = c.ID, c.Name
	return json.Marshal(m)
}

// UnmarshalJSON reads a flat court entry.
func (c *CourtSpec) UnmarshalJSON(data []byte) error {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if m == nil {
		return errors.New("court entry must be an object")
	}
	var ok bool
	if v, has := m["id"]; has {
		if c.ID, ok = v.(string); !ok {
			return errors.New("court id must be a string")
		}
	}
	if v, has := m["name"]; has {
		if c.Name, ok = v.(string); !ok {
			return errors.New("court name must be a string")
		}
	}
	delete(m, "id")
	delete(m, "name")
	c.Params = normalizeMap(m)
	return nil
}

// UnmarshalJSON decodes a club; JSON numbers in driver keys that are whole
// become int, as the YAML decoder gives them to drivers.
func (c *ClubSpec) UnmarshalJSON(data []byte) error {
	type plain ClubSpec
	var p plain
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	p.CRM = normalizeMap(p.CRM)
	for k, v := range p.Services {
		p.Services[k] = normalizeMap(v)
	}
	*c = ClubSpec(p)
	return nil
}

// DecodeClubTolerant decodes a club as a gateway receives it from the
// hub: top-level fields this version does not know (a newer hub) are left
// out and returned by name, the rest is decoded as strictly as the hub's
// PUT decodes it.
func DecodeClubTolerant(data []byte) (ClubSpec, []string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return ClubSpec{}, nil, err
	}
	var unknown []string
	for k := range fields {
		if !slices.Contains(clubFields, k) {
			unknown = append(unknown, k)
			delete(fields, k)
		}
	}
	known, _ := json.Marshal(fields)
	var c ClubSpec
	err := json.Unmarshal(known, &c)
	slices.Sort(unknown)
	return c, unknown, err
}

// clubFields are the JSON names of ClubSpec's fields.
var clubFields = func() []string {
	var out []string
	t := reflect.TypeFor[ClubSpec]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		out = append(out, name)
	}
	return out
}()

func normalizeMap(m map[string]any) map[string]any {
	for k, v := range m {
		m[k] = normalize(v)
	}
	return m
}

func normalize(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && x >= math.MinInt32 && x <= math.MaxInt32 {
			return int(x)
		}
		return x
	case map[string]any:
		return normalizeMap(x)
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
		return x
	default:
		return v
	}
}

// ValidID reports whether id is a valid id of an installation, a club or a
// court: the one id rule of the gateway and the hub.
func ValidID(id string) bool { return slugRe.MatchString(id) }

// applyClub validates c and writes it into s (whose installation part is
// already filled). crmKeys are the keys c.CRM may set: a club from the hub
// may set its own keys (the studio) but never the installation's (the
// login, the CRM endpoints), or the hub could send the CRM password to a
// host of its choice. It returns the problems found, in Russian.
func applyClub(s *Settings, c ClubSpec, crmKeys []string) []string {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	s.ClubID = strings.TrimSpace(c.ID)
	s.ClubName = strings.TrimSpace(c.Name)
	switch {
	case s.ClubID == "":
		add("id: параметр обязателен (короткое имя латиницей, например my-club)")
	case !slugRe.MatchString(s.ClubID):
		add("id: «%s» — от 2 до 63 символов: строчные латинские буквы, цифры и дефис", s.ClubID)
	}
	if s.ClubName == "" {
		add("name: параметр обязателен (название клуба)")
	}

	s.TimezoneName = strings.TrimSpace(c.Timezone)
	s.Location = nil
	if s.TimezoneName == "" {
		add("timezone: параметр обязателен (например Europe/Moscow)")
	} else if loc, err := time.LoadLocation(s.TimezoneName); err != nil {
		add("timezone: неизвестный часовой пояс «%s» (пример: Europe/Moscow)", s.TimezoneName)
	} else {
		s.Location = loc
	}
	s.SlotMinutes = c.SlotMinutes
	slotOK := true
	if s.SlotMinutes < 5 || s.SlotMinutes > 240 || 1440%s.SlotMinutes != 0 {
		add("slot_minutes: %d — шаг сетки в минутах, от 5 до 240, и сутки должны делиться на него без остатка (например 30 или 60)", s.SlotMinutes)
		slotOK = false
	}
	s.Hours = [7]*DayHours{}
	if len(c.Hours) == 0 {
		add("hours: параметр обязателен (часы работы по дням недели, например mon-fri: \"07:00-23:00\")")
	} else {
		hours, herrs := parseHours("hours", c.Hours)
		errs = append(errs, herrs...)
		s.Hours = hours
		if slotOK && len(herrs) == 0 {
			for i, h := range hours {
				if h != nil && (h.Open%s.SlotMinutes != 0 || h.Close%s.SlotMinutes != 0) {
					add("hours: часы работы в %s не совпадают с сеткой %d мин", dayNamesRu[i], s.SlotMinutes)
				}
			}
		}
	}

	s.Courts, s.Driver.Courts = nil, nil
	if len(c.Courts) == 0 {
		add("courts: нужен хотя бы один корт")
	}
	seenCourt := map[string]bool{}
	for i, ct := range c.Courts {
		path := fmt.Sprintf("courts[%d]", i)
		id := strings.TrimSpace(ct.ID)
		name := strings.TrimSpace(ct.Name)
		switch {
		case id == "":
			add("%s.id: параметр обязателен (например court-1)", path)
		case !slugRe.MatchString(id):
			add("%s.id: «%s» — от 2 до 63 символов: строчные латинские буквы, цифры и дефис", path, id)
		case seenCourt[id]:
			add("%s.id: корт «%s» указан дважды", path, id)
		}
		seenCourt[id] = true
		if name == "" {
			add("%s.name: параметр обязателен (название корта)", path)
		}
		s.Courts = append(s.Courts, Court{ID: id, Name: name})
		s.Driver.Courts = append(s.Driver.Courts, driver.CourtConfig{ID: id, Name: name, Params: driver.Params(ct.Params)})
	}

	s.Driver.Services = map[string]driver.Params{}
	for k, v := range c.Services {
		if k != "booking" && k != "tournament" {
			add("services: неизвестная услуга «%s» (допустимы booking и tournament)", k)
			continue
		}
		if v == nil {
			continue
		}
		s.Driver.Services[k] = driver.Params(v)
	}

	// the club's part of the crm section goes over the installation's
	crm := maps.Clone(s.installCRM)
	if crm == nil {
		crm = driver.Params{}
	}
	for _, k := range slices.Sorted(maps.Keys(c.CRM)) {
		if !slices.Contains(crmKeys, k) {
			add("crm.%s: задаётся только в gateway.yaml установки, не в клубе (клуб может задать: %s)", k, strings.Join(crmKeys, ", "))
			continue
		}
		crm[k] = c.CRM[k]
	}
	s.Driver.CRM = crm
	s.Driver.Timezone = s.Location
	return errs
}

// WithClub returns the settings of one club served by this installation:
// the installation part of base (CRM type and login, the hub) with
// the club spec applied. crmKeys are the crm keys a club may set (see
// drivers.ClubCRMKeys). It never modifies base.
func (base *Settings) WithClub(c ClubSpec, crmKeys []string) (*Settings, error) {
	s := *base
	s.Driver.Courts, s.Driver.Services = nil, nil
	if errs := applyClub(&s, c, crmKeys); len(errs) > 0 {
		id := strings.TrimSpace(c.ID)
		for i := range errs {
			errs[i] = "клуб «" + id + "»: " + errs[i]
		}
		return nil, &ConfigError{Problems: errs}
	}
	return &s, nil
}

// ValidateClubSpec checks a club spec as far as the hub can: everything
// CRM-agnostic, and that the crm section holds only club-level keys
// (crmKeys); the rest of the driver keys are checked by the gateway, which
// knows its CRM.
func ValidateClubSpec(c ClubSpec, crmKeys []string) error {
	s := &Settings{}
	if errs := applyClub(s, c, crmKeys); len(errs) > 0 {
		return &ConfigError{Problems: errs}
	}
	return nil
}

// ---- drafts ----

// DraftClub turns discovered CRM material into a club spec draft. It is a
// draft: the id is left to the operator, the grid may be a default, and
// every guess comes with a notice. When the CRM tells the time zone it is
// the club's: an asked zone with another offset from UTC is replaced, with
// a notice (the gateway would refuse such a club).
func DraftClub(d driver.Discovery, timezone string, slot int) (ClubSpec, []driver.Notice) {
	notes := append([]driver.Notice(nil), d.Notices...)
	switch {
	case d.Timezone != "" && timezone != "" && !sameOffsetNow(timezone, d.Timezone):
		notes = append(notes, driver.Notice{Code: "TIMEZONE_FROM_CRM", Message: fmt.Sprintf("часовой пояс %s не подходит: CRM считает дни студии по %s — он и подставлен", timezone, d.Timezone)})
		timezone = d.Timezone
	case timezone != "":
	case d.Timezone != "":
		timezone = d.Timezone
		notes = append(notes, driver.Notice{Code: "TIMEZONE_FROM_CRM", Message: fmt.Sprintf("часовой пояс %s выведен из смещения студии от UTC — при желании замените на зону города с тем же смещением (например Europe/Moscow)", timezone)})
	default:
		timezone = "Europe/Moscow"
		notes = append(notes, driver.Notice{Code: "TIMEZONE_DEFAULTED", Message: "часовой пояс Europe/Moscow подставлен по умолчанию — проверьте"})
	}
	if slot <= 0 {
		slot = 30
		notes = append(notes, driver.Notice{Code: "SLOT_DEFAULTED", Message: "шаг сетки 30 мин подставлен по умолчанию — проверьте"})
	}
	spec := ClubSpec{Name: d.ClubName, Timezone: timezone, SlotMinutes: slot, CRM: map[string]any{}}
	for k, v := range d.CRM {
		spec.CRM[k] = v
	}
	for i, c := range d.Courts {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = fmt.Sprintf("Корт %d", i+1)
		}
		params := map[string]any{}
		for k, v := range c.Params {
			params[k] = v
		}
		spec.Courts = append(spec.Courts, CourtSpec{ID: fmt.Sprintf("court-%d", i+1), Name: name, Params: params})
	}
	hours := d.Hours
	empty := true
	for _, h := range hours {
		if h != nil {
			empty = false
		}
	}
	if empty {
		for i := range hours {
			hours[i] = &[2]int{7 * 60, 23 * 60}
		}
		notes = append(notes, driver.Notice{Code: "HOURS_DEFAULTED", Message: "часы работы не удалось вывести: подставлено 07:00–23:00 ежедневно — проверьте"})
	} else if roundHours(&hours, slot) {
		notes = append(notes, driver.Notice{Code: "HOURS_ROUNDED", Message: fmt.Sprintf("часы работы округлены до сетки %d мин — проверьте", slot)})
	}
	spec.Hours = HoursMap(hours)
	return spec, notes
}

// sameOffsetNow reports whether two zones are as far from UTC right now.
func sameOffsetNow(a, b string) bool {
	la, err1 := time.LoadLocation(a)
	lb, err2 := time.LoadLocation(b)
	if err1 != nil || err2 != nil {
		return false
	}
	now := time.Now()
	_, oa := now.In(la).Zone()
	_, ob := now.In(lb).Zone()
	return oa == ob
}

// roundHours widens the hours to the slot grid; true if anything changed.
func roundHours(hours *[7]*[2]int, slot int) bool {
	changed := false
	for i, h := range hours {
		if h == nil {
			continue
		}
		o := h[0] / slot * slot
		c := (h[1] + slot - 1) / slot * slot
		if c > 1440 {
			c = 1440
		}
		if o != h[0] || c != h[1] {
			hours[i] = &[2]int{o, c}
			changed = true
		}
	}
	return changed
}

func dayText(h *[2]int) string {
	if h == nil || h[0] >= h[1] {
		return "closed"
	}
	return formatClock(h[0]) + "-" + formatClock(h[1])
}

// HoursMap groups consecutive days with the same hours: mon-fri, sat-sun.
func HoursMap(hours [7]*[2]int) map[string]string {
	out := map[string]string{}
	for i := 0; i < 7; {
		j := i
		for j+1 < 7 && dayText(hours[j+1]) == dayText(hours[i]) {
			j++
		}
		key := dayKeys[i]
		if j > i {
			key += "-" + dayKeys[j]
		}
		out[key] = dayText(hours[i])
		i = j + 1
	}
	return out
}

// SortedClubIDs returns the ids of specs in order.
func SortedClubIDs(specs []ClubSpec) []string {
	out := make([]string, len(specs))
	for i, c := range specs {
		out[i] = c.ID
	}
	sort.Strings(out)
	return out
}
