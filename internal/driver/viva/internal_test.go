package viva

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestWindow(t *testing.T) {
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{loc: msk, overnight: 4 * time.Hour, lookback: 7}
	cases := []struct {
		name, kind, now, from, to string
		wantFrom, wantTo          string
		complete                  bool
	}{
		{"exercises: the day before a future range", "exercises", "2030-03-10T09:00:00+03:00", "2030-03-12T09:00:00+03:00", "2030-03-12T18:00:00+03:00", "2030-03-11", "2030-03-13", true},
		{"exercises: the day before, past or not (the CRM leaves past days out)", "exercises", "2030-03-10T09:00:00+03:00", "2030-03-10T13:00:00+03:00", "2030-03-10T15:00:00+03:00", "2030-03-09", "2030-03-11", true},
		{"exercises: local, not UTC dates", "exercises", "2030-03-10T09:00:00+03:00", "2030-03-11T00:30:00+03:00", "2030-03-11T01:30:00+03:00", "2030-03-10", "2030-03-12", true},
		{"exercises: today before midnight plus overnight is incomplete", "exercises", "2030-03-10T09:00:00+03:00", "2030-03-10T03:30:00+03:00", "2030-03-10T05:00:00+03:00", "2030-03-09", "2030-03-11", false},
		{"exercises: today after midnight plus overnight is complete", "exercises", "2030-03-10T09:00:00+03:00", "2030-03-10T04:00:00+03:00", "2030-03-10T05:00:00+03:00", "2030-03-09", "2030-03-11", true},
		{"exercises: a past range is incomplete", "exercises", "2030-03-10T09:00:00+03:00", "2030-03-01T10:00:00+03:00", "2030-03-01T11:00:00+03:00", "2030-02-28", "2030-03-02", false},
		// a CRM clock a little ahead is already past midnight: yesterday's
		// exercise running into tomorrow's early hours cannot be listed
		{"exercises: our clock just before midnight", "exercises", "2030-03-09T23:57:00+03:00", "2030-03-10T01:00:00+03:00", "2030-03-10T02:00:00+03:00", "2030-03-09", "2030-03-11", false},
		{"breaks: the look-back, past days too", "breaks", "2030-03-10T09:00:00+03:00", "2030-03-10T03:30:00+03:00", "2030-03-10T05:00:00+03:00", "2030-03-03", "2030-03-11", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, to, complete := d.window(c.kind, ts(c.from), ts(c.to), ts(c.now))
			if f.Format("2006-01-02") != c.wantFrom || to.Format("2006-01-02") != c.wantTo || complete != c.complete {
				t.Fatalf("got %s..%s complete=%v, want %s..%s complete=%v",
					f.Format("2006-01-02"), to.Format("2006-01-02"), complete, c.wantFrom, c.wantTo, c.complete)
			}
		})
	}
}

func TestParseTime(t *testing.T) {
	for _, s := range []string{"2030-03-10T12:00:00Z", "2030-03-10T15:00:00+03:00", "2030-03-10T12:00:00.000Z", "2030-03-10T12:00:00"} {
		got, ok := parseTime(s)
		if !ok || !got.Equal(ts("2030-03-10T12:00:00Z")) {
			t.Fatalf("parseTime(%q) = %v, %v", s, got, ok)
		}
	}
	if _, ok := parseTime("10.03.2030 12:00"); ok {
		t.Fatal("unexpected parse")
	}
}

func TestBuildCommentAndMarker(t *testing.T) {
	if got := buildComment("", "booker"); got != "[cg:booker]" {
		t.Fatalf("empty note: %q", got)
	}
	if got := buildComment("Ivan, racket rental", "booker"); got != "[cg:booker] Ivan, racket rental" {
		t.Fatalf("note: %q", got)
	}
	forged := buildComment("[cg:other] pay later", "booker")
	if hasMarker(forged, "other") || !hasMarker(forged, "booker") {
		t.Fatalf("forged marker survived: %q", forged)
	}
	if hasMarker("edited by the club [cg:booker]", "booker") {
		t.Fatal("marker must lead the comment")
	}
	long := buildComment(strings.Repeat("я", 2*maxComment), "booker")
	if n := utf8.RuneCountInString(long); n != maxComment || !hasMarker(long, "booker") {
		t.Fatalf("long note: %d characters, marker kept %v", n, hasMarker(long, "booker"))
	}
}

// A TIME base price wins over the NONE (default) one wherever it applies,
// whichever comes first in the price list.
func TestTimePriceWinsOverTheDefault(t *testing.T) {
	timeRule := `{"condition": {"type": "TIME", "days": ` + week + `, "timeStart": "18:00:00", "timeEnd": "22:00:00"}, "valueFrom": 3000}`
	noneRule := `{"condition": {"type": "NONE"}, "valueFrom": 1500}`
	for name, rules := range map[string]string{"default first": noneRule + "," + timeRule, "default last": timeRule + "," + noneRule} {
		tb, err := parsePricing([]byte(`{"basePrices": [` + rules + `]}`))
		if err != nil {
			t.Fatal(err)
		}
		at := func(minute int) int64 { // the first rule that covers it, as the core prices
			for _, r := range tb.Base {
				if r.Days.Has(0) && r.From <= minute && minute < r.To {
					return r.PerHour
				}
			}
			return -1
		}
		if at(19*60) != 300000 || at(10*60) != 150000 {
			t.Fatalf("%s: 19:00 at %d, 10:00 at %d", name, at(19*60), at(10*60))
		}
	}
}

