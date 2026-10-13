package viva_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

func call(t *testing.T, f *fixture, method, path, key string, body string) (int, map[string]any, string) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	f.built.Handler().ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Body.String()
}

func q(t time.Time) string { return url.QueryEscape(t.Format(time.RFC3339)) }

func futureDay() time.Time {
	n := time.Now().UTC().AddDate(0, 0, 3)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func occupancyPath(from, to time.Time) string {
	return "/v1/clubs/test-club/occupancy?from=" + q(from) + "&to=" + q(to)
}

func TestOccupancyUsesUTCDayMarginAndCutsOverlap(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := futureDay()
	// Starts on the previous UTC day and runs past midnight.
	crossing := f.seedClubBooking(room1, d.Add(-time.Hour), d.Add(time.Hour))
	// Inside the margin day but not overlapping the range.
	early := f.seedClubBooking(room1, d.Add(-5*time.Hour), d.Add(-4*time.Hour))
	// Next day, inside the dateTo margin, not overlapping.
	late := f.seedClubBooking(room1, d.Add(26*time.Hour), d.Add(27*time.Hour))
	// A room that is not a configured court.
	elsewhere := f.seedClubBooking(roomOther, d.Add(time.Hour), d.Add(2*time.Hour))

	status, body, raw := call(t, f, http.MethodGet, occupancyPath(d.Add(30*time.Minute), d.Add(3*time.Hour)), contract.ReadKey, "")
	if status != http.StatusOK || body["complete"] != true {
		t.Fatalf("status %d: %s", status, raw)
	}
	if !strings.Contains(raw, "e-"+crossing) {
		t.Fatalf("booking crossing UTC midnight missing: %s", raw)
	}
	for _, id := range []string{early, late, elsewhere} {
		if strings.Contains(raw, id) {
			t.Fatalf("non-overlapping or foreign-room booking %s returned: %s", id, raw)
		}
	}
	// The list call asked for whole days with a day of margin.
	want := "dateFrom=" + d.AddDate(0, 0, -1).Format("2006-01-02")
	found := false
	for _, c := range f.stub.Calls() {
		if strings.HasPrefix(c, "GET /exercises?") && strings.Contains(c, want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no list call with %s: %v", want, f.stub.Calls())
	}
}

func TestOccupancyInThePastIsIncomplete(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	now := time.Now().UTC()
	status, body, raw := call(t, f, http.MethodGet, occupancyPath(now.Add(-48*time.Hour), now.Add(-46*time.Hour)), contract.ReadKey, "")
	if status != http.StatusOK || body["complete"] != false {
		t.Fatalf("past range must be complete:false; status %d: %s", status, raw)
	}
	// a range reaching from yesterday into the future is incomplete too
	_, body, raw = call(t, f, http.MethodGet, occupancyPath(now.Add(-26*time.Hour), now.Add(26*time.Hour)), contract.ReadKey, "")
	if body["complete"] != false {
		t.Fatalf("range touching the past must be complete:false: %s", raw)
	}
	// and a booking there is refused instead of trusting an empty answer
	// (the core refuses past starts before asking the driver)
	start := now.Truncate(30 * time.Minute).Add(-time.Hour)
	st, b, _ := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey,
		`{"court":"court-1","start":"`+start.Format(time.RFC3339)+`","end":"`+start.Add(time.Hour).Format(time.RFC3339)+`","kind":"game"}`)
	if st != http.StatusBadRequest || b["code"] != "INVALID_REQUEST" {
		t.Fatalf("past booking: %d %v", st, b)
	}
}

func TestMalformedUpstreamIsNeverFree(t *testing.T) {
	d := futureDay()
	cases := map[string]string{
		"object instead of array": `{"content": []}`,
		"null":                    `null`,
		"html":                    `<html>maintenance</html>`,
		"item without room":       `[{"id": "00000000-0000-4000-8000-000000000001", "timeFrom": "2030-01-01T10:00:00Z", "timeTo": "2030-01-01T11:00:00Z"}]`,
		"item with bad time":      `[{"id": "00000000-0000-4000-8000-000000000001", "timeFrom": "soon", "timeTo": "later", "room": {"id": "` + room1 + `"}}]`,
		"item without id":         `[{"timeFrom": "2030-01-01T10:00:00Z", "timeTo": "2030-01-01T11:00:00Z", "room": {"id": "` + room1 + `"}}]`,
		"item with wrong types":   `[{"id": 5, "timeFrom": 1, "timeTo": 2, "room": "x"}]`,
		"end before start":        `[{"id": "00000000-0000-4000-8000-000000000001", "timeFrom": "2030-01-01T11:00:00Z", "timeTo": "2030-01-01T10:00:00Z", "room": {"id": "` + room1 + `"}}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			f.stub.FailNext(http.MethodGet, "/breaks", http.StatusOK, body)
			status, m, raw := call(t, f, http.MethodGet, occupancyPath(d, d.Add(24*time.Hour)), contract.ReadKey, "")
			if status != http.StatusBadGateway || m["code"] != "UPSTREAM_INVALID" {
				t.Fatalf("status %d: %s", status, raw)
			}
			// the booking check must refuse too, not create
			f.stub.FailNext(http.MethodGet, "/exercises", http.StatusOK, body)
			start := d.Add(10 * time.Hour)
			st, _, raw := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey,
				`{"court":"court-1","start":"`+start.Format(time.RFC3339)+`","end":"`+start.Add(time.Hour).Format(time.RFC3339)+`","kind":"game"}`)
			if st != http.StatusBadGateway {
				t.Fatalf("create on malformed occupancy: %d %s", st, raw)
			}
			for _, c := range f.stub.Calls() {
				if strings.HasPrefix(c, "POST ") {
					t.Fatalf("created despite malformed occupancy: %v", f.stub.Calls())
				}
			}
		})
	}
}

func TestUpstreamFailuresMapToCodes(t *testing.T) {
	d := futureDay()
	f := newFixture(t, fixtureOpts{})
	f.stub.FailNext(http.MethodGet, "/exercises", http.StatusInternalServerError, `{}`)
	if st, m, raw := call(t, f, http.MethodGet, occupancyPath(d, d.Add(time.Hour)), contract.ReadKey, ""); st != http.StatusBadGateway || m["code"] != "UPSTREAM_UNAVAILABLE" {
		t.Fatalf("5xx: %d %s", st, raw)
	}
	f.stub.FailNext(http.MethodGet, "/exercises", http.StatusBadRequest, `{"message":"bad"}`)
	if st, m, raw := call(t, f, http.MethodGet, occupancyPath(d, d.Add(time.Hour)), contract.ReadKey, ""); st != http.StatusBadGateway || m["code"] != "UPSTREAM_REJECTED" {
		t.Fatalf("4xx: %d %s", st, raw)
	}
}

func TestReloginOnceOn401(t *testing.T) {
	d := futureDay()
	f := newFixture(t, fixtureOpts{})
	if st, _, raw := call(t, f, http.MethodGet, occupancyPath(d, d.Add(time.Hour)), contract.ReadKey, ""); st != 200 {
		t.Fatalf("%d %s", st, raw)
	}
	if f.stub.Logins() != 1 {
		t.Fatalf("logins = %d", f.stub.Logins())
	}
	f.stub.RevokeTokens()
	if st, _, raw := call(t, f, http.MethodGet, occupancyPath(d, d.Add(time.Hour)), contract.ReadKey, ""); st != 200 {
		t.Fatalf("after revoke: %d %s", st, raw)
	}
	if f.stub.Logins() != 2 {
		t.Fatalf("logins after revoke = %d, want 2", f.stub.Logins())
	}
	// 401 even after a fresh login: give up after one retry
	f.stub.FailNext(http.MethodGet, "/exercises", http.StatusUnauthorized, `{}`)
	f.stub.FailNext(http.MethodGet, "/exercises", http.StatusUnauthorized, `{}`)
	st, m, raw := call(t, f, http.MethodGet, occupancyPath(d, d.Add(time.Hour)), contract.ReadKey, "")
	if st != http.StatusBadGateway || m["code"] != "UPSTREAM_AUTH" {
		t.Fatalf("double 401: %d %s", st, raw)
	}
	if f.stub.Logins() != 3 {
		t.Fatalf("logins = %d, want exactly one re-login (3)", f.stub.Logins())
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestTokenCachedUntilExpiryMinus30s(t *testing.T) {
	clk := &clock{t: time.Now()}
	f := newFixture(t, fixtureOpts{now: clk.now})
	ctx := context.Background()
	q := driver.OccupancyQuery{Courts: []string{"court-1"}, From: clk.now().Add(48 * time.Hour), To: clk.now().Add(49 * time.Hour)}
	for i := 0; i < 3; i++ {
		if _, err := f.drv.Occupancy(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if f.stub.Logins() != 1 {
		t.Fatalf("token not cached: %d logins", f.stub.Logins())
	}
	clk.add(269 * time.Second) // TTL 300 - 30 = 270
	if _, err := f.drv.Occupancy(ctx, q); err != nil {
		t.Fatal(err)
	}
	if f.stub.Logins() != 1 {
		t.Fatalf("re-login too early: %d", f.stub.Logins())
	}
	clk.add(2 * time.Second)
	if _, err := f.drv.Occupancy(ctx, q); err != nil {
		t.Fatal(err)
	}
	if f.stub.Logins() != 2 {
		t.Fatalf("no re-login after expiry-30s: %d", f.stub.Logins())
	}
}

func TestReadyReasons(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	if st, m, raw := call(t, f, http.MethodGet, "/health/ready", "", ""); st != 200 || m["status"] != "ready" {
		t.Fatalf("ready: %d %s", st, raw)
	}

	bad := newFixture(t, fixtureOpts{password: "wrong-password-0000"})
	st, m, raw := call(t, bad, http.MethodGet, "/health/ready", "", "")
	if st != http.StatusServiceUnavailable || m["reason"] != "CRM_AUTH_FAILED" {
		t.Fatalf("wrong password: %d %s", st, raw)
	}
	if strings.Contains(raw, "wrong-password-0000") || strings.Contains(raw, stubUser) {
		t.Fatalf("ready leaks credentials: %s", raw)
	}
	if strings.Contains(bad.built.Logs.String(), "wrong-password-0000") {
		t.Fatal("password in logs")
	}

	down := newFixture(t, fixtureOpts{})
	down.srv.Close()
	st, m, raw = call(t, down, http.MethodGet, "/health/ready", "", "")
	if st != http.StatusServiceUnavailable || m["reason"] != "CRM_UNREACHABLE" {
		t.Fatalf("stub down: %d %s", st, raw)
	}

	broken := newFixture(t, fixtureOpts{})
	broken.stub.FailNext(http.MethodPost, vivastub.TokenPath, http.StatusOK, `{"no":"token"}`)
	st, m, raw = call(t, broken, http.MethodGet, "/health/ready", "", "")
	if st != http.StatusServiceUnavailable || m["reason"] != "CRM_INVALID_RESPONSE" {
		t.Fatalf("broken login: %d %s", st, raw)
	}
}

func createBody(start time.Time, kind, note string) string {
	b, _ := json.Marshal(map[string]string{
		"court": "court-1", "start": start.Format(time.RFC3339), "end": start.Add(time.Hour).Format(time.RFC3339), "kind": kind, "note": note,
	})
	return string(b)
}

func TestCreatePayloads(t *testing.T) {
	f := newFixture(t, fixtureOpts{services: "services:\n  booking: { direction: 12, type: 3 }\n  tournament: { direction: 12, type: 7 }\n"})
	start := futureDay().Add(9 * time.Hour)
	st, m, raw := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, createBody(start, "game", "Ivan"))
	if st != http.StatusCreated {
		t.Fatalf("%d %s", st, raw)
	}
	id := m["id"].(string)
	if !strings.HasPrefix(id, "e-") {
		t.Fatalf("game with a service must be an exercise: %s", id)
	}
	rec := f.stub.Exercise(strings.TrimPrefix(id, "e-"))
	if rec["comment"] != "Ivan [cg:booker]" || rec["maxClientsCount"] != float64(1) || rec["timeFrom"] != start.UTC().Format("2006-01-02T15:04:05.000Z") {
		t.Fatalf("stored exercise: %v", rec)
	}
	if typ := rec["type"].(map[string]any)["id"]; typ != float64(3) {
		t.Fatalf("type = %v", typ)
	}
	st, m, _ = call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, createBody(start.Add(2*time.Hour), "tournament", ""))
	if st != http.StatusCreated {
		t.Fatal(st)
	}
	rec = f.stub.Exercise(strings.TrimPrefix(m["id"].(string), "e-"))
	if typ := rec["type"].(map[string]any)["id"]; typ != float64(7) || rec["comment"] != "[cg:booker]" {
		t.Fatalf("tournament exercise: %v", rec)
	}

	g := newFixture(t, fixtureOpts{extraCRM: "  break_title: \"Внешняя бронь\"\n"})
	st, m, raw = call(t, g, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, createBody(start, "game", ""))
	if st != http.StatusCreated || !strings.HasPrefix(m["id"].(string), "b-") {
		t.Fatalf("no service must create a break: %d %s", st, raw)
	}
	br := g.stub.Break(strings.TrimPrefix(m["id"].(string), "b-"))
	if br["title"] != "Внешняя бронь" || br["comment"] != "[cg:booker]" {
		t.Fatalf("stored break: %v", br)
	}
}

func TestCreateWithoutUsableIDIsAnError(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.stub.FailNext(http.MethodPost, "/breaks", http.StatusOK, `{"status":"ok"}`)
	st, m, raw := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, createBody(futureDay().Add(9*time.Hour), "game", ""))
	if st != http.StatusBadGateway || m["code"] != "UPSTREAM_INVALID" {
		t.Fatalf("%d %s", st, raw)
	}
	f.stub.FailNext(http.MethodPost, "/breaks", http.StatusConflict, `{}`)
	st, m, raw = call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, createBody(futureDay().Add(9*time.Hour), "game", ""))
	if st != http.StatusConflict || m["code"] != "SLOT_UNAVAILABLE" {
		t.Fatalf("%d %s", st, raw)
	}
}

func TestCancelOwnershipRules(t *testing.T) {
	d := futureDay()
	ctx := context.Background()
	f := newFixture(t, fixtureOpts{})
	seedEx := func(fields map[string]any) string {
		rec := map[string]any{
			"timeFrom": d.Add(10 * time.Hour).Format(time.RFC3339), "timeTo": d.Add(11 * time.Hour).Format(time.RFC3339),
			"room": map[string]any{"id": room1, "name": "R"}, "canceled": false,
		}
		for k, v := range fields {
			rec[k] = v
		}
		return f.stub.SeedExercise(rec)
	}
	seedBr := func(fields map[string]any) string {
		rec := map[string]any{
			"timeFrom": d.Add(10 * time.Hour).Format(time.RFC3339), "timeTo": d.Add(11 * time.Hour).Format(time.RFC3339),
			"room": map[string]any{"id": room1, "name": "R"}, "canceled": false,
		}
		for k, v := range fields {
			rec[k] = v
		}
		return f.stub.SeedBreak(rec)
	}

	cases := []struct {
		name    string
		id      string
		omitCmt bool
		omitTtl bool
		want    error
	}{
		{"club booking with client record", "e-" + seedEx(map[string]any{"masterServiceBooking": map[string]any{"client": map[string]any{}}, "comment": "x [cg:booker]"}), false, false, driver.ErrNotOwned},
		{"exercise with clients", "e-" + seedEx(map[string]any{"clientsCount": 2, "comment": "[cg:booker]"}), false, false, driver.ErrNotOwned},
		{"comment without marker", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": "club note"}), false, false, driver.ErrNotOwned},
		{"null comment", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": nil}), false, false, driver.ErrNotOwned},
		{"another partner's marker", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": "[cg:other]"}), false, false, driver.ErrNotOwned},
		{"room that is not a court", "e-" + f.stub.SeedExercise(map[string]any{"timeFrom": d.Format(time.RFC3339), "timeTo": d.Add(time.Hour).Format(time.RFC3339), "room": map[string]any{"id": roomOther}, "comment": "[cg:booker]"}), false, false, driver.ErrNotOwned},
		{"own exercise", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": "note [cg:booker]"}), false, false, nil},
		{"exercise read without comment: refused", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": "note [cg:booker]"}), true, false, driver.ErrNotOwned},
		{"own break", "b-" + seedBr(map[string]any{"title": viva.DefaultBreakTitle, "comment": "[cg:booker]"}), false, false, nil},
		{"club break, other comment", "b-" + seedBr(map[string]any{"title": viva.DefaultBreakTitle, "comment": "repairs"}), false, false, driver.ErrNotOwned},
		{"club break, no comment returned, foreign title", "b-" + seedBr(map[string]any{"title": "Repairs", "comment": "x"}), true, false, driver.ErrNotOwned},
		{"break, no comment returned, gateway title", "b-" + seedBr(map[string]any{"title": viva.DefaultBreakTitle, "comment": "x"}), true, false, nil},
		{"break, nothing returned: residual, allowed", "b-" + seedBr(map[string]any{"title": "Repairs", "comment": "x"}), true, true, nil},
		{"unknown id is already gone", "e-00000000-0000-4000-8000-00000000dead", false, false, nil},
		{"malformed id", "e-nope", false, false, driver.ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.stub.OmitCommentOnRead, f.stub.OmitTitleOnRead = c.omitCmt, c.omitTtl
			err := f.drv.Cancel(ctx, c.id, "booker")
			if (c.want == nil && err != nil) || (c.want != nil && !errors.Is(err, c.want)) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if _, uuid, ok := strings.Cut(c.id, "-"); ok && strings.HasPrefix(c.id, "e-") {
				rec := f.stub.Exercise(uuid)
				if rec != nil && (rec["canceled"] == true) != (c.want == nil) {
					t.Fatalf("canceled = %v, want %v", rec["canceled"], c.want == nil)
				}
			}
		})
	}
	f.stub.OmitCommentOnRead, f.stub.OmitTitleOnRead = false, false

	// A malformed read is refused, not treated as "not owned" or "gone".
	id := seedEx(map[string]any{"clientsCount": "many", "comment": "[cg:booker]"})
	var ue *driver.UpstreamError
	if err := f.drv.Cancel(ctx, "e-"+id, "booker"); !errors.As(err, &ue) || ue.Kind != driver.UpstreamInvalid {
		t.Fatalf("malformed clientsCount: %v", err)
	}
}

func TestGetBooking(t *testing.T) {
	d := futureDay()
	f := newFixture(t, fixtureOpts{})
	id := f.seedClubBooking(room2, d.Add(10*time.Hour), d.Add(11*time.Hour))
	st, m, raw := call(t, f, http.MethodGet, "/v1/clubs/test-club/bookings/e-"+id, contract.ReadKey, "")
	if st != 200 || m["court"] != "court-2" || m["status"] != "active" || len(m) != 5 {
		t.Fatalf("%d %s", st, raw)
	}
	if leak := containsAny(raw, privacyMarkers); leak != "" {
		t.Fatalf("leak %s", leak)
	}
	// a record in a room that is not a configured court is not exposed
	other := f.seedClubBooking(roomOther, d.Add(10*time.Hour), d.Add(11*time.Hour))
	if st, _, raw := call(t, f, http.MethodGet, "/v1/clubs/test-club/bookings/e-"+other, contract.ReadKey, ""); st != 404 {
		t.Fatalf("%d %s", st, raw)
	}
	// a break id is read from the breaks endpoint
	if st, _, _ := call(t, f, http.MethodGet, "/v1/clubs/test-club/bookings/b-"+id, contract.ReadKey, ""); st != 404 {
		t.Fatalf("exercise id read as a break: %d", st)
	}
}

func TestPricesEndpoint(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	// find a Saturday in the future
	d := futureDay()
	for d.Weekday() != time.Saturday {
		d = d.AddDate(0, 0, 1)
	}
	st, m, raw := call(t, f, http.MethodGet, "/v1/clubs/test-club/prices?court=court-1&date="+d.Format("2006-01-02"), contract.ReadKey, "")
	if st != 200 {
		t.Fatalf("%d %s", st, raw)
	}
	got, _ := json.Marshal(m["periods"])
	want := `[{"end":"12:00","price_per_hour":1500,"start":"07:00"},{"end":"16:00","price_per_hour":2000,"start":"12:00"},{"end":"24:00","price_per_hour":3000,"start":"16:00"}]`
	if string(got) != want {
		t.Fatalf("periods = %s\nwant      %s", got, want)
	}
	// a quote across the 16:00 boundary up to midnight, Moscow time
	msk, _ := time.LoadLocation("Europe/Moscow")
	s := time.Date(d.Year(), d.Month(), d.Day(), 15, 0, 0, 0, msk)
	e := time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, msk)
	st, m, raw = call(t, f, http.MethodGet, "/v1/clubs/test-club/quote?court=court-1&start="+q(s)+"&end="+q(e), contract.ReadKey, "")
	if st != 200 || m["total"] != float64(2000+8*3000) {
		t.Fatalf("quote: %d %s", st, raw)
	}
	// before 07:00 there is no price
	s = time.Date(d.Year(), d.Month(), d.Day(), 6, 0, 0, 0, msk)
	st, m, raw = call(t, f, http.MethodGet, "/v1/clubs/test-club/quote?court=court-1&start="+q(s)+"&end="+q(s.Add(2*time.Hour)), contract.ReadKey, "")
	if st != http.StatusUnprocessableEntity || m["code"] != "PRICE_UNAVAILABLE" {
		t.Fatalf("no price: %d %s", st, raw)
	}
	// a broken price list is an upstream error
	f.stub.SetPricing(priceList, `{"basePrices":[{"condition":{"type":"MOON"},"valueFrom":1}]}`)
	st, m, raw = call(t, f, http.MethodGet, "/v1/clubs/test-club/prices?court=court-1&date="+d.Format("2006-01-02"), contract.ReadKey, "")
	if st != http.StatusBadGateway || m["code"] != "UPSTREAM_INVALID" {
		t.Fatalf("broken price list: %d %s", st, raw)
	}
}

func TestDriverConfigValidation(t *testing.T) {
	base := func() driver.Config {
		return driver.Config{
			SlotMinutes: 30,
			Password:    "x",
			CRM:         driver.Params{"username": "u", "studio": studio},
			Courts:      []driver.CourtConfig{{ID: "court-1", Name: "C", Params: driver.Params{"viva_room": room1}}},
			Services:    map[string]driver.Params{},
		}
	}
	if _, err := viva.NewDriver(base()); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	cases := map[string]struct {
		mut  func(*driver.Config)
		want string
	}{
		"no username":       {func(c *driver.Config) { delete(c.CRM, "username") }, "crm.username"},
		"bad studio":        {func(c *driver.Config) { c.CRM["studio"] = "123" }, "crm.studio"},
		"unknown crm key":   {func(c *driver.Config) { c.CRM["passwrd"] = "x" }, "неизвестные параметры: passwrd"},
		"plain http remote": {func(c *driver.Config) { c.CRM["api_url"] = "http://crm.example.test/api" }, "https://"},
		"no password":       {func(c *driver.Config) { c.Password = "" }, "CRM_PASSWORD"},
		"no room":           {func(c *driver.Config) { c.Courts[0].Params = driver.Params{} }, "courts[0].viva_room"},
		"bad price list":    {func(c *driver.Config) { c.Courts[0].Params["price_list"] = "abc" }, "price_list"},
		"duplicate room": {func(c *driver.Config) {
			c.Courts = append(c.Courts, driver.CourtConfig{ID: "court-2", Name: "D", Params: driver.Params{"viva_room": room1}})
		}, "уже привязана"},
		"service w/o type": {func(c *driver.Config) { c.Services["booking"] = driver.Params{"direction": 1} }, "services.booking.type"},
		"service as text":  {func(c *driver.Config) { c.Services["booking"] = driver.Params{"direction": "12", "type": 3} }, "целое число"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			c.mut(&cfg)
			_, err := viva.NewDriver(cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want mention of %q", err, c.want)
			}
		})
	}
}

// Cancelled records are dropped even if the CRM returns them despite
// includeCanceled=false.
func TestCancelledItemsAreNotOccupancy(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := futureDay()
	item := `[{"id": "00000000-0000-4000-8000-0000000000e1", "timeFrom": "` + d.Add(10*time.Hour).Format(time.RFC3339) +
		`", "timeTo": "` + d.Add(11*time.Hour).Format(time.RFC3339) + `", "room": {"id": "` + room1 + `"}, "canceled": true}]`
	f.stub.FailNext(http.MethodGet, "/exercises", http.StatusOK, item)
	st, _, raw := call(t, f, http.MethodGet, occupancyPath(d, d.Add(24*time.Hour)), contract.ReadKey, "")
	if st != 200 || strings.Contains(raw, "0000000000e1") {
		t.Fatalf("cancelled item returned: %d %s", st, raw)
	}
}

// A Viva discount from 10:15 to 10:45 on the club's 30-minute grid is part
// of the quote and of /prices, not silently dropped.
func TestPriceImpactOffTheGrid(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.stub.SetPricing(priceList, `{"basePrices":[{"condition":{"type":"NONE"},"valueFrom":2000}],"priceImpacts":{"TIME":[`+
		`{"impactType":"FIXED","impactDirection":"DISCOUNT","value":500,"condition":{"type":"TIME","days":`+allDaysJSON+`,"timeStart":"10:15:00","timeEnd":"10:45:00"}}]}}`)
	day := futureDay().In(f.built.Settings.Location)
	from := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
	st, m, raw := call(t, f, http.MethodGet, "/v1/clubs/test-club/quote?court=court-1&start="+q(from)+"&end="+q(from.Add(time.Hour)), contract.ReadKey, "")
	if st != http.StatusOK || m["total"] != float64(1750) {
		t.Fatalf("quote: %d %s; want total 1750 (half an hour at 2000/h, half an hour at 1500/h)", st, raw)
	}
	st, m, raw = call(t, f, http.MethodGet, "/v1/clubs/test-club/prices?court=court-1&date="+from.Format("2006-01-02"), contract.ReadKey, "")
	got, _ := json.Marshal(m["periods"])
	want := `[{"end":"10:15","price_per_hour":2000,"start":"00:00"},{"end":"10:45","price_per_hour":1500,"start":"10:15"},{"end":"24:00","price_per_hour":2000,"start":"10:45"}]`
	if st != http.StatusOK || string(got) != want {
		t.Fatalf("prices: %d %s", st, raw)
	}
}

// Two partners book the same court and hour at the same moment, through
// two clubs (or two configs, as during a reload) that map a court to the
// same room. The occupancy check and the create are serialised per room:
// exactly one create reaches the CRM, the other partner gets 409.
func TestConcurrentCreatesOfOneRoom(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.AddRoom(room1, "Room one")
	stub.AddRoom(room2, "Room two")
	stub.SetPricing(priceList, pricingJSON)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/breaks") {
			time.Sleep(300 * time.Millisecond) // CRM latency on create
		}
		stub.ServeHTTP(w, r)
	}))
	defer srv.Close()
	env := contract.KeyEnv()
	env["CRM_PASSWORD"] = stubPass
	build := func() http.Handler {
		return contract.Build(t, configYAML(srv.URL, fixtureOpts{}), env, "viva", viva.New, func(c *driver.Config) { c.HTTPClient = srv.Client() }).Handler()
	}
	handlers := []http.Handler{build(), build()} // two drivers, one room
	d := futureDay().Add(10 * time.Hour)
	body := `{"court":"court-1","start":"` + d.Format(time.RFC3339) + `","end":"` + d.Add(time.Hour).Format(time.RFC3339) + `","kind":"game"}`
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, key := range []string{contract.BookKey, contract.OtherBookKey} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/clubs/test-club/bookings", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+key)
			rec := httptest.NewRecorder()
			handlers[i].ServeHTTP(rec, req)
			codes[i] = rec.Code
		}()
	}
	wg.Wait()
	posts := 0
	for _, c := range stub.Calls() {
		if strings.HasPrefix(c, "POST /breaks") {
			posts++
		}
	}
	if posts != 1 || codes[0]+codes[1] != http.StatusCreated+http.StatusConflict {
		t.Fatalf("creates sent to the CRM: %d, answers %v; want exactly one create, 201 and 409", posts, codes)
	}
}
