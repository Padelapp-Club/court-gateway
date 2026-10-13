// Package contract is the driver-agnostic conformance suite of the partner
// API. A driver passes when Run succeeds against a gateway handler built
// around it. The suite talks HTTP to the handler only; it never looks at
// driver internals, so a new driver needs nothing but a Setup function.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// PriceCase is a known quote the configured price table must produce.
type PriceCase struct {
	Court string
	Start time.Time
	End   time.Time
	Total int64
}

// Env is one freshly built gateway.
type Env struct {
	Handler http.Handler
	Club    string
	// OtherClubs are the other clubs the same handler serves (an
	// installation with several clubs); they must be listed too.
	OtherClubs []string
	// ReadKey has only the read scope; BookKey has read and book;
	// OtherBookKey belongs to a second partner with read and book.
	ReadKey      string
	BookKey      string
	OtherBookKey string
	// Courts are configured court ids; at least two.
	Courts      []string
	Location    *time.Location
	SlotMinutes int
	// SeedForeign creates a booking as the club would (not through the
	// gateway) and returns its id as the API exposes it. Drivers that can
	// should fill it with personal data containing PrivacyMarkers.
	SeedForeign func(t *testing.T, court string, start, end time.Time) string
	// Price is a quote the suite checks exactly. Its interval must be in
	// the future and free.
	Price func(day time.Time) PriceCase
	// Logs returns everything the gateway logged so far.
	Logs func() string
	// PrivacyMarkers must never appear in any response or log line.
	PrivacyMarkers []string
}

// Setup builds a fresh Env for one subtest.
type Setup func(t *testing.T) *Env

type client struct {
	t   *testing.T
	env *Env
}

type response struct {
	Status int
	Body   []byte
	JSON   map[string]any
}

func (c *client) do(method, path, key string, body any) response {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			if err != nil {
				c.t.Fatal(err)
			}
			rd = bytes.NewReader(raw)
		}
	}
	req := httptest.NewRequest(method, path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c.env.Handler.ServeHTTP(rec, req)
	r := response{Status: rec.Code, Body: rec.Body.Bytes()}
	if len(bytes.TrimSpace(r.Body)) > 0 {
		var m map[string]any
		if json.Unmarshal(r.Body, &m) == nil {
			r.JSON = m
		}
	}
	return r
}

func (c *client) base() string { return "/v1/clubs/" + c.env.Club }

func expect(t *testing.T, r response, status int, code string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status = %d, want %d; body: %s", r.Status, status, r.Body)
	}
	if code != "" {
		if got, _ := r.JSON["code"].(string); got != code {
			t.Fatalf("code = %q, want %q; body: %s", got, code, r.Body)
		}
		if rid, _ := r.JSON["request_id"].(string); rid == "" {
			t.Fatalf("error without request_id: %s", r.Body)
		}
		if msg, _ := r.JSON["message"].(string); msg == "" {
			t.Fatalf("error without message: %s", r.Body)
		}
	}
}

// day returns local midnight three days from now: far enough to be in the
// future for every driver, close enough for every CRM to answer.
func day(env *Env) time.Time {
	n := time.Now().In(env.Location).AddDate(0, 0, 3)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, env.Location)
}

func at(d time.Time, hh, mm int) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, d.Location())
}

func rfc(t time.Time) string { return t.Format(time.RFC3339) }

