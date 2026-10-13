package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

type loginAccount struct{ ok bool }

func (a loginAccount) CheckLogin(context.Context) error {
	if !a.ok {
		return driver.Upstream(driver.UpstreamAuth, "rejected")
	}
	return nil
}

func (loginAccount) Discover(context.Context, driver.DiscoverQuery) (driver.Discovery, error) {
	return driver.Discovery{}, nil
}

// A changed CRM password goes into .env without a join code: checked with
// a CRM login first, only its line changes, quoted as setup quotes it, and
// the folder and file are closed before and after the write.
func TestSetPasswordReplacesOnlyThePassword(t *testing.T) {
	dir := t.TempDir()
	cfg, envPath := filepath.Join(dir, "gateway.yaml"), filepath.Join(dir, core.EnvFileName)
	d := &draft{HubURL: "https://hub.example.test", CRMType: "viva", Username: "admin@example.test"}
	if err := os.WriteFile(cfg, []byte(d.render("inst-1", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	before := "# keep me\nCRM_PASSWORD=old-pass\n" + core.DefaultHubSecretEnv + "=cgs_0123456789abcdef0123456789abcdef\nHTTPS_PROXY=http://proxy.example.test:3128\n"
	if err := os.WriteFile(envPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	const newPassword = " new pa$s 'q' "
	var protected []string
	deps := Deps{
		Accounts: map[string]driver.AccountFactory{"viva": func(c driver.Config) (driver.Account, error) {
			return loginAccount{ok: c.Password != "wrong" && c.CRM["username"] == "admin@example.test"}, nil
		}},
		ProtectSecrets: func(p string) error { protected = append(protected, filepath.Base(p)); return nil },
	}
	getenv := func(string) string { return "" }

	err := SetPassword(context.Background(), cfg, "wrong", getenv, deps)
	var p *Problem
	if !errors.As(err, &p) || p.Code != "CRM_AUTH_FAILED" {
		t.Fatalf("wrong password: %v", err)
	}
	if got, _ := os.ReadFile(envPath); string(got) != before || len(protected) != 0 {
		t.Fatalf("a rejected password changed .env or its rights: %q %v", got, protected)
	}

	if err := SetPassword(context.Background(), cfg, newPassword, getenv, deps); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(envPath)
	vals, err := core.ParseDotEnv(got)
	if err != nil || vals[core.PasswordEnv] != newPassword {
		t.Fatalf("password read back %q (%v)", vals[core.PasswordEnv], err)
	}
	if want := strings.Replace(before, "CRM_PASSWORD=old-pass", "CRM_PASSWORD="+envValue(newPassword), 1); string(got) != want {
		t.Fatalf(".env\n%s\nwant\n%s", got, want)
	}
	if fi, _ := os.Stat(envPath); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode %v", fi.Mode().Perm())
	}
	if strings.Join(protected, " ") != filepath.Base(dir)+" .env "+filepath.Base(dir)+" .env" {
		t.Fatalf("protected %v", protected)
	}

	deps.ProtectSecrets = func(string) error { return errors.New("synthetic ACL failure") }
	if err := SetPassword(context.Background(), cfg, "another-pass", getenv, deps); !errors.As(err, &p) || p.Code != "FOLDER_NOT_RESTRICTED" {
		t.Fatalf("unprotectable folder: %v", err)
	}
	if again, _ := os.ReadFile(envPath); string(again) != string(got) {
		t.Fatal(".env written into a folder that could not be closed")
	}
}
