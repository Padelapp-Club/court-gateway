package viva

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// DiscoverConfig is what `setup` knows before a config file exists.
type DiscoverConfig struct {
	AuthURL    string
	APIURL     string
	Username   string
	Password   string
	Studio     string
	HTTPClient *http.Client
	Now        func() time.Time
	// Days of future occupancy to scan for rooms (default 28).
	Days int
}

// Room is a VivaCRM room seen in the occupancy, i.e. a court candidate.
type Room struct {
	ID       string
	Name     string
	Bookings int
}

// PriceListInfo is a price list of the studio.
type PriceListInfo struct {
	ID   string
	Name string
}

// Discovery is the draft material for a config file.
type Discovery struct {
	Rooms      []Room
	PriceLists []PriceListInfo
	// Hours are opening hours per ISO weekday (0 = Monday) in minutes,
	// derived from the bounds of the TIME rules of every price list; nil
	// where no rule covers the day.
	Hours [7]*[2]int
	// PriceListErrors names price lists that could not be read.
	PriceListErrors []string
}

type discoverSlot struct {
	Room *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"room"`
	Canceled *bool `json:"canceled"`
}

// Discover logs in to VivaCRM and collects rooms from the coming Days of
// occupancy, and the studio's price lists with the hours their TIME rules
// cover. A room with no booking in that window is not seen — the caller
// must say so. Nothing about clients is decoded.
func Discover(ctx context.Context, cfg DiscoverConfig) (Discovery, error) {
	var out Discovery
	if cfg.AuthURL == "" {
		cfg.AuthURL = DefaultAuthURL
	}
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Days <= 0 {
		cfg.Days = 28
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	c := &client{
		authURL: cfg.AuthURL, apiURL: strings.TrimRight(cfg.APIURL, "/"), username: cfg.Username,
		password: cfg.Password, http: cfg.HTTPClient, now: cfg.Now,
	}
	if _, err := c.accessToken(ctx); err != nil {
		return out, err
	}

	rooms := map[string]*Room{}
	today := utcDay(cfg.Now())
	for off := 0; off < cfg.Days; off += 7 {
		from := today.AddDate(0, 0, off)
		to := from.AddDate(0, 0, 7)
		if end := today.AddDate(0, 0, cfg.Days); to.After(end) {
			to = end
		}
		for _, kind := range []string{"exercises", "breaks"} {
			q := url.Values{
				"studioId": {cfg.Studio}, "dateFrom": {from.Format("2006-01-02")},
				"dateTo": {to.Format("2006-01-02")}, "includeCanceled": {"false"},
			}
			status, data, err := c.do(ctx, http.MethodGet, "/"+kind, q, nil)
			if err != nil {
				return out, err
			}
			if status != http.StatusOK {
				return out, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", kind, status), nil)
			}
			var items []discoverSlot
			trimmed := bytes.TrimSpace(data)
			if len(trimmed) == 0 || trimmed[0] != '[' || json.Unmarshal(trimmed, &items) != nil {
				return out, driver.Upstream(driver.UpstreamInvalid, "GET /"+kind+": response is not a JSON array", nil)
			}
			for _, it := range items {
				if it.Room == nil || !uuidRe.MatchString(it.Room.ID) || (it.Canceled != nil && *it.Canceled) {
					continue
				}
				id := strings.ToLower(it.Room.ID)
				r, ok := rooms[id]
				if !ok {
					r = &Room{ID: id, Name: strings.TrimSpace(it.Room.Name)}
					rooms[id] = r
				}
				r.Bookings++
			}
		}
	}
	for _, r := range rooms {
		out.Rooms = append(out.Rooms, *r)
	}
	sort.Slice(out.Rooms, func(i, j int) bool {
		if out.Rooms[i].Name != out.Rooms[j].Name {
			return naturalLess(out.Rooms[i].Name, out.Rooms[j].Name)
		}
		return out.Rooms[i].ID < out.Rooms[j].ID
	})

	status, data, err := c.do(ctx, http.MethodGet, "/pricing", url.Values{"studioId": {cfg.Studio}, "page": {"0"}, "size": {"200"}}, nil)
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
		out.PriceLists = append(out.PriceLists, PriceListInfo{ID: strings.ToLower(p.ID), Name: strings.TrimSpace(p.Name)})
		status, body, err := c.do(ctx, http.MethodGet, "/pricing/"+p.ID, nil, nil)
		if err != nil {
			return out, err
		}
		var table driver.PriceTable
		if status == http.StatusOK {
			table, err = parsePricing(body)
		}
		if status != http.StatusOK || err != nil {
			out.PriceListErrors = append(out.PriceListErrors, p.ID)
			continue
		}
		for _, rule := range table.Base {
			for d, on := range rule.Days {
				if !on {
					continue
				}
				if out.Hours[d] == nil {
					out.Hours[d] = &[2]int{rule.From, rule.To}
					continue
				}
				out.Hours[d][0] = min(out.Hours[d][0], rule.From)
				out.Hours[d][1] = max(out.Hours[d][1], rule.To)
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
