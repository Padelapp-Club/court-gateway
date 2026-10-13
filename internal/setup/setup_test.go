package setup

import (
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

func TestEnvValueRoundTrip(t *testing.T) {
	for _, v := range []string{"plain", "with space", " lead", "trail ", "has#hash", `dq"inside`, "sq'inside", `both"and'`, "=eq=", "кириллица",
		`"starts`, "'starts", "a #b", "", "pa$word", "$HOME", "${X}", `back\slash`, `trailing\`, `it's $5\`, "tick`s", `\'`, "$$"} {
		vals, err := core.ParseDotEnv([]byte("K=" + envValue(v) + "\n"))
		if err != nil || vals["K"] != v {
			t.Fatalf("%q -> %q -> %q (%v)", v, envValue(v), vals["K"], err)
		}
	}
}

// docker compose interpolates $ in bare and double-quoted env_file values,
// so a password with $ must never be written that way.
func TestEnvValueQuotesDollar(t *testing.T) {
	for v, want := range map[string]string{"pa$word": `'pa$word'`, "$HOME": `'$HOME'`, "it's $5": `"it's $$5"`, "plain-Value_1": "plain-Value_1"} {
		if got := envValue(v); got != want {
			t.Errorf("envValue(%q) = %s, want %s", v, got, want)
		}
	}
}

// The written config is an installation: hub and CRM login, no club.
func TestRenderedConfigHasNoClub(t *testing.T) {
	d := &draft{HubURL: "https://hub.example.test", CRMType: "viva", Username: "admin@example.test"}
	if p := validate(d, "pw", "secret"); p != nil {
		t.Fatalf("%v\n%s", p, d.render("inst-1", "Сеть"))
	}
	text := d.render("inst-1", "Сеть")
	s, err := core.Load([]byte(text), envFor("pw", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if s.HasClub || s.Hub == nil || s.Driver.CRM["username"] != "admin@example.test" {
		t.Fatalf("loaded: %+v", s)
	}
	for _, w := range []string{"club:", "venue:", "courts:", "studio"} {
		if strings.Contains(text, w) {
			t.Fatalf("installation config holds %q:\n%s", w, text)
		}
	}
}

func TestMissingInputs(t *testing.T) {
	m := strings.Join(Input{}.Missing(), " ")
	for _, w := range []string{"--hub-url", "--join-code", "--crm-username", "CRM_PASSWORD"} {
		if !strings.Contains(m, w) {
			t.Fatalf("missing list %q lacks %q", m, w)
		}
	}
	if strings.Contains(m, "studio") {
		t.Fatalf("setup asks for a studio: %q", m)
	}
	demo := Input{CRMType: "demo", HubURL: "https://x", JoinCode: "y"}
	if m := demo.Missing(); len(m) != 0 {
		t.Fatalf("demo needs no CRM login: %v", m)
	}
}
