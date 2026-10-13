package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

const hubInstallation = `hub: { url: "https://hub.example.test" }
crm: { type: demo, username: admin }
`

var hubEnv = map[string]string{DefaultHubSecretEnv: "cgs_test"}

const clubJSON = `{"id":"club-a","name":"Клуб А","timezone":"Europe/Moscow","slot_minutes":30,
 "hours":{"mon-sun":"07:00-23:00"},"crm":{"studio":"s-1"},
 "courts":[{"id":"court-1","name":"Корт 1","room":"r-1","weight":2},{"id":"court-2","name":"Корт 2"}],
 "services":{"booking":{"direction":12,"type":3}}}`

// A hub installation needs no club in gateway.yaml.
func TestInstallationConfigWithoutClub(t *testing.T) {
	s, err := Load([]byte(hubInstallation), env(hubEnv))
	if err != nil {
		t.Fatal(err)
	}
	if s.ClubID != "" || s.Driver.CRM["username"] != "admin" {
		t.Fatalf("%+v", s)
	}
	// without a hub the club stays required
	if _, err := Load([]byte("crm: { type: demo }\npartners: []\n"), env(goodEnv)); err == nil || !strings.Contains(err.Error(), "club.id: параметр обязателен") {
		t.Fatalf("no hub, no club: %v", err)
	}
}

func TestClubSpecJSONAndWithClub(t *testing.T) {
	var spec ClubSpec
	if err := json.Unmarshal([]byte(clubJSON), &spec); err != nil {
		t.Fatal(err)
	}
	// JSON numbers in driver keys arrive as int, like YAML gives them
	if spec.Courts[0].Params["weight"] != 2 || spec.Services["booking"]["type"] != 3 || spec.Courts[0].Params["room"] != "r-1" {
		t.Fatalf("spec: %+v", spec)
	}
	raw, _ := json.Marshal(spec)
	var back ClubSpec
	if err := json.Unmarshal(raw, &back); err != nil || back.Courts[0].ID != "court-1" || back.Courts[0].Params["room"] != "r-1" {
		t.Fatalf("round trip: %s %v", raw, err)
	}
	base, err := Load([]byte(hubInstallation), env(hubEnv))
	if err != nil {
		t.Fatal(err)
	}
	s, err := base.WithClub(spec, []string{"studio"})
	if err != nil {
		t.Fatal(err)
	}
	// the club's crm keys go over the installation's; base is untouched
	if s.ClubID != "club-a" || s.Driver.CRM["studio"] != "s-1" || s.Driver.CRM["username"] != "admin" || len(s.Courts) != 2 {
		t.Fatalf("club settings: %+v", s)
	}
	if base.ClubID != "" || base.Driver.CRM["studio"] != nil {
		t.Fatal("WithClub changed the installation settings")
	}
	if s.Driver.Courts[0].Params["room"] != "r-1" || s.Driver.Services["booking"]["direction"] != 12 {
		t.Fatalf("driver params: %+v", s.Driver)
	}
	// unknown top-level keys are refused
	if err := json.Unmarshal([]byte(`{"id":"x","venue":{}}`), &spec); err == nil {
		t.Fatal("unknown key accepted")
	}
}

// A club from the hub may set only club-level crm keys. The login and the
// CRM endpoints belong to the installation: a hub-chosen auth_url would
// receive the CRM password.
func TestClubFromTheHubCannotSetInstallationKeys(t *testing.T) {
	base, err := Load([]byte(hubInstallation), env(hubEnv))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"auth_url", "api_url", "username", "type", "anything"} {
		spec := ClubSpec{ID: "club-a", Name: "A", Timezone: "Europe/Moscow", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "07:00-23:00"},
			CRM: map[string]any{"studio": "s-1", key: "https://crm.example.test/token"}, Courts: []CourtSpec{{ID: "court-1", Name: "C"}}}
		if _, err := base.WithClub(spec, []string{"studio"}); err == nil || !strings.Contains(err.Error(), "crm."+key) {
			t.Errorf("club with crm.%s accepted: %v", key, err)
		}
		if err := ValidateClubSpec(spec, []string{"studio"}); err == nil || !strings.Contains(err.Error(), "crm."+key) {
			t.Errorf("hub accepted a club with crm.%s: %v", key, err)
		}
	}
	if base.Driver.CRM["auth_url"] != nil {
		t.Fatal("the installation settings changed")
	}
}

func TestValidateClubSpecNamesJSONKeys(t *testing.T) {
	err := ValidateClubSpec(ClubSpec{ID: "Bad Id", Timezone: "Mars/Base", SlotMinutes: 7, Hours: map[string]string{"mon": "9-10"},
		Courts: []CourtSpec{{ID: "c1"}}, CRM: map[string]any{"type": "viva"}}, []string{"studio"})
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"id: «Bad Id»", "name: параметр обязателен", "timezone: неизвестный часовой пояс", "slot_minutes: 7", "hours.mon: «9-10»", "courts[0].name", "crm.type: задаётся только в gateway.yaml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "venue.") {
		t.Errorf("JSON club errors name YAML keys:\n%s", msg)
	}
}

func TestDraftClub(t *testing.T) {
	var d driver.Discovery
	d.Studio, d.ClubName, d.Timezone = "s-1", "Студия", "Etc/GMT-3"
	d.CRM = driver.Params{"studio": "s-1"}
	d.Courts = []driver.DiscoveredCourt{{Name: "Корт 2", Params: driver.Params{"room": "r2"}}, {Name: "", Params: driver.Params{}}}
	for i := 0; i < 5; i++ {
		d.Hours[i] = &[2]int{7 * 60, 23*60 + 10} // off the grid
	}
	d.Hours[5] = &[2]int{0, 1440}
	spec, notes := DraftClub(d, "", 0)
	if spec.Timezone != "Etc/GMT-3" || spec.SlotMinutes != 30 || spec.Name != "Студия" || spec.CRM["studio"] != "s-1" {
		t.Fatalf("draft: %+v", spec)
	}
	if spec.Courts[0].ID != "court-1" || spec.Courts[1].Name != "Корт 2" || spec.Courts[0].Params["room"] != "r2" {
		t.Fatalf("courts: %+v", spec.Courts)
	}
	want := map[string]string{"mon-fri": "07:00-23:30", "sat": "00:00-24:00", "sun": "closed"}
	for k, v := range want {
		if spec.Hours[k] != v {
			t.Fatalf("hours = %v, want %v", spec.Hours, want)
		}
	}
	codes := ""
	for _, n := range notes {
		codes += n.Code + " "
	}
	if !strings.Contains(codes, "HOURS_ROUNDED") || !strings.Contains(codes, "TIMEZONE_FROM_CRM") {
		t.Fatalf("notices: %s", codes)
	}
	// with an id the draft is a valid club
	spec.ID = "club-x"
	if err := ValidateClubSpec(spec, []string{"studio"}); err != nil {
		t.Fatalf("draft does not validate: %v", err)
	}
}