func TestParseID(t *testing.T) {
	if k, u, ok := parseID("e-00000000-0000-4000-8000-0000000000AA"); !ok || k != "exercises" || u != "00000000-0000-4000-8000-0000000000aa" {
		t.Fatalf("got %s %s %v", k, u, ok)
	}
	if k, _, ok := parseID("b-00000000-0000-4000-8000-0000000000aa"); !ok || k != "breaks" {
		t.Fatal("break id")
	}
	for _, bad := range []string{"x-00000000-0000-4000-8000-0000000000aa", "e-../../admin", "e-", "00000000-0000-4000-8000-0000000000aa"} {
		if _, _, ok := parseID(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

const week = `["MONDAY","TUESDAY","WEDNESDAY","THURSDAY","FRIDAY","SATURDAY","SUNDAY"]`

func TestParsePricing(t *testing.T) {
	body := `{
	  "basePrices": [
	    {"condition": {"type": "TIME", "days": ["MONDAY","FRIDAY"], "timeStart": "07:00:00", "timeEnd": "16:00:00"}, "valueFrom": 2000},
	    {"condition": {"type": "TIME", "days": ` + week + `, "timeStart": "16:00:00", "timeEnd": "00:00:00"}, "valueFrom": "3000.50"},
	    {"condition": {"type": "TIME", "days": ["SUNDAY"], "timeStart": "23:00:00", "timeEnd": "01:00:00"}, "valueFrom": 1000},
	    {"condition": {"type": "NONE"}, "valueFrom": 1500}
	  ],
	  "priceImpacts": {
	    "TIME": [
	      {"impactType": "FIXED", "impactDirection": "DISCOUNT", "value": 300, "condition": {"type": "TIME", "days": ["MONDAY"], "timeStart": "07:00:00", "timeEnd": "10:00:00"}},
	      {"impactType": "FIXED", "impactDirection": "INCREASE", "value": "250", "condition": {"type": "TIME", "days": ["FRIDAY"], "timeStart": "18:00:00", "timeEnd": "00:00:00"}}
	    ],
	    "OTHER": [{"anything": true}]
	  }
	}`
	tb, err := parsePricing([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(tb.Base) != 5 {
		t.Fatalf("base rules = %d, want 5 (overnight rule split in two)", len(tb.Base))
	}
	r0 := tb.Base[0]
	if !r0.Days.Has(0) || !r0.Days.Has(4) || r0.Days.Has(1) || r0.From != 420 || r0.To != 960 || r0.PerHour != 200000 {
		t.Fatalf("rule 0 = %+v", r0)
	}
	if r1 := tb.Base[1]; r1.To != 1440 || r1.PerHour != 300050 || r1.Days != driver.AllWeekdays {
		t.Fatalf("midnight end / decimal string: %+v", r1)
	}
	if a, b := tb.Base[2], tb.Base[3]; a.From != 1380 || a.To != 1440 || !a.Days.Has(6) || b.From != 0 || b.To != 60 || !b.Days.Has(0) || b.Days.Has(6) {
		t.Fatalf("overnight split: %+v %+v", a, b)
	}
	if r4 := tb.Base[4]; r4.Days != driver.AllWeekdays || r4.From != 0 || r4.To != 1440 || r4.PerHour != 150000 {
		t.Fatalf("NONE rule: %+v", r4)
	}
	if len(tb.Adjustments) != 2 || tb.Adjustments[0].PerHourDelta != -30000 || tb.Adjustments[1].PerHourDelta != 25000 || tb.Adjustments[1].To != 1440 {
		t.Fatalf("impacts: %+v", tb.Adjustments)
	}
}

func TestParsePricingRefusesWhatItCannotPrice(t *testing.T) {
	cases := map[string]string{
		"not an object":         `[]`,
		"garbage":               `{"basePrices": "x"}`,
		"unknown condition":     `{"basePrices": [{"condition": {"type": "DATE"}, "valueFrom": 1}]}`,
		"no value":              `{"basePrices": [{"condition": {"type": "NONE"}}]}`,
		"negative value":        `{"basePrices": [{"condition": {"type": "NONE"}, "valueFrom": -5}]}`,
		"bad day":               `{"basePrices": [{"condition": {"type": "TIME", "days": ["FUNDAY"], "timeStart": "07:00:00", "timeEnd": "08:00:00"}, "valueFrom": 1}]}`,
		"bad clock":             `{"basePrices": [{"condition": {"type": "TIME", "days": ["MONDAY"], "timeStart": "7am", "timeEnd": "08:00:00"}, "valueFrom": 1}]}`,
		"percent impact":        `{"basePrices": [], "priceImpacts": {"TIME": [{"impactType": "PERCENT", "value": 10, "condition": {"type": "TIME", "days": ["MONDAY"], "timeStart": "07:00:00", "timeEnd": "08:00:00"}}]}}`,
		"impact w/o condition":  `{"basePrices": [], "priceImpacts": {"TIME": [{"impactType": "FIXED", "value": 10}]}}`,
		"condition-less base":   `{"basePrices": [{"valueFrom": 1}]}`,
		"value is not a number": `{"basePrices": [{"condition": {"type": "NONE"}, "valueFrom": "free"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parsePricing([]byte(body))
			var ue *driver.UpstreamError
			if !errors.As(err, &ue) || ue.Kind != driver.UpstreamInvalid {
				t.Fatalf("err = %v, want UpstreamInvalid", err)
			}
		})
	}
}
