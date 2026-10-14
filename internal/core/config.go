package core

import (
	"bytes"
	"cmp"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// PasswordEnv is the environment variable holding the CRM password.
const PasswordEnv = "CRM_PASSWORD"

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

// MinKeyLength is the minimum length of a partner or operator key taken
// from the environment.
const MinKeyLength = 24

// fileConfig mirrors gateway.yaml: the installation only (its clubs are on
// the hub); `crm` driver keys are handed to the driver raw.
type fileConfig struct {
	Hub *struct {
		URL          string `yaml:"url"`
		Installation string `yaml:"installation"`
		Key          string `yaml:"key"`
		CAFile       string `yaml:"ca_file"`
		SecretEnv    string `yaml:"secret_env"`
		Polls        int    `yaml:"polls"`
		ProxyURL     string `yaml:"proxy_url"`
		NoProxy      string `yaml:"no_proxy"`
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
}

// PartnerEntry is a partner as hub.yaml lists it.
type PartnerEntry struct {
	Name            string   `yaml:"name"`
	KeyEnv          string   `yaml:"key_env"`
	Scopes          []string `yaml:"scopes"`
	WritesPerMinute *int     `yaml:"writes_per_minute"`
}

// ParsePartners validates partner entries and reads their keys with getenv
// (problems to add, in Russian); it returns them and the keys to redact.
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
			case len(key) < MinKeyLength:
				add("%s.key_env: ключ в %s слишком короткий — нужно не меньше %d символов", path, envName, MinKeyLength)
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

// HubSettings configure the outbound connection to the hub.
type HubSettings struct {
	URL string
	// Installation and Key (the hub's public signing key) are pinned at
	// registration: the gateway acts only on what that key signed for it.
	Installation string
	Key          ed25519.PublicKey
	CAFile       string
	Secret       string
	// Proxy, when set, routes every outbound request (ProxyFunc).
	Proxy ProxyFunc
	Polls int
}

// UpdateSettings configure automatic updates.
type UpdateSettings struct {
	// Enabled is false only when the config says so; a build without an
	// embedded release key never updates regardless.
	Enabled     bool
	ManifestURL string
}

// Settings is a validated configuration: the installation and, after
// WithClub, one club configured on the hub.
type Settings struct {
	ClubID   string
	ClubName string
	Hub      *HubSettings
	Update   UpdateSettings
	// StateDir holds the health status file and update bookkeeping. It
	// never holds CRM data.
	StateDir     string
	CRMType      string
	TimezoneName string
	Location     *time.Location
	SlotMinutes  int
	// Hours is indexed by ISO weekday (0 = Monday); nil means closed.
	Hours  [7]*[2]int // per ISO weekday: opening and closing minute (up to 1440); nil closed
	Courts []Court
	// Driver is the input for the driver factory.
	Driver driver.Config
	// Secrets are values that must never appear in logs.
	Secrets []string

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
	slugRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
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
	s.StateDir = StateDir(path, getenv)
	return s, nil
}

// StateDir is the state directory of the config at cfgPath, read without
// secrets: $GATEWAY_STATE_DIR, else state_dir, else "state" next to it.
func StateDir(cfgPath string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if d := strings.TrimSpace(getenv(StateDirEnv)); d != "" {
		return d
	}
	var c struct {
		StateDir string `yaml:"state_dir"`
	}
	if data, err := os.ReadFile(cfgPath); err == nil {
		_ = yaml.Unmarshal(data, &c)
	}
	if d := strings.TrimSpace(c.StateDir); d != "" {
		return d
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		abs = cfgPath
	}
	return filepath.Join(filepath.Dir(abs), "state")
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

	// hub: the only way partners reach a gateway
	if fc.Hub == nil {
		add("hub: раздел обязателен — шлюз работает только через хаб; выполните court-gateway setup с кодом подключения")
	} else {
		h := &HubSettings{URL: strings.TrimSpace(fc.Hub.URL), CAFile: strings.TrimSpace(fc.Hub.CAFile), Polls: fc.Hub.Polls}
		if h.URL == "" {
			add("hub.url: параметр обязателен (адрес хаба, например https://hub.example.com)")
		} else if err := CheckHTTPSURL("hub.url", h.URL); err != nil {
			add("%s", err.Error())
		}
		if h.Installation = strings.TrimSpace(fc.Hub.Installation); !ValidID(h.Installation) {
			add("hub.installation: параметр обязателен (идентификатор установки) — выполните court-gateway setup с новым кодом подключения")
		}
		if k, err := ParseHubKey(fc.Hub.Key); err != nil {
			add("hub.key: открытый ключ хаба отсутствует или неверен — выполните court-gateway setup с новым кодом подключения")
		} else {
			h.Key = k
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
		// a corporate proxy: gateway.yaml, else HTTPS_PROXY / NO_PROXY
		// (also from .env, which a service reads and a shell export is not)
		if raw := cmp.Or(strings.TrimSpace(fc.Hub.ProxyURL), getenv("HTTPS_PROXY"), getenv("https_proxy")); raw != "" {
			p, secrets, err := parseProxy(raw, cmp.Or(fc.Hub.NoProxy, getenv("NO_PROXY"), getenv("no_proxy")))
			if err != nil {
				add("hub.proxy_url: %v", err)
			}
			s.Secrets = append(s.Secrets, secrets...)
			h.Proxy, s.Driver.Proxy = p, p
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

	// state_dir (LoadFile defaults it to the config's directory)
	s.StateDir = cmp.Or(strings.TrimSpace(getenv(StateDirEnv)), strings.TrimSpace(fc.StateDir))

	// crm
	s.CRMType = strings.TrimSpace(fc.CRM.Type)
	if s.CRMType == "" {
		add("crm.type: параметр обязателен (тип CRM клуба)")
	}

	// every club comes from the hub (WithClub)
	s.installCRM = driver.Params(fc.CRM.Rest)
	if s.installCRM == nil {
		s.installCRM = driver.Params{}
	}
	s.Driver.CRM = maps.Clone(s.installCRM)

	password := getenv(PasswordEnv)
	if password != "" {
		s.Secrets = append(s.Secrets, password)
	}

	if len(errs) > 0 {
		return nil, &ConfigError{Problems: errs}
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
		known := slices.Sorted(maps.Keys(factories))
		return nil, &ConfigError{Problems: []string{fmt.Sprintf("crm.type: неизвестный тип CRM «%s» (поддерживаются: %s)", s.CRMType, strings.Join(known, ", "))}}
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

// BuildAccount runs the account factory registered for the configured
// CRM type on cfg (normally s.Driver). Errors are *ConfigError.
func BuildAccount(s *Settings, accounts map[string]driver.AccountFactory, cfg driver.Config) (driver.Account, error) {
	f, ok := accounts[s.CRMType]
	if !ok {
		return nil, &ConfigError{Problems: []string{fmt.Sprintf("crm.type: неизвестный тип CRM «%s»", s.CRMType)}}
	}
	a, err := f(cfg)
	if err != nil {
		return nil, &ConfigError{Problems: strings.Split(err.Error(), "\n")}
	}
	return a, nil
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

// EncodeHubKey writes the hub's public signing key as gateway.yaml and the
// registration answer carry it.
func EncodeHubKey(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }

// ParseHubKey reads a key written by EncodeHubKey.
func ParseHubKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not an ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// HubKeyFingerprint checks the hub's key out of band (shown with the join
// code): the key's SHA-256, first 16 bytes, base32 in groups of four.
func HubKeyFingerprint(k ed25519.PublicKey) string {
	sum := sha256.Sum256(k)
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16])
	var groups []string
	for ; len(s) > 4; s = s[4:] {
		groups = append(groups, s[:4])
	}
	return strings.Join(append(groups, s), "-")
}

// NormalizeFingerprint upper-cases a typed fingerprint and drops the
// dashes and spaces; "" when it is not a fingerprint's 26 base32 letters.
func NormalizeFingerprint(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
	if len(s) != 26 || strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
		return ""
	}
	return s
}

// CheckHTTPSURL accepts https URLs, plain http only for loopback hosts, and
// neither user, password (they would reach files and logs) nor fragment;
// an error never repeats such a URL.
func CheckHTTPSURL(path, raw string) error {
	u, err := url.Parse(raw)
	switch {
	case strings.Contains(raw, "@") && (err != nil || u.User != nil):
		return fmt.Errorf("%s: в адресе не должно быть имени пользователя и пароля (…@)", path)
	case err != nil || u.Host == "":
		return fmt.Errorf("%s: «%s» — неверный адрес", path, raw)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("%s: «%s» — в адресе не должно быть «#»", path, raw)
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
