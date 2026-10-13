package setup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	if s.ClubID != "" || s.Hub == nil || s.Driver.CRM["username"] != "admin@example.test" {
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

type statusTransport int

func (s statusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: int(s), Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: r}, nil
}

// A hub that limits failed registrations answers 429: setup says to try
// again later instead of reporting the hub as unreachable.
func TestSetupRateLimitedByTheHub(t *testing.T) {
	in := Input{ConfigPath: filepath.Join(t.TempDir(), "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA", CRMType: "demo"}
	res, err := Run(context.Background(), in, Deps{HubTransport: statusTransport(http.StatusTooManyRequests)})
	if err == nil || res.Error == nil || res.Error.Code != "HUB_RATE_LIMITED" || !strings.Contains(res.Error.Message, "повторите позже") {
		t.Fatalf("result: %+v %v", res.Error, err)
	}
}

type registerTransport struct{}

func (registerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"installation_id":"inst-1","name":"Сеть","secret":"cgs_0123456789abcdef0123456789abcdef"}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

// On Windows setup closes the config folder before the secrets are written
// to it; when it cannot, setup still succeeds and says so.
func TestSetupRestrictsTheFolderFirst(t *testing.T) {
	dir := t.TempDir()
	in := Input{ConfigPath: filepath.Join(dir, "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA", CRMType: "demo"}
	var restricted []string
	deps := Deps{HubTransport: registerTransport{}, RestrictDir: func(d string) error {
		if _, err := os.Stat(filepath.Join(d, core.EnvFileName)); err == nil {
			t.Error(".env was written before the folder was restricted")
		}
		restricted = append(restricted, d)
		return nil
	}}
	res, err := Run(context.Background(), in, deps)
	if err != nil || len(restricted) != 1 || restricted[0] != dir || len(res.Warnings) != 0 {
		t.Fatalf("restricted %v, result %+v, %v", restricted, res, err)
	}
	in.Force = true
	deps.RestrictDir = func(string) error { return errors.New("access denied") }
	res, err = Run(context.Background(), in, deps)
	if err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != "FOLDER_NOT_RESTRICTED" {
		t.Fatalf("failed restriction: %+v, %v", res, err)
	}
}
