package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var goodEnv = map[string]string{DefaultHubSecretEnv: "cgs_test", "CRM_PASSWORD": "pw-secret"}

func testClub() ClubSpec {
	return ClubSpec{ID: "my-club", Name: "Мой клуб", Timezone: "Europe/Moscow", SlotMinutes: 30,
		Hours:    map[string]string{"mon-fri": "07:00-23:00", "sat": "08:00-24:00", "sun": "closed"},
		Courts:   []CourtSpec{{ID: "court-1", Name: "Корт 1", Params: map[string]any{"room": "abc"}}},
		Services: map[string]map[string]any{"booking": {"direction": 12, "type": 3}}}
}

func TestLoadValid(t *testing.T) {
	base, err := Load([]byte(hubInstallation), env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	s, err := base.WithClub(testClub(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ClubID != "my-club" || s.SlotMinutes != 30 || s.Location.String() != "Europe/Moscow" {
		t.Fatalf("%+v", s)
	}
	if h := s.Hours[0]; h == nil || h[0] != 420 || h[1] != 1380 {
		t.Fatalf("monday = %+v", h)
	}
	if h := s.Hours[5]; h == nil || h[1] != 1440 {
		t.Fatalf("saturday = %+v", h)
	}
	if s.Hours[6] != nil {
		t.Fatal("sunday must be closed")
	}
	if s.Driver.Courts[0].Params["room"] != "abc" || s.Driver.Services["booking"]["type"] != 3 {
		t.Fatalf("driver params not passed through: %+v", s.Driver)
	}
	if s.Driver.Password != "pw-secret" {
		t.Fatal("password not passed")
	}
	if !strings.Contains(strings.Join(s.Secrets, "|"), "pw-secret") {
		t.Fatal("secrets not collected for redaction")
	}
}

func TestLoadYAMLErrorsAreTranslated(t *testing.T) {
	_, err := Load([]byte(hubInstallation+"venu: {}\n"), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "неизвестный параметр «venu»") {
		t.Fatalf("unknown top-level key: %v", err)
	}
	_, err = Load([]byte("hub: { polls: four }\n"), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "ожидается целое число") {
		t.Fatalf("type error: %v", err)
	}
	_, err = Load([]byte("hub: [unclosed\n"), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "ошибка синтаксиса YAML") {
		t.Fatalf("syntax error: %v", err)
	}
	_, err = Load([]byte("  \n"), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "пуст") {
		t.Fatalf("empty: %v", err)
	}
}

// Partner keys live on the hub only; the gateway has no local port and no
// club of its own, so their former sections are unknown keys.
func TestLoadRefusesTheLocalMode(t *testing.T) {
	for _, section := range []string{"listen: \":8080\"\n", "partners: []\n", "club: { id: x }\n", "courts: []\n", "venue: {}\n"} {
		if _, err := Load([]byte(hubInstallation+section), env(goodEnv)); err == nil || !strings.Contains(err.Error(), "неизвестный параметр") {
			t.Errorf("%q accepted: %v", section, err)
		}
	}
	if _, err := Load([]byte("crm: { type: demo }\n"), env(goodEnv)); err == nil || !strings.Contains(err.Error(), "hub: раздел обязателен") {
		t.Fatalf("no hub: %v", err)
	}
}

func TestParsePartnersRefusesSharedKeysAndPasswordEnv(t *testing.T) {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, f) }
	ParsePartners([]PartnerEntry{
		{Name: "Agg", KeyEnv: "lower_case", Scopes: []string{"write"}},
		{Name: "b", KeyEnv: "SHORT_KEY"},
		{Name: "one", KeyEnv: "K1", Scopes: []string{"read"}},
		{Name: "twin", KeyEnv: "K2", Scopes: []string{"read"}},
		{Name: "pw", KeyEnv: PasswordEnv, Scopes: []string{"read"}},
	}, env(map[string]string{"SHORT_KEY": "short", "K1": "k-0123456789abcdef0123456789", "K2": "k-0123456789abcdef0123456789"}), add)
	msg := strings.Join(problems, "\n")
	for _, want := range []string{"допустимы строчные", "имя переменной окружения", "неизвестное право", "слишком короткий", "укажите права", "одинаковый ключ", "занята паролем CRM"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestHoursMustFitSlotGrid(t *testing.T) {
	c := testClub()
	c.Hours["mon-fri"] = "07:15-23:00"
	if err := ValidateClubSpec(c, nil); err == nil || !strings.Contains(err.Error(), "не совпадают с сеткой 30 мин") {
		t.Fatalf("err = %v", err)
	}
	c = testClub()
	c.Hours["fri"] = "closed"
	if err := ValidateClubSpec(c, nil); err == nil || !strings.Contains(err.Error(), "указаны дважды") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildDriverUnknownTypeAndDriverErrors(t *testing.T) {
	base, err := Load([]byte(hubInstallation), env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	s, err := base.WithClub(testClub(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildDriver(s, map[string]driver.Factory{"other": nil})
	if err == nil || !strings.Contains(err.Error(), "неизвестный тип CRM «demo»") {
		t.Fatalf("err = %v", err)
	}
	_, err = BuildDriver(s, map[string]driver.Factory{"demo": func(driver.Config) (driver.Driver, error) {
		return nil, errors.New("courts[0].room: плохо\ncrm.x: тоже")
	}})
	var ce *ConfigError
	if !errors.As(err, &ce) || len(ce.Problems) != 2 {
		t.Fatalf("driver errors must become config problems: %v", err)
	}
}

// A URL in the config may carry neither a user and password nor a
// fragment, and the error does not repeat the password.
func TestURLsWithCredentialsAreRefusedUnechoed(t *testing.T) {
	const marker = "FIXTURE-URL-PASSWORD"
	const key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for name, yaml := range map[string]string{
		"hub":             "hub: {url: https://operator:" + marker + "@hub.example.test, installation: n1, key: " + key + "}\ncrm: {type: demo}\n",
		"hub, bad port":   "hub: {url: \"https://operator:" + marker + "@hub.example.test:x\", installation: n1, key: " + key + "}\ncrm: {type: demo}\n",
		"hub, # password": "hub: {url: \"https://operator:" + marker + "#1@hub.example.test\", installation: n1, key: " + key + "}\ncrm: {type: demo}\n",
		"hub fragment":    "hub: {url: \"https://hub.example.test/#" + marker + "\", installation: n1, key: " + key + "}\ncrm: {type: demo}\n",
		"manifest":        "hub: {url: https://hub.example.test, installation: n1, key: " + key + "}\nupdate: {manifest_url: https://u:" + marker + "@example.test/m.json}\ncrm: {type: demo}\n",
	} {
		_, err := Load([]byte(yaml), env(goodEnv))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), marker) && name != "hub fragment" {
			t.Errorf("%s: the error repeats the password: %v", name, err)
		}
	}
	for _, ok := range []string{"https://hub.example.test/prefix", "http://127.0.0.1:8443", "https://hub.example.test/a@b"} {
		if err := CheckHTTPSURL("u", ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
}
