package lunda_test

// The price of a booking the gateway makes: the adjustment is minus the
// price of the booked court alone. Read back PAID without payments, it is
// the gateway's at whatever net its price lines read: one other than 0 is
// logged at WARN, not rolled back.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// rewriteCourts answers the first request to path as the stub does, but
// with the courts[] of the booking replaced by courts; later requests go
// to the stub unchanged.
func rewriteCourts(t *testing.T, f *acctFixture, path string, courts []any) {
	t.Helper()
	rewriteResult(t, f, path, func(b map[string]any) { b["courts"] = courts })
}

// rewriteResult answers the first request to path as the stub does, but
// with the booking it answers changed by edit; later requests go to the
// stub unchanged.
func rewriteResult(t *testing.T, f *acctFixture, path string, edit func(map[string]any)) {
	t.Helper()
	stub := f.srv.Config.Handler
	var done atomic.Bool
	intercept(f, path, func(w http.ResponseWriter, r *http.Request) {
		if done.Swap(true) {
			stub.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		stub.ServeHTTP(rec, r)
		var env map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Errorf("stub answer: %v", err)
			return
		}
		edit(env["result"].(map[string]any))
		w.WriteHeader(rec.Code)
		_ = json.NewEncoder(w).Encode(env)
	})
}

// assertRolledBack: the create failed, its booking was cancelled (one
// cancel, logged at WARN) and nothing stays in Lunda.
func assertRolledBack(t *testing.T, f *acctFixture, err error, why string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), why) || strings.Contains(err.Error(), "STAYS IN LUNDA") {
		t.Fatalf("create: %v", err)
	}
	bs := f.stub.Bookings()
	if len(bs) != 1 || bs[0].Status != "CANCELLED" || f.stub.CallsTo("/club/booking/booking-cancel") != 1 {
		t.Fatalf("bookings %+v, cancels %d", bs, f.stub.CallsTo("/club/booking/booking-cancel"))
	}
	if !strings.Contains(f.logs.String(), `"level":"WARN"`) || !strings.Contains(f.logs.String(), "l-"+bs[0].ID) {
		t.Fatalf("logs: %s", f.logs.String())
	}
}

func row(court string, price any) map[string]any {
	return map[string]any{"courtUid": court, "price": price}
}

// discountRow is the entry a price adjustment adds to courts[], as live
// (Kosmos, 2026-10-18): no courtUid, a positive price, the description.
func discountRow(price any) map[string]any {
	return map[string]any{"id": "d1", "price": price, "description": "Court discount"}
}

// The create's price is the booked court's: a price that does not read
// (negative, a string, null, absent), an entry on another court, a
// discount already, or no entry on ours is no price — the booking is
// rolled back, never adjusted.
func TestCreatePriceIsTheBookedCourtsOnly(t *testing.T) {
	for name, courts := range map[string][]any{
		"negative":           {row(court1, -1500)},
		"string":             {row(court1, "1500")},
		"null":               {row(court1, nil)},
		"no price":           {map[string]any{"courtUid": court1}},
		"kopecks too fine":   {row(court1, 1500.005)},
		"another court too":  {row(court1, 1500), row(court2, 0)},
		"another court only": {row(court2, 1500)},
		"no court entry":     {},
		"a discount already": {row(court1, 1500), discountRow(300)},
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			rewriteCourts(t, f, "/club/booking/booking-create", courts)
			_, err := d.Create(context.Background(), createReq(futureDay(), 10))
			assertRolledBack(t, f, err, "no valid price of the court")
			if n := f.stub.CallsTo("/club/booking/update-courts-price-adjustment"); n != 0 {
				t.Fatalf("adjusted %d times", n)
			}
		})
	}
}

// netWarning is the "lines" of the WARN that booking, read back PAID
// without payments, does not net to 0; ok is false when there is none.
func netWarning(logs, booking string) (lines string, ok bool) {
	for _, l := range strings.Split(logs, "\n") {
		var e struct{ Level, Msg, Booking, Lines string }
		if json.Unmarshal([]byte(l), &e) == nil && e.Level == "WARN" && strings.Contains(e.Msg, "do not net to 0") && e.Booking == booking {
			return e.Lines, true
		}
	}
	return "", false
}