type interval struct {
	ID    string `json:"id"`
	Court string `json:"court"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type occupancy struct {
	Complete  bool       `json:"complete"`
	Intervals []interval `json:"intervals"`
}

func (c *client) occupancy(from, to time.Time, court string) occupancy {
	c.t.Helper()
	q := "from=" + urlQ(rfc(from)) + "&to=" + urlQ(rfc(to))
	if court != "" {
		q += "&court=" + court
	}
	r := c.do(http.MethodGet, c.base()+"/occupancy?"+q, c.env.ReadKey, nil)
	expect(c.t, r, http.StatusOK, "")
	var o occupancy
	if err := json.Unmarshal(r.Body, &o); err != nil {
		c.t.Fatalf("occupancy body: %v: %s", err, r.Body)
	}
	if o.Intervals == nil {
		c.t.Fatalf("occupancy must return an intervals array: %s", r.Body)
	}
	return o
}

func urlQ(s string) string { return strings.NewReplacer("+", "%2B", ":", "%3A").Replace(s) }

func findInterval(o occupancy, id string) (interval, bool) {
	for _, iv := range o.Intervals {
		if iv.ID == id {
			return iv, true
		}
	}
	return interval{}, false
}

func sameInstant(t *testing.T, got string, want time.Time) {
	t.Helper()
	g, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("%q is not RFC3339: %v", got, err)
	}
	if !g.Equal(want) {
		t.Fatalf("time = %s, want %s", g, want)
	}
}

func (c *client) create(key, court string, start, end time.Time, kind, note string) response {
	c.t.Helper()
	return c.do(http.MethodPost, c.base()+"/bookings", key, map[string]any{
		"court": court, "start": rfc(start), "end": rfc(end), "kind": kind, "note": note,
	})
}

// Run executes the whole suite.
func Run(t *testing.T, setup Setup) {
	t.Run("discovery", func(t *testing.T) { testDiscovery(t, setup(t)) })
	t.Run("auth", func(t *testing.T) { testAuth(t, setup(t)) })
	t.Run("unknown club and endpoint", func(t *testing.T) { testNotFound(t, setup(t)) })
	t.Run("occupancy shows foreign bookings", func(t *testing.T) { testOccupancyForeign(t, setup(t)) })
	t.Run("occupancy validation", func(t *testing.T) { testOccupancyValidation(t, setup(t)) })
	t.Run("booking lifecycle", func(t *testing.T) { testLifecycle(t, setup(t)) })
	t.Run("tournament booking", func(t *testing.T) { testTournament(t, setup(t)) })
	t.Run("invalid booking requests", func(t *testing.T) { testInvalidBooking(t, setup(t)) })
	t.Run("foreign booking cannot be cancelled", func(t *testing.T) { testForeignCancel(t, setup(t)) })
	t.Run("another partner's booking cannot be cancelled", func(t *testing.T) { testOtherPartnerCancel(t, setup(t)) })
	t.Run("payment follows capabilities", func(t *testing.T) { testPayment(t, setup(t)) })
	t.Run("prices and quote", func(t *testing.T) { testPrices(t, setup(t)) })
	t.Run("privacy: foreign data never leaks", func(t *testing.T) { testPrivacy(t, setup(t)) })
	t.Run("health", func(t *testing.T) { testHealth(t, setup(t)) })
}

func testDiscovery(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	r := c.do(http.MethodGet, "/v1/clubs", env.ReadKey, nil)
	expect(t, r, http.StatusOK, "")
	clubs, _ := r.JSON["clubs"].([]any)
	listed := map[any]bool{}
	for _, c := range clubs {
		listed[c.(map[string]any)["id"]] = true
	}
	if len(clubs) != 1+len(env.OtherClubs) || !listed[env.Club] {
		t.Fatalf("clubs = %s", r.Body)
	}
	for _, o := range env.OtherClubs {
		if !listed[o] {
			t.Fatalf("clubs = %s, lacks %s", r.Body, o)
		}
	}
	r = c.do(http.MethodGet, c.base(), env.ReadKey, nil)
	expect(t, r, http.StatusOK, "")
	if r.JSON["timezone"] != env.Location.String() {
		t.Fatalf("timezone = %v", r.JSON["timezone"])
	}
	if sm, _ := r.JSON["slot_minutes"].(float64); int(sm) != env.SlotMinutes {
		t.Fatalf("slot_minutes = %v", r.JSON["slot_minutes"])
	}
	hours, _ := r.JSON["hours"].(map[string]any)
	for _, d := range []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"} {
		if _, ok := hours[d]; !ok {
			t.Fatalf("hours lacks %s: %s", d, r.Body)
		}
	}
	caps, _ := r.JSON["capabilities"].(map[string]any)
	if _, ok := caps["payments"].(bool); !ok {
		t.Fatalf("capabilities.payments missing: %s", r.Body)
	}
	r = c.do(http.MethodGet, c.base()+"/courts", env.ReadKey, nil)
	expect(t, r, http.StatusOK, "")
	courts, _ := r.JSON["courts"].([]any)
	if len(courts) != len(env.Courts) {
		t.Fatalf("courts = %s", r.Body)
	}
	for i, id := range env.Courts {
		m := courts[i].(map[string]any)
		if m["id"] != id || m["name"] == "" {
			t.Fatalf("court %d = %v", i, m)
		}
		if len(m) != 2 {
			t.Fatalf("court must expose only id and name: %v", m)
		}
	}
}

func testAuth(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	expect(t, c.do(http.MethodGet, c.base()+"/courts", "", nil), http.StatusUnauthorized, "UNAUTHORIZED")
	expect(t, c.do(http.MethodGet, c.base()+"/courts", "wrong-key-wrong-key-wrong-key", nil), http.StatusUnauthorized, "UNAUTHORIZED")
	expect(t, c.do(http.MethodGet, "/v1/clubs", "", nil), http.StatusUnauthorized, "UNAUTHORIZED")
	r := c.create(env.ReadKey, env.Courts[0], at(d, 9, 0), at(d, 10, 0), "game", "")
	expect(t, r, http.StatusForbidden, "FORBIDDEN")
	expect(t, c.do(http.MethodDelete, c.base()+"/bookings/x-1", env.ReadKey, nil), http.StatusForbidden, "FORBIDDEN")
	expect(t, c.do(http.MethodPost, c.base()+"/bookings/x-1/payment", env.ReadKey, nil), http.StatusForbidden, "FORBIDDEN")
	// a refused write must not have created anything
	if o := c.occupancy(at(d, 9, 0), at(d, 10, 0), env.Courts[0]); len(o.Intervals) != 0 {
		t.Fatalf("forbidden create left a booking: %+v", o)
	}
	// book key reads too
	expect(t, c.do(http.MethodGet, c.base()+"/courts", env.BookKey, nil), http.StatusOK, "")
}

func testNotFound(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	for _, p := range []string{"/v1/clubs/no-such-club", "/v1/clubs/no-such-club/courts", "/v1/clubs/no-such-club/occupancy", "/v1/nothing"} {
		expect(t, c.do(http.MethodGet, p, env.ReadKey, nil), http.StatusNotFound, "NOT_FOUND")
	}
	expect(t, c.do(http.MethodGet, c.base()+"/bookings/not-a-real-id", env.ReadKey, nil), http.StatusNotFound, "NOT_FOUND")
	expect(t, c.do(http.MethodPut, c.base()+"/bookings", env.BookKey, nil), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
}

func testOccupancyForeign(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	start, end := at(d, 10, 0), at(d, 11, 0)
	id := env.SeedForeign(t, env.Courts[0], start, end)

	o := c.occupancy(d, d.AddDate(0, 0, 1), "")
	if !o.Complete {
		t.Fatalf("future occupancy must be complete")
	}
	iv, ok := findInterval(o, id)
	if !ok {
		t.Fatalf("foreign booking %s not in occupancy: %+v", id, o)
	}
	if iv.Court != env.Courts[0] {
		t.Fatalf("court = %s", iv.Court)
	}
	sameInstant(t, iv.Start, start)
	sameInstant(t, iv.End, end)

	// court filter
	if _, ok := findInterval(c.occupancy(d, d.AddDate(0, 0, 1), env.Courts[1]), id); ok {
		t.Fatalf("court filter ignored")
	}
	// overlap only: a range touching the booking's end does not include it
	if _, ok := findInterval(c.occupancy(end, end.Add(time.Hour), ""), id); ok {
		t.Fatalf("a range starting at the booking's end must not include it")
	}
	// a range that partially overlaps does
	if _, ok := findInterval(c.occupancy(start.Add(30*time.Minute), end.Add(time.Hour), env.Courts[0]), id); !ok {
		t.Fatalf("partial overlap must include the booking")
	}
}

func testOccupancyValidation(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	b := c.base() + "/occupancy?"
	expect(t, c.do(http.MethodGet, b+"to="+urlQ(rfc(d)), env.ReadKey, nil), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.do(http.MethodGet, b+"from=yesterday&to="+urlQ(rfc(d)), env.ReadKey, nil), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.do(http.MethodGet, b+"from="+urlQ(rfc(d))+"&to="+urlQ(rfc(d.Add(-time.Hour))), env.ReadKey, nil), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.do(http.MethodGet, b+"from="+urlQ(rfc(d))+"&to="+urlQ(rfc(d.AddDate(0, 2, 0))), env.ReadKey, nil), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.do(http.MethodGet, b+"from="+urlQ(rfc(d))+"&to="+urlQ(rfc(d.Add(time.Hour)))+"&court=nope", env.ReadKey, nil), http.StatusNotFound, "NOT_FOUND")
}

func testLifecycle(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	court := env.Courts[1]
	start, end := at(d, 12, 0), at(d, 13, 30)

	r := c.create(env.BookKey, court, start, end, "game", "Player booking")
	expect(t, r, http.StatusCreated, "")
	id, _ := r.JSON["id"].(string)
	if id == "" || r.JSON["court"] != court || r.JSON["status"] != "active" || r.JSON["kind"] != "game" {
		t.Fatalf("create response = %s", r.Body)
	}
	if _, leaked := r.JSON["note"]; leaked {
		t.Fatalf("note must not be echoed: %s", r.Body)
	}
	sameInstant(t, r.JSON["start"].(string), start)
	sameInstant(t, r.JSON["end"].(string), end)

	iv, ok := findInterval(c.occupancy(d, d.AddDate(0, 0, 1), court), id)
	if !ok {
		t.Fatalf("created booking %s not in occupancy", id)
	}
	sameInstant(t, iv.Start, start)

	g := c.do(http.MethodGet, c.base()+"/bookings/"+id, env.ReadKey, nil)
	expect(t, g, http.StatusOK, "")
	if g.JSON["id"] != id || g.JSON["court"] != court || g.JSON["status"] != "active" {
		t.Fatalf("get = %s", g.Body)
	}
	sameInstant(t, g.JSON["end"].(string), end)

	// overlapping our own booking
	expect(t, c.create(env.BookKey, court, at(d, 13, 0), at(d, 14, 0), "game", ""), http.StatusConflict, "SLOT_UNAVAILABLE")
	// overlapping a club booking
	env.SeedForeign(t, court, at(d, 15, 0), at(d, 16, 0))
	expect(t, c.create(env.BookKey, court, at(d, 15, 30), at(d, 16, 30), "game", ""), http.StatusConflict, "SLOT_UNAVAILABLE")
	// adjacent is fine
	adj := c.create(env.BookKey, court, end, end.Add(30*time.Minute), "game", "")
	expect(t, adj, http.StatusCreated, "")

	expect(t, c.do(http.MethodDelete, c.base()+"/bookings/"+id, env.BookKey, nil), http.StatusNoContent, "")
	if _, ok := findInterval(c.occupancy(d, d.AddDate(0, 0, 1), court), id); ok {
		t.Fatalf("cancelled booking still occupies the court")
	}
	// repeating a cancel is safe
	expect(t, c.do(http.MethodDelete, c.base()+"/bookings/"+id, env.BookKey, nil), http.StatusNoContent, "")
	// reading a cancelled booking: either gone or marked cancelled
	g = c.do(http.MethodGet, c.base()+"/bookings/"+id, env.ReadKey, nil)
	if g.Status == http.StatusOK {
		if g.JSON["status"] != "cancelled" {
			t.Fatalf("cancelled booking read as %s", g.Body)
		}
	} else {
		expect(t, g, http.StatusNotFound, "NOT_FOUND")
	}
	// the slot is free again
	expect(t, c.create(env.BookKey, court, start, end, "game", ""), http.StatusCreated, "")
}

func testTournament(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	r := c.create(env.BookKey, env.Courts[0], at(d, 18, 0), at(d, 20, 0), "tournament", "Cup")
	expect(t, r, http.StatusCreated, "")
	if r.JSON["kind"] != "tournament" {
		t.Fatalf("kind = %v", r.JSON["kind"])
	}
	id := r.JSON["id"].(string)
	if _, ok := findInterval(c.occupancy(d, d.AddDate(0, 0, 1), env.Courts[0]), id); !ok {
		t.Fatalf("tournament block not in occupancy")
	}
	expect(t, c.do(http.MethodDelete, c.base()+"/bookings/"+id, env.BookKey, nil), http.StatusNoContent, "")
}

func testInvalidBooking(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	k := env.BookKey
	expect(t, c.create(k, "no-such-court", at(d, 9, 0), at(d, 10, 0), "game", ""), http.StatusNotFound, "NOT_FOUND")
	expect(t, c.create(k, env.Courts[0], at(d, 9, 0), at(d, 10, 0), "party", ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.create(k, env.Courts[0], at(d, 10, 0), at(d, 9, 0), "game", ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.create(k, env.Courts[0], at(d, 9, 7), at(d, 10, 0), "game", ""), http.StatusBadRequest, "INVALID_REQUEST")
	past := at(d.AddDate(0, 0, -5), 9, 0)
	expect(t, c.create(k, env.Courts[0], past, past.Add(time.Hour), "game", ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.create(k, env.Courts[0], at(d, 9, 0), at(d, 10, 0), "game", strings.Repeat("x", 501)), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.do(http.MethodPost, c.base()+"/bookings", k, `{"court":"`+env.Courts[0]+`","extra":1}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.do(http.MethodPost, c.base()+"/bookings", k, `not json`), http.StatusBadRequest, "INVALID_REQUEST")
	if o := c.occupancy(d, d.AddDate(0, 0, 1), ""); len(o.Intervals) != 0 {
		t.Fatalf("invalid requests created bookings: %+v", o)
	}
}

