// Package hub is the public side of the system: partners call it, gateways
// connect to it from the clubs (outbound only), and it watches every
// gateway with a heartbeat. The hub keeps a little state on disk — the
// gateways it has registered and the pending join codes — and nothing
// about any club's bookings. Gateways report their own health with a
// heartbeat; the hub turns that into status files for an external checker.
package hub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/version"
)

// Defaults.
const (
	DefaultListen         = ":8443"
	DefaultRequestTimeout = 30 * time.Second
	DefaultPollWait       = 25 * time.Second
	DefaultInterval       = 60 * time.Second
	DefaultOfflineAfter   = 3 * time.Minute
	DefaultReadyFailures  = 3
	minKeyLength          = 24
)

type fileConfig struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url"`
	TLS       struct {
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
	} `yaml:"tls"`
	StateDir       string   `yaml:"state_dir"`
	StatusDir      string   `yaml:"status_dir"`
	TrustedProxies []string `yaml:"trusted_proxies"`
	OperatorKeyEnv string   `yaml:"operator_key_env"`
	LatestVersion  string   `yaml:"latest_version"`
	RequestTimeout string   `yaml:"request_timeout"`
	PollWait       string   `yaml:"poll_wait"`
	Partners       []struct {
		core.PartnerEntry `yaml:",inline"`
		Clubs             []string `yaml:"clubs"`
	} `yaml:"partners"`
	Heartbeat struct {
		Interval      string `yaml:"interval"`
		OfflineAfter  string `yaml:"offline_after"`
		ReadyFailures int    `yaml:"ready_failures"`
	} `yaml:"heartbeat"`
}

// Settings is a validated hub configuration.
type Settings struct {
	Listen      string
	PublicURL   string
	TLSCertFile string
	TLSKeyFile  string
	StateDir    string
	// StatusDir receives status.json and metrics for an external checker;
	// empty means the files are not written.
	StatusDir   string
	OperatorKey string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For names
	// the client; from anyone else the header is ignored.
	TrustedProxies []netip.Prefix
	LatestVersion  string
	RequestTimeout time.Duration
	PollWait       time.Duration
	Partners       []auth.Partner
	// PartnerClubs lists the clubs each partner may reach; "*" is all.
	PartnerClubs  map[string][]string
	Interval      time.Duration
	OfflineAfter  time.Duration
	ReadyFailures int
	// Secrets must never appear in logs.
	Secrets []string

	// publicPath is the path of PublicURL ("" at the root of its host):
	// the prefix of every path partners see.
	publicPath string
}

// ValidClubID reports whether id is a valid club (or installation) id.
func ValidClubID(id string) bool { return core.ValidID(id) }

// LoadFile reads hub.yaml; secrets come from getenv, which callers usually
// extend with the .env file next to hub.yaml.
func LoadFile(path string, getenv func(string) string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать файл конфигурации хаба %s: %w", path, err)
	}
	return Load(data, getenv)
}

