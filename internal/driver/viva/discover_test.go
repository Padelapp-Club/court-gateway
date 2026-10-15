package viva_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

func discover(t *testing.T, stub *vivastub.Stub) driver.Discovery {
	t.Helper()
	d, err := newAccount(t, stub, stubPass).Discover(context.Background(), driver.DiscoverQuery{Studio: studio})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func noticeCodes(d driver.Discovery) []string {
	var out []string
	for _, n := range d.Notices {
		out = append(out, n.Code)
	}
	return out
}

// Hours past midnight belong to the next day: the draft keeps them, as
// that day's opening, joined to its own hours when they meet; when they do
// not (a club has one span a day) the draft says so. Around the clock
// stays around the clock.
func TestDiscoveryKeepsHoursPastMidnight(t *testing.T) {
	for name, tc := range map[string]struct {
		work   map[string][2]string
		hours  map[string]string
		notice bool
	}{
		"into a closed day": {map[string][2]string{"MONDAY": {"20:00+03:00", "02:00+03:00"}},
			map[string]string{"mon": "20:00-24:00", "tue": "00:00-02:00", "wed-sun": "closed"}, false},
		"joined to the next day": {map[string][2]string{"MONDAY": {"20:00+03:00", "02:00+03:00"}, "TUESDAY": {"01:00+03:00", "23:00+03:00"}},
			map[string]string{"mon": "20:00-24:00", "tue": "00:00-23:00", "wed-sun": "closed"}, false},
		"sunday into monday": {map[string][2]string{"SUNDAY": {"18:00+03:00", "03:00+03:00"}},
			map[string]string{"mon": "00:00-03:00", "tue-sat": "closed", "sun": "18:00-24:00"}, false},
		"apart from the next day's hours": {map[string][2]string{"MONDAY": {"20:00+03:00", "02:00+03:00"}, "TUESDAY": {"08:00+03:00", "23:00+03:00"}},
			map[string]string{"mon": "20:00-24:00", "tue": "08:00-23:00", "wed-sun": "closed"}, true},
		"around the clock": {map[string][2]string{"SATURDAY": {"00:00+03:00", "00:00+03:00"}},
			map[string]string{"mon-fri": "closed", "sat": "00:00-24:00", "sun": "closed"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			stub := vivastub.New(stubUser, stubPass, studio)
			stub.WorkTime = tc.work
			d := discover(t, stub)
			draft, notices := core.DraftClub(d, "", 30)
			if !maps.Equal(draft.Hours, tc.hours) {
				t.Fatalf("hours %v, want %v", draft.Hours, tc.hours)
			}
			var codes []string
			for _, n := range notices {
				codes = append(codes, n.Code)
			}
			if slices.Contains(codes, "HOURS_UNRESOLVED") != tc.notice {
				t.Fatalf("notices %v", codes)
			}
		})
	}
}

func listCalls(stub *vivastub.Stub) int {
	n := 0
	for _, c := range stub.Calls() {
		if strings.HasPrefix(c, "GET /pricing?") {
			n++
		}
	}
	return n
}

// Discovery reads every page of the price lists, and says so when it
// stops before the last one.
func TestDiscoveryReadsEveryPriceListPage(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	for i := 1; i <= 201; i++ {
		stub.SetPricing(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), `{"basePrices":[{"condition":{"type":"NONE"},"valueFrom":1000}]}`)
	}
	d := discover(t, stub)
	if len(d.PriceLists) != 201 || listCalls(stub) != 2 || slices.Contains(noticeCodes(d), "PRICE_LISTS_INCOMPLETE") {
		t.Fatalf("%d price lists in %d pages, notices %v", len(d.PriceLists), listCalls(stub), noticeCodes(d))
	}
	// each list read costs a request: only the first 50 are read, and the
	// draft says so
	reads := 0
	for _, c := range stub.Calls() {
		if strings.HasPrefix(c, "GET /pricing/") {
			reads++
		}
	}
	if reads != 50 || !slices.Contains(noticeCodes(d), "PRICE_LISTS_PARTLY_READ") {
		t.Fatalf("%d price lists read, notices %v", reads, noticeCodes(d))
	}

	// a CRM that always has another page: discovery stops and says so
	stub = vivastub.New(stubUser, stubPass, studio)
	page, _ := json.Marshal(map[string]any{"content": []map[string]string{{"id": "not-a-list", "name": "x"}}, "last": false})
	for range 10 {
		stub.FailNext("GET", "/pricing", 200, string(page))
	}
	d = discover(t, stub)
	if n := listCalls(stub); n < 2 || n > 10 || !slices.Contains(noticeCodes(d), "PRICE_LISTS_INCOMPLETE") {
		t.Fatalf("endless pages: %d requests, notices %v", n, noticeCodes(d))
	}
}