func testForeignCancel(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	id := env.SeedForeign(t, env.Courts[0], at(d, 8, 0), at(d, 9, 0))
	expect(t, c.do(http.MethodDelete, c.base()+"/bookings/"+id, env.BookKey, nil), http.StatusForbidden, "NOT_OWNED")
	if _, ok := findInterval(c.occupancy(d, d.AddDate(0, 0, 1), env.Courts[0]), id); !ok {
		t.Fatalf("foreign booking disappeared after a refused cancel")
	}
}

func testOtherPartnerCancel(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	r := c.create(env.BookKey, env.Courts[0], at(d, 14, 0), at(d, 15, 0), "game", "")
	expect(t, r, http.StatusCreated, "")
	id := r.JSON["id"].(string)
	expect(t, c.do(http.MethodDelete, c.base()+"/bookings/"+id, env.OtherBookKey, nil), http.StatusForbidden, "NOT_OWNED")
	expect(t, c.do(http.MethodDelete, c.base()+"/bookings/"+id, env.BookKey, nil), http.StatusNoContent, "")
}

func testPayment(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	club := c.do(http.MethodGet, c.base(), env.ReadKey, nil)
	payments := club.JSON["capabilities"].(map[string]any)["payments"].(bool)
	r := c.create(env.BookKey, env.Courts[0], at(d, 16, 0), at(d, 17, 0), "game", "")
	expect(t, r, http.StatusCreated, "")
	id := r.JSON["id"].(string)
	p := c.do(http.MethodPost, c.base()+"/bookings/"+id+"/payment", env.BookKey, nil)
	if payments {
		expect(t, p, http.StatusNoContent, "")
	} else {
		expect(t, p, http.StatusNotImplemented, "NOT_SUPPORTED")
	}
}

