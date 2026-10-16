package hub_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// A gateway whose registration is replaced (a new join code redeemed) or
// removed (the installation deleted and recreated for another account)
// gets no new work on the poll it still holds open: the poll ends empty
// at once, and a partner request finds the club offline until the new
// registration polls.
func TestReplacedRegistrationGetsNoNewWork(t *testing.T) {
	t.Run("secret rotated", func(t *testing.T) { staleRegistration(t, false) })
	t.Run("installation deleted and recreated", func(t *testing.T) { staleRegistration(t, true) })
}

func staleRegistration(t *testing.T, recreate bool) {
	store, err := hub.OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInstallation("gen-net", "Gen"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PutClub("gen-net", core.ClubSpec{ID: "gen-club", Name: "Gen"}); err != nil {
		t.Fatal(err)
	}
	redeem := func() string {
		code, err := store.CreateJoinCode("gen-net", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, secret, err := store.Redeem(code.Code)
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	oldSecret := redeem()
	const partnerKey = "synthetic-partner-key-0123456789"
	cfg := &hub.Settings{PollWait: 10 * time.Second, RequestTimeout: time.Second, OfflineAfter: 3 * time.Minute,
		Partners:     []auth.Partner{{Name: "partner", Key: partnerKey, Scopes: []auth.Scope{auth.ScopeRead, auth.ScopeBook}}},
		PartnerClubs: map[string][]string{"partner": {"gen-club"}}}
	srv, err := hub.NewServer(cfg, store, hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewTLSServer(srv.Handler())
	defer hs.Close()
	client := hs.Client()
	poll, _ := http.NewRequest("POST", hs.URL+tunnel.PathPoll, nil)
	poll.Header.Set("Authorization", "Bearer "+oldSecret)
	resp, err := client.Do(poll)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for deadline := time.Now().Add(5 * time.Second); !srv.Broker().Connected("gen-net"); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("old poll never connected")
		}
	}
	if recreate {
		if err := store.DeleteInstallation("gen-net"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateInstallation("gen-net", "Another CRM account"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.PutClub("gen-net", core.ClubSpec{ID: "gen-club", Name: "Another club"}); err != nil {
			t.Fatal(err)
		}
	}
	redeem()
	if _, err := store.Authenticate(oldSecret); err == nil {
		t.Fatal("old secret still authenticates")
	}
	// the partner's request is made before the old poll's body is read: it
	// must not reach that poll
	post, _ := http.NewRequest("POST", hs.URL+"/v1/clubs/gen-club/bookings", bytes.NewBufferString(`{"court":"court-1"}`))
	post.Header.Set("Authorization", "Bearer "+partnerKey)
	pr, err := client.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	pbody, _ := io.ReadAll(pr.Body)
	pr.Body.Close()
	if pr.StatusCode != http.StatusServiceUnavailable || !bytes.Contains(pbody, []byte("CLUB_OFFLINE")) {
		t.Fatalf("partner request: %d %s; want 503 CLUB_OFFLINE", pr.StatusCode, pbody)
	}
	got := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(resp.Body); got <- b }()
	select {
	case b := <-got:
		if len(bytes.TrimSpace(b)) != 0 {
			t.Fatalf("the old registration's poll received work: %s", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the old registration's poll stayed open")
	}
}

// The race inside the broker: a request handed to a poll of the old
// registration just as the registration is replaced is dropped by that
// poll and reported offline, never run.
func TestRetiredBetweenHandoffAndPickup(t *testing.T) {
	b := hub.NewBroker(time.Minute, nil)
	polled := make(chan *tunnel.Request, 1)
	hub.SetPickedUp(b, func() { b.Retire("inst", 2) })
	go func() { polled <- b.Poll(context.Background(), "inst", 1, hub.ConnMeta{}) }()
	for !b.Connected("inst") {
		time.Sleep(time.Millisecond)
	}
	if _, err := b.Do(context.Background(), "inst", 1, tunnel.Request{ID: "x"}, time.Second, time.Minute); err != hub.ErrOffline {
		t.Fatalf("Do = %v, want ErrOffline", err)
	}
	if rq := <-polled; rq != nil {
		t.Fatalf("a retired registration's poll took %+v", rq)
	}
	// a poll of the retired generation arriving late ends at once and does
	// not make the installation look connected
	late := make(chan *tunnel.Request, 1)
	go func() { late <- b.Poll(context.Background(), "inst", 1, hub.ConnMeta{}) }()
	select {
	case rq := <-late:
		if rq != nil {
			t.Fatalf("late poll of a retired generation: %+v", rq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a late poll of a retired generation stays open")
	}
	if b.Connected("inst") {
		t.Fatal("a retired generation shows the installation connected")
	}
}

// A request the old registration had already picked up when it was
// replaced ends at once with ErrReplaced (partners: 502 OUTCOME_UNKNOWN),
// not after the full wait: its reply can no longer be delivered.
func TestReplacedWhileRunningEndsAtOnce(t *testing.T) {
	b := hub.NewBroker(time.Minute, nil)
	picked := make(chan *tunnel.Request, 1)
	go func() { picked <- b.Poll(context.Background(), "inst", 1, hub.ConnMeta{}) }()
	for !b.Connected("inst") {
		time.Sleep(time.Millisecond)
	}
	result := make(chan error, 1)
	go func() {
		_, err := b.Do(context.Background(), "inst", 1, tunnel.Request{ID: "running"}, time.Second, time.Minute)
		result <- err
	}()
	if rq := <-picked; rq == nil {
		t.Fatal("not picked up")
	}
	b.Retire("inst", 2)
	select {
	case err := <-result:
		if !errors.Is(err, hub.ErrReplaced) {
			t.Fatalf("Do = %v, want ErrReplaced", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller still waits for a reply that cannot arrive")
	}
	if b.Reply("inst", tunnel.Response{ID: "running", Status: http.StatusCreated}) {
		t.Fatal("a reply for the replaced registration was taken")
	}
}

// A state directory that cannot be read for a moment is not a removed
// registration: the gateway gets 503 (and retries with the same secret),
// not 401, and nothing is forgotten once the directory is readable again.
func TestUnreadableStateIsNotDeregistration(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced for this user")
	}
	dir := t.TempDir()
	store, err := hub.OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, err := hub.CreateJoinCode(hub.FileState(dir), "perm-net", "", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := store.Redeem(code.Code)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := hub.NewServer(&hub.Settings{PollWait: time.Second, RequestTimeout: time.Second, OfflineAfter: time.Minute}, store, hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	gateways := filepath.Join(dir, "gateways")
	if err := os.Chmod(gateways, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(gateways, 0o700)
	if _, err := store.Authenticate(secret); !errors.Is(err, hub.ErrStateUnavailable) {
		t.Fatalf("Authenticate = %v, want ErrStateUnavailable", err)
	}
	req := httptest.NewRequest("GET", tunnel.PathWhoami, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !bytes.Contains(rec.Body.Bytes(), []byte("HUB_UNAVAILABLE")) {
		t.Fatalf("whoami with an unreadable state dir: %d %s; want 503 HUB_UNAVAILABLE", rec.Code, rec.Body)
	}
	if err := os.Chmod(gateways, 0o700); err != nil {
		t.Fatal(err)
	}
	if g, err := store.Authenticate(secret); err != nil || g.Installation != "perm-net" {
		t.Fatalf("after the directory is readable again: %+v, %v", g, err)
	}
}

// The hub makes its signing key on the first start, keeps it closed in
// state_dir and the same across restarts, refuses to start on a damaged
// one, and hands its public half to every gateway that registers.
func TestHubSigningKeyIsMadeOnceAndPinnedAtRegistration(t *testing.T) {
	dir := t.TempDir()
	first, err := hub.OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := hub.OpenStore(dir, nil)
	if err != nil || !again.HubKey().Equal(first.HubKey()) {
		t.Fatalf("key changed across a restart: %v", err)
	}
	p := filepath.Join(dir, "hub-signing.key")
	if fi, err := os.Stat(p); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("key file: %v %v", fi, err)
	}
	h := testkit.StartHub(t, "", nil, hub.Options{})
	rr, err := tunnel.Register(context.Background(), h.Transport(), h.URL(), h.JoinCode("key-net", ""), "1.0.0")
	if err != nil || rr.HubKey != core.EncodeHubKey(h.Server().Store().HubKey()) {
		t.Fatalf("registration answer: %+v %v", rr, err)
	}
	if err := os.WriteFile(p, []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.OpenStore(dir, nil); err == nil {
		t.Fatal("the hub starts with a damaged signing key")
	}
}
