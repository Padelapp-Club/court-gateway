package setup

import (
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

func TestHourGroupsAndRounding(t *testing.T) {
	d := &draft{Slot: 30}
	for i := 0; i < 5; i++ {
		d.Hours[i] = &[2]int{7 * 60, 23 * 60}
	}
	d.Hours[5] = &[2]int{8*60 + 10, 22*60 + 50} // not on the grid
	d.Hours[6] = nil
	if !d.roundHours() {
		t.Fatal("off-grid hours not rounded")
	}
	got := d.hoursMap()
	want := map[string]string{"mon-fri": "07:00-23:00", "sat": "08:00-23:00", "sun": "closed"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("hours = %v, want %v", got, want)
		}
	}
	d.Hours[6] = &[2]int{0, 1440}
	if d.hoursMap()["sun"] != "00:00-24:00" {
		t.Fatalf("full day: %v", d.hoursMap())
	}
}

func TestEnvValueRoundTrip(t *testing.T) {
	for _, v := range []string{"plain", "with space", " lead", "trail ", "has#hash", `dq"inside`, "sq'inside", `both"and'`, "=eq=", "кириллица", `"starts`, "'starts", "a #b", ""} {
		vals, err := core.ParseDotEnv([]byte("K=" + envValue(v) + "\n"))
		if err != nil || vals["K"] != v {
			t.Fatalf("%q -> %q -> %q (%v)", v, envValue(v), vals["K"], err)
		}
	}
}

func TestRenderedDraftLoads(t *testing.T) {
	d := &draft{
		ClubID: "c1", ClubName: `Клуб "Мяч"`, HubURL: "https://hub.example.test", CRMType: "viva",
		Username: "admin@example.test", Studio: "00000000-0000-4000-8000-0000000000aa", Timezone: "Europe/Moscow", Slot: 30,
		Hours:  defaultHours(),
		Courts: []Court{{ID: "court-1", Name: "Корт: «1»", Room: "00000000-0000-4000-8000-0000000000b1"}},
	}
	if p := validate(d, "pw"); p != nil {
		t.Fatalf("%v\n%s", p, d.render())
	}
	text := d.render()
	for _, w := range []string{"проверьте", "НЕ попал", "hub:", "# services:"} {
		if !strings.Contains(text, w) {
			t.Fatalf("draft lacks %q:\n%s", w, text)
		}
	}
}

func TestMissingInputs(t *testing.T) {
	m := strings.Join(Input{}.Missing(), " ")
	for _, w := range []string{"--hub-url", "--join-code", "--crm-username", "CRM_PASSWORD", "--crm-studio"} {
		if !strings.Contains(m, w) {
			t.Fatalf("missing list %q lacks %q", m, w)
		}
	}
	demo := Input{CRMType: "demo", HubURL: "https://x", JoinCode: "y"}
	if m := demo.Missing(); len(m) != 0 {
		t.Fatalf("demo needs no CRM login: %v", m)
	}
}