func testPrices(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	pc := env.Price(d)
	q := fmt.Sprintf("%s/quote?court=%s&start=%s&end=%s", c.base(), pc.Court, urlQ(rfc(pc.Start)), urlQ(rfc(pc.End)))
	r := c.do(http.MethodGet, q, env.ReadKey, nil)
	expect(t, r, http.StatusOK, "")
	if total, _ := r.JSON["total"].(float64); int64(total) != pc.Total || total != float64(int64(total)) {
		t.Fatalf("quote total = %v, want %d: %s", r.JSON["total"], pc.Total, r.Body)
	}
	if r.JSON["currency"] != "RUB" {
		t.Fatalf("currency = %v", r.JSON["currency"])
	}
	bad := fmt.Sprintf("%s/quote?court=%s&start=%s&end=%s", c.base(), pc.Court, urlQ(rfc(pc.Start.Add(7*time.Minute))), urlQ(rfc(pc.End)))
	expect(t, c.do(http.MethodGet, bad, env.ReadKey, nil), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.do(http.MethodGet, c.base()+"/quote?court="+pc.Court, env.ReadKey, nil), http.StatusBadRequest, "INVALID_REQUEST")

	r = c.do(http.MethodGet, c.base()+"/prices?court="+pc.Court+"&date="+pc.Start.Format("2006-01-02"), env.ReadKey, nil)
	expect(t, r, http.StatusOK, "")
	periods, _ := r.JSON["periods"].([]any)
	if len(periods) == 0 {
		t.Fatalf("no price periods: %s", r.Body)
	}
	prevEnd := ""
	for _, p := range periods {
		m := p.(map[string]any)
		s, e := m["start"].(string), m["end"].(string)
		if s >= e || s < prevEnd {
			t.Fatalf("periods not ordered: %s", r.Body)
		}
		if pp, _ := m["price_per_hour"].(float64); pp <= 0 {
			t.Fatalf("bad price: %s", r.Body)
		}
		prevEnd = e
	}
	expect(t, c.do(http.MethodGet, c.base()+"/prices?court="+pc.Court+"&date=tomorrow", env.ReadKey, nil), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, c.do(http.MethodGet, c.base()+"/prices?court=nope&date="+pc.Start.Format("2006-01-02"), env.ReadKey, nil), http.StatusNotFound, "NOT_FOUND")
}

