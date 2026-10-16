package viva_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

// EndSession ends the session of the account's login with an OpenID
// Connect logout (its refresh token, the web admin's client id): nothing
// stays open, the next call logs in again, and a second EndSession has
// nothing to end.
func TestEndSessionLogsOut(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	a := newAccount(t, stub, stubPass)
	ctx := context.Background()
	if err := a.CheckLogin(ctx); err != nil || stub.OpenSessions() != 1 {
		t.Fatalf("login: %v, %d sessions", err, stub.OpenSessions())
	}
	if err := driver.EndSession(ctx, a); err != nil || stub.OpenSessions() != 0 || stub.Logouts() != 1 {
		t.Fatalf("end: %v, %d sessions open, %d logouts", err, stub.OpenSessions(), stub.Logouts())
	}
	if err := driver.EndSession(ctx, a); err != nil || stub.Logouts() != 1 {
		t.Fatalf("nothing to end: %v, %d logouts", err, stub.Logouts())
	}
	if err := a.CheckLogin(ctx); err != nil || stub.Logins() != 2 || stub.OpenSessions() != 1 {
		t.Fatalf("after the end: %v, %d logins, %d sessions", err, stub.Logins(), stub.OpenSessions())
	}
}

// A session that cannot be ended (a login answer without a refresh token,
// a refused logout) is an error that holds no token.
func TestEndSessionFailuresHoldNoToken(t *testing.T) {
	ctx := context.Background()
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.OmitRefreshToken = true
	a := newAccount(t, stub, stubPass)
	if err := a.CheckLogin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := driver.EndSession(ctx, a); err == nil || !strings.Contains(err.Error(), "refresh_token") || stub.OpenSessions() != 1 {
		t.Fatalf("no refresh token: %v, %d sessions", err, stub.OpenSessions())
	}

	stub = vivastub.New(stubUser, stubPass, studio)
	a = newAccount(t, stub, stubPass)
	if err := a.CheckLogin(ctx); err != nil {
		t.Fatal(err)
	}
	stub.FailNext(http.MethodPost, vivastub.LogoutPath, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"tok-echo ref-echo"}`)
	err := driver.EndSession(ctx, a)
	if err == nil || strings.Contains(err.Error(), "tok-") || strings.Contains(err.Error(), "ref-") || strings.Contains(err.Error(), stubPass) {
		t.Fatalf("refused logout: %v", err)
	}
}
