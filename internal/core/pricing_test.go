package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

var (
	weekdaysOnly = driver.Weekdays{true, true, true, true, true, false, false}
	weekendOnly  = driver.Weekdays{false, false, false, false, false, true, true}
)

// table: weekdays 07-17 1800/h, 17-24 2600/h; weekends all day 2400/h;
// weekday mornings 07-09 discounted 300/h; Friday 20-24 +400/h.
func testTable() driver.PriceTable {
	return driver.PriceTable{
		Base: []driver.PriceRule{
			{Days: weekdaysOnly, From: 420, To: 1020, PerHour: 1800_00},
			{Days: weekdaysOnly, From: 1020, To: 1440, PerHour: 2600_00},
			{Days: weekendOnly, From: 0, To: 1440, PerHour: 2400_00},
		},
		Adjustments: []driver.PriceAdjustment{
			{Days: weekdaysOnly, From: 420, To: 540, PerHourDelta: -300_00},
			{Days: driver.Weekdays{4: true}, From: 1200, To: 1440, PerHourDelta: 400_00},
		},
	}
}

func mustLoc(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestQuote(t *testing.T) {
	loc := mustLoc(t)
	// 2030-03-04 is a Monday, 2030-03-08 a Friday, 2030-03-09 a Saturday.
	at := func(day, hh, mm int) time.Time { return time.Date(2030, 3, day, hh, mm, 0, 0, loc) }
	cases := []struct {
		name       string
		start, end time.Time
		want       int64
	}{
		{"plain weekday hour", at(4, 10, 0), at(4, 11, 0), 1800},
		{"discounted morning", at(4, 7, 0), at(4, 8, 30), 2250},       // 1.5 x 1500
		{"discount ends inside", at(4, 8, 30), at(4, 9, 30), 1650},    // .5x1500 + .5x1800
		{"crosses a rule boundary", at(4, 16, 0), at(4, 18, 0), 4400}, // 1800 + 2600
		{"ends at midnight", at(4, 22, 0), at(5, 0, 0), 5200},
		{"friday evening surcharge", at(8, 19, 30), at(8, 21, 0), 4300}, // .5x2600 + 1x3000
		{"weekend", at(9, 9, 0), at(9, 10, 30), 3600},
		{"across midnight into the weekend", at(8, 23, 0), at(9, 1, 0), 3000 + 2400},
		{"input in UTC is placed in club time", time.Date(2030, 3, 4, 7, 0, 0, 0, time.UTC), time.Date(2030, 3, 4, 8, 0, 0, 0, time.UTC), 1800},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := quote(context.Background(), testTable(), loc, 30, c.start, c.end)
			if err != nil || got != c.want {
				t.Fatalf("quote = %d, %v; want %d", got, err, c.want)
			}
		})
	}
}

// Daylight saving time: the price is for the time that actually passes.
// In Europe/Berlin clocks go back at 03:00 on 2030-10-27 (02:00-03:00 is
// lived twice) and forward at 02:00 on 2030-03-31 (02:00-03:00 never
// happens).
func TestQuoteAcrossDaylightSavingChanges(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	all := driver.Weekdays{true, true, true, true, true, true, true}
	flat := driver.PriceTable{Base: []driver.PriceRule{{Days: all, From: 0, To: 1440, PerHour: 2000_00}}}
	// 2000/h until 02:30 local, 4000/h after
	split := driver.PriceTable{Base: []driver.PriceRule{{Days: all, From: 0, To: 150, PerHour: 2000_00}, {Days: all, From: 150, To: 1440, PerHour: 4000_00}}}
	summer, winter := time.FixedZone("CEST", 2*3600), time.FixedZone("CET", 3600)
	cases := []struct {
		name       string
		table      driver.PriceTable
		start, end time.Time
		want       int64
	}{
		// one hour passes between the two 02:30s
		{"repeated hour, flat", flat, time.Date(2030, 10, 27, 2, 30, 0, 0, summer), time.Date(2030, 10, 27, 2, 30, 0, 0, winter), 2000},
		// 02:30-03:00 summer at 4000, then 02:00-02:30 winter at 2000
		{"repeated hour, split", split, time.Date(2030, 10, 27, 2, 30, 0, 0, summer), time.Date(2030, 10, 27, 2, 30, 0, 0, winter), 3000},
		{"the whole autumn night", flat, time.Date(2030, 10, 27, 0, 0, 0, 0, loc), time.Date(2030, 10, 27, 6, 0, 0, 0, loc), 14000},
		// 01:30 to 03:30 local is one hour of real time
		{"skipped hour, flat", flat, time.Date(2030, 3, 31, 1, 30, 0, 0, loc), time.Date(2030, 3, 31, 3, 30, 0, 0, loc), 2000},
		// 01:30-02:00 at 2000, 03:00-03:30 at 4000
		{"skipped hour, split", split, time.Date(2030, 3, 31, 1, 30, 0, 0, loc), time.Date(2030, 3, 31, 3, 30, 0, 0, loc), 3000},
		{"the whole spring night", flat, time.Date(2030, 3, 31, 0, 0, 0, 0, loc), time.Date(2030, 3, 31, 6, 0, 0, 0, loc), 10000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := quote(context.Background(), c.table, loc, 30, c.start, c.end)
			if err != nil || got != c.want {
				t.Fatalf("quote = %d, %v; want %d", got, err, c.want)
			}
		})
	}
}

