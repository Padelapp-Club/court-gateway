package lunda_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
)

// loggedIn is an account fixture with a club session and a driver on it.
func loggedIn(t *testing.T) (*acctFixture, *lunda.Driver) {
	t.Helper()
	f := newAcctFixture(t)
	f.store.set(lunda.EncodeSession(f.stub.IssueSession(), clubUID, clubName))
	a := f.account(t)
	return f, f.driver(t, a)
}

func createReq(day time.Time, hh int) driver.CreateRequest {
	return driver.CreateRequest{Court: "court-1", Start: day.Add(time.Duration(hh) * time.Hour), End: day.Add(time.Duration(hh+1) * time.Hour),
		Kind: driver.KindGame, Note: "PadelApp 123", Partner: "booker"}
}

// A booking is an ordinary booking of the service client, adjusted by
// minus its price: PAID, no payment, notes with the marker; its id is
// "l-<id>".
func TestCreateIsPaidWithoutPayment(t *testing.T) {
	f, d := loggedIn(t)
	day := futureDay()
	b, err := d.Create(context.Background(), createReq(day, 10))
	if err != nil {
		t.Fatal(err)
	}
	uid := strings.TrimPrefix(b.ID, "l-")
	got := f.stub.Booking(uid)
	if got == nil || got.Status != "PAID" || len(got.Payments) != 0 || got.Customer != serviceUID || got.Goal != "GAME" ||
		got.Price+got.Adjustment != 0 || got.Notes != "[cg:booker] PadelApp 123" {
		t.Fatalf("booking in Lunda: %+v", got)
	}
	create := f.stub.Calls()[findCall(f, "/club/booking/booking-create")].Params
	slot := create["slots"].([]any)[0].(map[string]any)
	if create["placeholder"] != false || create["addPromisedPayment"] != false || slot["begin"] != day.Format("2006-01-02")+"T10:00:00" ||
		slot["end"] != day.Format("2006-01-02")+"T11:00:00" || slot["courtUid"] != court1 {
		t.Fatalf("booking-create: %+v", create)
	}
	if b.Court != "court-1" || !b.Start.Equal(day.Add(10*time.Hour)) {
		t.Fatalf("booking: %+v", b)
	}
	// a tournament is a booking of goal TOURNAMENT
	req := createReq(day, 14)
	req.Kind = driver.KindTournament
	tb, err := d.Create(context.Background(), req)
	if err != nil || f.stub.Booking(strings.TrimPrefix(tb.ID, "l-")).Goal != "TOURNAMENT" {
		t.Fatalf("tournament: %v", err)
	}
}

