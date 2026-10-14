package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// The hub key and registration of the client tests: installation "inst",
// secret "s".
var (
	testPub, testKey, _ = ed25519.GenerateKey(nil)
	_, otherTestKey, _  = ed25519.GenerateKey(nil)
	testRegistration    = RegistrationOf(SecretSHA256("s"))
)

// testNonce is the nonce of the poll the test requests are signed for.
const testNonce = "poll"

// signed signs rq for the test registration, with a fresh id.
func signed(rq Request) Request {
	var id [8]byte
	_, _ = rand.Read(id[:])
	rq.ID = hex.EncodeToString(id[:])
	rq.Sign(testKey, "inst", testRegistration, testNonce, time.Now())
	return rq
}

// synced gives v the hub's time hubNow, as a quick poll answer does.
func synced(t *testing.T, v *Verifier, hubNow time.Time) *Verifier {
	t.Helper()
	if err := v.Time("sync", SignTime(testKey, "inst", testRegistration, "sync", "0", hubNow), "0", v.now()); err != nil {
		t.Fatal(err)
	}
	return v
}

// testClient is a client of the test registration that knows the hub's
// time.
func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", HubKey: testPub, Installation: "inst", Handler: h})
	if err != nil {
		t.Fatal(err)
	}
	synced(t, c.verify, time.Now())
	return c
}

