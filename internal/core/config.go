package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"court-gateway/internal/auth"
	"court-gateway/internal/driver"
)

// PasswordEnv is the environment variable holding the CRM password.
const PasswordEnv = "CRM_PASSWORD"

// DefaultListen is used when the config has no `listen`.
const DefaultListen = ":8080"

// DefaultWritesPerMinute is the per-key write limit when a partner entry
// does not set one.
const DefaultWritesPerMinute = 60

// minKeyLength is the minimum partner key length accepted from the
// environment.
const minKeyLength = 24

// fileConfig mirrors gateway.yaml. Driver-specific keys of `crm`, of each
// court and of `services` are kept as raw maps and handed to the driver.
type fileConfig struct {
	Club struct {
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
	} `yaml:"club"`
	Listen string `yaml:"listen"`
	CRM    struct {
		Type string         `yaml:"type"`
		Rest map[string]any `yaml:",inline"`
	} `yaml:"crm"`
	Venue struct {
		Timezone    string            `yaml:"timezone"`
		SlotMinutes int               `yaml:"slot_minutes"`
		Hours       map[string]string `yaml:"hours"`
	} `yaml:"venue"`
	Courts []struct {
		ID   string         `yaml:"id"`
		Name string         `yaml:"name"`
		Rest map[string]any `yaml:",inline"`
	} `yaml:"courts"`
	Services map[string]map[string]any `yaml:"services"`
	Partners []struct {
		Name            string   `yaml:"name"`
		KeyEnv          string   `yaml:"key_env"`
		Scopes          []string `yaml:"scopes"`
		WritesPerMinute *int     `yaml:"writes_per_minute"`
	} `yaml:"partners"`
}

// Court is a configured court as exposed by the API.
type Court struct {
	ID   string
	Name string
}

// DayHours are the opening hours of one weekday in minutes since local
// midnight; Close may be 1440.
type DayHours struct {
	Open  int
	Close int
}

// Settings is a validated configuration.
type Settings struct {
	ClubID       string
	ClubName     string
	Listen       string
	CRMType      string
	TimezoneName string
	Location     *time.Location
	SlotMinutes  int
	// Hours is indexed by ISO weekday (0 = Monday); nil means closed.
	Hours    [7]*DayHours
	Courts   []Court
	Partners []auth.Partner
	// Driver is the input for the driver factory.
	Driver driver.Config
	// Secrets are values that must never appear in logs.
	Secrets []string
}

// CourtByID returns a configured court.
func (s *Settings) CourtByID(id string) (Court, bool) {
	for _, c := range s.Courts {
		if c.ID == id {
			return c, true
		}
	}
	return Court{}, false
}

var (
	slugRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	partnerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	envNameRe     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// LoadFile reads and validates a config file.
func LoadFile(path string, getenv func(string) string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать файл конфигурации %s: %w", path, err)
	}
	return Load(data, getenv)
}

