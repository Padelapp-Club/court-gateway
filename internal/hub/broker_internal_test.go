package hub

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// A gateway whose every poll is serving a request is busy, not offline:
// it stays connected while it works, and a request nobody picks up in
// time is ErrBusy.
func TestBusyGatewayIsNotOffline(t *testing.T) {
	b := NewBroker(time.Second, nil)
	b.grace = 10 * time.Millisecond
	picked := make(chan *tunnel.Request, 1)
	go func() { picked <- b.Poll(context.Background(), "inst", 1, ConnMeta{}) }() // the gateway's only poll
	for !b.Connected("inst") {
		time.Sleep(time.Millisecond)
	}
	first := make(chan error, 1)
	go func() {
		_, err := b.Do(context.Background(), "inst", 1, tunnel.Request{ID: "a"}, time.Second, 2*time.Second)
		first <- err
	}()
	rq := <-picked // the poll took the request; the gateway is working on it
	time.Sleep(50 * time.Millisecond)
	if !b.Connected("inst") {
		t.Fatal("a gateway serving a request is reported offline")
	}
	if _, err := b.Do(context.Background(), "inst", 1, tunnel.Request{ID: "b"}, 50*time.Millisecond, time.Second); !errors.Is(err, ErrBusy) {
		t.Fatalf("second request: %v, want ErrBusy", err)
	}
	if !b.Reply("inst", tunnel.Response{ID: rq.ID, Status: 200}) {
		t.Fatal("reply not delivered")
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if b.Connected("inst") {
		t.Fatal("a gateway with no poll and nothing in flight is still connected")
	}
	if _, err := b.Do(context.Background(), "inst", 1, tunnel.Request{ID: "c"}, 50*time.Millisecond, time.Second); !errors.Is(err, ErrOffline) {
		t.Fatalf("after it left: %v, want ErrOffline", err)
	}
}

// A partner that gives up right after a poll took its request leaves
// nothing behind: the poll, even if it records the call later than the
// partner's cleanup ran, drops it, so no reply owner is missing and the
// gateway is not shown connected by a call nobody will ever answer.
func TestCancelAtPickupLeavesNothingInFlight(t *testing.T) {
	b := NewBroker(time.Second, nil)
	b.grace = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var doErr error
	b.pickedUp = func() { // the partner goes away before the poll records the call
		cancel()
		doErr = <-done
	}
	polled := make(chan *tunnel.Request, 1)
	go func() { polled <- b.Poll(context.Background(), "inst", 1, ConnMeta{}) }()
	for !b.Connected("inst") {
		time.Sleep(time.Millisecond)
	}
	go func() {
		_, err := b.Do(ctx, "inst", 1, tunnel.Request{ID: "gone"}, time.Second, time.Minute)
		done <- err
	}()
	if rq := <-polled; rq != nil {
		t.Fatalf("the poll handed out a request nobody waits for: %+v", rq)
	}
	if !errors.Is(doErr, context.Canceled) {
		t.Fatalf("Do: %v", doErr)
	}
	b.mu.Lock()
	n := len(b.conns["inst"].inflight)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d calls retained in flight", n)
	}
	time.Sleep(50 * time.Millisecond)
	if b.Connected("inst") {
		t.Fatal("a gateway with no poll and nothing to answer is still connected")
	}
}

// SetPickedUp is for the external tests.
func SetPickedUp(b *Broker, f func()) { b.pickedUp = f }

// Discovery reads a whole CRM account: the gateway is given
// DiscoverTimeout for it, the gateway's cap allows it, and the HTTP
// server's write timeout outlasts it.
func TestDiscoverGetsItsOwnTimeout(t *testing.T) {
	st, err := OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstallation("disc-net", ""); err != nil {
		t.Fatal(err)
	}
	code, err := st.CreateJoinCode("disc-net", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := st.Redeem(code.Code)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Settings{PollWait: time.Second, RequestTimeout: time.Second, OfflineAfter: time.Minute, OperatorKey: "synthetic-operator-key-0123456789"}
	srv, err := NewServer(cfg, st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	polled := make(chan *tunnel.Request, 1)
	go func() { polled <- srv.broker.Poll(context.Background(), "disc-net", g.gen, ConnMeta{}) }()
	for !srv.broker.Connected("disc-net") {
		time.Sleep(time.Millisecond)
	}
	req := httptest.NewRequest("POST", "/hub/installations/disc-net/discover", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+cfg.OperatorKey)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { rec := httptest.NewRecorder(); srv.Handler().ServeHTTP(rec, req); done <- rec }()
	rq := <-polled
	if rq == nil || time.Duration(rq.TimeoutMS)*time.Millisecond != DiscoverTimeout || DiscoverTimeout > tunnel.MaxRequestTimeout {
		t.Fatalf("discover request %+v: want a timeout of %s within the gateway's cap %s", rq, DiscoverTimeout, tunnel.MaxRequestTimeout)
	}
	if writeTimeout(cfg) <= DiscoverTimeout+5*time.Second {
		t.Fatalf("write timeout %s does not outlast the discovery", writeTimeout(cfg))
	}
	srv.broker.Reply("disc-net", tunnel.Response{ID: rq.ID, Status: 200, Body: []byte("{}")})
	if rec := <-done; rec.Code != 200 {
		t.Fatalf("discover: %d %s", rec.Code, rec.Body)
	}
}