// A local midnight the clocks skip or repeat. In America/Santiago clocks
// jump from 2030-09-07 24:00 to 2030-09-08 01:00 (midnight never happens)
// and go back from 2030-04-06 24:00 to 23:00 (Saturday's last hour is
// lived twice). Saturday costs 2000/h, Sunday 4000/h. Each case runs
// under a deadline: the old midnight arithmetic never terminated here.
func TestQuoteAcrossSkippedAndRepeatedMidnight(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	tb := driver.PriceTable{Base: []driver.PriceRule{
		{Days: driver.Weekdays{5: true}, From: 0, To: 1440, PerHour: 2000_00},
		{Days: driver.Weekdays{6: true}, From: 0, To: 1440, PerHour: 4000_00},
	}}
	minus4, minus3 := time.FixedZone("-04", -4*3600), time.FixedZone("-03", -3*3600)
	cases := []struct {
		name       string
		start, end time.Time
		want       int64
	}{
		// Sat 23:30-24:00 at 2000, Sun 01:00-01:30 at 4000: one hour passes
		{"skipped midnight", time.Date(2030, 9, 7, 23, 30, 0, 0, minus4), time.Date(2030, 9, 8, 1, 30, 0, 0, minus3), 1000 + 2000},
		// Sat 23:30-24:00 (-03), Sat 23:00-24:00 (-04), Sun 00:00-00:30: two hours
		{"repeated midnight", time.Date(2030, 4, 6, 23, 30, 0, 0, minus3), time.Date(2030, 4, 7, 0, 30, 0, 0, minus4), 1000 + 2000 + 2000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			type res struct {
				got int64
				err error
			}
			done := make(chan res, 1)
			go func() {
				got, err := quote(context.Background(), tb, loc, 30, c.start, c.end)
				done <- res{got, err}
			}()
			select {
			case r := <-done:
				if r.err != nil || r.got != c.want {
					t.Fatalf("quote = %d, %v; want %d", r.got, r.err, c.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("quote did not terminate")
			}
		})
	}
	// The boundary after any instant of either night is strictly later.
	for _, d := range []time.Time{time.Date(2030, 9, 7, 22, 0, 0, 0, minus4), time.Date(2030, 4, 6, 22, 0, 0, 0, minus3)} {
		for c := d; c.Before(d.Add(4 * time.Hour)); c = c.Add(30 * time.Minute) {
			if next := nextBoundary(c, loc); !next.After(c) {
				t.Fatalf("nextBoundary(%s) = %s, not after it", c, next)
			}
		}
	}
}

func TestQuoteStopsWhenCancelled(t *testing.T) {
	loc := mustLoc(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := time.Date(2030, 3, 4, 10, 0, 0, 0, loc)
	if _, err := quote(ctx, testTable(), loc, 30, s, s.Add(time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestQuoteRefusals(t *testing.T) {
	loc := mustLoc(t)
	at := func(hh, mm int) time.Time { return time.Date(2030, 3, 4, hh, mm, 0, 0, loc) }
	if _, err := quote(context.Background(), testTable(), loc, 30, at(6, 0), at(8, 0)); !errors.Is(err, errNoPrice) {
		t.Fatalf("uncovered slot: %v", err)
	}
	if _, err := quote(context.Background(), testTable(), loc, 30, at(10, 15), at(11, 0)); !errors.Is(err, errNotAligned) {
		t.Fatalf("misaligned: %v", err)
	}
	if _, err := quote(context.Background(), testTable(), loc, 30, at(11, 0), at(10, 0)); !errors.Is(err, errNotAligned) {
		t.Fatalf("reversed: %v", err)
	}
}

func TestQuoteRoundsToWholeRubles(t *testing.T) {
	loc := mustLoc(t)
	tb := driver.PriceTable{Base: []driver.PriceRule{{Days: driver.AllWeekdays, From: 0, To: 1440, PerHour: 1001_00}}}
	s := time.Date(2030, 3, 4, 10, 0, 0, 0, loc)
	// 15 minutes of 1001/h = 250.25 -> 250
	if got, _ := quote(context.Background(), tb, loc, 15, s, s.Add(15*time.Minute)); got != 250 {
		t.Fatalf("got %d", got)
	}
	// 30 minutes = 500.5 -> 501 (half up)
	if got, _ := quote(context.Background(), tb, loc, 15, s, s.Add(30*time.Minute)); got != 501 {
		t.Fatalf("got %d", got)
	}
}

func TestFirstMatchingBaseRuleWins(t *testing.T) {
	tb := driver.PriceTable{Base: []driver.PriceRule{
		{Days: driver.AllWeekdays, From: 600, To: 720, PerHour: 5000_00},
		{Days: driver.AllWeekdays, From: 0, To: 1440, PerHour: 1000_00},
	}}
	loc := mustLoc(t)
	at := func(hh, mm int) time.Time { return time.Date(2030, 3, 4, hh, mm, 0, 0, loc) }
	// 10:00-10:30 inside the first rule
	if got, _ := quote(context.Background(), tb, loc, 30, at(10, 0), at(10, 30)); got != 2500 {
		t.Fatalf("got %d", got)
	}
	// 11:30-12:30: half an hour of each rule
	if got, _ := quote(context.Background(), tb, loc, 30, at(11, 30), at(12, 30)); got != 2500+500 {
		t.Fatalf("got %d", got)
	}
}

// Price rules need not follow the booking grid: a discount from 10:15 to
// 10:45 on a 30-minute grid applies to the quarter hours it covers, and
// /prices shows the same periods.
func TestPriceAdjustmentOffTheGrid(t *testing.T) {
	loc := mustLoc(t)
	tb := driver.PriceTable{
		Base:        []driver.PriceRule{{Days: driver.AllWeekdays, From: 0, To: 1440, PerHour: 2000_00}},
		Adjustments: []driver.PriceAdjustment{{Days: driver.AllWeekdays, From: 615, To: 645, PerHourDelta: -500_00}},
	}
	at := func(hh, mm int) time.Time { return time.Date(2030, 3, 4, hh, mm, 0, 0, loc) }
	if got, err := quote(context.Background(), tb, loc, 30, at(10, 0), at(11, 0)); err != nil || got != 1750 {
		t.Fatalf("quote = %d, %v; want 1750", got, err)
	}
	if got, err := quote(context.Background(), tb, loc, 30, at(10, 0), at(10, 30)); err != nil || got != 875 {
		t.Fatalf("first half hour = %d, %v; want 875 (15 min at 2000/h + 15 min at 1500/h)", got, err)
	}
	want := []pricePeriod{{0, 615, 2000_00, true}, {615, 645, 1500_00, true}, {645, 1440, 2000_00, true}}
	got := dayPeriods(tb, 0)
	if len(got) != len(want) {
		t.Fatalf("periods %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("period %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDayPeriods(t *testing.T) {
	got := dayPeriods(testTable(), 4) // Friday
	want := []pricePeriod{{420, 540, 1500_00, true}, {540, 1020, 1800_00, true}, {1020, 1200, 2600_00, true}, {1200, 1440, 3000_00, true}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("period %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if rubles(1500_49) != 1500 || rubles(1500_50) != 1501 || rubles(-1500_50) != -1501 {
		t.Fatal("rubles rounding")
	}
}