// Load parses and validates config bytes. All problems found are reported
// together, one per line, in Russian: the reader is a club administrator.
func Load(data []byte, getenv func(string) string) (*Settings, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("файл конфигурации пуст")
	}
	var fc fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("файл конфигурации пуст")
		}
		return nil, translateYAMLError(err)
	}

	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	s := &Settings{}

	// club
	s.ClubID = strings.TrimSpace(fc.Club.ID)
	s.ClubName = strings.TrimSpace(fc.Club.Name)
	switch {
	case s.ClubID == "":
		add("club.id: параметр обязателен (короткое имя латиницей, например my-club)")
	case !slugRe.MatchString(s.ClubID):
		add("club.id: «%s» — допустимы строчные латинские буквы, цифры и дефис, до 63 символов", s.ClubID)
	}
	if s.ClubName == "" {
		add("club.name: параметр обязателен (название клуба)")
	}

	// listen
	s.Listen = strings.TrimSpace(fc.Listen)
	if s.Listen == "" {
		s.Listen = DefaultListen
	}
	if _, port, err := net.SplitHostPort(s.Listen); err != nil {
		add("listen: «%s» — ожидается адрес вида :8080 или 127.0.0.1:8080", s.Listen)
	} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		add("listen: неверный порт «%s»", port)
	}

	// crm
	s.CRMType = strings.TrimSpace(fc.CRM.Type)
	if s.CRMType == "" {
		add("crm.type: параметр обязателен (тип CRM клуба, например viva)")
	}

	// venue
	s.TimezoneName = strings.TrimSpace(fc.Venue.Timezone)
	if s.TimezoneName == "" {
		add("venue.timezone: параметр обязателен (например Europe/Moscow)")
	} else if loc, err := time.LoadLocation(s.TimezoneName); err != nil {
		add("venue.timezone: неизвестный часовой пояс «%s» (пример: Europe/Moscow)", s.TimezoneName)
	} else {
		s.Location = loc
	}
	s.SlotMinutes = fc.Venue.SlotMinutes
	slotOK := true
	if s.SlotMinutes < 5 || s.SlotMinutes > 240 || 1440%s.SlotMinutes != 0 {
		add("venue.slot_minutes: %d — шаг сетки в минутах, от 5 до 240, и сутки должны делиться на него без остатка (например 30 или 60)", s.SlotMinutes)
		slotOK = false
	}
	if len(fc.Venue.Hours) == 0 {
		add("venue.hours: параметр обязателен (часы работы по дням недели, например mon-fri: \"07:00-23:00\")")
	} else {
		hours, herrs := parseHours(fc.Venue.Hours)
		errs = append(errs, herrs...)
		s.Hours = hours
		if slotOK && len(herrs) == 0 {
			for i, h := range hours {
				if h != nil && (h.Open%s.SlotMinutes != 0 || h.Close%s.SlotMinutes != 0) {
					add("venue.hours: часы работы в %s не совпадают с сеткой %d мин", dayNamesRu[i], s.SlotMinutes)
				}
			}
		}
	}

	// courts
	if len(fc.Courts) == 0 {
		add("courts: нужен хотя бы один корт")
	}
	seenCourt := map[string]bool{}
	for i, c := range fc.Courts {
		path := fmt.Sprintf("courts[%d]", i)
		id := strings.TrimSpace(c.ID)
		name := strings.TrimSpace(c.Name)
		switch {
		case id == "":
			add("%s.id: параметр обязателен (например court-1)", path)
		case !slugRe.MatchString(id):
			add("%s.id: «%s» — допустимы строчные латинские буквы, цифры и дефис", path, id)
		case seenCourt[id]:
			add("%s.id: корт «%s» указан дважды", path, id)
		}
		seenCourt[id] = true
		if name == "" {
			add("%s.name: параметр обязателен (название корта)", path)
		}
		s.Courts = append(s.Courts, Court{ID: id, Name: name})
		s.Driver.Courts = append(s.Driver.Courts, driver.CourtConfig{ID: id, Name: name, Params: driver.Params(c.Rest)})
	}

	// services
	s.Driver.Services = map[string]driver.Params{}
	for k, v := range fc.Services {
		if k != "booking" && k != "tournament" {
			add("services: неизвестная услуга «%s» (допустимы booking и tournament)", k)
			continue
		}
		if v == nil {
			continue
		}
		s.Driver.Services[k] = driver.Params(v)
	}

	// partners
	if len(fc.Partners) == 0 {
		add("partners: нужен хотя бы один партнёр с ключом доступа")
	}
	seenName := map[string]bool{}
	seenKey := map[string]string{}
	for i, p := range fc.Partners {
		path := fmt.Sprintf("partners[%d]", i)
		name := strings.TrimSpace(p.Name)
		switch {
		case name == "":
			add("%s.name: параметр обязателен", path)
		case !partnerNameRe.MatchString(name):
			add("%s.name: «%s» — допустимы строчные латинские буквы, цифры, дефис и подчёркивание, до 32 символов", path, name)
		case seenName[name]:
			add("%s.name: партнёр «%s» указан дважды", path, name)
		}
		seenName[name] = true
		var key string
		envName := strings.TrimSpace(p.KeyEnv)
		switch {
		case envName == "":
			add("%s.key_env: параметр обязателен — имя переменной окружения с ключом партнёра (например PARTNER_KEY_1)", path)
		case !envNameRe.MatchString(envName):
			add("%s.key_env: «%s» — имя переменной окружения пишется заглавными латинскими буквами, цифрами и _", path, envName)
		case envName == PasswordEnv:
			add("%s.key_env: переменная %s занята паролем CRM", path, PasswordEnv)
		default:
			key = strings.TrimSpace(getenv(envName))
			switch {
			case key == "":
				add("%s.key_env: переменная окружения %s не задана или пуста", path, envName)
			case len(key) < minKeyLength:
				add("%s.key_env: ключ в %s слишком короткий — нужно не меньше %d символов", path, envName, minKeyLength)
			default:
				if other, dup := seenKey[key]; dup {
					add("%s.key_env: у партнёров «%s» и «%s» одинаковый ключ", path, other, name)
				}
				seenKey[key] = name
				s.Secrets = append(s.Secrets, key)
			}
		}
		var scopes []auth.Scope
		if len(p.Scopes) == 0 {
			add("%s.scopes: укажите права: [read] — только сверка, [read, book] — сверка и бронирование", path)
		}
		for _, sc := range p.Scopes {
			switch auth.Scope(sc) {
			case auth.ScopeRead, auth.ScopeBook:
				scopes = append(scopes, auth.Scope(sc))
			default:
				add("%s.scopes: неизвестное право «%s» (допустимы read и book)", path, sc)
			}
		}
		rate := DefaultWritesPerMinute
		if p.WritesPerMinute != nil {
			rate = *p.WritesPerMinute
			if rate < 1 || rate > 6000 {
				add("%s.writes_per_minute: %d — от 1 до 6000", path, rate)
			}
		}
		s.Partners = append(s.Partners, auth.Partner{Name: name, Key: key, Scopes: scopes, WritesPerMinute: rate})
	}

	password := getenv(PasswordEnv)
	if password != "" {
		s.Secrets = append(s.Secrets, password)
	}

	if len(errs) > 0 {
		return nil, &ConfigError{Problems: errs}
	}

	s.Driver.Timezone = s.Location
	s.Driver.SlotMinutes = s.SlotMinutes
	s.Driver.CRM = driver.Params(fc.CRM.Rest)
	if s.Driver.CRM == nil {
		s.Driver.CRM = driver.Params{}
	}
	s.Driver.Password = password
	return s, nil
}

