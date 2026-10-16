package testkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestDatabaseEnv names the URL of a Postgres server for tests, whose user
// may create databases. With it set, every test hub keeps its state in a
// database of its own, made from it and dropped after the test; without
// it, in a state_dir.
const TestDatabaseEnv = "COURT_GATEWAY_TEST_DATABASE_URL"

// TestDatabase makes an empty database on the TestDatabaseEnv server for
// t and returns its URL; "" when TestDatabaseEnv is not set.
func TestDatabase(t testing.TB) string {
	t.Helper()
	base := os.Getenv(TestDatabaseEnv)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		t.Fatalf("%s: a postgres:// URL is expected", TestDatabaseEnv)
	}
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	name := "cgtest_" + hex.EncodeToString(raw[:])
	exec := func(q string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			return err
		}
		defer conn.Close(context.Background())
		_, err = conn.Exec(ctx, q)
		return err
	}
	if err := exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("test database: %v", err)
	}
	t.Cleanup(func() {
		if err := exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	u.Path = "/" + name
	return u.String()
}

// DatabaseRows returns every row the hub keeps in its database, as text.
func (h *Hub) DatabaseRows(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, h.Settings.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var out []string
	for _, table := range []string{"installations", "clubs", "gateways", "join_codes"} {
		rows, _ := conn.Query(ctx, "SELECT t::text FROM court_gateway_hub."+table+" t")
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got...)
	}
	return out
}
