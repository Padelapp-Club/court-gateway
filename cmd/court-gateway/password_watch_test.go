package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

type passwordAccount struct {
	driver.Account
	set []string
}

func (a *passwordAccount) SetPassword(p string) { a.set = append(a.set, p) }

func (*passwordAccount) CheckLogin(context.Context) error { return nil }

// The running service re-reads the CRM password when set-password rewrites
// .env, and only then: that is what ends its stop after a rejected login.
func TestServiceTakesTheNewPasswordFromEnvFile(t *testing.T) {
	dir := t.TempDir()
	cfg, env := filepath.Join(dir, "gateway.yaml"), filepath.Join(dir, ".env")
	write := func(content string, at time.Time) {
		if err := os.WriteFile(env, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(env, at, at); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().Add(-time.Hour)
	write("CRM_PASSWORD='old-password'\n", t0)
	acct := &passwordAccount{}
	w := newPasswordWatch(cfg, func(string) string { return "" }, acct, slog.New(slog.DiscardHandler))
	w.check()
	if len(acct.set) != 0 {
		t.Fatalf("an unchanged .env set the password: %v", acct.set)
	}
	write("CRM_PASSWORD='new-password'\n", t0.Add(time.Minute))
	w.check()
	w.check()
	if len(acct.set) != 1 || acct.set[0] != "new-password" {
		t.Fatalf("passwords set: %v", acct.set)
	}
}
