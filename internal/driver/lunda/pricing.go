package lunda

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Lunda prices a court by its tariff: a price per hour for stretches of the
// day, by category of day. A date's category comes from the club's
// calendar (calendar/load?year=): REGULAR and WEEKEND are what working
// days and weekends are by default, HOLIDAY and the PRE_* categories mark
// dates. The gateway's price table is weekly, so it takes REGULAR for
// Monday to Friday and WEEKEND for Saturday and Sunday; a date the
// calendar gives another category that prices differently is priced as
// its weekday, and is logged.
//
// [UNVERIFIED] the whole model: tariff/load?uid= answering {uid, name,
// prices: [{dayCategory, timeFrom, timeTo, price}]} with price in rubles
// per hour, and calendar/load answering {days: [{date, category}]}.
// Observed only: a 30-minute slot of 750 ₽ in a club whose hour is 1500 ₽.

type lundaTariffRef struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type lundaTariff struct {
	UID    string `json:"uid"`
	Prices []struct {
		DayCategory string          `json:"dayCategory"`
		TimeFrom    string          `json:"timeFrom"`
		TimeTo      string          `json:"timeTo"`
		Price       json.RawMessage `json:"price"`
	} `json:"prices"`
}

// defaultCategory is the category of a weekday by default (ISO index).
func defaultCategory(iso int) string {
	if iso >= 5 {
		return "WEEKEND"
	}
	return "REGULAR"
}

// calendarHorizon is how far ahead special dates are looked for.
const calendarHorizon = 31 * 24 * time.Hour

// tariffList reads the club's tariffs.
func tariffList(ctx context.Context, c *client) ([]lundaTariffRef, error) {
	status, res, err := c.call(ctx, opTariffs, nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opTariffs, status))
	}
	var list []lundaTariffRef
	if decodeList(res, &list) != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opTariffs+": the answer is not a list of tariffs")
	}
	out := list[:0]
	for _, t := range list {
		if uidRe.MatchString(t.UID) {
			out = append(out, t)
		}
	}
	return out, nil
}

// rule is a price rule of one category of day.
type rule struct {
	from, to int
	perHour  int64 // kopecks
}

// PriceTable implements driver.Driver: the court's tariff (lunda_tariff;
// driver.ErrNotFound without one), REGULAR on working days and WEEKEND on
// weekends, in the club's zone.
func (d *Driver) PriceTable(ctx context.Context, courtID string) (driver.PriceTable, error) {
	c, ok := d.byCourt[courtID]
	if !ok || c.tariff == "" {
		return driver.PriceTable{}, fmt.Errorf("tariff of court %q: %w", courtID, driver.ErrNotFound)
	}
	status, res, err := d.c.call(ctx, opTariff, url.Values{"uid": {c.tariff}}, nil)
	if err != nil {
		return driver.PriceTable{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return driver.PriceTable{}, fmt.Errorf("tariff of court %q: %w", courtID, driver.ErrNotFound)
	default:
		return driver.PriceTable{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opTariff, status))
	}
	byCat, err := parseTariff(res)
	if err != nil {
		return driver.PriceTable{}, err
	}
	var t driver.PriceTable
	for iso := 0; iso < 7; iso++ {
		var days driver.Weekdays
		days[iso] = true
		for _, r := range byCat[defaultCategory(iso)] {
			t.Base = append(t.Base, driver.PriceRule{Days: days, From: r.from, To: r.to, PerHour: r.perHour})
		}
	}
	d.noteSpecialDates(ctx, byCat)
	return t, nil
}

// parseTariff reads a tariff's rules by category. A rule the driver
// cannot read makes the whole tariff invalid: one wrong price would make
// any quote wrong.
func parseTariff(res json.RawMessage) (map[string][]rule, error) {
	invalid := func(what string) (map[string][]rule, error) {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opTariff+": "+what)
	}
	var t lundaTariff
	if json.Unmarshal(res, &t) != nil || t.Prices == nil {
		return invalid("the answer is not a tariff")
	}
	out := map[string][]rule{}
	for _, p := range t.Prices {
		from, ok1 := core.ParseClock(p.TimeFrom)
		to, ok2 := core.ParseClock(p.TimeTo)
		if to == 0 {
			to = 1440
		}
		v, ok3 := kopecks(p.Price)
		if p.DayCategory == "" || !ok1 || !ok2 || !ok3 || from >= to {
			return invalid("a price without a category, a valid stretch of the day or a price")
		}
		out[strings.ToUpper(p.DayCategory)] = append(out[strings.ToUpper(p.DayCategory)], rule{from: from, to: to, perHour: v})
	}
	for _, rs := range out {
		sort.Slice(rs, func(i, j int) bool { return rs[i].from < rs[j].from })
	}
	return out, nil
}

// kopecks reads a JSON number or numeric string of rubles as kopecks.
func kopecks(r json.RawMessage) (int64, bool) {
	s := strings.TrimSpace(string(r))
	if s == "" || s == "null" {
		return 0, false
	}
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(r, &s); err != nil {
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1e9 {
		return 0, false
	}
	return int64(math.Round(f * 100)), true
}

// specialDates reads the calendar of the years the next calendarHorizon
// touches and returns its dates in that window (local "2006-01-02" ->
// category) whose category is not their weekday's.
func (d *Driver) specialDates(ctx context.Context) (map[string]string, error) {
	now, _ := d.clock()
	from := localDay(now, d.loc)
	to := from.Add(calendarHorizon)
	out := map[string]string{}
	for year := from.Year(); year <= to.Year(); year++ {
		status, res, err := d.c.call(ctx, opCalendar, url.Values{"year": {strconv.Itoa(year)}}, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opCalendar, status))
		}
		var cal struct {
			Days []struct {
				Date     string `json:"date"`
				Category string `json:"category"`
			} `json:"days"`
		}
		if json.Unmarshal(res, &cal) != nil {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opCalendar+": the answer is not a calendar")
		}
		for _, day := range cal.Days {
			t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(day.Date), d.loc)
			cat := strings.ToUpper(strings.TrimSpace(day.Category))
			if err != nil || cat == "" || t.Before(from) || t.After(to) {
				continue
			}
			if cat != defaultCategory((int(t.Weekday())+6)%7) {
				out[t.Format("2006-01-02")] = cat
			}
		}
	}
	return out, nil
}

// noteSpecialDates logs the dates of the next calendarHorizon that the
// weekly table prices wrong: their category prices differently from their
// weekday's. The table cannot say a date (driver.PriceTable is weekly); a
// calendar that does not read only costs the note.
func (d *Driver) noteSpecialDates(ctx context.Context, byCat map[string][]rule) {
	dates, err := d.specialDates(ctx)
	if err != nil {
		d.log.Warn("the Lunda price calendar could not be read: holidays are priced as their weekday", "error", err)
		return
	}
	var wrong []string
	for date, cat := range dates {
		t, _ := time.Parse("2006-01-02", date)
		if !sameRules(byCat[cat], byCat[defaultCategory((int(t.Weekday())+6)%7)]) {
			wrong = append(wrong, date+" "+cat)
		}
	}
	if len(wrong) > 0 {
		sort.Strings(wrong)
		d.log.Warn("the Lunda price calendar has dates priced otherwise than their weekday: the gateway quotes them as their weekday", "dates", strings.Join(wrong, ", "))
	}
}

func sameRules(a, b []rule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
