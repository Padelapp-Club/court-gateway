package lunda_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
)

// writePaths are the operations that change the club's data.
var writePaths = []string{"/club/booking/booking-create", "/club/booking/update-courts-price-adjustment",
	"/club/booking/booking-notes-update", "/club/booking/booking-cancel"}

func (f *acctFixture) writes() int {
	n := 0
	for _, p := range writePaths {
		n += f.stub.CallsTo(p)
	}
	return n
}

func occQuery(day time.Time) driver.OccupancyQuery {
	return driver.OccupancyQuery{Courts: []string{"court-1"}, From: day.Add(9 * time.Hour), To: day.Add(12 * time.Hour)}
}

// futureDay is local midnight three days ahead in Moscow.
func futureDay() time.Time {
	msk, _ := time.LoadLocation("Europe/Moscow")
	n := time.Now().In(msk).AddDate(0, 0, 3)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, msk)
}

// When an answer carries refresh-required, the session is renewed
// (auth/refresh with the current token), the new token is saved and used,
// and the old one, which Lunda stopped taking, is not sent again.
func TestRenewalOnRefreshRequired(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	f.loginQR(t, a)
	d := f.driver(t, a)
	ctx := context.Background()
	old := f.store.token()
	saves := f.store.saves
	f.stub.RequireRefresh()
	if _, err := d.Occupancy(ctx, occQuery(futureDay())); err != nil {
		t.Fatalf("occupancy with refresh-required: %v", err)
	}
	if f.stub.Renewals() != 1 {
		t.Fatalf("renewals = %d, want 1", f.stub.Renewals())
	}
	renewed := f.store.token()
	if renewed == old || renewed == "" || f.store.saves != saves+1 || !contains(f.stub.Sessions(), renewed) {
		t.Fatalf("the renewed token was not saved")
	}
	refresh := f.stub.Calls()[findCall(f, "/auth/refresh")]
	if refresh.Token != old || refresh.Params["publicKey"] == "" {
		t.Fatalf("auth/refresh: %+v", refresh)
	}
	n := len(f.stub.Calls())
	if _, err := d.Occupancy(ctx, occQuery(futureDay())); err != nil {
		t.Fatalf("occupancy after the renewal: %v", err)
	}
	for _, c := range f.stub.Calls()[n:] {
		if c.Token != renewed {
			t.Fatalf("%s went with another token after the renewal", c.Path)
		}
	}
	// a restart takes the renewed session
	if err := f.account(t).CheckLogin(ctx); err != nil {
		t.Fatalf("restart after the renewal: %v", err)
	}
}

