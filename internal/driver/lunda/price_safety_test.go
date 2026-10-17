package lunda_test

// The price of a booking the gateway makes: the adjustment is minus the
// price of the booked court alone, and the booking reads back at a net
// court price of 0 (review r8, follow-ups 1 and 2).

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
		env["result"].(map[string]any)["courts"] = courts
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

// Read back, the booked court's price less the "Court discount" entries
// (live: no courtUid, a positive price) must be 0. Over- or
// under-adjusted, a negative price anywhere, an entry without courtUid
// that is not the discount, an entry on another court, or prices that do
// not read: it is rolled back even though Lunda calls it PAID.
func TestCreateReadsBackANetPriceOfZero(t *testing.T) {
	for name, courts := range map[string][]any{
		"over-adjusted":             {row(court1, 1500), discountRow(3000)},
		"under-adjusted":            {row(court1, 1500), discountRow(1499)},
		"discount missing":          {row(court1, 1500)},
		"negative discount":         {row(court1, 1500), discountRow(-1500)},
		"negative court":            {row(court1, -1500), discountRow(0)},
		"both negative":             {row(court1, -1500), discountRow(-1500)},
		"discount on the court":     {row(court1, 1500), map[string]any{"courtUid": court1, "price": 1500, "description": "Court discount"}},
		"no courtUid, not discount": {row(court1, 1500), map[string]any{"price": 1500, "description": "Bonus"}},
		"no courtUid, no desc":      {row(court1, 1500), map[string]any{"price": 1500}},
		"another court":             {row(court1, 1500), row(court2, 0), discountRow(1500)},
		"only a discount":           {discountRow(0)},
		"unreadable":                {row(court1, "1500"), discountRow(1500)},
		"null discount":             {row(court1, 1500), discountRow(nil)},
		"no courts":                 {},
	} {
		t.Run(name, func(t *testing.T) {
			f, d := loggedIn(t)
			rewriteCourts(t, f, "/club/booking/booking-load", courts)
			_, err := d.Create(context.Background(), createReq(futureDay(), 10))
			assertRolledBack(t, f, err, "net court price of 0")
		})
	}
	// the cancel failing too: the booking stays, said and logged at ERROR
	f, d := loggedIn(t)
	rewriteCourts(t, f, "/club/booking/booking-load", []any{row(court1, 1500), discountRow(3000)})
	f.stub.FailNext(http.MethodDelete, "/club/booking/booking-cancel", http.StatusInternalServerError, `{}`)
	_, err := d.Create(context.Background(), createReq(futureDay(), 10))
	bs := f.stub.Bookings()
	if err == nil || len(bs) != 1 || !strings.Contains(err.Error(), "STAYS IN LUNDA") || !errorLogged(f.logs.String(), "l-"+bs[0].ID) {
		t.Fatalf("create: %v; logs %s", err, f.logs.String())
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
