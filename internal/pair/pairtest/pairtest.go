// Package pairtest serves a club kept in two CRM stubs — a primary CRM
// and Lunda, the mirror — the way a gateway serves it (core config with a
// mirror section, the club from the hub, core.BuildDriver, the partner
// API), so a scenario can be run over every pair the same way: each CRM
// is driven and read through CRM, the gateway through Env.
package pairtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/pair"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// Kind is what an entry of a CRM is, as the stub stores it.
type Kind int

const (
	// Club: made by the club's staff.
	Club Kind = iota
	// Partner: a partner's booking made through the gateway ([cg:<partner>]).
	Partner
	// Mirror: a block the gateway's loop made.
	Mirror
)

func (k Kind) String() string { return [...]string{"club", "partner", "mirror"}[k] }

// Entry is one live booking or block of a CRM stub on a court of the club.
type Entry struct {
	Handle     string
	Court      string // the gateway's court id
	Start, End time.Time
	Kind       Kind
	Note       string // its comment or notes as stored
}

// CRM is one CRM stub of the pair as a scenario drives it: what the
// club's staff do in the CRM directly, and what is in it now, read from
// the stub itself (not through the driver under test).
type CRM interface {
	Type() string
	// Book books court [start, end) as the club's staff would.
	Book(t testing.TB, court string, start, end time.Time) string
	// Cancel cancels a booking of the club.
	Cancel(t testing.TB, handle string)
	// Move moves a booking of the club, keeping its id.
	Move(t testing.TB, handle string, start, end time.Time)
	// Annotate sets a booking's comment, as the club's staff may.
	Annotate(t testing.TB, handle, note string)
	// Live lists every live entry on the club's courts.
	Live(t testing.TB) []Entry
	// Fail makes the next write of kind (create or delete) fail upstream.
	Fail(t testing.TB, write string)
}

// Env is a club served by a pair.
type Env struct {
	Primary, Mirror CRM
	Pair            *pair.Pair
	Built           *contract.Built
	Handler         http.Handler
	Courts          []string
	Location        *time.Location
	// Partner is the name of the partner of contract.BookKey.
	Partner string
	// LundaStub is the mirror's stub, for failures a scenario injects.
	LundaStub *lundastub.Stub

	rebuild func(t *testing.T, m core.MirrorSpec) *pair.Pair
}

// Reconfigure serves the club again with mirror section m, as a new config
// from the hub would (the operator raising the fuse), on the same CRMs.
func (e *Env) Reconfigure(t *testing.T, m core.MirrorSpec) {
	t.Helper()
	e.Pair = e.rebuild(t, m)
}

// Options adjust a setup.
type Options struct {
	// Mirror is the club's mirror section (nil: mirror: {}).
	Mirror *core.MirrorSpec
}

// Setup builds an Env.
type Setup func(t *testing.T, o Options) *Env

// Pass runs one pass of the loop.
func (e *Env) Pass(t testing.TB) pair.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.Pair.Reconcile(ctx)
}

// Day is a day ahead (in the club's zone) at hh:mm.
func (e *Env) Day(days, hh, mm int) time.Time {
	n := time.Now().In(e.Location)
	return time.Date(n.Year(), n.Month(), n.Day()+days, hh, mm, 0, 0, e.Location)
}

type answer struct {
	Status int
	Body   map[string]any
	Raw    string
}

func (e *Env) call(t testing.TB, method, path string, body any) answer {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Header.Set("Authorization", "Bearer "+contract.BookKey)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.Handler.ServeHTTP(w, r)
	a := answer{Status: w.Code, Raw: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &a.Body)
	return a
}

// Create books through the partner API: its status and the booking's id.
func (e *Env) Create(t testing.TB, court string, start, end time.Time) (int, string) {
	t.Helper()
	a := e.call(t, http.MethodPost, "/v1/clubs/test-club/bookings", map[string]string{
		"court": court, "start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339), "kind": "game", "note": "Игра PRIVACY-MARKER-NOTE-7f3a"})
	id, _ := a.Body["id"].(string)
	if a.Status != http.StatusCreated {
		t.Logf("create %s %s: %d %s", court, start.Format(time.RFC3339), a.Status, a.Raw)
	}
	return a.Status, id
}

// CancelBooking cancels through the partner API and returns the status.
func (e *Env) CancelBooking(t testing.TB, id string) int {
	t.Helper()
	a := e.call(t, http.MethodDelete, "/v1/clubs/test-club/bookings/"+id, nil)
	if a.Status != http.StatusNoContent {
		t.Logf("cancel %s: %d %s", id, a.Status, a.Raw)
	}
	return a.Status
}

// Busy reports whether the partner API shows court occupied in any part
// of [start, end) (and that the answer is complete).
func (e *Env) Busy(t testing.TB, court string, start, end time.Time) bool {
	t.Helper()
	q := url.Values{"from": {start.Format(time.RFC3339)}, "to": {end.Format(time.RFC3339)}, "court": {court}}
	a := e.call(t, http.MethodGet, "/v1/clubs/test-club/occupancy?"+q.Encode(), nil)
	if a.Status != http.StatusOK {
		t.Fatalf("occupancy: %d %s", a.Status, a.Raw)
	}
	if c, _ := a.Body["complete"].(bool); !c {
		t.Fatalf("occupancy is incomplete: %s", a.Raw)
	}
	ivs, _ := a.Body["intervals"].([]any)
	return len(ivs) > 0
}

// Of filters entries by kind and court ("" any court).
func Of(entries []Entry, kind Kind, court string) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Kind == kind && (court == "" || e.Court == court) {
			out = append(out, e)
		}
	}
	return out
}

