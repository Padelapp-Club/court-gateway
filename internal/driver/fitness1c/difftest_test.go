package fitness1c

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

// The differential check: the grid rules of this driver against the
// backend adapter's (fitness1c-crm.service.ts) on the same bodies.
// testdata/difftest/cases.json holds the adapter's fixtures and variations
// of them (gen_cases.py); ts-golden.json is what the TypeScript adapter answered
// to each (ts/fitness1c-difftest.spec.ts, run against the monorepo commit
// named in ts-golden.source). Every busy half-hour of every room and day,
// every probe of an own slot, every refusal and every window's days must
// be the same.

type diffCase struct {
	Name     string              `json:"name"`
	TimeZone string              `json:"timeZone"`
	Days     []string            `json:"days"`
	PerDay   [][]json.RawMessage `json:"perDay"`
	Slots    [][2]string         `json:"slots"`
}

type diffProbe struct {
	Busy         bool `json:"busy"`
	ExplicitFree bool `json:"explicitFree"`
}

type diffAnswer struct {
	Name  string `json:"name"`
	Error bool   `json:"error"`
	Rooms map[string]struct {
		Busy   map[string][]string `json:"busy"`
		Probes []diffProbe         `json:"probes"`
	} `json:"rooms"`
}

type diffWindow struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	TimeZone string    `json:"timeZone"`
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestDifferentialAgainstBackendAdapter(t *testing.T) {
	var in struct {
		Cases   []diffCase   `json:"cases"`
		Windows []diffWindow `json:"windows"`
	}
	var golden struct {
		Cases   []diffAnswer `json:"cases"`
		Windows [][]string   `json:"windows"`
	}
	readJSON(t, "testdata/difftest/cases.json", &in)
	readJSON(t, "testdata/difftest/ts-golden.json", &golden)
	if len(in.Cases) != len(golden.Cases) || len(in.Windows) != len(golden.Windows) || len(in.Cases) < 20 {
		t.Fatalf("cases %d / answers %d, windows %d / %d: regenerate ts-golden.json", len(in.Cases), len(golden.Cases), len(in.Windows), len(golden.Windows))
	}
	compared := 0
	for i, c := range in.Cases {
		want := golden.Cases[i]
		t.Run(c.Name, func(t *testing.T) {
			if want.Name != c.Name {
				t.Fatalf("answer %q for case %q: regenerate ts-golden.json", want.Name, c.Name)
			}
			got := answer(t, c)
			if got.Error != want.Error {
				t.Fatalf("refused: driver %v, backend %v", got.Error, want.Error)
			}
			if !reflect.DeepEqual(roomsOf(got), roomsOf(want)) {
				t.Fatalf("rooms: driver %v, backend %v", roomsOf(got), roomsOf(want))
			}
			for room, w := range want.Rooms {
				g := got.Rooms[room]
				for day, keys := range w.Busy {
					if !reflect.DeepEqual(nonNil(g.Busy[day]), nonNil(keys)) {
						t.Errorf("%s %s busy: driver %v, backend %v", room, day, g.Busy[day], keys)
					}
					compared += 48
				}
				if !reflect.DeepEqual(g.Probes, w.Probes) && !(len(g.Probes) == 0 && len(w.Probes) == 0) {
					t.Errorf("%s probes %v: driver %v, backend %v", room, c.Slots, g.Probes, w.Probes)
				}
				compared += len(w.Probes)
			}
		})
	}
	for i, w := range in.Windows {
		loc, err := time.LoadLocation(w.TimeZone)
		if err != nil {
			t.Fatal(err)
		}
		if got := daysBetween(w.From, w.To, loc); !reflect.DeepEqual(got, golden.Windows[i]) {
			t.Errorf("window %d days: driver %v, backend %v", i, got, golden.Windows[i])
		}
	}
	t.Logf("%d cases, %d half-hours and probes, %d windows compared", len(in.Cases), compared, len(in.Windows))
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func roomsOf(a diffAnswer) []string {
	var out []string
	for r := range a.Rooms {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// answer is the driver's answer to one case, in the backend's form.
func answer(t *testing.T, c diffCase) diffAnswer {
	t.Helper()
	loc, err := time.LoadLocation(c.TimeZone)
	if err != nil {
		t.Fatal(err)
	}
	perDay := make([][]gridRoom, len(c.PerDay))
	for i, day := range c.PerDay {
		raw, _ := json.Marshal(day)
		rooms, err := decodeDay(raw)
		if err != nil {
			return diffAnswer{Name: c.Name, Error: true}
		}
		perDay[i] = rooms
	}
	g, err := normalizeGrid(c.Days, perDay)
	if err != nil {
		return diffAnswer{Name: c.Name, Error: true}
	}
	out := diffAnswer{Name: c.Name}
	out.Rooms = map[string]struct {
		Busy   map[string][]string `json:"busy"`
		Probes []diffProbe         `json:"probes"`
	}{}
	for id, rg := range g.rooms {
		busy := map[string][]string{}
		var all []span
		for _, day := range c.Days {
			spans, err := rg.busySpans(day, loc, seance, nil)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, spans...)
			keys := []string{}
			for _, s := range spans {
				for h := s.Start; h.Before(s.End); h = h.Add(seance * time.Minute) {
					keys = append(keys, h.In(loc).Format("15:04"))
				}
			}
			busy[day] = keys
		}
		var probes []diffProbe
		for _, p := range c.Slots {
			s, ok := parseSpan(p[0], p[1], loc)
			if !ok {
				t.Fatalf("probe %v", p)
			}
			hit := false
			for _, b := range all {
				hit = hit || (b.Start.Before(s.End) && b.End.After(s.Start))
			}
			probes = append(probes, diffProbe{Busy: hit, ExplicitFree: !hit})
		}
		out.Rooms[id] = struct {
			Busy   map[string][]string `json:"busy"`
			Probes []diffProbe         `json:"probes"`
		}{busy, probes}
	}
	return out
}
