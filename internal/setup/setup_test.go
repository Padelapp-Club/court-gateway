package setup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
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
	d := &draft{HubURL: "https://hub.example.test", HubKey: placeholderKey, CRMType: "viva", CRM: [][2]string{{"username", "admin@example.test"}}}
	text, p := validate(d, driver.Params{"username": "admin@example.test"}, nil, "inst-1", "Сеть", "pw", "secret")
	if p != nil {
		t.Fatalf("%v\n%s", p, d.render("inst-1", "Сеть"))
	}
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

// statusTransport answers a registration with its status.
type statusTransport int

func (s statusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path != tunnel.PathRegister {
		return registerTransport.RoundTrip(r)
	}
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

// Fixture hub keys: testHub is the hub registerTransport plays (its public
// key testHubKey), otherHub another one, e.g. an interceptor's own.
var (
	testHub    = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	otherHub   = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	testHubKey = publicOf(testHub)
)

func publicOf(k ed25519.PrivateKey) string { return core.EncodeHubKey(k.Public().(ed25519.PublicKey)) }

// keyTransport answers as a hub with key would: its key, and a
// registration signed for the join code asked; nil answers without a key.
type keyTransport struct{ key ed25519.PrivateKey }

func (k keyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var rq tunnel.RegisterRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&rq)
	}
	rr := tunnel.RegisterResponse{Installation: "inst-1", Name: "Сеть", Secret: "cgs_0123456789abcdef0123456789abcdef"}
	if k.key != nil {
		rr.HubKey = publicOf(k.key)
		rr.Sign(k.key, rq.Code)
	}
	body, _ := json.Marshal(rr) // its hub_key also answers PathHubKey
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

var registerTransport = keyTransport{testHub}

// countingTransport counts the join codes redeemed with hub (by default
// registerTransport).
type countingTransport struct {
	calls *int
	hub   http.RoundTripper
}

