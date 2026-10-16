package viva_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

// memRejections is a driver.RejectionStore in memory, shared by the
// "runs" of one installation.
type memRejections struct {
	mu sync.Mutex
	r  driver.Rejections
}

func (m *memRejections) LoadRejections() (driver.Rejections, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.r, nil
}

func (m *memRejections) SaveRejections(r driver.Rejections) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.r = r
	return nil
}

// lockoutCRM is the stub behind Keycloak's documented temporary lockout:
// 30 failed password logins with no 12-hour pause between them disable the
// account for a while, for the right password too.
type lockoutCRM struct {
	stub     *vivastub.Stub
	clk      *clock
	mu       sync.Mutex
	failures int
	last     time.Time
	until    time.Time
}

func (l *lockoutCRM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == vivastub.TokenPath && r.ParseForm() == nil && r.PostForm.Get("grant_type") == "password" {
		l.mu.Lock()
		now := l.clk.now()
		locked := now.Before(l.until)
		if !locked && r.PostForm.Get("password") != stubPass {
			if now.Sub(l.last) > 12*time.Hour {
				l.failures = 0
			}
			l.failures++
			l.last = now
			if l.failures >= 30 {
				l.until = now.Add(time.Duration(l.failures/30) * time.Minute)
			}
		}
		l.mu.Unlock()
		if locked {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Account temporarily disabled"}`))
			return
		}
	}
	l.stub.ServeHTTP(w, r)
}

func (l *lockoutCRM) everLocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.until.IsZero()
}

func accountOn(t *testing.T, srv *httptest.Server, password string, clk *clock, store driver.RejectionStore) driver.Account {
	t.Helper()
	a, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": srv.URL + "/token", "api_url": srv.URL + "/api/v1"},
		Password: password, HTTPClient: srv.Client(), Now: clk.now, Rejections: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A wrong password, asked about every minute for thirty days, never gets
// the account locked under Keycloak's lockout policy, and neither does a
// gateway restarted every ten minutes all that time: the rejections are
// counted across restarts, a restart tries once only four hours after the
// last attempt, and never past the cap. The right password then logs in.
func TestWrongPasswordNeverLocksTheAccount(t *testing.T) {
	for name, restartEvery := range map[string]time.Duration{"one run": 0, "restart storm": 10 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			clk := &clock{t: start}
			crm := &lockoutCRM{stub: vivastub.New(stubUser, stubPass, studio), clk: clk}
			srv := httptest.NewServer(crm)
			t.Cleanup(srv.Close)
			store := &memRejections{}
			acct := accountOn(t, srv, "wrong-password-0000", clk, store)
			for minute := 0; minute < 30*24*60; minute++ {
				if restartEvery > 0 && minute > 0 && time.Duration(minute)*time.Minute%restartEvery == 0 {
					acct = accountOn(t, srv, "wrong-password-0000", clk, store)
				}
				if err := acct.CheckLogin(context.Background()); core.ReadyReason(err) != "CRM_AUTH_FAILED" {
					t.Fatalf("minute %d: %v", minute, err)
				}
				clk.add(time.Minute)
			}
			want := 4
			if restartEvery > 0 {
				want = 8
			}
			if n := crm.stub.LoginAttempts(); n != want || crm.everLocked() {
				t.Fatalf("%d logins in thirty days (want %d), locked %v", n, want, crm.everLocked())
			}
			if err := accountOn(t, srv, stubPass, clk, store).CheckLogin(context.Background()); err != nil {
				t.Fatalf("the right password: %v", err)
			}
			if r, _ := store.LoadRejections(); r.Count != 0 {
				t.Fatalf("rejections kept after the right password: %+v", r)
			}
		})
	}
}

// A restart counts on from the rejections kept: within the budget at the
// times of the first rejection's schedule, not afresh; with it spent, one
// login only when the last was four hours ago. Rejections of other
// credentials (the password changed in .env while the gateway was
// stopped) do not count: it logs in at once.
func TestRejectionsAreKeptAcrossRestarts(t *testing.T) {
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := &clock{t: start}
	stub := vivastub.New(stubUser, stubPass, studio)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	store := &memRejections{}
	check := func(password string) error {
		return accountOn(t, srv, password, clk, store).CheckLogin(context.Background())
	}
	_ = check("wrong-password-0000") // the first rejection
	clk.add(10 * time.Minute)
	if err := check("wrong-password-0000"); err == nil || stub.LoginAttempts() != 1 {
		t.Fatalf("a restart before the first retry logged in: %d attempts", stub.LoginAttempts())
	}
	clk.add(5 * time.Minute)
	if _ = check("wrong-password-0000"); stub.LoginAttempts() != 2 {
		t.Fatalf("the retry at 15 minutes: %d attempts", stub.LoginAttempts())
	}
	clk.add(4 * time.Hour) // both remaining retries are due: one login now
	if _ = check("wrong-password-0000"); stub.LoginAttempts() != 3 {
		t.Fatalf("restarted with retries due: %d attempts", stub.LoginAttempts())
	}
	store.r.Count = 4 // spent
	clk.add(3 * time.Hour)
	if _ = check("wrong-password-0000"); stub.LoginAttempts() != 3 {
		t.Fatalf("restarted three hours after the last: %d attempts", stub.LoginAttempts())
	}
	clk.add(time.Hour)
	if _ = check("wrong-password-0000"); stub.LoginAttempts() != 4 {
		t.Fatalf("restarted four hours after the last: %d attempts", stub.LoginAttempts())
	}
	if err := check(stubPass); err != nil || stub.LoginAttempts() != 5 {
		t.Fatalf("another password: %v, %d attempts", err, stub.LoginAttempts())
	}
}

