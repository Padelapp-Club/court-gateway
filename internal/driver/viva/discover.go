package viva

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Discovery reads the account's studios. Verified against a live account
// (read-only): GET /studios answers an array of every studio of the
// account, each with id, name, offset (hours from UTC), workTime and its
// rooms (id, name, studioId, ...). GET /rooms does not exist. Price lists
// still come from GET /pricing?studioId=. Nothing about clients is read.

type vivaDay struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type vivaWorkTime struct {
	Daily map[string]*vivaDay `json:"dailyWorkTime"`
}

type vivaRoom struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	StudioID string `json:"studioId"`
}

type vivaStudio struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Offset   *float64      `json:"offset"`
	WorkTime *vivaWorkTime `json:"workTime"`
	Rooms    []vivaRoom    `json:"rooms"`
}

var vivaDays = [7]string{"MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}

// listStudios reads GET /studios. Only id, name, offset, workTime and the
// rooms' id and name are decoded; rates, directions and the rest are not.
func listStudios(ctx context.Context, c *client) ([]vivaStudio, error) {
	status, data, err := c.do(ctx, http.MethodGet, "/studios", nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /studios answered HTTP %d", status), nil)
	}
	var list []vivaStudio
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /studios: response is not a JSON array of studios", nil)
	}
	out := list[:0]
	for _, st := range list {
		if uuidRe.MatchString(st.ID) {
			st.ID = strings.ToLower(st.ID)
			st.Name = strings.TrimSpace(st.Name)
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return naturalLess(out[i].Name, out[j].Name)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// studioClock parses "HH:MM" or "HH:MM+03:00" (the offset is the studio's own,
// so the clock part is already local time).
func studioClock(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 5 || s[2] != ':' {
		return 0, false
	}
	var h, m int
	if _, err := fmt.Sscanf(s[:5], "%02d:%02d", &h, &m); err != nil || h > 24 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// studioHours turns workTime into hours per ISO weekday. from == to means
// open around the clock; a day missing from workTime is unknown (nil).
func studioHours(wt *vivaWorkTime) ([7]*[2]int, bool) {
	var out [7]*[2]int
	if wt == nil || len(wt.Daily) == 0 {
		return out, false
	}
	found := false
	for i, name := range vivaDays {
		d := wt.Daily[name]
		if d == nil {
			continue
		}
		from, ok1 := studioClock(d.From)
		to, ok2 := studioClock(d.To)
		if !ok1 || !ok2 {
			continue
		}
		switch {
		case from == to:
			from, to = 0, 1440
		case to == 0:
			to = 1440
		case to < from:
			to = 1440 // past midnight is not supported by the gateway: until midnight
		}
		out[i] = &[2]int{from, to}
		found = true
	}
	return out, found
}

// zoneForOffset is a fixed-offset IANA zone for a whole number of hours
// (Etc/GMT-3 is UTC+3: the sign is inverted in that namespace).
func zoneForOffset(off *float64) string {
	if off == nil || *off != math.Trunc(*off) || *off < -12 || *off > 14 {
		return ""
	}
	h := int(*off)
	switch {
	case h == 0:
		return "Etc/UTC"
	case h > 0:
		return fmt.Sprintf("Etc/GMT-%d", h)
	default:
		return fmt.Sprintf("Etc/GMT+%d", -h)
	}
}

// priceListInfo is a price list of a studio and the hours its TIME rules cover.
type priceLists struct {
	lists  []driver.PriceListInfo
	hours  [7]*[2]int
	failed int
}

func readPriceLists(ctx context.Context, c *client, studio string) (priceLists, error) {
	var out priceLists
	status, data, err := c.do(ctx, http.MethodGet, "/pricing", url.Values{"studioId": {studio}, "page": {"0"}, "size": {"200"}}, nil)
	if err != nil {
		return out, err
	}
	if status != http.StatusOK {
		return out, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /pricing answered HTTP %d", status), nil)
	}
	var page struct {
		Content []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		return out, driver.Upstream(driver.UpstreamInvalid, "GET /pricing: malformed page", nil)
	}
	for _, p := range page.Content {
		if !uuidRe.MatchString(p.ID) {
			continue
		}
		out.lists = append(out.lists, driver.PriceListInfo{ID: strings.ToLower(p.ID), Name: strings.TrimSpace(p.Name)})
		status, body, err := c.do(ctx, http.MethodGet, "/pricing/"+p.ID, nil, nil)
		if err != nil {
			return out, err
		}
		var table driver.PriceTable
		if status == http.StatusOK {
			table, err = parsePricing(body)
		}
		if status != http.StatusOK || err != nil {
			out.failed++
			continue
		}
		for _, rule := range table.Base {
			for d, on := range rule.Days {
				if !on {
					continue
				}
				if out.hours[d] == nil {
					out.hours[d] = &[2]int{rule.From, rule.To}
					continue
				}
				out.hours[d][0] = min(out.hours[d][0], rule.From)
				out.hours[d][1] = max(out.hours[d][1], rule.To)
			}
		}
	}
	return out, nil
}

// naturalLess orders "Корт 2" before "Корт 10".
func naturalLess(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if isDigit(ra[i]) && isDigit(rb[j]) {
			si, sj := i, j
			for i < len(ra) && isDigit(ra[i]) {
				i++
			}
			for j < len(rb) && isDigit(rb[j]) {
				j++
			}
			na, nb := strings.TrimLeft(string(ra[si:i]), "0"), strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ra[i] != rb[j] {
			return ra[i] < rb[j]
		}
		i++
		j++
	}
	return len(ra)-i < len(rb)-j
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
