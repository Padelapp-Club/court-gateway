package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	d := &draft{HubURL: "https://hub.example.test", HubKey: placeholderKey, CRMType: "viva", Username: "admin@example.test"}
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
	for _, w := range []string{"--hub-url", "--join-code", "--hub-key-fingerprint", "--crm-username", "CRM_PASSWORD"} {
		if !strings.Contains(m, w) {
			t.Fatalf("missing list %q lacks %q", m, w)
		}
	}
	if strings.Contains(m, "studio") {
		t.Fatalf("setup asks for a studio: %q", m)
	}
	demo := Input{CRMType: "demo", HubURL: "https://x", JoinCode: "y", HubKeyFingerprint: "z"}
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
	in := Input{ConfigPath: filepath.Join(t.TempDir(), "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA", CRMType: "demo", HubKeyFingerprint: printOf(testHubKey)}
	res, err := Run(context.Background(), in, Deps{HubTransport: statusTransport(http.StatusTooManyRequests)})
	if err == nil || res.Error == nil || res.Error.Code != "HUB_RATE_LIMITED" || !strings.Contains(res.Error.Message, "повторите позже") {
		t.Fatalf("result: %+v %v", res.Error, err)
	}
}

// testHubKey is the hub key registerTransport answers with.
const testHubKey = "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="

type registerTransport struct{}

func (registerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"installation_id":"inst-1","name":"Сеть","secret":"cgs_0123456789abcdef0123456789abcdef","hub_key":"` + testHubKey + `"}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

type countingTransport struct{ calls *int }

func (c countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*c.calls++
	return registerTransport{}.RoundTrip(r)
}

// On Windows setup closes the config folder (and an existing .env) before
// the join code is redeemed and before any secret is written, and the new
// .env after writing. A failure to close or verify aborts setup: the join
// code stays unspent and no secret lands in an open folder.
func TestSetupProtectsSecretsFirstAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, core.EnvFileName)
	in := Input{ConfigPath: filepath.Join(dir, "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA", CRMType: "demo", HubKeyFingerprint: printOf(testHubKey)}
	var events []string
	hubCalls := 0
	deps := Deps{HubTransport: countingTransport{&hubCalls}, ProtectSecrets: func(p string) error {
		_, envErr := os.Stat(env)
		events = append(events, fmt.Sprintf("%s hub=%d env=%v", filepath.Base(p), hubCalls, envErr == nil))
		return nil
	}}
	if _, err := Run(context.Background(), in, deps); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Base(dir) + " hub=0 env=false", ".env hub=1 env=true"}
	if !slices.Equal(events, want) {
		t.Fatalf("protection order %q, want %q", events, want)
	}

	in.Force, events = true, nil
	if _, err := Run(context.Background(), in, deps); err != nil || len(events) != 3 || events[1] != ".env hub=1 env=true" {
		t.Fatalf("existing .env not closed before redeeming: %q %v", events, err)
	}

	for _, failOn := range []string{"folder", core.EnvFileName} {
		fresh := t.TempDir()
		in := Input{ConfigPath: filepath.Join(fresh, "gateway.yaml"), HubURL: in.HubURL, JoinCode: in.JoinCode, CRMType: "demo", HubKeyFingerprint: in.HubKeyFingerprint}
		hubCalls = 0
		deps.ProtectSecrets = func(p string) error {
			if (filepath.Base(p) == core.EnvFileName) == (failOn == core.EnvFileName) {
				return errors.New("synthetic ACL failure")
			}
			return nil
		}
		res, err := Run(context.Background(), in, deps)
		if err == nil || res.OK || res.Error.Code != "FOLDER_NOT_RESTRICTED" {
			t.Fatalf("failing on %s: setup succeeded: %+v %v", failOn, res, err)
		}
		if _, err := os.Stat(filepath.Join(fresh, core.EnvFileName)); err == nil {
			t.Fatalf("failing on %s: .env left behind", failOn)
		}
		if failOn == "folder" && hubCalls != 0 {
			t.Fatal("join code redeemed although the folder could not be closed")
		}
	}
}

// keyTransport answers a registration with the hub key key ("" omits it).
type keyTransport string

func (k keyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"installation_id":"inst-1","name":"Сеть","secret":"cgs_0123456789abcdef0123456789abcdef","hub_key":"` + string(k) + `"}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

