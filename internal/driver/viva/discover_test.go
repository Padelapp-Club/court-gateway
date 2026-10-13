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