// Without refresh-required no renewal is made, however many calls.
func TestNoRenewalUnasked(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	f.loginQR(t, a)
	d := f.driver(t, a)
	for range 5 {
		if _, err := d.Occupancy(context.Background(), occQuery(futureDay())); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.stub.CallsTo("/auth/refresh"); n != 0 {
		t.Fatalf("%d renewals unasked", n)
	}
}

// A renewal that fails for another reason than a refusal is tried again
// with the next call (not on a timer), and the session goes on meanwhile.
func TestFailedRenewalIsTriedAgain(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	f.loginQR(t, a)
	d := f.driver(t, a)
	ctx := context.Background()
	f.stub.RequireRefresh()
	f.stub.FailNext(http.MethodPost, "/auth/refresh", http.StatusBadGateway, `{}`)
	if _, err := d.Occupancy(ctx, occQuery(futureDay())); err != nil {
		t.Fatalf("the call itself must pass: %v", err)
	}
	if f.stub.Renewals() != 0 || f.stub.CallsTo("/auth/refresh") != 1 {
		t.Fatalf("renewals %d", f.stub.Renewals())
	}
	if _, err := d.Occupancy(ctx, occQuery(futureDay())); err != nil {
		t.Fatal(err)
	}
	if f.stub.Renewals() != 1 {
		t.Fatalf("the renewal was not tried again")
	}
}

// Lunda ends the session: the call is an UpstreamAuth naming
// ErrNoSession, and from then on nothing at all — no create, no cancel —
// reaches Lunda until a new session is saved; then the gateway takes it
// up without a restart.
func TestLostSessionStopsEveryWrite(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	f.loginQR(t, a)
	d := f.driver(t, a)
	ctx := context.Background()
	day := futureDay()
	own, err := d.Create(ctx, driver.CreateRequest{Court: "court-1", Start: day.Add(18 * time.Hour), End: day.Add(19 * time.Hour), Kind: driver.KindGame, Partner: "booker"})
	if err != nil {
		t.Fatal(err)
	}
	writes := f.writes()
	f.stub.Revoke()
	_, err = d.Occupancy(ctx, occQuery(day))
	if !errors.Is(err, driver.ErrNoSession) || upstreamKind(err) != driver.UpstreamAuth {
		t.Fatalf("occupancy after the revoke: %v", err)
	}
	calls := len(f.stub.Calls())
	if _, err := d.Create(ctx, driver.CreateRequest{Court: "court-1", Start: day.Add(10 * time.Hour), End: day.Add(11 * time.Hour), Kind: driver.KindGame, Partner: "booker"}); !errors.Is(err, driver.ErrNoSession) {
		t.Fatalf("create after the loss: %v", err)
	}
	if err := d.Cancel(ctx, own.ID, "booker"); !errors.Is(err, driver.ErrNoSession) {
		t.Fatalf("cancel after the loss: %v", err)
	}
	if err := d.Ready(ctx); !errors.Is(err, driver.ErrNoSession) {
		t.Fatalf("ready after the loss: %v", err)
	}
	if f.writes() != writes || len(f.stub.Calls()) != calls {
		t.Fatalf("calls reached Lunda after the loss: %+v", f.stub.Calls()[calls:])
	}
	// a person logs the gateway in again, in another process
	f.loginQR(t, f.account(t))
	if err := d.Ready(ctx); err != nil {
		t.Fatalf("ready with the new session: %v", err)
	}
	if err := d.Cancel(ctx, own.ID, "booker"); err != nil {
		t.Fatalf("cancel with the new session: %v", err)
	}
}

// The service started before anyone logged it in: each call looks at the
// store again, and the session a login saves there is taken up.
func TestSessionSavedLaterIsTakenUp(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	d := f.driver(t, a)
	ctx := context.Background()
	if err := d.Ready(ctx); !errors.Is(err, driver.ErrNoSession) || upstreamKind(err) != driver.UpstreamAuth {
		t.Fatalf("ready without a session: %v", err)
	}
	if len(f.stub.Calls()) != 0 {
		t.Fatalf("calls without a session: %+v", f.stub.Calls())
	}
	f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName))
	if err := d.Ready(ctx); err != nil {
		t.Fatalf("ready after the login: %v", err)
	}
}

// A call refused while the store already holds another session (saved by
// a login meanwhile) is sent once more with it, and passes.
func TestRefusedCallRetriesWithTheStoredSession(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	f.loginQR(t, a)
	d := f.driver(t, a)
	f.stub.Revoke()
	f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName))
	if _, err := d.Occupancy(context.Background(), occQuery(futureDay())); err != nil {
		t.Fatalf("occupancy: %v", err)
	}
}

// A renewal Lunda refuses loses the session as a refused call does.
func TestRefusedRenewalLosesTheSession(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	f.loginQR(t, a)
	d := f.driver(t, a)
	ctx := context.Background()
	f.stub.RequireRefresh()
	f.stub.FailNext(http.MethodPost, "/auth/refresh", http.StatusUnauthorized, `{}`)
	if _, err := d.Occupancy(ctx, occQuery(futureDay())); err != nil {
		t.Fatalf("the call itself passed: %v", err)
	}
	writes := f.writes()
	if _, err := d.Create(ctx, driver.CreateRequest{Court: "court-1", Start: futureDay().Add(10 * time.Hour), End: futureDay().Add(11 * time.Hour), Kind: driver.KindGame, Partner: "booker"}); !errors.Is(err, driver.ErrNoSession) {
		t.Fatalf("create after a refused renewal: %v", err)
	}
	if f.writes() != writes {
		t.Fatal("a write after a refused renewal")
	}
}

// A 403 of one operation while the session still works is a refusal of
// that operation (a staff role without the right), not a lost session.
func TestForbiddenOperationKeepsTheSession(t *testing.T) {
	f := newAcctFixture(t)
	a := f.account(t)
	f.loginQR(t, a)
	d := f.driver(t, a)
	ctx := context.Background()
	f.stub.FailNext(http.MethodGet, "/club/settings/tariff/load", http.StatusForbidden, `{}`)
	if _, err := d.PriceTable(ctx, "court-1"); upstreamKind(err) != driver.UpstreamRejected || errors.Is(err, driver.ErrNoSession) {
		t.Fatalf("forbidden tariff: %v", err)
	}
	if err := d.Ready(ctx); err != nil {
		t.Fatalf("the session was lost: %v", err)
	}
}

func findCall(f *acctFixture, path string) int {
	for i, c := range f.stub.Calls() {
		if c.Path == path {
			return i
		}
	}
	return -1
}
