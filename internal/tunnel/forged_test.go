package tunnel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Whoever stands in for the hub with a certificate the club's computer
// trusts (a TLS-inspecting proxy) knows the gateway secret, but cannot
// make the gateway book: only what the hub's pinned key signed for this
// registration, within its window and once, reaches the CRM. Every forged
// request here would book a free slot if it were run.
func TestForgedRequestsDoNotReachTheCRM(t *testing.T) {
	const secret = "fixture-hub-secret"
	clubs, stub := vivaClub(t, secret)

	hubPub, hubKey, _ := ed25519.GenerateKey(nil)
	_, otherKey, _ := ed25519.GenerateKey(nil)
	registration := tunnel.RegistrationOf(tunnel.SecretSHA256(secret))
	envelopes := make(chan tunnel.Request)
	replies := make(chan tunnel.Response)
	fake := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.PathPoll:
			w.Header().Set(tunnel.HeaderHubTime, tunnel.SignTime(hubKey, "inst", registration, r.Header.Get(tunnel.HeaderNonce), "", time.Now()))
			w.WriteHeader(http.StatusOK)
			select {
			case rq := <-envelopes:
				_ = json.NewEncoder(w).Encode(rq)
			case <-r.Context().Done():
			}
		case tunnel.PathReply:
			var resp tunnel.Response
			_ = json.NewDecoder(r.Body).Decode(&resp)
			w.WriteHeader(http.StatusNoContent)
			replies <- resp
		}
	}))
	defer fake.Close()
	roots := x509.NewCertPool()
	roots.AddCert(fake.Certificate()) // the machine trusts the interceptor
	c, err := tunnel.NewClient(tunnel.ClientConfig{HubURL: fake.URL, Secret: secret, HubKey: hubPub, Installation: "inst", Polls: 1,
		Handler: clubs.TunnelHandler(), Transport: tunnel.NewTransport(roots, nil)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	defer func() { cancel(); <-done }()

	hour := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	n := 0
	// booking is a request to book a slot no other case books
	booking := func() tunnel.Request {
		n++
		start := hour.Add(time.Duration(n) * time.Hour)
		body, _ := json.Marshal(map[string]string{"court": "court-one", "start": start.Format(time.RFC3339), "end": start.Add(time.Hour).Format(time.RFC3339), "kind": "game"})
		return tunnel.Request{ID: fmt.Sprintf("call-%d", n), RequestID: "rq", Method: http.MethodPost, Path: "/v1/clubs/club-one/bookings",
			Header: map[string]string{"Content-Type": "application/json"}, Body: body, Partner: "booker", Scopes: []string{"read", "book"}}
	}
	signed := func(rq tunnel.Request) tunnel.Request {
		rq.Sign(hubKey, "inst", registration, time.Now())
		return rq
	}
	run := func(rq tunnel.Request) tunnel.Response {
		t.Helper()
		select {
		case envelopes <- rq:
		case <-time.After(5 * time.Second):
			t.Fatal("not polled")
		}
		select {
		case r := <-replies:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("no reply")
		}
		return tunnel.Response{}
	}
	writes := func() int {
		k := 0
		for _, call := range stub.Calls() {
			if strings.HasPrefix(call, "POST ") {
				k++
			}
		}
		return k
	}

	genuine := signed(booking())
	if r := run(genuine); r.Status != http.StatusCreated || writes() != 1 {
		t.Fatalf("a signed booking: %d %s, %d CRM writes", r.Status, r.Body, writes())
	}
	for name, forge := range map[string]func() tunnel.Request{
		"unsigned":                 booking,
		"signed by another key":    func() tunnel.Request { rq := booking(); rq.Sign(otherKey, "inst", registration, time.Now()); return rq },
		"for another installation": func() tunnel.Request { rq := booking(); rq.Sign(hubKey, "inst-b", registration, time.Now()); return rq },
		"for an earlier registration": func() tunnel.Request {
			rq := booking()
			rq.Sign(hubKey, "inst", tunnel.RegistrationOf("old"), time.Now())
			return rq
		},
		"expired": func() tunnel.Request {
			rq := booking()
			rq.Sign(hubKey, "inst", registration, time.Now().Add(-5*time.Minute))
			return rq
		},
		"replayed":        func() tunnel.Request { return genuine },
		"body swapped":    func() tunnel.Request { rq := signed(booking()); rq.Body = booking().Body; return rq },
		"path changed":    func() tunnel.Request { rq := signed(booking()); rq.Path = "/v1/clubs/club-one/bookings/"; return rq },
		"partner changed": func() tunnel.Request { rq := signed(booking()); rq.Partner = "invented-partner"; return rq },
		"scope added": func() tunnel.Request {
			rq := booking()
			rq.Scopes = []string{"read"}
			rq = signed(rq)
			rq.Scopes = []string{"read", "book"}
			return rq
		},
	} {
		r := run(forge())
		if r.Status != http.StatusBadGateway || !strings.Contains(string(r.Body), "SIGNATURE_REJECTED") || writes() != 1 {
			t.Errorf("%s: %d %s, %d CRM writes (want 502 SIGNATURE_REJECTED, no write)", name, r.Status, r.Body, writes())
		}
	}
	if got := c.Refused(); got != 10 {
		t.Errorf("refused %d, want 10", got)
	}
}

// vivaClub serves club-one (court-one) of a Viva stub as a gateway does.
func vivaClub(t *testing.T, secret string) (*core.Clubs, *vivastub.Stub) {
	t.Helper()
	const studio, room = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000011"
	stub := vivastub.New("fixture@example.test", "fixture-password", studio)
	stub.AddRoom(room, "Court")
	crm := httptest.NewServer(stub)
	t.Cleanup(crm.Close)
	base, err := core.Load([]byte(fmt.Sprintf("hub: {url: https://hub.example.test, installation: inst, key: %s}\ncrm: {type: viva, username: fixture@example.test, auth_url: %s/token, api_url: %s/api/v1}\n",
		core.EncodeHubKey(make([]byte, 32)), crm.URL, crm.URL)), func(k string) string {
		return map[string]string{core.PasswordEnv: "fixture-password", core.DefaultHubSecretEnv: secret}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := core.ClubSpec{ID: "club-one", Name: "Club", Timezone: "Etc/GMT-3", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "00:00-24:00"},
		CRM: map[string]any{"studio": studio}, Courts: []core.CourtSpec{{ID: "court-one", Name: "Court", Params: map[string]any{"viva_room": room}}}}
	s, err := base.WithClub(spec, viva.ClubCRMKeys)
	if err != nil {
		t.Fatal(err)
	}
	d, err := viva.NewDriver(s.Driver)
	if err != nil {
		t.Fatal(err)
	}
	clubs := core.NewClubs(core.ClubsOptions{})
	clubs.Set([]*core.Server{core.NewServer(s, d, nil)})
	return clubs, stub
}

// A signed booking replayed to a restarted gateway is refused, however
// fresh its signature: the gateway forgot the ids it ran, but the request
// was issued before this process started by the hub's clock. The booking
// was cancelled in between, so running the replay would book it again.
func TestReplayAfterRestartIsRefused(t *testing.T) {
	sg := testkit.NewSigning("inst")
	clubs, stub := vivaClub(t, sg.Secret)
	posts := func() int {
		n := 0
		for _, call := range stub.Calls() {
			if strings.HasPrefix(call, "POST /breaks") {
				n++
			}
		}
		return n
	}
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	body, _ := json.Marshal(map[string]string{"court": "court-one", "start": start.Format(time.RFC3339), "end": start.Add(time.Hour).Format(time.RFC3339), "kind": "game"})
	c := sg.Client(t, clubs.TunnelHandler())
	create := sg.Sign(tunnel.Request{Method: http.MethodPost, Path: "/v1/clubs/club-one/bookings", Body: body, Partner: "booker", Scopes: []string{"read", "book"}})
	first := c.Execute(t.Context(), create)
	var b struct{ ID string }
	if json.Unmarshal(first.Body, &b); first.Status != http.StatusCreated {
		t.Fatalf("create: %d %s", first.Status, first.Body)
	}
	if r := c.Execute(t.Context(), sg.Sign(tunnel.Request{Method: http.MethodDelete, Path: create.Path + "/" + b.ID, Partner: "booker", Scopes: []string{"book"}})); r.Status != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", r.Status, r.Body)
	}
	restarted := sg.Client(t, clubs.TunnelHandler())
	if r := restarted.Execute(t.Context(), create); r.Status != http.StatusBadGateway || !strings.Contains(string(r.Body), "SIGNATURE_REJECTED") || posts() != 1 {
		t.Fatalf("the create replayed after a restart: %d %s, %d CRM writes", r.Status, r.Body, posts())
	}
}

// A club config is applied only when the pinned hub key signed it for
// this gateway: FetchConfig refuses anything else.
func TestForgedConfigIsNotFetched(t *testing.T) {
	hubPub, hubKey, _ := ed25519.GenerateKey(nil)
	_, otherKey, _ := ed25519.GenerateKey(nil)
	var serve tunnel.RemoteConfig
	fake := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(serve)
	}))
	defer fake.Close()
	c, err := tunnel.NewClient(tunnel.ClientConfig{HubURL: fake.URL, Secret: "s", HubKey: hubPub, Installation: "inst",
		Handler: http.NotFoundHandler(), Transport: fake.Client().Transport})
	if err != nil {
		t.Fatal(err)
	}
	registration := tunnel.RegistrationOf(tunnel.SecretSHA256("s"))
	club := json.RawMessage(`{"id":"club-one","name":"Club <&>"}`)
	for _, tc := range []struct {
		name string
		key  ed25519.PrivateKey
		ok   bool
	}{{"signed by the hub", hubKey, true}, {"signed by another key", otherKey, false}, {"unsigned", nil, false}} {
		serve = tunnel.RemoteConfig{Version: 2, Clubs: []json.RawMessage{club}}
		if tc.key != nil {
			serve.Sign(tc.key, "inst", registration, time.Now())
		}
		rc, err := c.FetchConfig(context.Background())
		if tc.ok != (err == nil) || (tc.ok && rc.Version != 2) || (!tc.ok && !errors.Is(err, tunnel.ErrBadSignature)) {
			t.Errorf("%s: %+v %v", tc.name, rc, err)
		}
	}
}