// Spans writes entries for a failure message.
func Spans(entries []Entry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "[%s %s %s–%s %q] ", e.Kind, e.Court, e.Start.Format("01-02 15:04"), e.End.Format("15:04"), e.Note)
	}
	return b.String()
}

// Has reports whether entries hold one of kind on court over exactly [start, end).
func Has(entries []Entry, kind Kind, court string, start, end time.Time) bool {
	for _, e := range Of(entries, kind, court) {
		if e.Start.Equal(start) && e.End.Equal(end) {
			return true
		}
	}
	return false
}

// memStore is a driver.SessionStore in memory.
type memStore struct {
	mu   sync.Mutex
	data []byte
}

func (m *memStore) LoadSession() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.data...), nil
}

func (m *memStore) SaveSession(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = append([]byte(nil), b...)
	return nil
}

// build serves spec as a gateway would: the installation's crm and mirror
// sections (YAML), the mirror's Lunda account logged in by a QR code on
// lstub, the club from the hub, the pair from core.BuildDriver.
func build(t *testing.T, crmYAML, mirrorYAML string, spec core.ClubSpec, env map[string]string, primaryClient *http.Client, lstub *lundastub.Stub, lsrv *httptest.Server) (*contract.Built, *pair.Pair, func(*testing.T, core.MirrorSpec) *pair.Pair) {
	t.Helper()
	getenv := func(k string) string {
		if k == core.DefaultHubSecretEnv {
			return "test-hub-secret"
		}
		return env[k]
	}
	base, err := core.Load([]byte("hub: { url: https://hub.example.test, installation: test-net, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= }\n"+crmYAML+mirrorYAML), getenv)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	logs := &contract.LogBuffer{}
	log := core.NewLogger(logs, slog.LevelDebug, base.Secrets)
	base.Driver.Logger = log
	base.Driver.HTTPClient = primaryClient
	base.Mirror.Driver.HTTPClient = lsrv.Client()
	base.Mirror.Driver.Sessions = &memStore{}
	base.Mirror.Driver.Logger = log
	acct, err := core.BuildAccount(&core.Settings{CRMType: base.Mirror.CRMType}, drivers.Accounts, base.Mirror.Driver)
	if err != nil {
		t.Fatalf("mirror account: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = driver.EndSession(ctx, acct)
	})
	loginQR(t, acct, lstub)
	base.Mirror.Driver.Account = acct
	s, err := base.WithClub(spec, drivers.ClubCRMKeys(base.CRMType))
	if err != nil {
		t.Fatalf("club: %v", err)
	}
	d, err := core.BuildDriver(s, drivers.Factories)
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	p, ok := d.(*pair.Pair)
	if !ok {
		t.Fatalf("the club with a mirror section is served by %T, not a pair", d)
	}
	srv := core.NewServer(s, d, log)
	clubs := core.NewClubs(core.ClubsOptions{Logger: log})
	clubs.Set("", []*core.Server{srv})
	b := &contract.Built{Settings: s, Driver: d, Server: srv, Clubs: clubs, Logs: logs}
	rebuild := func(t *testing.T, m core.MirrorSpec) *pair.Pair {
		t.Helper()
		spec.Mirror = &m
		s, err := base.WithClub(spec, drivers.ClubCRMKeys(base.CRMType))
		if err != nil {
			t.Fatalf("club: %v", err)
		}
		d, err := core.BuildDriver(s, drivers.Factories)
		if err != nil {
			t.Fatalf("driver: %v", err)
		}
		srv := core.NewServer(s, d, log)
		clubs.Set("", []*core.Server{srv})
		b.Settings, b.Driver, b.Server = s, d, srv
		return d.(*pair.Pair)
	}
	return b, p, rebuild
}

// loginQR logs the Lunda account in as a staff member would by a QR code.
func loginQR(t *testing.T, acct driver.Account, stub *lundastub.Stub) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	il := acct.(driver.InteractiveLogin)
	if _, err := il.StartLogin(ctx, driver.LoginQR, ""); err != nil {
		t.Fatalf("Lunda login: %v", err)
	}
	stub.ConfirmQR()
	st, err := il.PollLogin(ctx)
	if err != nil || !st.Done {
		t.Fatalf("Lunda login: %v %+v", err, st)
	}
	if err := il.SelectClub(ctx, stub.Club.UID); err != nil {
		t.Fatalf("Lunda club: %v", err)
	}
	testkit.WaitFor(t, "the Lunda keep-alive's WebSocket", 5*time.Second, func() bool {
		open, _, _ := stub.WebSockets()
		return open > 0
	})
}
