package setup

import (
	"os"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

func init() {
	driver.RegisterSetup("spectest", driver.SetupSpec{
		Name: "Тест", Needs: "тестовый вход",
		Fields: []driver.SetupField{
			{Key: "login", Flag: "crm-login", Prompt: "Логин", Required: true},
			{Key: "base_url", Flag: "crm-base-url"},
		},
		Secrets: []driver.SetupSecret{{Env: "SPECTEST_TOKEN", Prompt: "Токен", Required: true}, {Env: core.PasswordEnv, Prompt: "Пароль"}},
	})
}

// Setup asks for what the CRM type declares, and only that: its required
// keys and secrets are missing, another type's are not.
func TestMissingFollowsTheCRMTypesSpec(t *testing.T) {
	in := Input{HubURL: "https://hub.example.test", JoinCode: "x", HubKeyFingerprint: "x", CRMType: "spectest"}
	got := strings.Join(in.Missing(), " | ")
	if !strings.Contains(got, "--crm-login") || !strings.Contains(got, "SPECTEST_TOKEN") || strings.Contains(got, "--crm-username") || strings.Contains(got, core.PasswordEnv) {
		t.Fatalf("missing = %s", got)
	}
	in.CRM = map[string]string{"login": "admin"}
	in.Secrets = map[string]string{"SPECTEST_TOKEN": "tok"}
	if m := in.Missing(); len(m) != 0 {
		t.Fatalf("missing = %v", m)
	}
}

// gateway.yaml gets the declared keys, .env the declared secrets; neither
// holds the other's, nor a key of another CRM type.
func TestRenderWritesTheDeclaredKeysAndSecrets(t *testing.T) {
	d := &draft{HubURL: "https://hub.example.test", HubKey: placeholderKey, CRMType: "spectest",
		CRM: [][2]string{{"login", "admin"}, {"base_url", "https://crm.example.test"}}}
	text, p := validate(d, driver.Params{"login": "admin", "base_url": "https://crm.example.test"}, "inst-1", "", "", "secret")
	if p != nil {
		t.Fatal(p)
	}
	if !strings.Contains(text, "type: spectest") || !strings.Contains(text, `login: "admin"`) || strings.Contains(text, "username") || strings.Contains(text, "tok-") {
		t.Fatalf("config:\n%s", text)
	}
	env := renderEnv([][2]string{{"SPECTEST_TOKEN", "tok-1"}}, "hub-secret")
	vals, err := core.ParseDotEnv([]byte(env))
	if err != nil || vals["SPECTEST_TOKEN"] != "tok-1" || vals[core.DefaultHubSecretEnv] != "hub-secret" {
		t.Fatalf("env %v (%v):\n%s", vals, err, env)
	}
	if _, ok := vals[core.PasswordEnv]; ok {
		t.Fatalf("a CRM_PASSWORD nobody gave was written:\n%s", env)
	}
}

// Setup and the config loader name no CRM: what a CRM needs comes from its
// driver's SetupSpec.
func TestSetupNamesNoCRM(t *testing.T) {
	for _, f := range []string{"setup.go", "render.go", "password.go", "../core/config.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(b)), "viva") {
			t.Errorf("%s names Viva", f)
		}
	}
}
