package viva

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Price list model, as far as the driver reads it.
//
// [ASSUMPTION] valueFrom of a base price and value of a FIXED time impact
// are rubles PER HOUR (they match clubs' hourly prices; a per-slot or
// per-booking price would not). Fractional values are kept to the kopeck.
type pricing struct {
	BasePrices   []basePrice `json:"basePrices"`
	PriceImpacts *struct {
		Time []impact `json:"TIME"`
	} `json:"priceImpacts"`
}

type condition struct {
	Type      string   `json:"type"`
	Days      []string `json:"days"`
	TimeStart string   `json:"timeStart"`
	TimeEnd   string   `json:"timeEnd"`
}

type basePrice struct {
	Condition *condition      `json:"condition"`
	ValueFrom json.RawMessage `json:"valueFrom"`
}

type impact struct {
	ImpactType      string          `json:"impactType"`
	ImpactDirection string          `json:"impactDirection"`
	Value           json.RawMessage `json:"value"`
	Condition       *condition      `json:"condition"`
}

var weekdays = map[string]int{
	"MONDAY": 0, "TUESDAY": 1, "WEDNESDAY": 2, "THURSDAY": 3, "FRIDAY": 4, "SATURDAY": 5, "SUNDAY": 6,
}

func (d *Driver) PriceTable(ctx context.Context, courtID string) (driver.PriceTable, error) {
	c, ok := d.byCourt[courtID]
	if !ok || c.priceList == "" {
		return driver.PriceTable{}, fmt.Errorf("price list of court %q: %w", courtID, driver.ErrNotFound)
	}
	loc, err := d.zoneNow(ctx) // the rules' times are the studio's
	if err != nil {
		return driver.PriceTable{}, err
	}
	status, data, err := d.c.do(ctx, http.MethodGet, "/pricing/"+c.priceList, nil, nil)
	if err != nil {
		return driver.PriceTable{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return driver.PriceTable{}, fmt.Errorf("price list of court %q: %w", courtID, driver.ErrNotFound)
	default:
		return driver.PriceTable{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /pricing/{id} answered HTTP %d", status))
	}
	t, err := parsePricing(data)
	t.Zone = loc
	return t, err
}

func parsePricing(data []byte) (driver.PriceTable, error) {
	invalid := func(what string) (driver.PriceTable, error) {
		return driver.PriceTable{}, driver.Upstream(driver.UpstreamInvalid, "GET /pricing/{id}: "+what)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return invalid("response is not a JSON object")
	}
	var p pricing
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return invalid("malformed price list")
	}
	var t driver.PriceTable
	var defaults []driver.PriceRule // NONE: only where no TIME rule applies
	for _, b := range p.BasePrices {
		if b.Condition == nil {
			return invalid("base price without condition")
		}
		v, ok := kopecks(b.ValueFrom)
		if !ok {
			return invalid("base price without a numeric valueFrom")
		}
		switch b.Condition.Type {
		case "NONE":
			defaults = append(defaults, driver.PriceRule{Days: driver.AllWeekdays, From: 0, To: 1440, PerHour: v})
		case "TIME":
			spans, ok := timeSpans(b.Condition)
			if !ok {
				return invalid("base price with a malformed TIME condition")
			}
			for _, s := range spans {
				t.Base = append(t.Base, driver.PriceRule{Days: s.days, From: s.from, To: s.to, PerHour: v})
			}
		default:
			// A condition the driver does not understand could make any
			// price wrong, so the whole table is refused.
			return invalid("unsupported base price condition " + strconv.Quote(b.Condition.Type))
		}
	}
	t.Base = append(t.Base, defaults...) // the core takes the first rule that covers a minute
	if p.PriceImpacts != nil {
		for _, im := range p.PriceImpacts.Time {
			if im.ImpactType != "FIXED" {
				return invalid("unsupported time impact type " + strconv.Quote(im.ImpactType))
			}
			v, ok := kopecks(im.Value)
			if !ok || im.Condition == nil {
				return invalid("time impact without value or condition")
			}
			// [ASSUMPTION] DISCOUNT subtracts; any other direction adds.
			if im.ImpactDirection == "DISCOUNT" {
				v = -v
			}
			spans, ok := timeSpans(im.Condition)
			if !ok {
				return invalid("time impact with a malformed condition")
			}
			for _, s := range spans {
				t.Adjustments = append(t.Adjustments, driver.PriceAdjustment{Days: s.days, From: s.from, To: s.to, PerHourDelta: v})
			}
		}
	}
	return t, nil
}

type span struct {
	days     driver.Weekdays
	from, to int
}

// timeSpans converts a TIME condition. An end of "00:00:00" means 24:00.
// [ASSUMPTION] a window whose start is after its end runs past midnight:
// its tail belongs to the next day.
func timeSpans(c *condition) ([]span, bool) {
	var days driver.Weekdays
	for _, s := range c.Days {
		i, ok := weekdays[strings.ToUpper(s)]
		if !ok {
			return nil, false
		}
		days[i] = true
	}
	from, ok1 := core.ParseClock(c.TimeStart)
	to, ok2 := core.ParseClock(c.TimeEnd)
	if !ok1 || !ok2 || from == 1440 || to == 1440 {
		return nil, false
	}
	if to == 0 {
		to = 1440
	}
	if from < to {
		return []span{{days: days, from: from, to: to}}, true
	}
	var next driver.Weekdays
	for i, on := range days {
		if on {
			next[(i+1)%7] = true
		}
	}
	return []span{{days: days, from: from, to: 1440}, {days: next, from: 0, to: to}}, true
}

// kopecks reads a JSON number or numeric string as an amount in kopecks.
func kopecks(r json.RawMessage) (int64, bool) {
	if len(r) == 0 || string(r) == "null" {
		return 0, false
	}
	s := string(r)
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
