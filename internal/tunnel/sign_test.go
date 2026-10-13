package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// The hub key and registration of the client tests: installation "inst",
// secret "s".
var (
	testPub, testKey, _ = ed25519.GenerateKey(nil)
	testRegistration    = RegistrationOf(SecretSHA256("s"))
)

// signed signs rq for the test registration, with a fresh id.
func signed(rq Request) Request {
	var id [8]byte
	_, _ = rand.Read(id[:])
	rq.ID = hex.EncodeToString(id[:])
	rq.Sign(testKey, "inst", testRegistration, time.Now())
	return rq
}

func sampleRequest() Request {
	return Request{ID: "call-1", RequestID: "rq-1", Method: http.MethodPost, Path: "/v1/clubs/a/bookings", Query: "x=1",
		Header: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"court":"c1"}`),
		Partner: "booker", Scopes: []string{"read", "book"}, TimeoutMS: 30000}
}

// Everything the gateway acts on is covered by the signature, which binds
// it to this installation, this registration and a short window; a
// request is accepted once.
func TestVerifierRefusesWhatTheHubDidNotSignForThisGateway(t *testing.T) {
	now := time.Now()
	verifier := func() *Verifier {
		v, err := NewVerifier(testPub, "inst", "s", func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	_, otherKey, _ := ed25519.GenerateKey(nil)
	good := func() Request {
		rq := sampleRequest()
		rq.Sign(testKey, "inst", testRegistration, now)
		return rq
	}
	// a seal the hub key made, with a window of the caller's choice
	window := func(issued time.Time, valid time.Duration) Request {
		rq := sampleRequest()
		rq.Seal = Seal{Installation: "inst", Registration: testRegistration, IssuedAt: issued.UnixMilli(), ExpiresAt: issued.Add(valid).UnixMilli()}
		rq.Signature = ed25519.Sign(testKey, rq.Seal.message("request", rq.fields))
		return rq
	}
	for name, rq := range map[string]func() Request{
		"unsigned":     func() Request { return sampleRequest() },
		"another key":  func() Request { rq := sampleRequest(); rq.Sign(otherKey, "inst", testRegistration, now); return rq },
		"another inst": func() Request { rq := sampleRequest(); rq.Sign(testKey, "inst-b", testRegistration, now); return rq },
		"earlier reg": func() Request {
			rq := sampleRequest()
			rq.Sign(testKey, "inst", RegistrationOf(SecretSHA256("old")), now)
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
			if err := verifier().Request(&r); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	t.Run("signed and fresh, once", func(t *testing.T) {
		v := verifier()
		for _, skew := range []time.Duration{0, -clockSkew + time.Second, SignedFor + clockSkew - time.Second} {
			rq := window(now.Add(-skew), SignedFor)
			rq.ID = "fresh-" + skew.String()
			rq.Signature = ed25519.Sign(testKey, rq.Seal.message("request", rq.fields))
			if err := v.Request(&rq); err != nil {
				t.Fatalf("issued %s ago: %v", skew, err)
			}
		}
		rq := good()
		if err := v.Request(&rq); err != nil {
			t.Fatal(err)
		}
		if err := v.Request(&rq); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("a replay was accepted: %v", err)
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
	c, _ := NewClient(ClientConfig{HubURL: "https://hub.invalid", Secret: "s", HubKey: testPub, Installation: "inst",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })})
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
		r := c.Execute(t.Context(), signed(Request{Method: tc.method, Path: "/v1/clubs/a/bookings", Partner: "p", Scopes: tc.scopes}))
		if r.Status != tc.want {
			t.Errorf("%s with %v: %d, want %d", tc.method, tc.scopes, r.Status, tc.want)
		}
	}
	if c.Refused() != 0 {
		t.Fatalf("signed requests counted as refused: %d", c.Refused())
	}
}
