package lunda

import (
	"bytes"
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

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Lunda prices a court by the tariff of its court category, as the admin
// reads it (its bundle, 2026-10-17):
//   - tariff/load-list?courtCategoryId= answers the tariff's versions,
//     result.infos [{beginDate}]: a version prices from its beginDate on,
//     until the next one begins;
//   - tariff/load?courtCategoryId=&beginDate= answers a version,
//     result.view {uid, courtCategoryId, beginDate, infos:
//     [{tariffCategory, hour, price}], modifications: [...]}: a price for
//     each hour of the day, by category of day;
//   - calendar/load?year= answers the categories of dates,
//     result.view.tariffCalendar {<CATEGORY>: {dates: ["2006-01-02", ...]}}
//     (seen live: HOLIDAY, PRE_HOLIDAY, PRE_WEEKEND, REGULAR, SPECIAL,
//     WEEKEND).
//
// A date the calendar does not name is REGULAR from Monday to Friday and
// WEEKEND on Saturday and Sunday. The price table is the version of today
// by weekday (Base), and, for every date from yesterday to today plus the
// club's booking depth (club/settings; 31 days when it says none) priced
// otherwise — another category in the calendar, another version — that
// date's rules (Dates; none: no price). Dates outside that range have no
// price (DatesFrom..DatesTo). Anything that does not read is an error:
// never a weekday's price on a holiday.
//
// [UNVERIFIED] that price is rubles per hour (seen: a 30-minute slot of
// 750 ₽ where the hour is 1500 ₽), that hour is the hour of the day it
// prices from, and that an hour of price 0 is not on sale (the driver
// gives it no price). modifications (priced extras the staff may add) are
// not read: they do not change a court's price.

// defaultDepthDays is the booking depth when club/settings tells none.
const defaultDepthDays = 31

// defaultCategory is the category of a weekday by default (ISO index).
func defaultCategory(iso int) string {
	if iso >= 5 {
		return "WEEKEND"
	}
	return "REGULAR"
}

func isoWeekday(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// rule is a price rule of one category of day.
type rule struct {
	from, to int
	perHour  int64 // kopecks
}

// PriceTable implements driver.Driver: the tariff of the court's category
// (driver.ErrNotFound when it has none), in the club's zone.
func (d *Driver) PriceTable(ctx context.Context, courtID string) (driver.PriceTable, error) {
	c, ok := d.byCourt[courtID]
	if !ok {
		return driver.PriceTable{}, fmt.Errorf("court %q: %w", courtID, driver.ErrNotFound)
	}
	courts, err := courtList(ctx, d.c)
	if err != nil {
		return driver.PriceTable{}, err
	}
	category := ""
	for _, ct := range courts {
		if ct.CourtUID == c.uid {
			category = ct.category()
		}
	}
	if category == "" {
		return driver.PriceTable{}, fmt.Errorf("tariff of court %q (no court category in Lunda): %w", courtID, driver.ErrNotFound)
	}
	cs, err := readClubSettings(ctx, d.c)
	if err != nil {
		return driver.PriceTable{}, err
	}
	now, _ := d.clock()
	today := localDay(now, d.loc)
	from, to := today.AddDate(0, 0, -1), today.AddDate(0, 0, cs.depthDays(defaultDepthDays))
	versions, err := tariffVersions(ctx, d.c, category)
	if err != nil {
		return driver.PriceTable{}, err
	}
	if len(versions) == 0 {
		return driver.PriceTable{}, fmt.Errorf("tariff of court %q (none in Lunda): %w", courtID, driver.ErrNotFound)
	}
	versionAt := func(date string) string { // the last version begun by date
		v := ""
		for _, b := range versions {
			if b <= date {
				v = b
			}
		}
		return v
	}
	rules := map[string]map[string][]rule{} // version -> category -> rules
	for _, b := range versions {
		if b > to.Format("2006-01-02") || b < versionAt(from.Format("2006-01-02")) {
			continue // begins after the range, or ended before it
		}
		if rules[b], err = tariffVersion(ctx, d.c, category, b); err != nil {
			return driver.PriceTable{}, err
		}
	}
	cal, err := d.calendar(ctx, from.Year(), to.Year())
	if err != nil {
		return driver.PriceTable{}, err
	}
	week := rules[versionAt(today.Format("2006-01-02"))] // nil: no version yet today
	var t driver.PriceTable
	for iso := 0; iso < 7; iso++ {
		var days driver.Weekdays
		days[iso] = true
		t.Base = append(t.Base, priceRules(week[defaultCategory(iso)], days)...)
	}
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		date, iso := day.Format("2006-01-02"), isoWeekday(day)
		cat, named := cal[date]
		if !named {
			cat = defaultCategory(iso)
		}
		var want []rule
		if v := versionAt(date); v != "" {
			want = rules[v][cat]
		}
		if sameRules(want, week[defaultCategory(iso)]) {
			continue
		}
		if t.Dates == nil {
			t.Dates = map[string][]driver.PriceRule{}
		}
		t.Dates[date] = append([]driver.PriceRule{}, priceRules(want, driver.AllWeekdays)...) // none: no price that date
	}
	t.DatesFrom, t.DatesTo = from.Format("2006-01-02"), to.Format("2006-01-02")
	return t, nil
}

func priceRules(rs []rule, days driver.Weekdays) []driver.PriceRule {
	var out []driver.PriceRule
	for _, r := range rs {
		out = append(out, driver.PriceRule{Days: days, From: r.from, To: r.to, PerHour: r.perHour})
	}
	return out
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

// validDate: a date as Lunda writes it.
func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// tariffVersions reads the begin dates of the versions of a court
// category's tariff, in order.
func tariffVersions(ctx context.Context, c *client, category string) ([]string, error) {
	status, res, err := c.call(ctx, opTariffs, url.Values{"courtCategoryId": {category}}, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opTariffs, status))
	}
	var r struct {
		Infos *[]struct {
			BeginDate string `json:"beginDate"`
		} `json:"infos"`
	}
	if json.Unmarshal(res, &r) != nil || r.Infos == nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opTariffs+": the answer is not a list of tariffs (result.infos)")
	}
	var out []string
	for _, v := range *r.Infos {
		if !validDate(v.BeginDate) {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opTariffs+": a tariff without a valid beginDate")
		}
		out = append(out, v.BeginDate)
	}
	sort.Strings(out)
	return out, nil
}

