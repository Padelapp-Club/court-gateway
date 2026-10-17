package lunda_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// Lunda refuses a session that keeps no CLUB_BOOKING ticket ~20 s after
// its login (seen live 2026-10-17: 403 "missing security details"); the
// stub does too, after SecurityWindow. The gateway keeps the session as
// the admin does — after a login, and when it starts with a saved one —
// so it is still served past that window, and the WebSocket answers the
// pings.
func TestSessionNeedsTheKeepAlive(t *testing.T) {
	t.Run("after a login", func(t *testing.T) {
		f := newAcctFixture(t)
		f.stub.SecurityWindow = 300 * time.Millisecond
		a := f.account(t)
		f.loginQR(t, a)
		d := f.driver(t, a)
		time.Sleep(600 * time.Millisecond)
		if err := d.Ready(context.Background()); err != nil {
			t.Fatalf("past the window: %v", err)
		}
		if _, err := d.Occupancy(context.Background(), occQuery(futureDay())); err != nil {
			t.Fatalf("occupancy past the window: %v", err)
		}
		testkit.WaitFor(t, "pongs", 5*time.Second, func() bool { _, _, pongs := f.stub.WebSockets(); return pongs > 0 })
		ticket := f.stub.Calls()[findCall(f, "/club/websocket/ticket/register")]
		if ticket.Query != "relatedEntityUid=&ticketType=CLUB_BOOKING" || ticket.Token != f.store.token() {
			t.Fatalf("ticket call: %+v", ticket)
		}
	})
	t.Run("a saved session", func(t *testing.T) {
		f := newAcctFixture(t)
		f.stub.SecurityWindow = 300 * time.Millisecond
		f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName))
		d := f.driver(t, f.account(t))
		time.Sleep(600 * time.Millisecond)
		if err := d.Ready(context.Background()); err != nil {
			t.Fatalf("past the window: %v", err)
		}
	})
}

// A new login while the WebSocket of the old session is open reconnects it
// with a ticket of the new session; a renewal of the same session does not.
func TestNewLoginReconnectsTheKeepAlive(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	f.loginQR(t, a)
	first := f.store.token()
	f.loginQR(t, a) // waits for a new WebSocket
	second := f.store.token()
	var tokens []string
	for _, c := range f.stub.Calls() {
		if c.Path == "/club/websocket/ticket/register" {
			tokens = append(tokens, c.Token)
		}
	}
	if len(tokens) != 2 || tokens[0] != first || tokens[1] != second {
		t.Fatalf("tickets registered with %d tokens", len(tokens))
	}
	f.stub.RequireRefresh()
	_, opened, _ := f.stub.WebSockets()
	if err := a.CheckLogin(context.Background()); err != nil || f.stub.Renewals() != 1 {
		t.Fatalf("renewal: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, now, _ := f.stub.WebSockets(); now != opened {
		t.Fatalf("a renewal reconnected the WebSocket")
	}
}

// Lunda closing the WebSocket: the gateway reconnects with a new ticket.
// Lunda refusing it: calls go on, one WARN for the whole outage; back, an
// INFO. Neither a ticket nor anything of a frame is logged.
func TestKeepAliveReconnectsAndWarnsOnce(t *testing.T) {
	defer lunda.SetWSRetry(20*time.Millisecond, 40*time.Millisecond)()
	f, d := loggedIn(t)
	_, opened, _ := f.stub.WebSockets()
	f.stub.DropWebSockets()
	waitKeepAlive(t, f.stub, opened)
	if n := len(f.stub.Tickets()); n != 2 {
		t.Fatalf("tickets: %d", n)
	}
	f.stub.RefuseWS = true
	f.stub.DropWebSockets()
	testkit.WaitFor(t, "fifteen refused openings", 10*time.Second, func() bool { return len(f.stub.Tickets()) >= 17 })
	if _, err := d.Occupancy(context.Background(), occQuery(futureDay())); err != nil {
		t.Fatalf("occupancy during the outage: %v", err)
	}
	if n := strings.Count(f.logs.String(), "keep-alive connection is down"); n != 1 {
		t.Fatalf("%d WARN lines for one outage: %s", n, f.logs.String())
	}
	_, opened, _ = f.stub.WebSockets()
	f.stub.RefuseWS = false
	waitKeepAlive(t, f.stub, opened)
	testkit.WaitFor(t, "the INFO", 5*time.Second, func() bool { return strings.Contains(f.logs.String(), "keep-alive connection is up again") })
	logs := f.logs.String()
	for _, tk := range f.stub.Tickets() {
		if strings.Contains(logs, tk) {
			t.Fatal("a ticket is logged")
		}
	}
	if strings.Contains(logs, "HELLO") {
		t.Fatal("a frame is logged")
	}
}

// The keep-alive stops with the account (EndSession): the WebSocket is
// closed and nothing is registered after; the session stays saved.
func TestKeepAliveStopsWithTheAccount(t *testing.T) {
	f, _ := loggedIn(t)
	a := f.account(t) // a second account on the same saved session
	waitKeepAlive(t, f.stub, 1)
	tickets := len(f.stub.Tickets())
	open, _, _ := f.stub.WebSockets()
	if err := driver.EndSession(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	testkit.WaitFor(t, "its WebSocket closed", 5*time.Second, func() bool { now, _, _ := f.stub.WebSockets(); return now == open-1 })
	time.Sleep(100 * time.Millisecond)
	if len(f.stub.Tickets()) != tickets || f.store.token() == "" {
		t.Fatalf("tickets %d after %d, session kept %v", len(f.stub.Tickets()), tickets, f.store.token() != "")
	}
}