func testPrivacy(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	d := day(env)
	court := env.Courts[0]
	id := env.SeedForeign(t, court, at(d, 10, 0), at(d, 11, 0))
	own := c.create(env.BookKey, court, at(d, 12, 0), at(d, 13, 0), "game", "note with "+strings.Join(env.PrivacyMarkers, " "))
	expect(t, own, http.StatusCreated, "")
	ownID := own.JSON["id"].(string)

	from, to := urlQ(rfc(d)), urlQ(rfc(d.AddDate(0, 0, 1)))
	date := d.Format("2006-01-02")
	pc := env.Price(d)
	calls := []struct{ method, path, key string }{
		{http.MethodGet, "/v1/clubs", env.ReadKey},
		{http.MethodGet, c.base(), env.ReadKey},
		{http.MethodGet, c.base() + "/courts", env.ReadKey},
		{http.MethodGet, c.base() + "/occupancy?from=" + from + "&to=" + to, env.ReadKey},
		{http.MethodGet, c.base() + "/occupancy?from=" + from + "&to=" + to + "&court=" + court, env.ReadKey},
		{http.MethodGet, c.base() + "/prices?court=" + court + "&date=" + date, env.ReadKey},
		{http.MethodGet, fmt.Sprintf("%s/quote?court=%s&start=%s&end=%s", c.base(), pc.Court, urlQ(rfc(pc.Start)), urlQ(rfc(pc.End))), env.ReadKey},
		{http.MethodGet, c.base() + "/bookings/" + id, env.ReadKey},
		{http.MethodGet, c.base() + "/bookings/" + ownID, env.ReadKey},
		{http.MethodDelete, c.base() + "/bookings/" + id, env.BookKey},
		{http.MethodPost, c.base() + "/bookings/" + id + "/payment", env.BookKey},
		{http.MethodPost, c.base() + "/bookings/" + ownID + "/payment", env.BookKey},
		{http.MethodGet, "/health/live", ""},
		{http.MethodGet, "/health/ready", ""},
	}
	sawForeign := false
	for _, call := range calls {
		r := c.do(call.method, call.path, call.key, nil)
		if r.Status >= 500 && r.Status != http.StatusNotImplemented {
			t.Fatalf("%s %s: status %d: %s", call.method, call.path, r.Status, r.Body)
		}
		body := string(r.Body)
		if strings.Contains(body, id) {
			sawForeign = true
		}
		for _, m := range env.PrivacyMarkers {
			if strings.Contains(body, m) {
				t.Fatalf("%s %s leaks %q: %s", call.method, call.path, m, body)
			}
		}
		for _, k := range []string{env.ReadKey, env.BookKey, env.OtherBookKey} {
			if strings.Contains(body, k) {
				t.Fatalf("%s %s leaks an API key", call.method, call.path)
			}
		}
	}
	if !sawForeign {
		t.Fatalf("the foreign booking was never visible, so the privacy check proved nothing")
	}
	logs := env.Logs()
	if logs == "" {
		t.Fatalf("no logs captured, so the log check proves nothing")
	}
	for _, m := range env.PrivacyMarkers {
		if strings.Contains(logs, m) {
			t.Fatalf("logs leak %q", m)
		}
	}
	for _, k := range []string{env.ReadKey, env.BookKey, env.OtherBookKey} {
		if strings.Contains(logs, k) {
			t.Fatalf("logs leak an API key")
		}
	}
}

func testHealth(t *testing.T, env *Env) {
	c := &client{t: t, env: env}
	expect(t, c.do(http.MethodGet, "/health/live", "", nil), http.StatusOK, "")
	r := c.do(http.MethodGet, "/health/ready", "", nil)
	expect(t, r, http.StatusOK, "")
	if r.JSON["status"] != "ready" {
		t.Fatalf("ready = %s", r.Body)
	}
}