// tariffVersion reads one version of a tariff: its rules by category of
// day, the hours of one price joined. A price the driver cannot read makes
// the whole version invalid: one wrong price would make any quote wrong.
func tariffVersion(ctx context.Context, c *client, category, begin string) (map[string][]rule, error) {
	invalid := func(what string) (map[string][]rule, error) {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opTariff+": "+what)
	}
	status, res, err := c.call(ctx, opTariff, url.Values{"courtCategoryId": {category}, "beginDate": {begin}}, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("tariff version %s: %w", begin, driver.ErrNotFound)
	default:
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opTariff, status))
	}
	var r struct {
		View *struct {
			Infos *[]struct {
				TariffCategory string          `json:"tariffCategory"`
				Hour           *int            `json:"hour"`
				Price          json.RawMessage `json:"price"`
			} `json:"infos"`
		} `json:"view"`
	}
	if json.Unmarshal(res, &r) != nil || r.View == nil || r.View.Infos == nil {
		return invalid("the answer is not a tariff (result.view.infos)")
	}
	perHour := map[string]map[int]int64{}
	for _, in := range *r.View.Infos {
		cat := strings.ToUpper(strings.TrimSpace(in.TariffCategory))
		v, ok := kopecks(in.Price)
		if cat == "" || in.Hour == nil || *in.Hour < 0 || *in.Hour > 23 || !ok {
			return invalid("a price without a category, an hour of the day or a price")
		}
		if perHour[cat] == nil {
			perHour[cat] = map[int]int64{}
		}
		if _, dup := perHour[cat][*in.Hour]; dup {
			return invalid("two prices for one hour")
		}
		perHour[cat][*in.Hour] = v
	}
	out := map[string][]rule{}
	for cat, hours := range perHour {
		var rs []rule
		for h := 0; h < 24; h++ {
			v, ok := hours[h]
			if !ok || v == 0 {
				continue // not on sale
			}
			if n := len(rs); n > 0 && rs[n-1].to == h*60 && rs[n-1].perHour == v {
				rs[n-1].to += 60
				continue
			}
			rs = append(rs, rule{from: h * 60, to: h*60 + 60, perHour: v})
		}
		out[cat] = rs
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

// calendar reads the calendar of the years y0..y1 whole: the dates it
// names, with their category. A calendar that does not read is an error.
func (d *Driver) calendar(ctx context.Context, y0, y1 int) (map[string]string, error) {
	invalid := func(what string) (map[string]string, error) {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opCalendar+": "+what)
	}
	out := map[string]string{}
	for year := y0; year <= y1; year++ {
		status, res, err := d.c.call(ctx, opCalendar, url.Values{"year": {strconv.Itoa(year)}}, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opCalendar, status))
		}
		var r struct {
			View *struct {
				TariffCalendar map[string]*struct {
					Dates *[]string `json:"dates"`
				} `json:"tariffCalendar"`
			} `json:"view"`
		}
		if t := bytes.TrimSpace(res); len(t) == 0 || t[0] != '{' || json.Unmarshal(t, &r) != nil || r.View == nil || r.View.TariffCalendar == nil {
			return invalid("the answer is not a calendar (result.view.tariffCalendar)")
		}
		for cat, c := range r.View.TariffCalendar {
			cat = strings.ToUpper(strings.TrimSpace(cat))
			if cat == "" || c == nil || c.Dates == nil {
				return invalid("a category without its dates")
			}
			for _, date := range *c.Dates {
				if !validDate(date) || date[:4] != strconv.Itoa(year) {
					return invalid("a date that is not of the year asked")
				}
				if other, dup := out[date]; dup && other != cat {
					return invalid("a date of two categories")
				}
				out[date] = cat
			}
		}
	}
	return out, nil
}