// ConfigError lists every problem found in a config file.
type ConfigError struct {
	Problems []string
}

func (e *ConfigError) Error() string {
	return "ошибки в конфигурации:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// BuildDriver runs the factory registered for the configured CRM type.
func BuildDriver(s *Settings, factories map[string]driver.Factory) (driver.Driver, error) {
	f, ok := factories[s.CRMType]
	if !ok {
		known := make([]string, 0, len(factories))
		for k := range factories {
			known = append(known, k)
		}
		return nil, &ConfigError{Problems: []string{fmt.Sprintf("crm.type: неизвестный тип CRM «%s» (поддерживаются: %s)", s.CRMType, strings.Join(sortedStrings(known), ", "))}}
	}
	d, err := f(s.Driver)
	if err != nil {
		var ce *ConfigError
		if errors.As(err, &ce) {
			return nil, err
		}
		return nil, &ConfigError{Problems: strings.Split(err.Error(), "\n")}
	}
	return d, nil
}

var (
	yamlUnknownRe = regexp.MustCompile(`line (\d+): field (\S+) not found in type`)
	yamlTypeRe    = regexp.MustCompile("line (\\d+): cannot unmarshal !!(\\w+) `?([^`]*)`? into (\\S+)")
)

func translateYAMLError(err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		var out []string
		for _, e := range te.Errors {
			if m := yamlUnknownRe.FindStringSubmatch(e); m != nil {
				out = append(out, fmt.Sprintf("строка %s: неизвестный параметр «%s» — проверьте написание", m[1], m[2]))
				continue
			}
			if m := yamlTypeRe.FindStringSubmatch(e); m != nil {
				out = append(out, fmt.Sprintf("строка %s: значение «%s» имеет неверный тип — ожидается %s", m[1], m[3], goTypeRu(m[4])))
				continue
			}
			out = append(out, "ошибка разбора: "+e)
		}
		return &ConfigError{Problems: out}
	}
	return &ConfigError{Problems: []string{"ошибка синтаксиса YAML: " + strings.TrimPrefix(err.Error(), "yaml: ")}}
}

func goTypeRu(t string) string {
	switch {
	case t == "string":
		return "строка"
	case strings.HasPrefix(t, "int") || strings.HasPrefix(t, "uint") || t == "*int":
		return "целое число"
	case strings.HasPrefix(t, "[]"):
		return "список"
	case strings.HasPrefix(t, "map") || strings.HasPrefix(t, "struct"):
		return "набор параметров"
	default:
		return t
	}
}