// Read back PAID without payments, with the marker and the service
// client, a booking is the gateway's whatever its price lines net to:
// Lunda calling it PAID with no payment rules out a debt of the service
// client. Live at Amur Padel (2026-10-31) bookings read back so in lines
// that did not net to 0 by the shape seen at Kosmos, and every one was
// rolled back. Over- or under-adjusted, a negative price, an entry
// without courtUid that is not the discount, an entry on another court,
// prices that do not read, no entry at all: the booking stays, and a WARN
// names it and describes its lines — kind, a price that is a number as it
// is, any other price and any description but the discount's only by
// shape — but no court uid and nothing about the customer, even when
// Lunda's text in the lines is a name and a phone.
func TestCreateAcceptsAPaidReadBackThatDoesNotNetToZero(t *testing.T) {
	const court = `booked price=1500 description=""`
	for name, tc := range map[string]struct {
		courts []any
		lines  string
	}{
		"over-adjusted":             {[]any{row(court1, 1500), discountRow(3000)}, court + `; none price=3000 description="Court discount"`},
		"under-adjusted":            {[]any{row(court1, 1500), discountRow(1499)}, court + `; none price=1499 description="Court discount"`},
		"discount missing":          {[]any{row(court1, 1500)}, court},
		"negative discount":         {[]any{row(court1, 1500), discountRow(-1500)}, court + `; none price=-1500 description="Court discount"`},
		"negative court":            {[]any{row(court1, -1500), discountRow(0)}, `booked price=-1500 description=""; none price=0 description="Court discount"`},
		"both negative":             {[]any{row(court1, -1500), discountRow(-1500)}, `booked price=-1500 description=""; none price=-1500 description="Court discount"`},
		"discount on the court":     {[]any{row(court1, 1500), map[string]any{"courtUid": court1, "price": 1500, "description": "Court discount"}}, court + `; booked price=1500 description="Court discount"`},
		"no courtUid, not discount": {[]any{row(court1, 1500), map[string]any{"price": 1500, "description": "Bonus"}}, court + `; none price=1500 description=text(len=5 latin words=1 digits=0)`},
		"discount in Russian":       {[]any{row(court1, 1500), map[string]any{"price": 1500, "description": "Скидка на корт"}}, court + `; none price=1500 description=text(len=14 cyrillic words=3 digits=0)`},
		"no courtUid, no desc":      {[]any{row(court1, 1500), map[string]any{"price": 1500}}, court + `; none price=1500 description=""`},
		"another court":             {[]any{row(court1, 1500), row(court2, 0), discountRow(1500)}, court + `; other price=0 description=""; none price=1500 description="Court discount"`},
		"only a discount":           {[]any{discountRow(0)}, `none price=0 description="Court discount"`},
		"unreadable":                {[]any{row(court1, "1500"), discountRow(1500)}, `booked price="1500" description=""; none price=1500 description="Court discount"`},
		"null discount":             {[]any{row(court1, 1500), discountRow(nil)}, court + `; none price=null description="Court discount"`},
		"no price":                  {[]any{map[string]any{"courtUid": court1}}, `booked price=absent description=""`},
		"no courts":                 {[]any{}, ``},
		"long text": {[]any{map[string]any{"courtUid": court1, "price": "123456789012345678901234", "description": strings.Repeat("я", 50)}},
			`booked price=unreadable(len=26) description=text(len=50 cyrillic words=1 digits=0)`},
		"a name and a phone": {[]any{row(court1, 1500), map[string]any{"price": "+79990000000", "description": "Jane Smith +79990000000"}, map[string]any{"price": 79990000000, "description": "Jane"}},
			court + `; none price=unreadable(len=14) description=text(len=23 latin words=3 digits=11); none price=unreadable(len=11) description=text(len=4 latin words=1 digits=0)`},
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			rewriteResult(t, f, "/club/booking/booking-load", func(b map[string]any) {
				b["courts"] = tc.courts
				b["customer"] = map[string]any{"firstName": "PRIVACY-MARKER-FIRSTNAME-91c2", "phone": "+79990000000"}
			})
			b, err := d.Create(context.Background(), createReq(futureDay(), 10))
			if err != nil {
				t.Fatal(err)
			}
			if got := f.stub.Booking(strings.TrimPrefix(b.ID, "l-")); got.Status != "PAID" || f.stub.CallsTo("/club/booking/booking-cancel") != 0 {
				t.Fatalf("rolled back: %+v", got)
			}
			logs := f.logs.String()
			if lines, ok := netWarning(logs, b.ID); !ok || lines != tc.lines {
				t.Fatalf("WARN lines %q, want %q; logs %s", lines, tc.lines, logs)
			}
			if strings.Contains(logs, "PRIVACY-MARKER") || strings.Contains(logs, "+7999") || strings.Contains(logs, "79990000000") || strings.Contains(logs, "Jane") || strings.Contains(logs, court1) {
				t.Fatalf("logs: %s", logs)
			}
		})
	}
	// at net 0 nothing is said
	f, d := loggedIn(t)
	b, err := d.Create(context.Background(), createReq(futureDay(), 10))
	if _, warned := netWarning(f.logs.String(), b.ID); err != nil || warned {
		t.Fatalf("create: %v; logs %s", err, f.logs.String())
	}
}

