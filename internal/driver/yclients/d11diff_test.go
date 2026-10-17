package yclients_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients"
)

// The D11 differential check (testdata/d11diff/README.md): the scenarios
// went through the backend's TypeScript adapter, which wrote what it read
// as occupied and every request it sent (ts-golden.json); here they go
// through the gateway driver, answered the same, and must give the same.

type diffScenario struct {
	Name      string                     `json:"name"`
	Kind      string                     `json:"kind"`
	TZ        string                     `json:"tz"`
	Company   string                     `json:"company"`
	Days      []string                   `json:"days"`
	StaffList json.RawMessage            `json:"staff"`
	Pages     []json.RawMessage          `json:"pages"`
	Schedules map[string]json.RawMessage `json:"schedules"`
	Card      json.RawMessage            `json:"card"`
	Start     string                     `json:"start"`
	End       string                     `json:"end"`
	Services  json.RawMessage            `json:"services"`
	RecordIDs []int64                    `json:"record_ids"`
	Records   []map[string]any           `json:"records"`
}

type diffRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Auth   string          `json:"auth"`
	Query  map[string]any  `json:"query"`
	Body   json.RawMessage `json:"body"`
}

type diffResult struct {
	Busy     map[string][]string `json:"busy"`
	ID       string              `json:"id"`
	Error    string              `json:"error"`
	Requests []diffRequest       `json:"requests"`
}

func (sc diffScenario) staff() []string {
	var many []string
	if json.Unmarshal(sc.StaffList, &many) == nil {
		return many
	}
	var one string
	_ = json.Unmarshal(sc.StaffList, &one)
	return []string{one}
}

var (
	reDiffSchedule = regexp.MustCompile(`^/api/v1/schedule/\d+/(\d+)/`)
	reDiffRecord   = regexp.MustCompile(`^/api/v1/record/\d+/(\d+)$`)
)