// Load parses and validates hub.yaml.
func Load(data []byte, getenv func(string) string) (*Settings, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var fc fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) {
		return nil, &core.ConfigError{Problems: []string{"ошибка разбора hub.yaml: " + strings.TrimPrefix(err.Error(), "yaml: ")}}
	}
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	s := &Settings{PartnerClubs: map[string][]string{}}

	s.Listen = strings.TrimSpace(fc.Listen)
	if s.Listen == "" {
		s.Listen = DefaultListen
	}
	if _, port, err := net.SplitHostPort(s.Listen); err != nil {
		add("listen: «%s» — ожидается адрес вида :8443", s.Listen)
	} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		add("listen: неверный порт «%s»", port)
	}
	s.PublicURL = strings.TrimRight(strings.TrimSpace(fc.PublicURL), "/")
	if s.PublicURL != "" {
		if err := core.CheckHTTPSURL("public_url", s.PublicURL); err != nil {
			add("%s", err.Error())
		} else if u, err := url.Parse(s.PublicURL); err == nil {
			s.publicPath = strings.TrimRight(u.EscapedPath(), "/")
		}
	}
	s.TLSCertFile, s.TLSKeyFile = strings.TrimSpace(fc.TLS.CertFile), strings.TrimSpace(fc.TLS.KeyFile)
	if (s.TLSCertFile == "") != (s.TLSKeyFile == "") {
		add("tls: укажите и cert_file, и key_file (или ни одного — тогда TLS снимает прокси перед хабом)")
	}
	s.StateDir = strings.TrimSpace(fc.StateDir)
	if s.StateDir == "" {
		add("state_dir: параметр обязателен (каталог, где хаб хранит зарегистрированные шлюзы и коды подключения)")
	}
	s.StatusDir = strings.TrimSpace(fc.StatusDir)
	if s.StatusDir != "" && s.StateDir != "" && filepath.Clean(s.StatusDir) == filepath.Clean(s.StateDir) {
		add("status_dir: должен отличаться от state_dir (файлы состояния читает внешняя проверка, а state_dir закрыт)")
	}
	for _, p := range fc.TrustedProxies {
		p = strings.TrimSpace(p)
		pfx, err := netip.ParsePrefix(p)
		if err != nil {
			var a netip.Addr
			if a, err = netip.ParseAddr(p); err == nil {
				pfx = netip.PrefixFrom(a, a.BitLen())
			}
		}
		if err != nil {
			add("trusted_proxies: «%s» — ожидается IP-адрес или сеть вида 10.0.0.0/8", p)
			continue
		}
		s.TrustedProxies = append(s.TrustedProxies, pfx.Masked())
	}
	if env := strings.TrimSpace(fc.OperatorKeyEnv); env != "" {
		s.OperatorKey = strings.TrimSpace(getenv(env))
		if len(s.OperatorKey) < minKeyLength {
			add("operator_key_env: ключ оператора в %s не задан или короче %d символов", env, minKeyLength)
		} else {
			s.Secrets = append(s.Secrets, s.OperatorKey)
		}
	}
	s.LatestVersion = strings.TrimSpace(fc.LatestVersion)
	if s.LatestVersion != "" && !version.Valid(s.LatestVersion) {
		add("latest_version: «%s» — ожидается версия вида 1.2.3", s.LatestVersion)
	}
	dur := func(name, v string, def, min, max time.Duration) time.Duration {
		if strings.TrimSpace(v) == "" {
			return def
		}
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil || d < min || d > max {
			add("%s: «%s» — длительность от %s до %s (например 30s, 3m)", name, v, min, max)
			return def
		}
		return d
	}
	s.RequestTimeout = dur("request_timeout", fc.RequestTimeout, DefaultRequestTimeout, time.Second, 2*time.Minute)
	s.PollWait = dur("poll_wait", fc.PollWait, DefaultPollWait, 100*time.Millisecond, 50*time.Second)
	s.Interval = dur("heartbeat.interval", fc.Heartbeat.Interval, DefaultInterval, time.Second, 10*time.Minute)
	s.OfflineAfter = dur("heartbeat.offline_after", fc.Heartbeat.OfflineAfter, DefaultOfflineAfter, time.Second, time.Hour)
	s.ReadyFailures = fc.Heartbeat.ReadyFailures
	if s.ReadyFailures == 0 {
		s.ReadyFailures = DefaultReadyFailures
	}
	if s.ReadyFailures < 1 || s.ReadyFailures > 100 {
		add("heartbeat.ready_failures: %d — от 1 до 100", s.ReadyFailures)
	}

	entries := make([]core.PartnerEntry, len(fc.Partners))
	for i, p := range fc.Partners {
		entries[i] = p.PartnerEntry
		path := fmt.Sprintf("partners[%d]", i)
		if len(p.Clubs) == 0 {
			add("%s.clubs: укажите клубы партнёра списком или [\"*\"] — все клубы", path)
		}
		for _, c := range p.Clubs {
			if c != "*" && !ValidClubID(c) {
				add("%s.clubs: «%s» — не идентификатор клуба", path, c)
			}
		}
		s.PartnerClubs[strings.TrimSpace(p.Name)] = append([]string(nil), p.Clubs...)
	}
	partners, keys := core.ParsePartners(entries, getenv, add)
	s.Partners, s.Secrets = partners, append(s.Secrets, keys...)

	if len(errs) > 0 {
		return nil, &core.ConfigError{Problems: errs}
	}
	return s, nil
}

// partnerMayReach reports whether partner may call club.
func (s *Settings) partnerMayReach(partner, club string) bool {
	for _, c := range s.PartnerClubs[partner] {
		if c == "*" || c == club {
			return true
		}
	}
	return false
}