// Keycloak's error_description is never passed on: a description that
// echoes a live token (of a refused renewal or a login) reaches neither the
// error nor the log, which names a fixed label instead.
func TestKeycloakTextNeverReachesTheLog(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	var mu sync.Mutex
	var echoed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == vivastub.TokenPath && r.ParseForm() == nil && r.PostForm.Get("grant_type") == "refresh_token" {
			mu.Lock()
			first := len(echoed) == 0
			if first {
				echoed = append(echoed, r.PostForm.Get("refresh_token"))
			}
			mu.Unlock()
			if first {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Rejected refresh: " + r.PostForm.Get("refresh_token")})
				return
			}
		}
		stub.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	clk := &clock{t: time.Now()}
	var logs bytes.Buffer
	acct, err := viva.NewAccount(driver.Config{
		CRM:      driver.Params{"username": stubUser, "auth_url": srv.URL + "/token", "api_url": srv.URL + "/api/v1"},
		Password: stubPass, HTTPClient: srv.Client(), Now: clk.now, Logger: core.NewLogger(&logs, slog.LevelInfo, []string{stubPass}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := acct.CheckLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	stub.FailNext(http.MethodPost, vivastub.LogoutPath, http.StatusServiceUnavailable, `{}`) // the echoed token stays live
	clk.add(5 * time.Minute)
	if err := acct.CheckLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a login whose refusal echoes a token too
	tok := "tok-echoed-in-a-login-refusal"
	stub.ExpireSessions()
	clk.add(5 * time.Minute)
	stub.FailNext(http.MethodPost, vivastub.TokenPath, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Session not active"}`)
	stub.FailNext(http.MethodPost, vivastub.TokenPath, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"bad `+tok+`"}`)
	err = acct.CheckLogin(context.Background())
	if len(echoed) != 1 || echoed[0] == "" {
		t.Fatal("no renewal was refused")
	}
	for _, secret := range []string{echoed[0], tok, "Rejected refresh", stubPass} {
		if strings.Contains(logs.String(), secret) || strings.Contains(err.Error(), secret) {
			t.Fatalf("%q passed on:\n%s\n%v", secret, logs.String(), err)
		}
	}
	if !strings.Contains(logs.String(), "invalid_grant: other") || !strings.Contains(err.Error(), "invalid_grant: other") {
		t.Fatalf("no label:\n%s\n%v", logs.String(), err)
	}
}

// The refresh token is kept until it really expires (not 30 seconds
// before), renewed early while the access token still serves, and a
// session whose refresh token did expire is ended before a login opens
// the next: whatever the lifetimes, at most one session is open.
func TestRefreshLifetimeBoundaries(t *testing.T) {
	for name, tc := range map[string]struct {
		access, refresh  int // the stub's lifetimes, seconds
		after            time.Duration
		logins, renewals int
	}{
		"short refresh, still valid":        {30, 40, 11 * time.Second, 1, 1},
		"refresh without an end":            {300, 0, 5 * time.Minute, 1, 1},
		"refresh expired":                   {30, 5, 10 * time.Second, 2, 0},
		"refresh ends before the access":    {600, 60, 35 * time.Second, 1, 1},
		"refresh ends, early renewal fails": {600, 60, 0, 1, 0},
	} {
		t.Run(name, func(t *testing.T) {
			stub := vivastub.New(stubUser, stubPass, studio)
			stub.TokenTTL, stub.RefreshTTL = tc.access, tc.refresh
			srv := httptest.NewServer(stub)
			t.Cleanup(srv.Close)
			clk := &clock{t: time.Now()}
			acct := accountOn(t, srv, stubPass, clk, nil)
			if err := acct.CheckLogin(context.Background()); err != nil {
				t.Fatal(err)
			}
			clk.add(tc.after)
			if name == "refresh ends, early renewal fails" {
				clk.add(35 * time.Second)
				stub.FailNext(http.MethodPost, vivastub.TokenPath, http.StatusServiceUnavailable, `{}`)
			}
			if err := acct.CheckLogin(context.Background()); err != nil {
				t.Fatal(err)
			}
			if stub.LoginAttempts() != tc.logins || stub.Refreshes() != tc.renewals || stub.OpenSessions() != 1 {
				t.Fatalf("%d logins, %d renewals, %d sessions open", stub.LoginAttempts(), stub.Refreshes(), stub.OpenSessions())
			}
		})
	}
}