// Viva answers GET /studios with a page of studios, like GET /pricing: an
// account with more studios than a page holds is read page by page.
func TestDiscoveryReadsEveryStudioPage(t *testing.T) {
	stub := vivastub.New(stubUser, stubPass, studio)
	for i := 1; i <= 120; i++ {
		stub.AddStudio(fmt.Sprintf("00000000-0000-4000-8000-1%011d", i), fmt.Sprintf("Студия %d", i), nil)
	}
	d, err := newAccount(t, stub, stubPass).Discover(context.Background(), driver.DiscoverQuery{})
	if err != nil {
		t.Fatal(err)
	}
	pages := 0
	for _, c := range stub.Calls() {
		if strings.HasPrefix(c, "GET /studios?") {
			pages++
		}
	}
	if len(d.Studios) != 121 || pages != 2 {
		t.Fatalf("%d studios in %d pages", len(d.Studios), pages)
	}
}

// A studio list the gateway cannot read to the end is an error, not a
// shorter list: a studio left out would read as one the account lacks.
func TestStudioListsThatCannotBeReadToTheEnd(t *testing.T) {
	st := func(id string) map[string]any { return map[string]any{"id": id, "name": "Студия"} }
	page := func(content []map[string]any, extra map[string]any) string {
		m := map[string]any{"content": content}
		maps.Copy(m, extra)
		b, _ := json.Marshal(m)
		return string(b)
	}
	cases := map[string][]string{
		"always another page": slices.Repeat([]string{page([]map[string]any{st(studio)}, map[string]any{"last": false})}, 12),
		"empty page before the last": {
			page([]map[string]any{st(studio)}, map[string]any{"last": false}),
			page([]map[string]any{}, map[string]any{"last": false}),
		},
		"no page marks":             {page([]map[string]any{st(studio)}, nil)},
		"null content":              {`{"content": null, "last": true}`},
		"neither a page nor a list": {`{"studios": []}`},
	}
	for name, answers := range cases {
		t.Run(name, func(t *testing.T) {
			stub := vivastub.New(stubUser, stubPass, studio)
			for _, a := range answers {
				stub.FailNext("GET", "/studios", 200, a)
			}
			_, err := newAccount(t, stub, stubPass).Discover(context.Background(), driver.DiscoverQuery{})
			if !strings.Contains(fmt.Sprint(err), "GET /studios") {
				t.Fatalf("got %v", err)
			}
		})
	}

	// a plain array of every studio is read as the whole list
	stub := vivastub.New(stubUser, stubPass, studio)
	arr, _ := json.Marshal([]map[string]any{st(studio), st(studio2)})
	stub.FailNext("GET", "/studios", 200, string(arr))
	d, err := newAccount(t, stub, stubPass).Discover(context.Background(), driver.DiscoverQuery{})
	if err != nil || len(d.Studios) != 2 {
		t.Fatalf("array: %+v, %v", d.Studios, err)
	}

	// a studio repeated on the next page is listed once
	stub = vivastub.New(stubUser, stubPass, studio)
	stub.FailNext("GET", "/studios", 200, page([]map[string]any{st(studio)}, map[string]any{"last": false}))
	stub.FailNext("GET", "/studios", 200, page([]map[string]any{st(strings.ToUpper(studio)), st(studio2)}, map[string]any{"last": true}))
	d, err = newAccount(t, stub, stubPass).Discover(context.Background(), driver.DiscoverQuery{})
	if err != nil || len(d.Studios) != 2 {
		t.Fatalf("repeated: %+v, %v", d.Studios, err)
	}
}
