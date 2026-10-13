package hub

import (
	"context"
	"errors"
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
	go func() { picked <- b.Poll(context.Background(), "inst", ConnMeta{}) }() // the gateway's only poll
	for !b.Connected("inst") {
		time.Sleep(time.Millisecond)
	}
	first := make(chan error, 1)
	go func() {
		_, err := b.Do(context.Background(), "inst", tunnel.Request{ID: "a"}, time.Second, 2*time.Second)
		first <- err
	}()
	rq := <-picked // the poll took the request; the gateway is working on it
	time.Sleep(50 * time.Millisecond)
	if !b.Connected("inst") {
		t.Fatal("a gateway serving a request is reported offline")
	}
	if _, err := b.Do(context.Background(), "inst", tunnel.Request{ID: "b"}, 50*time.Millisecond, time.Second); !errors.Is(err, ErrBusy) {
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
	if _, err := b.Do(context.Background(), "inst", tunnel.Request{ID: "c"}, 50*time.Millisecond, time.Second); !errors.Is(err, ErrOffline) {
		t.Fatalf("after it left: %v, want ErrOffline", err)
	}
}
