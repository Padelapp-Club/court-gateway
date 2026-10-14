package viva_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

func call(t *testing.T, f *fixture, method, path, key string, body string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
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

// futureMidnight is local (studio) midnight three days from now.
func futureMidnight() time.Time {
	msk, _ := time.LoadLocation("Europe/Moscow")
	n := time.Now().In(msk).AddDate(0, 0, 3)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, msk)
}

func TestOccupancyListsLocalDaysAndCutsOverlap(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	d := futureMidnight()
	// starts on the previous local day and runs past midnight
	crossing := f.seedClubBooking(room1, d.Add(-time.Hour), d.Add(time.Hour))
	// inside the margin day but not overlapping the range
	early := f.seedClubBooking(room1, d.Add(-5*time.Hour), d.Add(-4*time.Hour))
	// next day, not overlapping
	late := f.seedClubBooking(room1, d.Add(26*time.Hour), d.Add(27*time.Hour))
	// a room that is not a configured court
	elsewhere := f.seedClubBooking(roomOther, d.Add(time.Hour), d.Add(2*time.Hour))

	status, body, raw := call(t, f, http.MethodGet, occupancyPath(d.Add(30*time.Minute), d.Add(3*time.Hour)), contract.ReadKey, "")
	if status != http.StatusOK || body["complete"] != true {
		t.Fatalf("status %d: %s", status, raw)
	}
	if !strings.Contains(raw, "e-"+crossing) {
		t.Fatalf("booking crossing midnight missing: %s", raw)
	}
	for _, id := range []string{early, late, elsewhere} {
		if strings.Contains(raw, id) {
			t.Fatalf("non-overlapping or foreign-room booking %s returned: %s", id, raw)
		}
	}
	// exercises from the local day before, breaks with a week of look-back,
	// to the day after the range's last
	want := map[string]string{"GET /exercises?": d.AddDate(0, 0, -1).Format("2006-01-02"), "GET /breaks?": d.AddDate(0, 0, -viva.DefaultBreakLookbackDays).Format("2006-01-02")}
	for prefix, from := range want {
		found := false
		for _, c := range f.stub.Calls() {
			if strings.HasPrefix(c, prefix) && strings.Contains(c, "dateFrom="+from) && strings.Contains(c, "dateTo="+d.AddDate(0, 0, 1).Format("2006-01-02")) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no %s call with dateFrom=%s: %v", prefix, from, f.stub.Calls())
		}
	}
}

// A booking that started before UTC midnight is on the same local day in
// Moscow: seen after UTC midnight, it blocks the create.
func TestBookingAcrossUTCMidnightIsSeen(t *testing.T) {
	day := futureDay() // UTC midnight = 03:00 in Moscow
	clock := func() time.Time { return day.Add(10 * time.Minute) }
	f := newFixture(t, fixtureOpts{now: clock})
	id := f.seedClubBooking(room1, day.Add(-time.Hour), day.Add(2*time.Hour)) // 02:00-05:00 local
	from, to := day.Add(90*time.Minute), day.Add(150*time.Minute)             // 04:30-05:30 local
	st, m, raw := call(t, f, http.MethodGet, occupancyPath(from, to), contract.ReadKey, "")
	if st != http.StatusOK || !strings.Contains(raw, "e-"+id) || m["complete"] != true {
		t.Fatalf("occupancy: %d %s", st, raw)
	}
	body := `{"court":"court-1","start":"` + from.Format(time.RFC3339) + `","end":"` + to.Format(time.RFC3339) + `","kind":"game"}`
	if st, m, raw := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, body); st != http.StatusConflict || m["code"] != "SLOT_UNAVAILABLE" {
		t.Fatalf("create over the booking: %d %s", st, raw)
	}
}