// setup pins the hub's signing key from the registration answer, and
// every new join code pins it again; a hub that sends no key is refused.
func TestSetupPinsTheHubKey(t *testing.T) {
	dir := t.TempDir()
	in := Input{ConfigPath: filepath.Join(dir, "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA", CRMType: "demo"}
	other := core.EncodeHubKey(make([]byte, 32))
	for _, key := range []string{testHubKey, other} {
		in.HubKeyFingerprint = printOf(key)
		if _, err := Run(context.Background(), in, Deps{HubTransport: keyTransport(key)}); err != nil {
			t.Fatal(err)
		}
		s, err := core.LoadFile(in.ConfigPath, envFor("", "cgs_x"))
		if err != nil || core.EncodeHubKey(s.Hub.Key) != key || s.Hub.Installation != "inst-1" {
			t.Fatalf("pinned %+v (%v), want key %s", s.Hub, err, key)
		}
		in.Force = true
	}
	fresh := filepath.Join(t.TempDir(), "gateway.yaml")
	res, err := Run(context.Background(), Input{ConfigPath: fresh, HubURL: in.HubURL, JoinCode: in.JoinCode, CRMType: "demo", HubKeyFingerprint: in.HubKeyFingerprint}, Deps{HubTransport: keyTransport("")})
	if err == nil || res.Error.Code != "HUB_UNREACHABLE" {
		t.Fatalf("a hub without a key: %+v %v", res.Error, err)
	}
	if _, err := os.Stat(fresh); err == nil {
		t.Fatal("config written without a hub key")
	}
}

// setup refuses a hub or CRM address with a user and password before any
// network call, and neither its result nor any file repeats the password.
func TestSetupRefusesURLCredentials(t *testing.T) {
	const marker = "FIXTURE-URL-PASSWORD"
	for _, in := range []Input{
		{HubURL: "https://operator:" + marker + "@hub.example.test", CRMType: "demo"},
		{HubURL: "https://hub.example.test", Username: "admin@example.test", Password: "pw", AuthURL: "https://u:" + marker + "@crm.example.test/token"},
	} {
		dir := t.TempDir()
		in.ConfigPath, in.JoinCode, in.HubKeyFingerprint = filepath.Join(dir, "gateway.yaml"), "AAAA-AAAA-AAAA-AAAA", printOf(testHubKey)
		calls := 0
		res, err := Run(context.Background(), in, Deps{HubTransport: countingTransport{&calls}})
		encoded, _ := json.Marshal(res)
		if err == nil || res.Error.Code != "INVALID_INPUT" || calls != 0 || bytes.Contains(encoded, []byte(marker)) {
			t.Fatalf("%+v: %s (%v), hub calls %d", in, encoded, err, calls)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("files written: %v", entries)
		}
	}
}

func printOf(b64 string) string {
	k, err := core.ParseHubKey(b64)
	if err != nil {
		panic(err)
	}
	return core.HubKeyFingerprint(k)
}

// The key a hub answers with at registration is checked out of band
// before it is pinned: against the key a release embeds for its own hub,
// or against the fingerprint the operator gives with the join code (a
// self-build, a custom hub), also at re-enrollment (--force). A TLS
// interceptor substituting its own key is refused with HUB_KEY_MISMATCH
// and nothing is written; a malformed fingerprint is refused before the
// join code is spent.
func TestSetupAuthenticatesTheHubKey(t *testing.T) {
	forged := core.EncodeHubKey(make([]byte, 32)) // what an interceptor would put in
	for name, in := range map[string]Input{
		"embedded":    {HubKey: testHubKey},
		"fingerprint": {HubKeyFingerprint: strings.ToLower(printOf(testHubKey))}, // as typed: case and dashes do not matter
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			in.ConfigPath, in.HubURL, in.JoinCode, in.CRMType = filepath.Join(dir, "gateway.yaml"), "https://hub.example.test", "AAAA-AAAA-AAAA-AAAA", "demo"
			res, err := Run(context.Background(), in, Deps{HubTransport: keyTransport(forged)})
			if err == nil || res.Error.Code != "HUB_KEY_MISMATCH" {
				t.Fatalf("a substituted key: %+v %v", res.Error, err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("files written: %v", entries)
			}
			if _, err := Run(context.Background(), in, Deps{HubTransport: keyTransport(testHubKey)}); err != nil {
				t.Fatalf("the hub's own key: %v", err)
			}
			in.Force = true // re-enrollment checks again
			if res, err := Run(context.Background(), in, Deps{HubTransport: keyTransport(forged)}); err == nil || res.Error.Code != "HUB_KEY_MISMATCH" {
				t.Fatalf("a substituted key at --force: %+v %v", res.Error, err)
			}
			if s, err := core.LoadFile(in.ConfigPath, envFor("", "cgs_x")); err != nil || core.EncodeHubKey(s.Hub.Key) != testHubKey {
				t.Fatalf("pinned key after a refused --force: %v", err)
			}
		})
	}
	calls := 0
	in := Input{ConfigPath: filepath.Join(t.TempDir(), "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA", CRMType: "demo", HubKeyFingerprint: "NOT-A-FINGERPRINT"}
	if res, err := Run(context.Background(), in, Deps{HubTransport: countingTransport{&calls}}); err == nil || res.Error.Code != "INVALID_INPUT" || calls != 0 {
		t.Fatalf("a malformed fingerprint: %+v %v, hub calls %d", res.Error, err, calls)
	}
}
