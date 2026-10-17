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

func discountRow(price any) map[string]any {
	return map[string]any{"title": "Court discount", "price": price}
}

// The create's price is the booked court's: a price that does not read
// (negative, a string, null, absent), an entry on another court, or none
// on ours is no price — the booking is rolled back, never adjusted.
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
		"a discount already": {row(court1, 1500), discountRow(-300)},
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

// Read back, the booking's courts[] — the court's price and the separate
// "Court discount" entry the adjustment adds, as live — must sum to 0:
// over- or under-adjusted, or with prices that do not read, it is rolled
// back even though Lunda calls it PAID.
func TestCreateReadsBackANetPriceOfZero(t *testing.T) {
	for name, courts := range map[string][]any{
		"over-adjusted":    {row(court1, 1500), discountRow(-3000)},
		"under-adjusted":   {row(court1, 1500), discountRow(-1499.99)},
		"discount missing": {row(court1, 1500)},
		"net folded wrong": {row(court1, -1500)},
		"unreadable":       {row(court1, "1500"), discountRow(-1500)},
		"null discount":    {row(court1, 1500), discountRow(nil)},
		"no courts":        {},
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
	rewriteCourts(t, f, "/club/booking/booking-load", []any{row(court1, 1500), discountRow(-3000)})
	f.stub.FailNext(http.MethodDelete, "/club/booking/booking-cancel", http.StatusInternalServerError, `{}`)
	_, err := d.Create(context.Background(), createReq(futureDay(), 10))
	bs := f.stub.Bookings()
	if err == nil || len(bs) != 1 || !strings.Contains(err.Error(), "STAYS IN LUNDA") || !errorLogged(f.logs.String(), "l-"+bs[0].ID) {
		t.Fatalf("create: %v; logs %s", err, f.logs.String())
	}
}

// As live, the stub answers the adjusted booking with the court's own
// price and a separate negative discount entry; that reads as net 0.
func TestCreatePassesWithTheSeparateDiscountRow(t *testing.T) {
	f, d := loggedIn(t)
	var last []byte
	intercept(f, "/club/booking/booking-load", recorder(f.srv.Config.Handler, &last).ServeHTTP)
	if _, err := d.Create(context.Background(), createReq(futureDay(), 10)); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result struct {
			Status string `json:"status"`
			Courts []struct {
				CourtUID string  `json:"courtUid"`
				Price    float64 `json:"price"`
			} `json:"courts"`
		} `json:"result"`
	}
	if err := json.Unmarshal(last, &env); err != nil {
		t.Fatal(err)
	}
	c := env.Result.Courts
	if env.Result.Status != "PAID" || len(c) != 2 || c[0].CourtUID != court1 || c[0].Price != 1500 || c[1].Price != -1500 {
		t.Fatalf("read back: %s", last)
	}
}
