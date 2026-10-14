package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/update"
)

type passwordAccount struct {
	driver.Account
	set []string
}

func (a *passwordAccount) SetPassword(p string) { a.set = append(a.set, p) }

func (*passwordAccount) PasswordRejected() bool { return false }

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

// While the CRM rejects the password nothing logs in again, an update
// included: an update the release server announces is not even fetched,
// so its self-check (which logs in with the same password) never runs.
func TestUpdatesHeldWhileThePasswordIsRejected(t *testing.T) {
	stub := vivastub.New(crmUser, crmPass, studio)
	crm := httptest.NewServer(stub)
	defer crm.Close()
	login := func() driver.Account {
		acct, err := viva.NewAccount(driver.Config{CRM: driver.Params{"username": crmUser, "auth_url": crm.URL + "/token", "api_url": crm.URL + "/api/v1"},
			Password: "wrong-password-0000", HTTPClient: crm.Client()})
		if err != nil {
			t.Fatal(err)
		}
		_ = acct.CheckLogin(context.Background())
		return acct
	}
	acct := login() // the service's account: rejected once, and stopped
	// a newer release, signed, as the release server would offer it
	keyFile, pubB64, _ := update.GenerateKey()
	priv, _ := update.ParsePrivateKey(keyFile)
	pub, _ := update.ParsePublicKey(pubB64)
	bin := []byte("NEW BINARY 9.9.9")
	sum := sha256.Sum256(bin)
	var manifest []byte
	rel := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			_, _ = w.Write(manifest)
		case "/manifest.json.sig":
			_, _ = w.Write(update.Sign(priv, manifest))
		default:
			_, _ = w.Write(bin)
		}
	}))
	defer rel.Close()
	manifest, _ = json.Marshal(update.Manifest{Version: "9.9.9", Assets: map[string]update.Asset{"test-plat": {URL: rel.URL + "/bin", SHA256: hex.EncodeToString(sum[:])}}})
	u := &update.Updater{PublicKey: pub, ManifestURL: rel.URL + "/manifest.json", Current: "1.0.0", Platform: "test-plat", Client: rel.Client(),
		Versions: &update.Versions{Dir: t.TempDir()}, Hold: passwordRejected(acct),
		CheckBinary: func(context.Context, string, string) error { login(); return update.ErrNotRun }} // what the new binary's selfcheck does
	if _, err := u.Check(context.Background()); !errors.Is(err, update.ErrHeld) || stub.LoginAttempts() != 1 {
		t.Fatalf("an update announced while the password is rejected: %v, %d logins", err, stub.LoginAttempts())
	}
}