func sampleRequest() Request {
	return Request{ID: "call-1", RequestID: "rq-1", Method: http.MethodPost, Path: "/v1/clubs/a/bookings", Query: "x=1",
		Header: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"court":"c1"}`),
		Partner: "booker", Scopes: []string{"read", "book"}, TimeoutMS: 30000}
}

// Everything the gateway acts on is covered by the signature, which binds
// it to this installation, this registration, a short window and, for a
// request, the poll it answers.
func TestVerifierRefusesWhatTheHubDidNotSignForThisGateway(t *testing.T) {
	now := time.Now()
	verifier := func() *Verifier {
		v, err := NewVerifier(testPub, "inst", "s", func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return synced(t, v, now)
	}
	_, otherKey, _ := ed25519.GenerateKey(nil)
	good := func() Request {
		rq := sampleRequest()
		rq.Sign(testKey, "inst", testRegistration, testNonce, now)
		return rq
	}
	// a seal the hub key made, with a window of the caller's choice
	window := func(issued time.Time, valid time.Duration) Request {
		rq := sampleRequest()
		rq.Seal = Seal{Installation: "inst", Registration: testRegistration, IssuedAt: issued.UnixMilli(), ExpiresAt: issued.Add(valid).UnixMilli()}
		rq.Signature = ed25519.Sign(testKey, rq.message(testNonce))
		return rq
	}
	for name, rq := range map[string]func() Request{
		"unsigned": func() Request { return sampleRequest() },
		"another key": func() Request {
			rq := sampleRequest()
			rq.Sign(otherKey, "inst", testRegistration, testNonce, now)
			return rq
		},
		"another inst": func() Request {
			rq := sampleRequest()
			rq.Sign(testKey, "inst-b", testRegistration, testNonce, now)
			return rq
		},
		"another poll": func() Request {
			rq := sampleRequest()
			rq.Sign(testKey, "inst", testRegistration, "earlier-poll", now)
			return rq
		},
		"earlier reg": func() Request {
			rq := sampleRequest()
			rq.Sign(testKey, "inst", RegistrationOf(SecretSHA256("old")), testNonce, now)
			return rq
		},
		"expired":           func() Request { return window(now.Add(-SignedFor-clockSkew-time.Second), SignedFor) },
		"not yet valid":     func() Request { return window(now.Add(clockSkew+time.Second), SignedFor) },
		"too long a window": func() Request { return window(now, SignedFor+time.Second) },
		"id":                func() Request { rq := good(); rq.ID = "call-2"; return rq },
		"request id":        func() Request { rq := good(); rq.RequestID = "rq-2"; return rq },
		"method":            func() Request { rq := good(); rq.Method = http.MethodDelete; return rq },
		"path":              func() Request { rq := good(); rq.Path = "/v1/clubs/b/bookings"; return rq },
		"query":             func() Request { rq := good(); rq.Query = "x=2"; return rq },
		"header":            func() Request { rq := good(); rq.Header["Accept"] = "text/plain"; return rq },
		"body":              func() Request { rq := good(); rq.Body = []byte(`{"court":"c2"}`); return rq },
		"partner":           func() Request { rq := good(); rq.Partner = "other"; return rq },
		"scopes":            func() Request { rq := good(); rq.Scopes = []string{"read"}; return rq },
		"timeout":           func() Request { rq := good(); rq.TimeoutMS = 1; return rq },
		"issued":            func() Request { rq := good(); rq.IssuedAt++; return rq },
		"expires":           func() Request { rq := good(); rq.ExpiresAt++; return rq },
	} {
		t.Run(name, func(t *testing.T) {
			r := rq()
			if err := verifier().Request(&r, testNonce); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	t.Run("signed and fresh", func(t *testing.T) {
		clk := now.Add(-time.Hour) // the gateway started an hour ago
		v, err := NewVerifier(testPub, "inst", "s", func() time.Time { return clk })
		if err != nil {
			t.Fatal(err)
		}
		clk = now
		synced(t, v, now)
		for _, skew := range []time.Duration{0, -clockSkew + time.Second, SignedFor + clockSkew - time.Second} {
			rq := window(now.Add(-skew), SignedFor)
			rq.ID = "fresh-" + skew.String()
			rq.Signature = ed25519.Sign(testKey, rq.message(testNonce))
			if err := v.Request(&rq, testNonce); err != nil {
				t.Fatalf("issued %s ago: %v", skew, err)
			}
		}
	})
}

// A club configuration is signed as the bytes that arrive.
func TestVerifierRefusesAnUnsignedConfig(t *testing.T) {
	v, _ := NewVerifier(testPub, "inst", "s", nil)
	good := func() RemoteConfig {
		rc := RemoteConfig{Version: 3, Clubs: []json.RawMessage{[]byte(`{"id":"a"}`)}}
		rc.Sign(testKey, "inst", testRegistration, time.Now())
		// as the gateway reads it from the wire
		data, _ := json.Marshal(rc)
		var out RemoteConfig
		_ = json.Unmarshal(data, &out)
		return out
	}
	if rc := good(); v.Config(&rc) != nil {
		t.Fatal("a signed config refused")
	}
	for name, change := range map[string]func(*RemoteConfig){
		"unsigned":     func(rc *RemoteConfig) { rc.Seal = Seal{} },
		"version":      func(rc *RemoteConfig) { rc.Version = 4 },
		"club":         func(rc *RemoteConfig) { rc.Clubs[0] = []byte(`{"id":"b"}`) },
		"club added":   func(rc *RemoteConfig) { rc.Clubs = append(rc.Clubs, []byte(`{"id":"b"}`)) },
		"another inst": func(rc *RemoteConfig) { rc.Sign(testKey, "inst-b", testRegistration, time.Now()) },
		"expired":      func(rc *RemoteConfig) { rc.Sign(testKey, "inst", testRegistration, time.Now().Add(-2*SignedFor)) },
	} {
		rc := good()
		change(&rc)
		if err := v.Config(&rc); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

// The gateway checks the partner's scopes the hub signed: a read-only
// partner cannot write even through a signed request.
func TestExecuteChecksTheSignedScopes(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }))
	for _, tc := range []struct {
		method string
		scopes []string
		want   int
	}{
		{http.MethodPost, []string{"read"}, http.StatusForbidden},
		{http.MethodGet, []string{"book"}, http.StatusForbidden},
		{http.MethodPost, []string{"read", "book"}, http.StatusCreated},
		{http.MethodGet, []string{"read"}, http.StatusCreated},
	} {
		r := c.Execute(t.Context(), signed(Request{Method: tc.method, Path: "/v1/clubs/a/bookings", Partner: "p", Scopes: tc.scopes}), testNonce)
		if r.Status != tc.want {
			t.Errorf("%s with %v: %d, want %d", tc.method, tc.scopes, r.Status, tc.want)
		}
	}
	if c.Refused() != 0 {
		t.Fatalf("signed requests counted as refused: %d", c.Refused())
	}
}

// The gateway checks signatures against the hub's time, which the hub
// signs on every poll answer with the poll's nonce: a computer clock that
// is off (up to MaxClockSkew) does not refuse what the hub signs now, nor
// accept what it signed long ago; a stamp for another nonce or another
// announced version is refused. Beyond MaxClockSkew nothing is corrected.
func TestVerifierChecksAgainstTheHubsTime(t *testing.T) {
	hubNow := time.Now()
	for _, off := range []time.Duration{-50 * time.Minute, 10 * time.Minute} {
		local := hubNow.Add(-off)
		v, err := NewVerifier(testPub, "inst", "s", func() time.Time { return local })
		if err != nil {
			t.Fatal(err)
		}
		rq := sampleRequest()
		rq.Sign(testKey, "inst", testRegistration, testNonce, hubNow)
		if err := v.Request(&rq, testNonce); !errors.Is(err, ErrClockUnknown) {
			t.Fatalf("clock %s off, no hub time yet: accepted (%v)", off, err)
		}
		if forged := sampleRequest(); !errors.Is(v.Request(&forged, testNonce), ErrBadSignature) {
			t.Fatal("an unsigned request is not refused for its signature while the hub's time is unknown")
		}
		stamp := SignTime(testKey, "inst", testRegistration, "nonce-1", "7", hubNow)
		for name, err := range map[string]error{
			"another nonce":   v.Time("nonce-2", stamp, "7", local),
			"another version": v.Time("nonce-1", stamp, "8", local),
			"forged":          v.Time("nonce-1", SignTime(otherTestKey, "inst", testRegistration, "nonce-1", "7", hubNow), "7", local),
		} {
			if !errors.Is(err, ErrBadSignature) {
				t.Fatalf("%s: stamp accepted (%v)", name, err)
			}
		}
		if _, known := v.Now(); known {
			t.Fatal("a refused stamp made the hub's time known")
		}
		if err := v.Time("nonce-1", stamp, "7", local); err != nil || (v.offset-off).Abs() > time.Millisecond {
			t.Fatalf("stamp: %v, offset %s", err, v.offset)
		}
		rq.ID = "after-stamp"
		rq.Sign(testKey, "inst", testRegistration, testNonce, hubNow)
		if err := v.Request(&rq, testNonce); err != nil {
			t.Fatalf("clock %s off, with the hub's time: %v", off, err)
		}
		old := sampleRequest()
		old.ID = "old"
		old.Sign(testKey, "inst", testRegistration, testNonce, hubNow.Add(-SignedFor-clockSkew-time.Second))
		if err := v.Request(&old, testNonce); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("clock %s off: an expired request accepted (%v)", off, err)
		}
	}
	local := hubNow.Add(-2 * time.Hour)
	v, _ := NewVerifier(testPub, "inst", "s", func() time.Time { return local })
	if err := v.Time("n", SignTime(testKey, "inst", testRegistration, "n", "1", hubNow), "1", local); err != nil || (v.offset-2*time.Hour).Abs() > time.Millisecond {
		t.Fatalf("a large offset is still reported: %v %s", err, v.offset)
	}
	rq := sampleRequest()
	rq.Sign(testKey, "inst", testRegistration, testNonce, hubNow)
	if err := v.Request(&rq, testNonce); !errors.Is(err, ErrClockUnknown) {
		t.Fatalf("beyond MaxClockSkew a request was accepted: %v", err)
	}
	if _, known := v.Now(); known {
		t.Fatal("beyond MaxClockSkew the hub's time counts as known")
	}
}

// The hub's time is taken only from an answer that came back within
// MaxTimeRoundTrip: a slower one would leave it behind by as much.
func TestVerifierTakesTheHubsTimeOnlyFromAQuickAnswer(t *testing.T) {
	hubNow := time.Now()
	local := hubNow
	v, _ := NewVerifier(testPub, "inst", "s", func() time.Time { return local })
	sent := local.Add(-MaxTimeRoundTrip - time.Millisecond)
	if err := v.Time("n", SignTime(testKey, "inst", testRegistration, "n", "1", hubNow), "1", sent); !errors.Is(err, ErrSlowTime) {
		t.Fatalf("a slow answer: %v", err)
	}
	if _, known := v.Now(); known {
		t.Fatal("the time of a slow answer was taken")
	}
	if err := v.Time("n", SignTime(testKey, "inst", testRegistration, "n", "1", hubNow), "1", local.Add(-MaxTimeRoundTrip)); err != nil {
		t.Fatalf("an answer within the bound: %v", err)
	}
	if now, known := v.Now(); !known || now.Sub(hubNow.Truncate(time.Millisecond)).Abs() > time.Second {
		t.Fatalf("hub time %s (known %v), want %s", now, known, hubNow)
	}
}

// A config older than one applied, or than the version the hub announced
// in its signed time, is refused: an old config cannot be replayed.
func TestVerifierRefusesAnOlderConfig(t *testing.T) {
	v, _ := NewVerifier(testPub, "inst", "s", nil)
	config := func(version int64) *RemoteConfig {
		rc := &RemoteConfig{Version: version, Clubs: []json.RawMessage{}}
		rc.Sign(testKey, "inst", testRegistration, time.Now())
		return rc
	}
	if err := v.Config(config(5)); err != nil {
		t.Fatal(err)
	}
	if err := v.Config(config(4)); !errors.Is(err, ErrStaleConfig) {
		t.Fatalf("an older config than applied: %v", err)
	}
	if err := v.Config(config(5)); err != nil {
		t.Fatalf("the same config again: %v", err)
	}
	if err := v.Time("n", SignTime(testKey, "inst", testRegistration, "n", "7", time.Now()), "7", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := v.Config(config(6)); !errors.Is(err, ErrStaleConfig) {
		t.Fatalf("an older config than announced: %v", err)
	}
	if err := v.Config(config(7)); err != nil {
		t.Fatal(err)
	}
}

// The hub's time is carried on by the monotonic clock: a step of this
// computer's clock after the hub signed its time does not move it.
func TestClockStepDoesNotMoveTheHubsTime(t *testing.T) {
	hubNow := time.Now()
	local := hubNow.Add(30 * time.Minute) // this computer is half an hour ahead
	v, _ := NewVerifier(testPub, "inst", "s", func() time.Time { return local })
	synced(t, v, hubNow)
	local = hubNow // then its clock is set right
	if now, known := v.Now(); !known || now.Sub(hubNow).Abs() > time.Second {
		t.Fatalf("hub time %s (known %v) after a clock step, want %s", now, known, hubNow)
	}
}

// The config version the hub announces in its signed time stands, also
// when it went down (the hub was restored from a backup): that config is
// accepted, an older one still refused.
func TestAnnouncedConfigVersionStandsWhenItGoesDown(t *testing.T) {
	v, _ := NewVerifier(testPub, "inst", "s", nil)
	for _, step := range []struct {
		announced, version int64
		ok                 bool
	}{{5, 5, true}, {4, 4, true}, {4, 3, false}} {
		n := strconv.FormatInt(step.announced, 10)
		if err := v.Time("n", SignTime(testKey, "inst", testRegistration, "n", n, time.Now()), n, time.Now()); err != nil {
			t.Fatal(err)
		}
		rc := &RemoteConfig{Version: step.version, Clubs: []json.RawMessage{}}
		rc.Sign(testKey, "inst", testRegistration, time.Now())
		if err := v.Config(rc); (err == nil) != step.ok {
			t.Fatalf("config %d with %d announced: %v", step.version, step.announced, err)
		}
	}
}