func (c countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == tunnel.PathRegister {
		*c.calls++
	}
	if c.hub == nil {
		return registerTransport.RoundTrip(r)
	}
	return c.hub.RoundTrip(r)
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
	deps := Deps{HubTransport: countingTransport{calls: &hubCalls}, ProtectSecrets: func(p string) error {
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

// A config folder closed so that the user running setup can no longer
// write in it (an administrator without elevation on Windows) fails
// before the join code is redeemed: FOLDER_NOT_RESTRICTED, the code not
// spent, nothing written.
func TestSetupFailsBeforeRedeemingWhenTheClosedFolderTakesNoFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a folder without write permission is still writable here")
	}
	dir := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	in := Input{ConfigPath: filepath.Join(dir, "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA", CRMType: "demo", HubKeyFingerprint: printOf(testHubKey)}
	hubCalls := 0
	deps := Deps{HubTransport: countingTransport{calls: &hubCalls}, ProtectSecrets: func(p string) error { return os.Chmod(p, 0o500) }}
	res, err := Run(context.Background(), in, deps)
	if err == nil || res.OK || res.Error.Code != "FOLDER_NOT_RESTRICTED" || hubCalls != 0 {
		t.Fatalf("result %+v %v, join codes redeemed %d", res.Error, err, hubCalls)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("written: %v", entries)
	}
}

// setup pins the hub's signing key, and every new join code pins it
// again; a hub that sends no key is refused.
func TestSetupPinsTheHubKey(t *testing.T) {
	dir := t.TempDir()
	in := Input{ConfigPath: filepath.Join(dir, "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA", CRMType: "demo"}
	for _, hub := range []ed25519.PrivateKey{testHub, otherHub} {
		key := publicOf(hub)
		in.HubKeyFingerprint = printOf(key)
		if _, err := Run(context.Background(), in, Deps{HubTransport: keyTransport{hub}}); err != nil {
			t.Fatal(err)
		}
		s, err := core.LoadFile(in.ConfigPath, envFor("", "cgs_x"))
		if err != nil || core.EncodeHubKey(s.Hub.Key) != key || s.Hub.Installation != "inst-1" {
			t.Fatalf("pinned %+v (%v), want key %s", s.Hub, err, key)
		}
		in.Force = true
	}
	fresh := filepath.Join(t.TempDir(), "gateway.yaml")
	res, err := Run(context.Background(), Input{ConfigPath: fresh, HubURL: in.HubURL, JoinCode: in.JoinCode, CRMType: "demo", HubKeyFingerprint: in.HubKeyFingerprint}, Deps{HubTransport: keyTransport{}})
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
		{HubURL: "https://hub.example.test", CRM: map[string]string{"username": "admin@example.test", "auth_url": "https://u:" + marker + "@crm.example.test/token"}, Password: "pw"},
	} {
		dir := t.TempDir()
		in.ConfigPath, in.JoinCode, in.HubKeyFingerprint = filepath.Join(dir, "gateway.yaml"), "AAAA-AAAA-AAAA-AAAA", printOf(testHubKey)
		calls := 0
		res, err := Run(context.Background(), in, Deps{HubTransport: countingTransport{calls: &calls}})
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
// before the join code is spent, and nothing is written; a malformed
// fingerprint is refused before the hub is asked.
func TestSetupAuthenticatesTheHubKey(t *testing.T) {
	redeemed := 0
	forged := countingTransport{&redeemed, keyTransport{otherHub}} // an interceptor answers with its own key
	for name, in := range map[string]Input{
		"embedded":    {HubKey: testHubKey},
		"fingerprint": {HubKeyFingerprint: strings.ToLower(printOf(testHubKey))}, // as typed: case and dashes do not matter
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			in.ConfigPath, in.HubURL, in.JoinCode, in.CRMType = filepath.Join(dir, "gateway.yaml"), "https://hub.example.test", "AAAA-AAAA-AAAA-AAAA", "demo"
			res, err := Run(context.Background(), in, Deps{HubTransport: forged})
			if err == nil || res.Error.Code != "HUB_KEY_MISMATCH" || redeemed != 0 {
				t.Fatalf("a substituted key: %+v %v, join codes redeemed %d", res.Error, err, redeemed)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("files written: %v", entries)
			}
			if _, err := Run(context.Background(), in, Deps{HubTransport: registerTransport}); err != nil {
				t.Fatalf("the hub's own key: %v", err)
			}
			in.Force = true // re-enrollment checks again
			if res, err := Run(context.Background(), in, Deps{HubTransport: forged}); err == nil || res.Error.Code != "HUB_KEY_MISMATCH" {
				t.Fatalf("a substituted key at --force: %+v %v", res.Error, err)
			}
			if s, err := core.LoadFile(in.ConfigPath, envFor("", "cgs_x")); err != nil || core.EncodeHubKey(s.Hub.Key) != testHubKey {
				t.Fatalf("pinned key after a refused --force: %v", err)
			}
		})
	}
	calls := 0
	in := Input{ConfigPath: filepath.Join(t.TempDir(), "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA", CRMType: "demo", HubKeyFingerprint: "NOT-A-FINGERPRINT"}
	if res, err := Run(context.Background(), in, Deps{HubTransport: countingTransport{calls: &calls}}); err == nil || res.Error.Code != "INVALID_INPUT" || calls != 0 {
		t.Fatalf("a malformed fingerprint: %+v %v, hub calls %d", res.Error, err, calls)
	}
}

// Every code setup can fail with has a row in the AI agents' table of
// what to do (AGENTS.md, step 3): an agent must never meet a code the
// instructions do not name.
func TestAgentsTableNamesEverySetupCode(t *testing.T) {
	src, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatal(err)
	}
	agents, err := os.ReadFile("../../AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	codes := regexp.MustCompile(`problem\("([A-Z_]+)"`).FindAllSubmatch(src, -1)
	if len(codes) < 10 {
		t.Fatalf("found only %d codes in setup.go", len(codes))
	}
	for _, m := range codes {
		row := regexp.MustCompile("(?m)^\\| [^|]*`" + string(m[1]) + "`[^|]*\\|")
		if !row.Match(agents) {
			t.Errorf("AGENTS.md has no row for %s", m[1])
		}
	}
}

// tamperTransport answers as testHub would and then changes the
// registration answer on its way, as an interceptor of the TLS can.
type tamperTransport func(*tunnel.RegisterResponse)

func (f tamperTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, _ := registerTransport.RoundTrip(r)
	if r.URL.Path != tunnel.PathRegister {
		return resp, nil
	}
	var rr tunnel.RegisterResponse
	_ = json.NewDecoder(resp.Body).Decode(&rr)
	f(&rr)
	body, _ := json.Marshal(rr)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// The registration answer is used only when the pinned hub key signed all
// of it for this join code: an interceptor that changes any field, or
// passes off the answer to another code, gets nothing written. And the
// CRM settings written are only this computer's: even a signed name
// cannot add a CRM endpoint, so no CRM login ever goes to a host the hub
// named.
func TestSetupTakesNothingUnsignedFromTheHub(t *testing.T) {
	const password = "fixture-crm-password"
	stub := vivastub.New("admin@example.test", password, "00000000-0000-4000-8000-000000000001")
	crm := httptest.NewServer(stub)
	defer crm.Close()
	var evilCalls atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		evilCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer evil.Close()
	inject := fmt.Sprintf("\ncrm: {type: viva, username: admin@example.test, auth_url: %s/token, api_url: %s/api/v1}\n---\n#", evil.URL, evil.URL)
	input := func(dir string) Input {
		return Input{ConfigPath: filepath.Join(dir, "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA",
			HubKey: testHubKey, CRM: map[string]string{"username": "admin@example.test", "auth_url": crm.URL + "/token", "api_url": crm.URL + "/api/v1"}, Password: password}
	}
	for name, tamper := range map[string]func(*tunnel.RegisterResponse){
		"installation":   func(rr *tunnel.RegisterResponse) { rr.Installation = "inst-2" },
		"name":           func(rr *tunnel.RegisterResponse) { rr.Name = inject },
		"secret":         func(rr *tunnel.RegisterResponse) { rr.Secret = "cgs_interceptor-chosen-secret" },
		"signature":      func(rr *tunnel.RegisterResponse) { rr.Signature[0] ^= 1 },
		"unsigned":       func(rr *tunnel.RegisterResponse) { rr.Signature = nil },
		"another code's": func(rr *tunnel.RegisterResponse) { rr.Sign(testHub, "BBBB-BBBB-BBBB-BBBB") },
	} {
		dir := t.TempDir()
		res, err := Run(context.Background(), input(dir), Deps{HubTransport: tamperTransport(tamper), CRMClient: crm.Client()})
		if err == nil || res.Error.Code != "HUB_KEY_MISMATCH" {
			t.Fatalf("%s changed: %+v %v", name, res.Error, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("%s changed: files written: %v", name, entries)
		}
	}
	// the hub itself signs such a name: it stays a comment
	dir := t.TempDir()
	signed := tamperTransport(func(rr *tunnel.RegisterResponse) { rr.Name = inject; rr.Sign(testHub, "AAAA-AAAA-AAAA-AAAA") })
	if _, err := Run(context.Background(), input(dir), Deps{HubTransport: signed, CRMClient: crm.Client()}); err != nil {
		t.Fatal(err)
	}
	env, err := core.EnvWithDotEnv(filepath.Join(dir, "gateway.yaml"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.LoadFile(filepath.Join(dir, "gateway.yaml"), env)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := viva.NewAccount(s.Driver)
	if err != nil {
		t.Fatal(err)
	}
	if err := acct.CheckLogin(context.Background()); err != nil || evilCalls.Load() != 0 || s.Driver.CRM["auth_url"] != crm.URL+"/token" {
		t.Fatalf("CRM login after a hostile name: %v, %d calls to the hub's host, auth_url %v", err, evilCalls.Load(), s.Driver.CRM["auth_url"])
	}
}

// setup and set-password log in to check the login and then end that
// session: the installation's one CRM session is the service's. A session
// that cannot be ended is a note, not a failure, and the note holds no
// secret.
func TestSetupEndsItsCRMSession(t *testing.T) {
	const password = "fixture-crm-password"
	stub := vivastub.New("admin@example.test", password, "00000000-0000-4000-8000-000000000001")
	crm := httptest.NewServer(stub)
	defer crm.Close()
	var notes []string
	deps := Deps{HubTransport: registerTransport, CRMClient: crm.Client(), Note: func(s string) { notes = append(notes, s) }}
	in := Input{ConfigPath: filepath.Join(t.TempDir(), "gateway.yaml"), HubURL: "https://hub.example.test", JoinCode: "AAAA-AAAA-AAAA-AAAA",
		HubKey: testHubKey, CRM: map[string]string{"username": "admin@example.test", "auth_url": crm.URL + "/token", "api_url": crm.URL + "/api/v1"}, Password: password}
	if _, err := Run(context.Background(), in, deps); err != nil {
		t.Fatal(err)
	}
	if stub.Logins() != 1 || stub.OpenSessions() != 0 || len(notes) != 0 {
		t.Fatalf("setup: %d logins, %d sessions left open, notes %q", stub.Logins(), stub.OpenSessions(), notes)
	}
	noEnv := func(string) string { return "" }
	if err := SetPassword(context.Background(), in.ConfigPath, password, noEnv, deps); err != nil {
		t.Fatal(err)
	}
	if stub.Logins() != 2 || stub.OpenSessions() != 0 || len(notes) != 0 {
		t.Fatalf("set-password: %d logins, %d sessions left open, notes %q", stub.Logins(), stub.OpenSessions(), notes)
	}
	stub.FailNext(http.MethodPost, vivastub.LogoutPath, http.StatusServiceUnavailable, "")
	if err := SetPassword(context.Background(), in.ConfigPath, password, noEnv, deps); err != nil {
		t.Fatalf("a failed logout failed set-password: %v", err)
	}
	if len(notes) != 1 || strings.Contains(notes[0], password) || strings.Contains(notes[0], "ref-") || strings.Contains(notes[0], "tok-") {
		t.Fatalf("notes: %q", notes)
	}
}