// Any step after the create failing cancels the booking and fails the
// create: never a booking half made (unpaid, or not ours to cancel).
func TestCreateRollsBack(t *testing.T) {
	for name, fail := range map[string]func(*lundastub.Stub){
		"price adjustment refused": func(s *lundastub.Stub) {
			s.FailNext(http.MethodPost, "/club/booking/update-courts-price-adjustment", http.StatusBadRequest, `{}`)
		},
		"price adjustment unavailable": func(s *lundastub.Stub) {
			s.FailNext(http.MethodPost, "/club/booking/update-courts-price-adjustment", http.StatusServiceUnavailable, `{}`)
		},
		"notes refused": func(s *lundastub.Stub) {
			s.FailNext(http.MethodPost, "/club/booking/booking-notes-update", http.StatusBadRequest, `{}`)
		},
		"notes do not read back": func(s *lundastub.Stub) { s.OmitNotesOnLoad = true },
		"read back fails": func(s *lundastub.Stub) {
			s.FailNext(http.MethodGet, "/club/booking/booking-load", http.StatusInternalServerError, `{}`)
		},
		"not paid": func(s *lundastub.Stub) {
			s.FailNext(http.MethodPost, "/club/booking/update-courts-price-adjustment", http.StatusOK, `{"result":{}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			fail(f.stub)
			_, err := d.Create(context.Background(), createReq(futureDay(), 10))
			if err == nil {
				t.Fatal("the create passed")
			}
			if strings.Contains(err.Error(), "STAYS IN LUNDA") {
				t.Fatalf("not rolled back: %v", err)
			}
			for _, b := range f.stub.Bookings() {
				if b.Status != "CANCELLED" {
					t.Fatalf("a booking is left in Lunda: %+v", b)
				}
			}
			if f.stub.CallsTo("/club/booking/booking-cancel") != 1 {
				t.Fatalf("cancels: %d", f.stub.CallsTo("/club/booking/booking-cancel"))
			}
		})
	}
}

// A rollback that fails too says the booking stays in Lunda, by its id.
func TestFailedRollbackIsReported(t *testing.T) {
	f, d := loggedIn(t)
	f.stub.FailNext(http.MethodPost, "/club/booking/update-courts-price-adjustment", http.StatusBadRequest, `{}`)
	f.stub.FailNext(http.MethodDelete, "/club/booking/booking-cancel", http.StatusInternalServerError, `{}`)
	_, err := d.Create(context.Background(), createReq(futureDay(), 10))
	bs := f.stub.Bookings()
	if err == nil || len(bs) != 1 || !strings.Contains(err.Error(), "l-"+bs[0].ID) || !strings.Contains(err.Error(), "STAYS IN LUNDA") {
		t.Fatalf("error: %v", err)
	}
}

// A create whose answer is lost may have made the booking: it is looked
// for in the schedule (the service client, exactly that court and time)
// and cancelled.
func TestCreateWithLostAnswerIsUndone(t *testing.T) {
	f, d := loggedIn(t)
	day := futureDay()
	foreign := seedClubBooking(f.stub, court1, day.Add(10*time.Hour), day.Add(11*time.Hour))
	f.stub.SeedBooking(lundastub.Booking{Court: court2, Begin: day.Add(12 * time.Hour), End: day.Add(13 * time.Hour), Customer: serviceUID})
	// the stub makes the booking but its answer says nothing usable
	base := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/club/booking/booking-create" {
			base.ServeHTTP(&discard{header: http.Header{}}, r)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		base.ServeHTTP(w, r)
	})
	if _, err := d.Create(context.Background(), createReq(day, 12)); upstreamKind(err) != driver.UpstreamUnavailable {
		t.Fatalf("create: %v", err)
	}
	for _, b := range f.stub.Bookings() {
		lost := b.Court == court1 && b.Customer == serviceUID
		if lost && b.Status != "CANCELLED" || !lost && b.Status == "CANCELLED" {
			t.Fatalf("booking %+v (foreign %s)", b, foreign)
		}
	}
}

type discard struct{ header http.Header }

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

// Without a service client nothing is booked and nothing sent; reading
// works, and readiness says what is missing.
func TestNoServiceClientReadsButDoesNotBook(t *testing.T) {
	f := newFixture(t, fixtureOpts{noService: true})
	ctx := context.Background()
	_, err := f.drv.Create(ctx, createReq(futureDay(), 10))
	var ca *driver.ConfigAlert
	if !errors.Is(err, driver.ErrBookingNotConfigured) || errors.Is(err, driver.ErrNotSupported) || !errors.As(err, &ca) || ca.Code != "SERVICE_CLIENT_MISSING" {
		t.Fatalf("create: %v", err)
	}
	if f.stub.CallsTo("/club/booking/booking-create") != 0 {
		t.Fatal("booking-create was sent")
	}
	if err := f.drv.Ready(ctx); !errors.As(err, &ca) || ca.Code != "SERVICE_CLIENT_MISSING" {
		t.Fatalf("ready: %v", err)
	}
	if _, err := f.drv.Occupancy(ctx, occQuery(futureDay())); err != nil {
		t.Fatalf("occupancy: %v", err)
	}
}

// A booking of the service client is the club's own unless its notes start
// with the caller's marker (the club books its tournaments on that very
// client); one with the marker on another client is not ours either.
// Neither is cancelled.
func TestCancelOnlyOwnBookings(t *testing.T) {
	f, d := loggedIn(t)
	ctx := context.Background()
	day := futureDay()
	clubs := f.stub.SeedBooking(lundastub.Booking{Court: court1, Begin: day.Add(9 * time.Hour), End: day.Add(10 * time.Hour), Customer: serviceUID,
		Notes: "Турнир клуба"})
	forged := f.stub.SeedBooking(lundastub.Booking{Court: court1, Begin: day.Add(11 * time.Hour), End: day.Add(12 * time.Hour), Customer: clubClient,
		Notes: "[cg:booker] not on the service client"})
	inside := f.stub.SeedBooking(lundastub.Booking{Court: court1, Begin: day.Add(13 * time.Hour), End: day.Add(14 * time.Hour), Customer: serviceUID,
		Notes: "Корпоратив [cg:booker]"})
	for _, id := range []string{clubs, forged, inside} {
		if err := d.Cancel(ctx, "l-"+id, "booker"); !errors.Is(err, driver.ErrNotOwned) {
			t.Fatalf("cancel %s: %v", id, err)
		}
		if f.stub.Booking(id).Status == "CANCELLED" {
			t.Fatalf("%s was cancelled", id)
		}
	}
	if f.stub.CallsTo("/club/booking/booking-cancel") != 0 {
		t.Fatal("booking-cancel was sent")
	}
	own, err := d.Create(ctx, createReq(day, 16))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Cancel(ctx, own.ID, "other"); !errors.Is(err, driver.ErrNotOwned) {
		t.Fatalf("another partner: %v", err)
	}
	if err := d.Cancel(ctx, own.ID, "booker"); err != nil {
		t.Fatal(err)
	}
	p := f.stub.Calls()[findCall(f, "/club/booking/booking-cancel")].Params
	if p["bookingType"] != "COURT" || p["forced"] != false || p["reason"] == "" {
		t.Fatalf("booking-cancel: %+v", p)
	}
	// cancelled, then gone: no error
	if err := d.Cancel(ctx, own.ID, "booker"); err != nil {
		t.Fatal(err)
	}
	if err := d.Cancel(ctx, "l-00000000-0000-4000-8000-00000000dead", "booker"); err != nil {
		t.Fatal(err)
	}
}

func TestGet(t *testing.T) {
	f, d := loggedIn(t)
	ctx := context.Background()
	day := futureDay()
	id := seedClubBooking(f.stub, court2, day.Add(9*time.Hour), day.Add(10*time.Hour))
	b, err := d.Get(ctx, "l-"+id)
	if err != nil || b.Court != "court-2" || !b.Start.Equal(day.Add(9*time.Hour)) || b.Status != driver.StatusActive {
		t.Fatalf("get: %+v %v", b, err)
	}
	other := seedClubBooking(f.stub, courtOther, day.Add(9*time.Hour), day.Add(10*time.Hour))
	if _, err := d.Get(ctx, "l-"+other); !errors.Is(err, driver.ErrCourtNotConfigured) {
		t.Fatalf("a booking on another court: %v", err)
	}
	for _, id := range []string{"ls-0123456789abcdef", "e-" + id, "l-"} {
		if _, err := d.Get(ctx, id); !errors.Is(err, driver.ErrNotFound) {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

// MarkPaid is never anything but ErrNotSupported, and sends nothing: no
// payment is ever written to Lunda (the fixture's check fails on any
// payment call too).
func TestMarkPaidSendsNothing(t *testing.T) {
	f, d := loggedIn(t)
	ctx := context.Background()
	b, err := d.Create(ctx, createReq(futureDay(), 10))
	if err != nil {
		t.Fatal(err)
	}
	n := len(f.stub.Calls())
	if err := d.MarkPaid(ctx, b.ID, "booker"); !errors.Is(err, driver.ErrNotSupported) {
		t.Fatalf("mark paid: %v", err)
	}
	if len(f.stub.Calls()) != n {
		t.Fatalf("mark paid called Lunda: %+v", f.stub.Calls()[n:])
	}
	if got := f.stub.Booking(strings.TrimPrefix(b.ID, "l-")); len(got.Payments) != 0 {
		t.Fatalf("payments: %+v", got.Payments)
	}
	if d.Capabilities().Payments {
		t.Fatal("payments capability")
	}
}

// The driver's operations hold no payment or refund operation at all.
func TestNoPaymentOperation(t *testing.T) {
	for _, op := range lunda.Operations() {
		l := strings.ToLower(op)
		if strings.Contains(l, "payment") || strings.Contains(l, "refund") || strings.Contains(l, "settlement") {
			t.Fatalf("operation %s", op)
		}
	}
}