// diffServer answers a scenario as the TypeScript side's fake client did,
// and keeps every request the driver sent.
func diffServer(t *testing.T, sc diffScenario) (*httptest.Server, *[]diffRequest) {
	var reqs []diffRequest
	records := map[string]map[string]any{}
	for _, r := range sc.Records {
		cp := map[string]any{}
		for k, v := range r {
			cp[k] = v
		}
		records[strconv.FormatInt(int64(r["id"].(float64)), 10)] = cp
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := "partner"
		if strings.Contains(r.Header.Get("Authorization"), ", User ") {
			auth = "user"
		}
		q := map[string]any{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		dr := diffRequest{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/api/v1"), Auth: auth, Query: q}
		if len(body) > 0 {
			dr.Body = body
		}
		reqs = append(reqs, dr)
		answer := func(data any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data, "meta": []any{}})
		}
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/api/v1/records/"):
			n, _ := strconv.Atoi(r.URL.Query().Get("page"))
			page := json.RawMessage(`{"data": [], "meta": []}`)
			if n >= 1 && n <= len(sc.Pages) {
				page = sc.Pages[n-1]
			}
			var pg struct {
				Data json.RawMessage `json:"data"`
				Meta json.RawMessage `json:"meta"`
			}
			_ = json.Unmarshal(page, &pg)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": pg.Data, "meta": pg.Meta})
		case reDiffSchedule.MatchString(p):
			answer(sc.Schedules[reDiffSchedule.FindStringSubmatch(p)[1]])
		case p == "/api/v1/company/"+sc.Company+"/":
			answer(sc.Card)
		case p == "/api/v1/book_services/"+sc.Company:
			answer(map[string]any{"services": sc.Services})
		case p == "/api/v1/book_record/"+sc.Company:
			var out []any
			for i, id := range sc.RecordIDs {
				out = append(out, map[string]any{"id": i + 1, "record_id": id, "record_hash": "h"})
			}
			w.WriteHeader(http.StatusCreated)
			answer(out)
		case reDiffRecord.MatchString(p):
			rec := records[reDiffRecord.FindStringSubmatch(p)[1]]
			switch {
			case rec == nil:
				w.WriteHeader(http.StatusNotFound)
				answer(nil)
			case r.Method == http.MethodDelete:
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodPut:
				var in struct{ Comment string }
				_ = json.Unmarshal(body, &in)
				rec["comment"] = in.Comment
				answer(rec)
			default:
				answer(rec)
			}
		default:
			t.Errorf("%s: unexpected request %s %s", sc.Name, r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// slots are the 30-minute slots of the days, as the TypeScript side cut them.
func slots(days []string, loc *time.Location) [][2]time.Time {
	first, _ := time.ParseInLocation("2006-01-02", days[0], loc)
	last, _ := time.ParseInLocation("2006-01-02", days[len(days)-1], loc)
	end := time.Date(last.Year(), last.Month(), last.Day()+1, 0, 0, 0, 0, loc)
	var out [][2]time.Time
	for t := first; t.Before(end); t = t.Add(30 * time.Minute) {
		out = append(out, [2]time.Time{t, t.Add(30 * time.Minute)})
	}
	return out
}

func TestD11Differential(t *testing.T) {
	var input struct {
		SupportPhone string         `json:"support_phone"`
		SupportEmail string         `json:"support_email"`
		Scenarios    []diffScenario `json:"scenarios"`
	}
	var golden map[string]diffResult
	readJSON(t, "testdata/d11diff/scenarios.json", &input)
	readJSON(t, "testdata/d11diff/ts-golden.json", &golden)
	if len(input.Scenarios) == 0 || len(golden) != len(input.Scenarios) {
		t.Fatalf("%d scenarios, %d golden results: regenerate ts-golden.json", len(input.Scenarios), len(golden))
	}
	for _, sc := range input.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			want, ok := golden[sc.Name]
			if !ok {
				t.Fatalf("no golden result: regenerate ts-golden.json")
			}
			loc, err := time.LoadLocation(sc.TZ)
			if err != nil {
				t.Fatal(err)
			}
			srv, reqs := diffServer(t, sc)
			var courts []driver.CourtConfig
			for i, s := range sc.staff() {
				courts = append(courts, driver.CourtConfig{ID: "court-" + strconv.Itoa(i+1), Params: driver.Params{"staff_id": s}})
			}
			// before every booking of the scenarios
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			d, err := yclients.NewDriver(driver.Config{
				Timezone: loc,
				CRM: driver.Params{"api_url": srv.URL + "/api/v1", "company_id": sc.Company,
					"client_phone": input.SupportPhone, "client_name": "PadelApp", "client_email": input.SupportEmail},
				Secrets:    map[string]string{yclients.PartnerTokenEnv: "p", yclients.UserTokenEnv: "u"},
				Courts:     courts,
				HTTPClient: srv.Client(),
				Clock:      func() (time.Time, bool) { return now, true },
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			got := diffResult{}
			var gotErr error
			switch sc.Kind {
			case "occupancy":
				first, _ := time.ParseInLocation("2006-01-02", sc.Days[0], loc)
				last, _ := time.ParseInLocation("2006-01-02", sc.Days[len(sc.Days)-1], loc)
				var ids []string
				for _, c := range courts {
					ids = append(ids, c.ID)
				}
				occ, err := d.Occupancy(ctx, driver.OccupancyQuery{Courts: ids, From: first, To: time.Date(last.Year(), last.Month(), last.Day()+1, 0, 0, 0, 0, loc)})
				gotErr = err
				if err == nil {
					if !occ.Complete {
						t.Fatalf("occupancy not complete")
					}
					got.Busy = map[string][]string{}
					for i, c := range courts {
						staff := sc.staff()[i]
						got.Busy[staff] = []string{}
						for _, sl := range slots(sc.Days, loc) {
							for _, iv := range occ.Intervals {
								if iv.Court == c.ID && iv.Start.Before(sl[1]) && iv.End.After(sl[0]) {
									got.Busy[staff] = append(got.Busy[staff], sl[0].UTC().Format(time.RFC3339))
									break
								}
							}
						}
					}
				}
			case "create":
				start, _ := time.Parse(time.RFC3339, sc.Start)
				end, _ := time.Parse(time.RFC3339, sc.End)
				b, err := d.Create(ctx, driver.CreateRequest{Court: "court-1", Start: start, End: end, Kind: driver.KindGame, Note: "note", Partner: "padelapp"})
				gotErr = err
				if err == nil {
					got.ID = strings.ReplaceAll(strings.TrimPrefix(b.ID, "y-"), "-", ",")
				}
			case "cancel":
				gotErr = d.Cancel(ctx, "y-"+joinIDs(sc.Records, "-"), "padelapp")
			case "paid":
				gotErr = d.MarkPaid(ctx, "y-"+joinIDs(sc.Records, "-"), "padelapp")
			}
			if (gotErr != nil) != (want.Error != "") {
				t.Fatalf("error: gateway %v, D11 %q", gotErr, want.Error)
			}
			if !reflect.DeepEqual(got.Busy, want.Busy) && !(len(got.Busy) == 0 && len(want.Busy) == 0) {
				t.Fatalf("occupied slots differ:\n gateway %v\n D11     %v", got.Busy, want.Busy)
			}
			if got.ID != want.ID {
				t.Fatalf("booking id: gateway %q, D11 %q", got.ID, want.ID)
			}
			compareRequests(t, sc.Kind, *reqs, want.Requests)
		})
	}
}

func joinIDs(records []map[string]any, sep string) string {
	var ids []string
	for _, r := range records {
		ids = append(ids, strconv.FormatInt(int64(r["id"].(float64)), 10))
	}
	return strings.Join(ids, sep)
}

// compareRequests: the writes (and the services read that decides what a
// booking books) must be the same requests — method, path, token, query,
// body. Known, intended differences: the comment of book_record (D11 writes
// the player, the gateway its marker and the partner's note), and the
// reads, which differ by design (the gateway reads records from the day
// before and reads a record before deleting it).
func compareRequests(t *testing.T, kind string, got, want []diffRequest) {
	t.Helper()
	pick := func(rs []diffRequest) []diffRequest {
		var out []diffRequest
		for _, r := range rs {
			if r.Method != http.MethodGet || r.Path != "" && strings.HasPrefix(r.Path, "/book_services/") {
				out = append(out, r)
			}
		}
		return out
	}
	g, w := pick(got), pick(want)
	if len(g) != len(w) {
		t.Fatalf("%s: %d writes, D11 %d:\n gateway %s\n D11     %s", kind, len(g), len(w), dump(g), dump(w))
	}
	for i := range g {
		gb, wb := normBody(t, g[i].Body), normBody(t, w[i].Body)
		if kind == "create" && g[i].Method == http.MethodPost {
			gc, _ := gb["comment"].(string)
			if !strings.HasPrefix(gc, "[cg:padelapp]") {
				t.Fatalf("book_record comment %q lacks the marker", gc)
			}
			if _, ok := wb["comment"].(string); !ok {
				t.Fatalf("D11 book_record without a comment")
			}
			delete(gb, "comment")
			delete(wb, "comment")
		}
		if g[i].Method != w[i].Method || g[i].Path != w[i].Path || g[i].Auth != w[i].Auth ||
			!reflect.DeepEqual(normQuery(g[i].Query), normQuery(w[i].Query)) || !reflect.DeepEqual(gb, wb) {
			t.Fatalf("request %d differs:\n gateway %s %s %s %v %v\n D11     %s %s %s %v %v", i,
				g[i].Method, g[i].Path, g[i].Auth, normQuery(g[i].Query), gb, w[i].Method, w[i].Path, w[i].Auth, normQuery(w[i].Query), wb)
		}
	}
}

func normBody(t *testing.T, b json.RawMessage) map[string]any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body %s: %v", b, err)
	}
	return m
}

func normQuery(q map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range q {
		switch n := v.(type) {
		case float64:
			out[k] = strconv.FormatFloat(n, 'f', -1, 64)
		default:
			out[k] = strings.TrimSpace(strings.Trim(jsonString(v), `"`))
		}
	}
	return out
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }

func dump(rs []diffRequest) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Method+" "+r.Path)
	}
	sort.Strings(s)
	return strings.Join(s, ", ")
}