// After local midnight the exercises of yesterday cannot be listed, so a
// range in the first overnight_minutes of today is not reported complete
// and is not booked, however empty the answer is.
func TestEarlyMorningIsIncomplete(t *testing.T) {
	d := futureMidnight()
	clock := func() time.Time { return d.Add(10 * time.Minute) } // 00:10 local
	f := newFixture(t, fixtureOpts{now: clock})
	id := f.seedClubBooking(room1, d.Add(-time.Hour), d.Add(2*time.Hour)) // 23:00 yesterday to 02:00
	from, to := d.Add(30*time.Minute), d.Add(90*time.Minute)
	st, m, raw := call(t, f, http.MethodGet, occupancyPath(from, to), contract.ReadKey, "")
	if st != http.StatusOK || m["complete"] != false || strings.Contains(raw, id) {
		t.Fatalf("occupancy: %d %s", st, raw)
	}
	body := `{"court":"court-1","start":"` + from.Format(time.RFC3339) + `","end":"` + to.Format(time.RFC3339) + `","kind":"game"}`
	if st, m, raw := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, body); st != http.StatusConflict || m["code"] != "OCCUPANCY_UNKNOWN" {
		t.Fatalf("create: %d %s", st, raw)
	}
	// from midnight plus overnight_minutes (default 240) on, it is complete
	late := d.Add(4 * time.Hour)
	if _, m, raw := call(t, f, http.MethodGet, occupancyPath(late, late.Add(time.Hour)), contract.ReadKey, ""); m["complete"] != true {
		t.Fatalf("after the overnight window: %s", raw)
	}
	// and the window is configurable per club
	g := newFixture(t, fixtureOpts{now: clock, clubCRM: map[string]any{"overnight_minutes": 0}})
	if _, m, raw := call(t, g, http.MethodGet, occupancyPath(from, to), contract.ReadKey, ""); m["complete"] != true {
		t.Fatalf("overnight_minutes 0: %s", raw)
	}
}

// A maintenance break that started days before the asked range and is
// still running is seen, so the create is refused: by default up to
// break_lookback_days (31) before the range.
func TestMultiDayBreakIsSeen(t *testing.T) {
	d := futureMidnight()
	for name, start := range map[string]time.Time{"started two days earlier": d.Add(-48 * time.Hour), "started yesterday before midnight": d.Add(-time.Hour),
		"started thirty days earlier": d.AddDate(0, 0, -30)} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			id := f.stub.SeedBreak(map[string]any{"room": map[string]any{"id": room1}, "timeFrom": start.Format(time.RFC3339),
				"timeTo": d.Add(12 * time.Hour).Format(time.RFC3339), "comment": "maintenance", "canceled": false})
			from, to := d.Add(9*time.Hour), d.Add(10*time.Hour)
			st, m, raw := call(t, f, http.MethodGet, occupancyPath(from, to), contract.ReadKey, "")
			if st != http.StatusOK || m["complete"] != true || !strings.Contains(raw, "b-"+id) {
				t.Fatalf("occupancy: %d %s", st, raw)
			}
			body := `{"court":"court-1","start":"` + from.Format(time.RFC3339) + `","end":"` + to.Format(time.RFC3339) + `","kind":"game"}`
			if st, _, raw := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, body); st != http.StatusConflict {
				t.Fatalf("create over the break: %d %s", st, raw)
			}
		})
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
		"object instead of array":  `{"content": []}`,
		"null":                     `null`,
		"html":                     `<html>maintenance</html>`,
		"item with zone-less time": `[{"id": "00000000-0000-4000-8000-000000000001", "timeFrom": "2030-01-01T10:00:00", "timeTo": "2030-01-01T11:00:00", "room": {"id": "` + room1 + `"}}]`,
		"item with bad time":       `[{"id": "00000000-0000-4000-8000-000000000001", "timeFrom": "soon", "timeTo": "later", "room": {"id": "` + room1 + `"}}]`,
		"item without id":          `[{"timeFrom": "2030-01-01T10:00:00Z", "timeTo": "2030-01-01T11:00:00Z", "room": {"id": "` + room1 + `"}}]`,
		"item with wrong types":    `[{"id": 5, "timeFrom": 1, "timeTo": 2, "room": "x"}]`,
		"end before start":         `[{"id": "00000000-0000-4000-8000-000000000001", "timeFrom": "2030-01-01T11:00:00Z", "timeTo": "2030-01-01T10:00:00Z", "room": {"id": "` + room1 + `"}}]`,
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
	ready := func(f *fixture) (string, string) {
		err := f.built.Clubs.Ready(context.Background())
		if err == nil {
			return "", ""
		}
		return core.ReadyReason(err), err.Error()
	}
	if reason, _ := ready(newFixture(t, fixtureOpts{})); reason != "" {
		t.Fatalf("ready: %s", reason)
	}

	bad := newFixture(t, fixtureOpts{password: "wrong-password-0000"})
	reason, text := ready(bad)
	if reason != "CRM_AUTH_FAILED" {
		t.Fatalf("wrong password: %s", reason)
	}
	if strings.Contains(text, "wrong-password-0000") || strings.Contains(text, stubUser) {
		t.Fatalf("ready leaks credentials: %s", text)
	}
	if strings.Contains(bad.built.Logs.String(), "wrong-password-0000") {
		t.Fatal("password in logs")
	}

	down := newFixture(t, fixtureOpts{})
	down.srv.Close()
	if reason, _ := ready(down); reason != "CRM_UNREACHABLE" {
		t.Fatalf("stub down: %s", reason)
	}

	broken := newFixture(t, fixtureOpts{})
	broken.stub.FailNext(http.MethodPost, vivastub.TokenPath, http.StatusOK, `{"no":"token"}`)
	if reason, _ := ready(broken); reason != "CRM_INVALID_RESPONSE" {
		t.Fatalf("broken login: %s", reason)
	}
}