// Only the net is relaxed: read back not PAID, with a payment, without
// the gateway's marker or on another client, a booking whose lines do not
// net to 0 either is rolled back as before.
func TestCreateRollsBackANetOtherThanZeroNotPaidOrNotOurs(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(map[string]any)
		why  string
	}{
		"not paid": {func(b map[string]any) { b["status"] = "NOT_PAID" }, "not PAID without payments"},
		"a payment": {func(b map[string]any) {
			b["payments"] = []any{map[string]any{"type": "OFFLINE_PAYMENT", "amount": 1500}}
		}, "not PAID without payments"},
		"no notes":       {func(b map[string]any) { delete(b, "notes") }, "marker and service client"},
		"another marker": {func(b map[string]any) { b["notes"] = "[cg:other] PadelApp 123" }, "marker and service client"},
		"another client": {func(b map[string]any) { b["customerUid"] = clubClient }, "marker and service client"},
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			rewriteResult(t, f, "/club/booking/booking-load", func(b map[string]any) {
				b["courts"] = []any{row(court1, 1500), discountRow(3000)}
				tc.edit(b)
			})
			_, err := d.Create(context.Background(), createReq(futureDay(), 10))
			assertRolledBack(t, f, err, tc.why)
		})
	}
}

// The live shapes that read as net 0 pass: the court's price and one
// discount of the same size, the court split in rows, or several
// discounts adding up to it.
func TestCreatePassesAtANetPriceOfZero(t *testing.T) {
	for name, courts := range map[string][]any{
		"one row, one discount": {row(court1, 1500), discountRow(1500)},
		"court in two rows":     {row(court1, 750), row(court1, 750), discountRow(1500)},
		"two discounts":         {row(court1, 2000), discountRow(1500), discountRow(500)},
		"kopecks":               {row(court1, 1500.5), discountRow(1500.5)},
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			rewriteCourts(t, f, "/club/booking/booking-load", courts)
			if _, err := d.Create(context.Background(), createReq(futureDay(), 10)); err != nil {
				t.Fatal(err)
			}
			if f.stub.CallsTo("/club/booking/booking-cancel") != 0 {
				t.Fatal("rolled back")
			}
		})
	}
}

// As live, the stub answers the adjusted booking with the court's own
// price and a separate discount entry — no courtUid, a positive price,
// "Court discount" — and that reads as net 0.
func TestCreatePassesWithTheSeparateDiscountRow(t *testing.T) {
	f, d := loggedIn(t)
	var last []byte
	intercept(f, "/club/booking/booking-load", recorder(f.srv.Config.Handler, &last).ServeHTTP)
	if _, err := d.Create(context.Background(), createReq(futureDay(), 10)); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result struct {
			Status string           `json:"status"`
			Courts []map[string]any `json:"courts"`
		} `json:"result"`
	}
	if err := json.Unmarshal(last, &env); err != nil {
		t.Fatal(err)
	}
	c := env.Result.Courts
	if env.Result.Status != "PAID" || len(c) != 2 || c[0]["courtUid"] != court1 || c[0]["price"] != 1500.0 {
		t.Fatalf("read back: %s", last)
	}
	if _, has := c[1]["courtUid"]; has || c[1]["price"] != 1500.0 || c[1]["description"] != "Court discount" {
		t.Fatalf("discount entry: %v", c[1])
	}
}
