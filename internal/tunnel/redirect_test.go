package tunnel_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// plainHub is a plain-HTTP server that would play the hub: it counts the
// requests that reach it and answers each as the hub would, with a
// request to book in a poll answer.
func plainHub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == tunnel.PathPoll {
			_ = json.NewEncoder(w).Encode(tunnel.Request{ID: "forged", Method: http.MethodPost, Path: "/v1/clubs/club-one/bookings", Partner: "booker"})
			return
		}
		_, _ = w.Write([]byte(`{"version":1,"clubs":[],"installation_id":"inst","secret":"s"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
	}
}

// An HTTPS hub address that redirects to plain HTTP gets the gateway
// nowhere: no request follows the redirect — not a poll, a reply, a
// heartbeat or a config pull — so the secret never leaves the verified TLS
// connection and nothing from elsewhere is taken for the hub's answer.
func TestHubRedirectsAreNotFollowed(t *testing.T) {
	plain, plainHits := plainHub(t)
	pub, key, _ := ed25519.GenerateKey(nil)
	genuine := tunnel.Request{ID: "real", Method: http.MethodGet, Path: "/v1/clubs", Partner: "reader", Scopes: []string{"read"}}
	genuine.Sign(key, "inst", tunnel.RegistrationOf(tunnel.SecretSHA256("fixture-gateway-secret")), time.Now())
	var polls atomic.Int32
	replied := make(chan struct{})
	var once sync.Once
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == tunnel.PathPoll && polls.Add(1) == 1:
			// one genuine request, so that its reply goes out too
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(genuine)
			return
		case r.URL.Path == tunnel.PathReply:
			once.Do(func() { close(replied) })
		}
		redirectTo(plain.URL)(w, r)
	}))
	defer secure.Close()

	var mu sync.Mutex
	var partners []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PartnerName(r.Context())
		mu.Lock()
		partners = append(partners, p)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	c, err := tunnel.NewClient(tunnel.ClientConfig{HubURL: secure.URL, Secret: "fixture-gateway-secret", Polls: 1, Handler: handler,
		HubKey: pub, Installation: "inst",
		Transport: secure.Client().Transport, MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.FetchConfig(context.Background()); !errors.Is(err, tunnel.ErrRedirected) {
		t.Fatalf("config: %v, want ErrRedirected", err)
	}
	if err := c.SendHeartbeat(context.Background(), tunnel.Heartbeat{Ready: true}); !errors.Is(err, tunnel.ErrRedirected) {
		t.Fatalf("heartbeat: %v, want ErrRedirected", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	select {
	case <-replied:
	case <-ctx.Done():
		t.Fatal("no reply was sent")
	}
	for polls.Load() < 3 && ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond) // a few redirected polls after it
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("%d requests followed the redirect to plain HTTP", n)
	}
	if len(partners) != 1 || partners[0] != "reader" {
		t.Fatalf("requests run: %v, want only the genuine one", partners)
	}
	if c.Connected() {
		t.Fatal("a redirected poll counts as connected")
	}
}

// Registration (the join code) and whoami (the secret) do not follow a
// redirect either.
func TestRegisterAndWhoamiDoNotFollowRedirects(t *testing.T) {
	plain, plainHits := plainHub(t)
	secure := httptest.NewTLSServer(redirectTo(plain.URL))
	defer secure.Close()
	rt := secure.Client().Transport
	if _, err := tunnel.Register(context.Background(), rt, secure.URL, "ABCD-EFGH", "1.0.0"); !errors.Is(err, tunnel.ErrRedirected) {
		t.Fatalf("register: %v, want ErrRedirected", err)
	}
	if _, err := tunnel.FetchWhoami(context.Background(), rt, secure.URL, "fixture-gateway-secret"); !errors.Is(err, tunnel.ErrRedirected) {
		t.Fatalf("whoami: %v, want ErrRedirected", err)
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("%d requests followed the redirect to plain HTTP", n)
	}
}
