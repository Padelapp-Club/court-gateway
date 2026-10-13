package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

const validConfig = `club: { id: my-club, name: "Мой клуб" }
crm: { type: demo }
venue:
  timezone: Europe/Moscow
  slot_minutes: 30
  hours: { mon-fri: "07:00-23:00", sat: "08:00-24:00", sun: closed }
courts:
  - { id: court-1, name: "Корт 1", room: abc }
services:
  booking: { direction: 12, type: 3 }
partners:
  - { name: aggregator, key_env: PARTNER_KEY_1, scopes: [read, book], writes_per_minute: 10 }
`

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var goodEnv = map[string]string{"PARTNER_KEY_1": "k-0123456789abcdef0123456789", "CRM_PASSWORD": "pw-secret"}

func TestLoadValid(t *testing.T) {
	s, err := Load([]byte(validConfig), env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	if s.ClubID != "my-club" || s.Listen != DefaultListen || s.SlotMinutes != 30 || s.Location.String() != "Europe/Moscow" {
		t.Fatalf("%+v", s)
	}
	if h := s.Hours[0]; h == nil || h.Open != 420 || h.Close != 1380 {
		t.Fatalf("monday = %+v", h)
	}
	if h := s.Hours[5]; h == nil || h.Close != 1440 {
		t.Fatalf("saturday = %+v", h)
	}
	if s.Hours[6] != nil {
		t.Fatal("sunday must be closed")
	}
	if s.Driver.Courts[0].Params["room"] != "abc" || s.Driver.Services["booking"]["type"] != 3 {
		t.Fatalf("driver params not passed through: %+v", s.Driver)
	}
	if s.Partners[0].WritesPerMinute != 10 || s.Partners[0].Key != goodEnv["PARTNER_KEY_1"] {
		t.Fatalf("partner = %+v", s.Partners[0])
	}
	if s.Driver.Password != "pw-secret" {
		t.Fatal("password not passed")
	}
	joined := strings.Join(s.Secrets, "|")
	if !strings.Contains(joined, "pw-secret") || !strings.Contains(joined, goodEnv["PARTNER_KEY_1"]) {
		t.Fatal("secrets not collected for redaction")
	}
}

func TestLoadReportsEveryProblemInRussian(t *testing.T) {
	bad := `club: { id: "My Club" }
listen: "nonsense"
crm: {}
venue: { timezone: Mars/Olympus, slot_minutes: 7, hours: { mon-fri: "23:00-07:00", sat: "8-9" } }
courts:
  - { id: court-1, name: A }
  - { id: court-1, name: "" }
services: { training: { direction: 1, type: 1 } }
partners:
  - { name: Agg, key_env: lower_case, scopes: [write] }
  - { name: b, key_env: SHORT_KEY, scopes: [] }
  - { name: c, key_env: MISSING_KEY, scopes: [read] }
`
	_, err := Load([]byte(bad), env(map[string]string{"SHORT_KEY": "short"}))
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"club.id: «My Club»",
		"club.name: параметр обязателен",
		"listen: «nonsense»",
		"crm.type: параметр обязателен",
		"неизвестный часовой пояс «Mars/Olympus»",
		"venue.slot_minutes: 7",
		"закрытие должно быть позже открытия",
		"venue.hours.sat: «8-9»",
		"не указаны часы на воскресенье",
		"корт «court-1» указан дважды",
		"courts[1].name: параметр обязателен",
		"неизвестная услуга «training»",
		"partners[0].name: «Agg»",
		"partners[0].key_env: «lower_case»",
		"неизвестное право «write»",
		"ключ в SHORT_KEY слишком короткий",
		"partners[1].scopes: укажите права",
		"переменная окружения MISSING_KEY не задана",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestLoadYAMLErrorsAreTranslated(t *testing.T) {
	_, err := Load([]byte("club: { id: a, name: b }\nlisten: :8080\ncrm: { type: demo }\nvenu: {}\n"), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "неизвестный параметр «venu»") {
		t.Fatalf("unknown top-level key: %v", err)
	}
	_, err = Load([]byte("venue: { slot_minutes: thirty }\n"), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "ожидается целое число") {
		t.Fatalf("type error: %v", err)
	}
	_, err = Load([]byte("club: [unclosed\n"), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "ошибка синтаксиса YAML") {
		t.Fatalf("syntax error: %v", err)
	}
	_, err = Load([]byte("  \n"), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "пуст") {
		t.Fatalf("empty: %v", err)
	}
}

func TestLoadRejectsSharedKeysAndPasswordEnv(t *testing.T) {
	cfg := strings.Replace(validConfig, "partners:\n", "partners:\n  - { name: twin, key_env: PARTNER_KEY_1, scopes: [read] }\n  - { name: pw, key_env: CRM_PASSWORD, scopes: [read] }\n", 1)
	_, err := Load([]byte(cfg), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "одинаковый ключ") || !strings.Contains(err.Error(), "занята паролем CRM") {
		t.Fatalf("err = %v", err)
	}
}

func TestHoursMustFitSlotGrid(t *testing.T) {
	cfg := strings.Replace(validConfig, `mon-fri: "07:00-23:00"`, `mon-fri: "07:15-23:00"`, 1)
	_, err := Load([]byte(cfg), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "не совпадают с сеткой 30 мин") {
		t.Fatalf("err = %v", err)
	}
	cfg = strings.Replace(validConfig, `mon-fri: "07:00-23:00"`, `mon-fri: "07:00-23:00", fri: closed`, 1)
	_, err = Load([]byte(cfg), env(goodEnv))
	if err == nil || !strings.Contains(err.Error(), "указаны дважды") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildDriverUnknownTypeAndDriverErrors(t *testing.T) {
	s, err := Load([]byte(validConfig), env(goodEnv))
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