func createBody(start time.Time, kind, note string) string {
	b, _ := json.Marshal(map[string]string{
		"court": "court-1", "start": start.Format(time.RFC3339), "end": start.Add(time.Hour).Format(time.RFC3339), "kind": kind, "note": note,
	})
	return string(b)
}

func TestCreatePayloads(t *testing.T) {
	f := newFixture(t, fixtureOpts{services: map[string]map[string]any{"booking": {"direction": 12, "type": 3}, "tournament": {"direction": 12, "type": 7}}})
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
	if rec["comment"] != "[cg:booker] Ivan" || rec["maxClientsCount"] != float64(1) || rec["timeFrom"] != start.UTC().Format("2006-01-02T15:04:05.000Z") {
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

	g := newFixture(t, fixtureOpts{installCRM: ", break_title: \"Внешняя бронь\""})
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
		want    error
	}{
		{"club booking with client record", "e-" + seedEx(map[string]any{"masterServiceBooking": map[string]any{"client": map[string]any{}}, "comment": "[cg:booker] x"}), false, driver.ErrNotOwned},
		{"exercise with clients", "e-" + seedEx(map[string]any{"clientsCount": 2, "comment": "[cg:booker]"}), false, driver.ErrNotOwned},
		{"comment without marker", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": "club note"}), false, driver.ErrNotOwned},
		{"null comment", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": nil}), false, driver.ErrNotOwned},
		{"another partner's marker", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": "[cg:other]"}), false, driver.ErrNotOwned},
		{"room that is not a court", "e-" + f.stub.SeedExercise(map[string]any{"timeFrom": d.Format(time.RFC3339), "timeTo": d.Add(time.Hour).Format(time.RFC3339), "room": map[string]any{"id": roomOther}, "studio": map[string]any{"id": studio}, "comment": "[cg:booker]"}), false, driver.ErrCourtNotConfigured},
		{"another studio's record", "e-" + f.stub.SeedExercise(map[string]any{"timeFrom": d.Format(time.RFC3339), "timeTo": d.Add(time.Hour).Format(time.RFC3339), "room": map[string]any{"id": roomOther}, "studio": map[string]any{"id": otherStudio}, "comment": "[cg:booker]"}), false, driver.ErrNotFound},
		{"own exercise", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": "[cg:booker] note"}), false, nil},
		{"own exercise, empty client fields", "e-" + seedEx(map[string]any{"clientsCount": 0, "clients": []any{}, "guests": nil, "participants": map[string]any{}, "bookings": "", "comment": "[cg:booker]"}), false, nil},
		{"clients listed, no clientsCount", "e-" + seedEx(map[string]any{"clients": []any{map[string]any{"id": "fixture-client"}}, "comment": "[cg:booker]"}), false, driver.ErrNotOwned},
		{"clients listed, clientsCount 0", "e-" + seedEx(map[string]any{"clientsCount": 0, "clients": []any{map[string]any{"id": "fixture-client"}}, "comment": "[cg:booker]"}), false, driver.ErrNotOwned},
		{"guests listed", "e-" + seedEx(map[string]any{"clientsCount": 0, "guests": []any{"fixture-guest"}, "comment": "[cg:booker]"}), false, driver.ErrNotOwned},
		{"participants listed", "e-" + seedEx(map[string]any{"clientsCount": 0, "participants": []any{map[string]any{}}, "comment": "[cg:booker]"}), false, driver.ErrNotOwned},
		{"bookings listed", "e-" + seedEx(map[string]any{"clientsCount": 0, "bookings": map[string]any{"id": "fixture-booking"}, "comment": "[cg:booker]"}), false, driver.ErrNotOwned},
		{"negative clientsCount", "e-" + seedEx(map[string]any{"clientsCount": -1, "comment": "[cg:booker]"}), false, driver.ErrNotOwned},
		{"exercise read without comment: refused", "e-" + seedEx(map[string]any{"clientsCount": 0, "comment": "[cg:booker] note"}), true, driver.ErrNotOwned},
		{"own break", "b-" + seedBr(map[string]any{"title": viva.DefaultBreakTitle, "comment": "[cg:booker]"}), false, nil},
		{"club break, other comment", "b-" + seedBr(map[string]any{"title": viva.DefaultBreakTitle, "comment": "repairs"}), false, driver.ErrNotOwned},
		{"another partner's break with the gateway title", "b-" + seedBr(map[string]any{"title": viva.DefaultBreakTitle, "comment": "[cg:other]"}), false, driver.ErrNotOwned},
		{"break read without a comment: refused even with the gateway title", "b-" + seedBr(map[string]any{"title": viva.DefaultBreakTitle, "comment": "[cg:booker]"}), true, driver.ErrNotOwned},
		{"break read without a comment and without a title: refused", "b-" + seedBr(map[string]any{"comment": "[cg:booker]"}), true, driver.ErrNotOwned},
		{"title with the caller's marker is not evidence", "b-" + seedBr(map[string]any{"title": "[cg:booker]", "comment": "repairs"}), false, driver.ErrNotOwned},
		{"unknown id is already gone", "e-00000000-0000-4000-8000-00000000dead", false, nil},
		{"own break under the exercise prefix is not found", "e-" + seedBr(map[string]any{"title": viva.DefaultBreakTitle, "comment": "[cg:booker]"}), false, driver.ErrNotFound},
		{"malformed id", "e-nope", false, driver.ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.stub.OmitCommentOnRead = c.omitCmt
			err := f.drv.Cancel(ctx, c.id, "booker")
			if (c.want == nil && err != nil) || (c.want != nil && !errors.Is(err, c.want)) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			_, uuid, _ := strings.Cut(c.id, "-")
			rec := f.stub.Exercise(uuid)
			if rec == nil {
				rec = f.stub.Break(uuid)
			}
			if rec != nil && (rec["canceled"] == true) != (c.want == nil) {
				t.Fatalf("canceled = %v, want %v", rec["canceled"], c.want == nil)
			}
		})
	}
	f.stub.OmitCommentOnRead = false

	// A malformed read, or one without clientsCount, is refused, not
	// treated as "not owned" or "gone": without it the exercise may have
	// clients.
	for name, fields := range map[string]map[string]any{
		"malformed clientsCount": {"clientsCount": "many", "comment": "[cg:booker]"},
		"no clientsCount":        {"comment": "[cg:booker]"},
		"null clientsCount":      {"clientsCount": nil, "comment": "[cg:booker]"},
	} {
		id := seedEx(fields)
		var ue *driver.UpstreamError
		if err := f.drv.Cancel(ctx, "e-"+id, "booker"); !errors.As(err, &ue) || ue.Kind != driver.UpstreamInvalid {
			t.Fatalf("%s: %v", name, err)
		}
		if f.stub.Exercise(id)["canceled"] == true {
			t.Fatalf("%s: cancelled", name)
		}
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
	// a record in a room that is not (or no longer) a configured court is
	// not exposed: 409 COURT_NOT_CONFIGURED, for reading and cancelling;
	// another studio's (club's) record is no booking of this club: 404
	other := f.seedClubBooking(roomOther, d.Add(10*time.Hour), d.Add(11*time.Hour))
	elsewhere := f.stub.SeedExercise(map[string]any{"timeFrom": d.Add(10 * time.Hour).Format(time.RFC3339), "timeTo": d.Add(11 * time.Hour).Format(time.RFC3339),
		"room": map[string]any{"id": roomOther}, "studio": map[string]any{"id": otherStudio}, "comment": "[cg:booker]"})
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if st, m, raw := call(t, f, method, "/v1/clubs/test-club/bookings/e-"+other, contract.BookKey, ""); st != 409 || m["code"] != "COURT_NOT_CONFIGURED" || strings.Contains(raw, roomOther) {
			t.Fatalf("%s: %d %s", method, st, raw)
		}
		if st, m, raw := call(t, f, method, "/v1/clubs/test-club/bookings/e-"+elsewhere, contract.BookKey, ""); st != 404 || m["code"] != "NOT_FOUND" {
			t.Fatalf("%s, another club's booking: %d %s", method, st, raw)
		}
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
	if got := periodLabels(m); got != "07:00-12:00 1500, 12:00-16:00 2000, 16:00-24:00 3000" {
		t.Fatalf("periods = %s", got)
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
			Password: "x",
			CRM:      driver.Params{"username": "u", "studio": studio},
			Courts:   []driver.CourtConfig{{ID: "court-1", Name: "C", Params: driver.Params{"viva_room": room1}}},
			Services: map[string]driver.Params{},
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
	if st != http.StatusOK || periodLabels(m) != "00:00-10:15 2000, 10:15-10:45 1500, 10:45-24:00 2000" {
		t.Fatalf("prices: %d %s", st, raw)
	}
}

// periodLabels writes the periods of a /prices answer in the club's clock.
func periodLabels(m map[string]any) string {
	var out []string
	for _, p := range m["periods"].([]any) {
		p := p.(map[string]any)
		out = append(out, fmt.Sprint(p["start"], "-", p["end"], " ", p["price_per_hour"]))
	}
	return strings.Join(out, ", ")
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
	env := map[string]string{"CRM_PASSWORD": stubPass}
	build := func() http.Handler {
		return contract.Build(t, crmYAML(srv.URL, ""), testClub(fixtureOpts{}), env, "viva", viva.New, func(c *driver.Config) { c.HTTPClient = srv.Client() }).Handler()
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

// A quote across a midnight the clocks skip, end to end through the Viva
// driver and the API, finishes and charges the one hour that passes.
func TestQuoteAcrossSkippedMidnight(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := newDriver(t, loc)
	cfg := &core.Settings{ClubID: "c", Location: loc, SlotMinutes: 30, Courts: []core.Court{{ID: "court-1", Name: "Court"}}}
	api := core.NewServer(cfg, d, nil).APIHandler()
	req := httptest.NewRequest("GET", "/v1/clubs/c/quote?court=court-1&start=2030-09-07T23:30:00-04:00&end=2030-09-08T01:30:00-03:00", nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { rec := httptest.NewRecorder(); api.ServeHTTP(rec, req); done <- rec }()
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":2000`) {
			t.Fatalf("status %d, body %s; want 200 with total 2000", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quote did not finish")
	}
}

// A break without a room closes the studio: it occupies every court
// asked about, also when one court is asked (as every create does), so a
// booking during it is refused; read by its id it is on the first court. The break is seeded, not injected: the
// stub's list filters by roomId like the CRM may, and the driver asks for
// that filter on no list: rooms are filtered locally.
func TestRoomlessBreakBlocksEveryCourt(t *testing.T) {
	d := futureMidnight()
	f := newFixture(t, fixtureOpts{})
	from, to := d.Add(12*time.Hour), d.Add(13*time.Hour)
	roomless := func(studioID string) string {
		return f.stub.SeedBreak(map[string]any{"timeFrom": from.UTC().Format(time.RFC3339), "timeTo": to.UTC().Format(time.RFC3339), "room": nil,
			"studio": map[string]any{"id": studioID}, "canceled": false})
	}
	if status, m, raw := call(t, f, http.MethodGet, "/v1/clubs/test-club/bookings/b-"+roomless(studio), contract.ReadKey, ""); status != http.StatusOK || m["court"] != "court-1" {
		t.Fatalf("a room-less break read by its id: %d %s, want it on the first court", status, raw)
	}
	// another studio's (of the same account) is not this club's
	if status, _, raw := call(t, f, http.MethodGet, "/v1/clubs/test-club/bookings/b-"+roomless("00000000-0000-4000-8000-0000000000f7"), contract.ReadKey, ""); status != http.StatusNotFound {
		t.Fatalf("another studio's room-less break read by its id: %d %s, want 404", status, raw)
	}
	for _, path := range []string{occupancyPath(from, to), occupancyPath(from, to) + "&court=court-1"} {
		status, m, raw := call(t, f, http.MethodGet, path, contract.ReadKey, "")
		if status != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, status, raw)
		}
		courts := map[string]bool{}
		for _, it := range m["intervals"].([]any) {
			courts[it.(map[string]any)["court"].(string)] = true
		}
		if !courts["court-1"] || (!strings.Contains(path, "court=") && !courts["court-2"]) {
			t.Fatalf("%s: a room-less break did not block every court asked: %s", path, raw)
		}
	}
	if status, m, raw := call(t, f, http.MethodPost, "/v1/clubs/test-club/bookings", contract.BookKey, createBody(from, "game", "")); status != http.StatusConflict || m["code"] != "SLOT_UNAVAILABLE" {
		t.Fatalf("booking during a studio-wide break: %d %s, want 409 SLOT_UNAVAILABLE", status, raw)
	}
	for _, c := range f.stub.Calls() {
		if strings.HasPrefix(c, "GET /") && strings.Contains(c, "roomId") {
			t.Fatalf("listed with a room filter: %s", c)
		}
	}
	// and an exercise without a room is still invalid
	f.stub.FailNext(http.MethodGet, "/exercises", http.StatusOK, `[{"id": "00000000-0000-4000-8000-0000000000e1", "timeFrom": "`+from.UTC().Format(time.RFC3339)+`", "timeTo": "`+to.UTC().Format(time.RFC3339)+`"}]`)
	if st, m, raw := call(t, f, http.MethodGet, occupancyPath(d, d.Add(24*time.Hour)), contract.ReadKey, ""); st != http.StatusBadGateway || m["code"] != "UPSTREAM_INVALID" {
		t.Fatalf("room-less exercise: %d %s", st, raw)
	}
}

// Viva's lists are taken to be whole. A list exactly as long as a page a
// paging CRM would cut it to may be a first page: it is asked again day by
// day, and the occupancy is complete only when no day's list is as long.
func TestPageSizedListIsAskedAgainByDay(t *testing.T) {
	d := futureMidnight()
	from, to := d.Add(12*time.Hour), d.Add(13*time.Hour)
	list := func(n int) string {
		items := make([]string, n)
		for i := range items {
			start := d.Add(time.Duration(i) * time.Minute) // all before the interval asked
			items[i] = fmt.Sprintf(`{"id": "00000000-0000-4000-8000-%012d", "room": {"id": %q}, "timeFrom": %q, "timeTo": %q}`,
				i, room1, start.UTC().Format(time.RFC3339), start.Add(time.Minute).UTC().Format(time.RFC3339))
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	for _, c := range []struct {
		answers  []int // the range's list, then the days' (the rest empty)
		complete bool
	}{{[]int{99}, true}, {[]int{101}, true}, {[]int{100}, true}, {[]int{100, 100}, false}} {
		f := newFixture(t, fixtureOpts{})
		for _, n := range c.answers {
			f.stub.FailNext(http.MethodGet, "/exercises", http.StatusOK, list(n))
		}
		if st, m, raw := call(t, f, http.MethodGet, occupancyPath(from, to)+"&court=court-1", contract.ReadKey, ""); st != http.StatusOK || m["complete"] != c.complete {
			t.Fatalf("lists of %v: %d %.200s, want complete %v", c.answers, st, raw, c.complete)
		}
	}
}

// Readiness validates the items it reads as occupancy does: an answer
// occupancy refuses is not ready.
func TestReadinessRefusesUnusableItems(t *testing.T) {
	d, stub := newDriver(t, time.FixedZone("UTC+3", 3*3600))
	if err := d.Ready(context.Background()); err != nil {
		t.Fatalf("clean account: %v", err)
	}
	for _, kind := range []string{"/exercises", "/breaks"} {
		stub.FailNext(http.MethodGet, kind, http.StatusOK, `[{}]`)
		var ue *driver.UpstreamError
		if err := d.Ready(context.Background()); !errors.As(err, &ue) || ue.Kind != driver.UpstreamInvalid {
			t.Fatalf("%s answering [{}]: readiness error %v, want UpstreamInvalid", kind, err)
		}
	}
}

// newDriver builds the driver of a one-court club (room1, a flat price
// list) on a fresh stub whose studio is in loc.
func newDriver(t *testing.T, loc *time.Location) (*viva.Driver, *vivastub.Stub) {
	t.Helper()
	stub := vivastub.New(stubUser, stubPass, studio)
	stub.Zone = loc
	stub.AddRoom(room1, "Court")
	stub.SetPricing(priceList, `{"basePrices":[{"condition":{"type":"NONE"},"valueFrom":2000}]}`)
	hs := httptest.NewServer(stub)
	t.Cleanup(hs.Close)
	d, err := viva.NewDriver(driver.Config{CRM: driver.Params{"username": stubUser, "studio": studio, "auth_url": hs.URL + "/token", "api_url": hs.URL + "/api/v1"},
		Password: stubPass, Timezone: loc, Courts: []driver.CourtConfig{{ID: "court-1", Params: driver.Params{"viva_room": room1, "price_list": priceList}}}})
	if err != nil {
		t.Fatal(err)
	}
	return d, stub
}
