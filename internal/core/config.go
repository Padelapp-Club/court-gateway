package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// PasswordEnv is the environment variable holding the CRM password.
const PasswordEnv = "CRM_PASSWORD"

// DefaultListen is used when the config has neither `listen` nor `hub`.
// With a hub the local port stays closed unless `listen` is set.
const DefaultListen = ":8080"

// DefaultHubSecretEnv holds the gateway's secret for the hub.
const DefaultHubSecretEnv = "GATEWAY_HUB_SECRET"

// DefaultHubPolls is the number of concurrent long-poll requests a gateway
// keeps open to the hub, i.e. how many partner requests it serves at once.
const DefaultHubPolls = 4

// StateDirEnv overrides state_dir (used by the container image).
const StateDirEnv = "GATEWAY_STATE_DIR"

// DefaultWritesPerMinute is the per-key write limit when a partner entry
// does not set one.
const DefaultWritesPerMinute = 60

// minKeyLength is the minimum partner key length accepted from the
// environment.
const minKeyLength = 24

// fileConfig mirrors gateway.yaml. Driver-specific keys of `crm`, of each
// court and of `services` are kept as raw maps and handed to the driver.
type fileConfig struct {
	Club *struct {
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
	} `yaml:"club"`
	Listen string `yaml:"listen"`
	Hub    *struct {
		URL       string `yaml:"url"`
		CAFile    string `yaml:"ca_file"`
		SecretEnv string `yaml:"secret_env"`
		Polls     int    `yaml:"polls"`
	} `yaml:"hub"`
	Update *struct {
		Enabled     *bool  `yaml:"enabled"`
		ManifestURL string `yaml:"manifest_url"`
	} `yaml:"update"`
	StateDir string `yaml:"state_dir"`
	CRM      struct {
		Type string         `yaml:"type"`
		Rest map[string]any `yaml:",inline"`
	} `yaml:"crm"`
	Venue *struct {
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
	Partners []PartnerEntry            `yaml:"partners"`
}

// PartnerEntry is a partner as gateway.yaml and hub.yaml list it.
type PartnerEntry struct {
	Name            string   `yaml:"name"`
	KeyEnv          string   `yaml:"key_env"`
	Scopes          []string `yaml:"scopes"`
	WritesPerMinute *int     `yaml:"writes_per_minute"`
}

// ParsePartners validates partner entries and reads their keys with
// getenv; problems go to add, in Russian. It returns the partners and
// their keys (secrets for the log redaction).
func ParsePartners(entries []PartnerEntry, getenv func(string) string, add func(string, ...any)) ([]auth.Partner, []string) {
	var out []auth.Partner
	var keys []string
	seenName := map[string]bool{}
	seenKey := map[string]string{}
	for i, p := range entries {
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
				keys = append(keys, key)
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
		out = append(out, auth.Partner{Name: name, Key: key, Scopes: scopes, WritesPerMinute: rate})
	}
	return out, keys
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

// HubSettings configure the outbound connection to the hub.
type HubSettings struct {
	URL    string
	CAFile string
	Secret string
	Polls  int
}

// UpdateSettings configure automatic updates.
type UpdateSettings struct {
	// Enabled is false only when the config says so; a build without an
	// embedded release key never updates regardless.
	Enabled     bool
	ManifestURL string
}

// Settings is a validated configuration: the installation (CRM type and
// login, hub, partners, local port) and, without a hub, the one club of
// gateway.yaml. With a hub gateway.yaml has no club: the clubs of the
// installation are configured on the hub (see ClubSpec and WithClub).
type Settings struct {
	ClubID   string
	ClubName string
	// Listen is the local HTTP address; empty means no local port.
	Listen string
	// Hub is nil when the gateway is not connected to a hub.
	Hub    *HubSettings
	Update UpdateSettings
	// StateDir holds the health status file and update bookkeeping. It
	// never holds CRM data.
	StateDir     string
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

	stateDirDefaulted bool
	// installCRM is the crm section of gateway.yaml without type; a club's
	// own crm keys go over it.
	installCRM driver.Params
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
	s, err := Load(data, getenv)
	if err != nil {
		return nil, err
	}
	if s.stateDirDefaulted {
		s.StateDir = defaultStateDir(path, s.ClubID)
	}
	return s, nil
}

// defaultStateDir is a "state" directory next to the config file: stable
// for every process that reads the same config (service, healthcheck,
// doctor), unlike the per-user temporary directory.
func defaultStateDir(cfgPath, club string) string {
	if abs, err := filepath.Abs(cfgPath); err == nil {
		return filepath.Join(filepath.Dir(abs), "state")
	}
	return filepath.Join(os.TempDir(), tempStateName(club))
}

func tempStateName(club string) string {
	if club == "" {
		return "court-gateway"
	}
	return "court-gateway-" + club
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

	// hub
	if fc.Hub != nil {
		h := &HubSettings{URL: strings.TrimSpace(fc.Hub.URL), CAFile: strings.TrimSpace(fc.Hub.CAFile), Polls: fc.Hub.Polls}
		if h.URL == "" {
			add("hub.url: параметр обязателен (адрес хаба, например https://hub.example.com)")
		} else if err := CheckHTTPSURL("hub.url", h.URL); err != nil {
			add("%s", err.Error())
		}
		if h.Polls == 0 {
			h.Polls = DefaultHubPolls
		}
		if h.Polls < 1 || h.Polls > 32 {
			add("hub.polls: %d — от 1 до 32", h.Polls)
		}
		envName := strings.TrimSpace(fc.Hub.SecretEnv)
		if envName == "" {
			envName = DefaultHubSecretEnv
		}
		if !envNameRe.MatchString(envName) {
			add("hub.secret_env: «%s» — имя переменной окружения пишется заглавными латинскими буквами, цифрами и _", envName)
		} else if h.Secret = strings.TrimSpace(getenv(envName)); h.Secret == "" {
			add("hub: переменная окружения %s с секретом подключения не задана — выполните court-gateway setup с кодом подключения", envName)
		} else {
			s.Secrets = append(s.Secrets, h.Secret)
		}
		s.Hub = h
	}

	// listen
	s.Listen = strings.TrimSpace(fc.Listen)
	if s.Listen == "" && s.Hub == nil {
		s.Listen = DefaultListen
	}
	if s.Listen != "" {
		if _, port, err := net.SplitHostPort(s.Listen); err != nil {
			add("listen: «%s» — ожидается адрес вида :8080 или 127.0.0.1:8080", s.Listen)
		} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			add("listen: неверный порт «%s»", port)
		}
	}

	// update
	s.Update = UpdateSettings{Enabled: true}
	if fc.Update != nil {
		if fc.Update.Enabled != nil {
			s.Update.Enabled = *fc.Update.Enabled
		}
		s.Update.ManifestURL = strings.TrimSpace(fc.Update.ManifestURL)
		if s.Update.ManifestURL != "" {
			if err := CheckHTTPSURL("update.manifest_url", s.Update.ManifestURL); err != nil {
				add("%s", err.Error())
			}
		}
	}

	// state_dir
	s.StateDir = strings.TrimSpace(getenv(StateDirEnv))
	if s.StateDir == "" {
		s.StateDir = strings.TrimSpace(fc.StateDir)
	}

	// crm
	s.CRMType = strings.TrimSpace(fc.CRM.Type)
	if s.CRMType == "" {
		add("crm.type: параметр обязателен (тип CRM клуба)")
	}

	// the club: in gateway.yaml without a hub; with a hub every club comes
	// from the hub (WithClub)
	s.installCRM = driver.Params(fc.CRM.Rest)
	if s.installCRM == nil {
		s.installCRM = driver.Params{}
	}
	if s.Hub == nil {
		spec := ClubSpec{Services: fc.Services}
		if fc.Club != nil {
			spec.ID, spec.Name = fc.Club.ID, fc.Club.Name
		}
		if fc.Venue != nil {
			spec.Timezone, spec.SlotMinutes, spec.Hours = fc.Venue.Timezone, fc.Venue.SlotMinutes, fc.Venue.Hours
		}
		for _, c := range fc.Courts {
			spec.Courts = append(spec.Courts, CourtSpec{ID: c.ID, Name: c.Name, Params: c.Rest})
		}
		errs = append(errs, applyClub(s, spec, filePaths)...)
	} else {
		if fc.Club != nil || fc.Venue != nil || len(fc.Courts) > 0 || fc.Services != nil {
			add("club, venue, courts, services: с хабом клубы настраивает оператор на хабе — уберите эти разделы из gateway.yaml")
		}
		s.Driver.CRM = driver.Params{}
		for k, v := range s.installCRM {
			s.Driver.CRM[k] = v
		}
	}

	// partners: with a hub, partner keys live on the hub; a local list is
	// needed only to open the API on the local port.
	if len(fc.Partners) == 0 && s.Hub == nil {
		add("partners: нужен хотя бы один партнёр с ключом доступа (или раздел hub — тогда ключи партнёров хранит хаб)")
	}
	partners, keys := ParsePartners(fc.Partners, getenv, add)
	s.Partners, s.Secrets = partners, append(s.Secrets, keys...)

	password := getenv(PasswordEnv)
	if password != "" {
		s.Secrets = append(s.Secrets, password)
	}

	if len(errs) > 0 {
		return nil, &ConfigError{Problems: errs}
	}
	if s.StateDir == "" {
		s.stateDirDefaulted = true
		s.StateDir = filepath.Join(os.TempDir(), tempStateName(s.ClubID))
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

// CheckHTTPSURL accepts https URLs, and plain http only for loopback hosts
// (local test doubles and development).
func CheckHTTPSURL(path, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: «%s» — неверный адрес", path, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("%s: «%s» — адрес должен начинаться с https://", path, raw)
}

// StatePath returns the health status file and update bookkeeping
// directory derived from a config file without loading secrets. It is what
// `healthcheck` uses, so it must agree with Load.
func StatePath(cfgPath string, getenv func(string) string) (stateDir, listen string, hub bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var c struct {
		Club struct {
			ID string `yaml:"id"`
		} `yaml:"club"`
		Listen   string    `yaml:"listen"`
		StateDir string    `yaml:"state_dir"`
		Hub      yaml.Node `yaml:"hub"`
	}
	if data, err := os.ReadFile(cfgPath); err == nil {
		_ = yaml.Unmarshal(data, &c)
	}
	hub = !c.Hub.IsZero()
	listen = strings.TrimSpace(c.Listen)
	if listen == "" && !hub {
		listen = DefaultListen
	}
	stateDir = strings.TrimSpace(getenv(StateDirEnv))
	if stateDir == "" {
		stateDir = strings.TrimSpace(c.StateDir)
	}
	if stateDir == "" {
		stateDir = defaultStateDir(cfgPath, strings.TrimSpace(c.Club.ID))
	}
	return stateDir, listen, hub
}
