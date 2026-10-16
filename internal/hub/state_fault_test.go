package hub_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// answerDropper is a loopback proxy to Postgres that loses the answer to
// one statement after the server has run it: it drops the
// CommandComplete message of a given tag ("COMMIT", "INSERT 0 1") and the
// connection with it, as a network failing at that moment would.
type answerDropper struct {
	ln     net.Listener
	target string

	mu      sync.Mutex
	tag     string
	dropped bool
}

// drop arms the proxy to lose the next answer of tag.
func (d *answerDropper) drop(tag string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tag, d.dropped = tag, false
}

func (d *answerDropper) didDrop() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

func (d *answerDropper) serve() {
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return
		}
		go d.relay(c)
	}
}

func (d *answerDropper) relay(c net.Conn) {
	defer c.Close()
	db, err := net.Dial("tcp", d.target)
	if err != nil {
		return
	}
	defer db.Close()
	go func() {
		_, _ = io.Copy(db, c)
		db.Close()
	}()
	// the server's messages: a type byte and a length that counts itself
	for {
		var head [5]byte
		if _, err := io.ReadFull(db, head[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint32(head[1:])) - 4
		if n < 0 || n > 64<<20 {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(db, body); err != nil {
			return
		}
		d.mu.Lock()
		lose := head[0] == 'C' && d.tag != "" && strings.TrimSuffix(string(body), "\x00") == d.tag
		if lose {
			d.tag, d.dropped = "", true
		}
		d.mu.Unlock()
		if lose {
			return
		}
		if _, err := c.Write(append(head[:], body...)); err != nil {
			return
		}
	}
}

// faultyDatabase makes a test database and returns the hub's settings
// reaching it through an answerDropper and a testkit.Proxy (Break, Heal),
// and its direct URL; it skips the test without testkit.TestDatabaseEnv.
func faultyDatabase(t *testing.T) (*hub.Settings, string, *answerDropper, *testkit.Proxy) {
	t.Helper()
	direct := testkit.TestDatabase(t)
	if direct == "" {
		t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
	}
	u, err := url.Parse(direct)
	if err != nil {
		t.Fatal(err)
	}
	p := testkit.NewProxy(t, u.Host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	d := &answerDropper{ln: ln, target: strings.TrimPrefix(p.URL(), "https://")}
	go d.serve()
	u.Host = ln.Addr().String()
	q := u.Query()
	q.Set("sslmode", "disable") // the proxy reads the protocol in the clear
	u.RawQuery = q.Encode()
	return postgresSettings(t, u.String()), direct, d, p
}

// dbQuery runs one query on the database at url and scans its one row.
func dbQuery(t *testing.T, url, q string, dest ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if err := conn.QueryRow(ctx, q).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// registered makes the installation id with a registered gateway and
// returns its secret.
func registered(t *testing.T, s *hub.Store, id string) string {
	t.Helper()
	if _, err := s.CreateInstallation(id, ""); err != nil {
		t.Fatal(err)
	}
	jc, err := s.CreateJoinCode(id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := s.Redeem(jc.Code)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// A change whose COMMIT got no answer may have been written: until the
// hub has read its state again it signs no config and is not ready; then
// it serves what the database holds, and the next change goes above it.
func TestLostCommitIsReadAgain(t *testing.T) {
	cfg, direct, d, p := faultyDatabase(t)
	s := openStore(t, cfg)
	secret := registered(t, s, "net-c")
	if _, _, err := s.PutClub("net-c", demoClub("club-c", "studio-c", "Корт 1")); err != nil {
		t.Fatal(err)
	}
	srv, err := hub.NewServer(&hub.Settings{PollWait: time.Second, RequestTimeout: time.Second, OfflineAfter: time.Minute}, s, hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	d.drop("COMMIT")
	if _, err := s.DeleteClub("net-c", "club-c"); !errors.Is(err, hub.ErrStateUnavailable) || !d.didDrop() {
		t.Fatalf("DeleteClub = %v (answer lost: %v), want ErrStateUnavailable", err, d.didDrop())
	}
	var version int64
	var clubs int
	dbQuery(t, direct, `SELECT version, (SELECT count(*) FROM court_gateway_hub.clubs) FROM court_gateway_hub.installations WHERE id = 'net-c'`, &version, &clubs)
	if version != 2 || clubs != 0 {
		t.Fatalf("the database holds version %d with %d clubs, want the change written", version, clubs)
	}

	p.Break()
	if rec := get(tunnel.PathConfig); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("config before the state is read again: %d %s, want 503", rec.Code, rec.Body)
	}
	if rec := get("/health/ready"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready before the state is read again: %d", rec.Code)
	}
	if _, _, err := s.PutClub("net-c", demoClub("club-d", "studio-d", "Корт 1")); !errors.Is(err, hub.ErrStateUnavailable) {
		t.Fatalf("a change before the state is read again: %v", err)
	}

	p.Heal()
	if rec := get("/health/ready"); rec.Code != http.StatusOK {
		t.Fatalf("ready once the database is back: %d", rec.Code)
	}
	if v := s.ConfigVersion("net-c"); v != 2 {
		t.Fatalf("ready with version %d in memory, want the database's 2", v)
	}
	rec := get(tunnel.PathConfig)
	var cfgOut tunnel.RemoteConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &cfgOut); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("config after the database is back: %d %s", rec.Code, rec.Body)
	}
	if cfgOut.Version != 2 || len(cfgOut.Clubs) != 0 {
		t.Fatalf("config version %d with %d clubs, want the database's: version 2, no clubs", cfgOut.Version, len(cfgOut.Clubs))
	}
	if _, v, err := s.PutClub("net-c", demoClub("club-d", "studio-d", "Корт 1")); err != nil || v != 3 {
		t.Fatalf("the next change: version %d %v, want 3", v, err)
	}
	dbQuery(t, direct, `SELECT version FROM court_gateway_hub.installations WHERE id = 'net-c'`, &version)
	if version != 3 {
		t.Fatalf("the database holds version %d, want 3", version)
	}
}

// A registration whose answer got lost may have replaced the gateway's
// secret: once the database holds another one, the previous secret is
// refused.
func TestLostRegistrationAnswerRetiresTheOldSecret(t *testing.T) {
	cfg, direct, d, _ := faultyDatabase(t)
	s := openStore(t, cfg)
	old := registered(t, s, "net-r")
	jc, err := s.CreateJoinCode("net-r", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	d.drop("INSERT 0 1")
	if _, _, err := s.Redeem(jc.Code); !errors.Is(err, hub.ErrStateUnavailable) || !d.didDrop() {
		t.Fatalf("Redeem = %v (answer lost: %v), want ErrStateUnavailable", err, d.didDrop())
	}
	var hash string
	dbQuery(t, direct, `SELECT secret_sha256 FROM court_gateway_hub.gateways WHERE installation = 'net-r'`, &hash)
	if hash == tunnel.SecretSHA256(old) {
		t.Fatal("the registration was not written")
	}
	if _, err := s.Authenticate(old); !errors.Is(err, hub.ErrNotRegistered) {
		t.Fatalf("the replaced secret: %v, want ErrNotRegistered", err)
	}
	if err := s.Ready(); err != nil {
		t.Fatal(err)
	}
}

// A gateway secret replaced behind the hub — by a second hub on the same
// database, or in the database itself — is refused by the first hub,
// which then knows the registration the database holds.
func TestSecretReplacedBehindTheHub(t *testing.T) {
	url := testkit.TestDatabase(t)
	if url == "" {
		t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
	}
	cfg := postgresSettings(t, url)
	cli := openState(t, cfg)
	a := openStore(t, cfg)
	jc, err := hub.CreateJoinCode(cli, "net-p", "", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, first, err := a.Redeem(jc.Code)
	if err != nil {
		t.Fatal(err)
	}
	b := openStore(t, cfg)
	jc, err = hub.CreateJoinCode(cli, "net-p", "", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := b.Redeem(jc.Code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(first); !errors.Is(err, hub.ErrNotRegistered) {
		t.Fatalf("the first hub accepts the replaced secret: %v", err)
	}
	if g, err := a.Authenticate(second); err != nil || g.Installation != "net-p" {
		t.Fatalf("the first hub and the secret the database holds: %+v %v", g, err)
	}

	dbQuery(t, url, `UPDATE court_gateway_hub.gateways SET secret_sha256 = repeat('ab', 32) WHERE installation = 'net-p' RETURNING installation`, new(string))
	if _, err := a.Authenticate(second); !errors.Is(err, hub.ErrNotRegistered) {
		t.Fatalf("a secret the database no longer holds: %v", err)
	}
}

// A hub that has not seen a change made behind it writes nothing over
// it: its own change is refused, it reads the state again, and the next
// one goes above. Versions are never reused nor go down, and no
// acknowledged club is lost — also when a code redeemed on that hub names
// an installation made after it started.
func TestStaleHubWritesNothingOver(t *testing.T) {
	url := testkit.TestDatabase(t)
	if url == "" {
		t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
	}
	cfg := postgresSettings(t, url)
	a := openStore(t, cfg)
	if _, err := a.CreateInstallation("net-w", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.PutClub("net-w", demoClub("club-a", "studio-a", "Корт 1")); err != nil {
		t.Fatal(err)
	}
	b := openStore(t, cfg)
	stale := openStore(t, cfg) // started before net-z exists
	if _, v, err := b.PutClub("net-w", demoClub("club-b", "studio-b", "Корт 1")); err != nil || v != 2 {
		t.Fatalf("B: version %d %v", v, err)
	}
	if _, _, err := a.PutClub("net-w", demoClub("club-c", "studio-c", "Корт 1")); !errors.Is(err, hub.ErrConflict) {
		t.Fatalf("A over a version it has not seen: %v, want ErrConflict", err)
	}
	var version int64
	dbQuery(t, url, `SELECT version FROM court_gateway_hub.installations WHERE id = 'net-w'`, &version)
	if version != 2 {
		t.Fatalf("the database holds version %d, want 2", version)
	}
	if _, v, err := a.PutClub("net-w", demoClub("club-c", "studio-c", "Корт 1")); err != nil || v != 3 {
		t.Fatalf("A once it has read the state again: version %d %v, want 3", v, err)
	}
	if in, _ := openStore(t, cfg).Installation("net-w"); in.Version != 3 || len(in.Clubs) != 3 {
		t.Fatalf("after a restart: version %d, clubs %v, want 3 and all three", in.Version, in.ClubList())
	}

	if _, err := a.CreateInstallation("net-z", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.PutClub("net-z", demoClub("club-z", "studio-z", "Корт 1")); err != nil {
		t.Fatal(err)
	}
	jc, err := hub.CreateJoinCode(openState(t, cfg), "net-z", "", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if g, _, err := stale.Redeem(jc.Code); err != nil || g.Installation != "net-z" {
		t.Fatalf("a code of an installation made behind the hub: %+v %v", g, err)
	}
	if in, _ := openStore(t, cfg).Installation("net-z"); in.Version != 1 || len(in.Clubs) != 1 {
		t.Fatalf("net-z after the redemption: version %d, clubs %v, want 1 and club-z", in.Version, in.ClubList())
	}
	if _, err := stale.CreateInstallation("net-z", ""); !errors.Is(err, hub.ErrInstallationExists) {
		t.Fatalf("creating an installation made behind the hub: %v", err)
	}
}

// The database keeps a club in its installation: a hub that does not
// know the club is served elsewhere cannot move it.
func TestDatabaseKeepsAClubInItsInstallation(t *testing.T) {
	url := testkit.TestDatabase(t)
	if url == "" {
		t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
	}
	cfg := postgresSettings(t, url)
	a := openStore(t, cfg)
	if _, err := a.CreateInstallation("net-a", ""); err != nil {
		t.Fatal(err)
	}
	b := openStore(t, cfg)
	if _, err := b.CreateInstallation("net-b", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.PutClub("net-a", demoClub("club-x", "studio-a", "Корт 1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.PutClub("net-b", demoClub("club-x", "studio-b", "Корт 1")); !errors.Is(err, hub.ErrClubTaken) {
		t.Fatalf("a club of another installation: %v, want ErrClubTaken", err)
	}
	var inst string
	dbQuery(t, url, `SELECT installation FROM court_gateway_hub.clubs WHERE id = 'club-x'`, &inst)
	if inst != "net-a" {
		t.Fatalf("club-x is in %s", inst)
	}
	if owner, ok := b.Route("club-x"); !ok || owner != "net-a" {
		t.Fatalf("the refused hub then routes club-x to %q %v", owner, ok)
	}
}

// The hub reads its state in one snapshot: started while another hub
// writes (here, as fast as the database takes them, transactions that
// replace an installation with a new one and its club), it neither fails
// nor sees a club of an installation it has not read.
func TestStartWhileAnotherHubWrites(t *testing.T) {
	url := testkit.TestDatabase(t)
	if url == "" {
		t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
	}
	cfg := postgresSettings(t, url)
	openStore(t, cfg).Close() // the tables
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := fmt.Sprintf("net-%d", i)
			spec, _ := json.Marshal(demoClub("club-"+id, "studio-"+id, "Корт 1"))
			err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `DELETE FROM court_gateway_hub.installations`); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO court_gateway_hub.installations (id, name, created, version) VALUES ($1, $1, now(), 1)`, id); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO court_gateway_hub.clubs (id, installation, spec) VALUES ($1, $2, $3)`, "club-"+id, id, string(spec))
				return err
			})
			if err != nil {
				t.Errorf("the writer: %v", err)
				return
			}
		}
	})
	defer wg.Wait()
	defer close(stop)
	for range 200 {
		s, err := hub.Open(cfg, nil)
		if err != nil {
			t.Fatalf("a start while another hub writes: %v", err)
		}
		s.Close()
	}
}

// A redemption held up by a slow database does not hold up the gateways
// already registered.
func TestSlowRedemptionDoesNotHoldUpGateways(t *testing.T) {
	url := testkit.TestDatabase(t)
	if url == "" {
		t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
	}
	cfg := postgresSettings(t, url)
	s := openStore(t, cfg)
	secret := registered(t, s, "net-s")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE court_gateway_hub.join_codes IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var waiting atomic.Bool
	done := make(chan error, 1)
	go func() {
		waiting.Store(true)
		_, _, err := s.Redeem("AAAA-BBBB-CCCC-DDDD")
		done <- err
	}()
	for !waiting.Load() {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // the redemption waits for the lock
	start := time.Now()
	if _, err := s.Authenticate(secret); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("a gateway waited %v behind a redemption", took)
	}
	select {
	case err := <-done:
		t.Fatalf("the redemption did not wait for the database: %v", err)
	default:
	}
	_ = tx.Rollback(ctx)
	if err := <-done; !errors.Is(err, hub.ErrJoinCode) {
		t.Fatalf("the redemption: %v", err)
	}
}

// A database whose tables a newer hub made is refused, by the hub and the
// command line alike, rather than misread.
func TestNewerSchemaIsRefused(t *testing.T) {
	url := testkit.TestDatabase(t)
	if url == "" {
		t.Skip("set " + testkit.TestDatabaseEnv + " to run on Postgres")
	}
	cfg := postgresSettings(t, url)
	openStore(t, cfg).Close()
	var version int
	dbQuery(t, url, `SELECT version FROM court_gateway_hub.schema_version`, &version)
	if version != 1 {
		t.Fatalf("schema version %d", version)
	}
	dbQuery(t, url, `UPDATE court_gateway_hub.schema_version SET version = 2 RETURNING version`, &version)
	if _, err := hub.Open(cfg, nil); err == nil || !strings.Contains(err.Error(), "обновите хаб") {
		t.Fatalf("the hub on a newer schema: %v", err)
	}
	if _, err := hub.OpenState(cfg); err == nil || !strings.Contains(err.Error(), "обновите хаб") {
		t.Fatalf("the command line on a newer schema: %v", err)
	}
}
