package pairtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
)

// Obviously fake identifiers of the Viva studio.
const (
	VivaStudio = "00000000-0000-4000-8000-00000000b1aa"
	VivaRoom1  = "00000000-0000-4000-8000-00000000b1b1"
	VivaRoom2  = "00000000-0000-4000-8000-00000000b1b2"
	vivaPrices = "00000000-0000-4000-8000-00000000b1c1"
	vivaUser   = "admin@example.test"
	vivaPass   = "stub-password-5d1c"
)

const vivaPricing = `{"id": "` + vivaPrices + `", "name": "Test", "basePrices": [
 {"condition": {"type": "TIME", "days": ["MONDAY","TUESDAY","WEDNESDAY","THURSDAY","FRIDAY","SATURDAY","SUNDAY"], "timeStart": "07:00:00", "timeEnd": "00:00:00"}, "valueFrom": 2000}]}`

// VivaStub is the primary of a Viva + Lunda pair.
type VivaStub struct {
	Stub   *vivastub.Stub
	Server *httptest.Server
	rooms  map[string]string // gateway court -> room
}

func (v *VivaStub) Type() string { return "viva" }

func (v *VivaStub) court(room string) string {
	for c, r := range v.rooms {
		if strings.EqualFold(r, room) {
			return c
		}
	}
	return ""
}

func vivaTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Book books as the club's admin: an exercise with a client.
func (v *VivaStub) Book(t testing.TB, court string, start, end time.Time) string {
	return v.Stub.SeedExercise(map[string]any{
		"timeFrom": vivaTime(start), "timeTo": vivaTime(end),
		"room": map[string]any{"id": v.rooms[court], "name": "PRIVACY-MARKER-ROOM-NAME-e3f1"}, "studio": map[string]any{"id": VivaStudio},
		"comment": "Клиент PRIVACY-MARKER-COMMENT-a51f", "clientsCount": 1, "canceled": false,
		"clients": []any{map[string]any{"name": "PRIVACY-MARKER-FIRSTNAME-91c2", "phone": "PRIVACY-MARKER-PHONE-79990000000"}},
	})
}

// BookBreak closes the court as the club's admin would, by a break.
func (v *VivaStub) BookBreak(court string, start, end time.Time, comment string) string {
	return v.Stub.SeedBreak(map[string]any{
		"title": "Ремонт", "timeFrom": vivaTime(start), "timeTo": vivaTime(end),
		"room": map[string]any{"id": v.rooms[court]}, "studio": map[string]any{"id": VivaStudio}, "comment": comment, "canceled": false,
	})
}

func (v *VivaStub) edit(id string, f func(map[string]any)) {
	if v.Stub.Exercise(id) != nil {
		v.Stub.EditExercise(id, f)
		return
	}
	v.Stub.EditBreak(id, f)
}

func (v *VivaStub) Cancel(t testing.TB, id string) {
	v.edit(id, func(r map[string]any) { r["canceled"] = true })
}

func (v *VivaStub) Move(t testing.TB, id string, start, end time.Time) {
	v.edit(id, func(r map[string]any) { r["timeFrom"], r["timeTo"] = vivaTime(start), vivaTime(end) })
}

func (v *VivaStub) Annotate(t testing.TB, id, note string) {
	v.edit(id, func(r map[string]any) { r["comment"] = note })
}

func (v *VivaStub) Fail(t testing.TB, write string) {
	switch write {
	case "create":
		// a partner's booking or a mirror (both breaks: the club has no
		// service for the partners' games); not "/" — the token's POST would
		// match it before the gateway has logged in, a login refused
		v.Stub.FailNext(http.MethodPost, "/breaks", http.StatusBadRequest, `{"code":"BAD_REQUEST"}`)
	case "delete":
		v.Stub.FailNext(http.MethodDelete, "/", http.StatusInternalServerError, `{"code":"ERROR"}`)
	default:
		t.Fatalf("no failure %q", write)
	}
}

// Live: a record is the gateway's by its comment's marker; a mirror is a
// break.
func (v *VivaStub) Live(t testing.TB) []Entry {
	var out []Entry
	for kind, recs := range map[string][]map[string]any{"exercises": v.Stub.Exercises(), "breaks": v.Stub.Breaks()} {
		for _, r := range recs {
			if c, _ := r["canceled"].(bool); c {
				continue
			}
			room, _ := r["room"].(map[string]any)
			rid, _ := room["id"].(string)
			court := v.court(rid)
			if court == "" {
				continue
			}
			start, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(r["timeFrom"]))
			end, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(r["timeTo"]))
			note, _ := r["comment"].(string)
			id, _ := r["id"].(string)
			e := Entry{Handle: id, Court: court, Start: start, End: end, Kind: Club, Note: note}
			switch n := strings.TrimSpace(note); {
			case kind == "breaks" && strings.HasPrefix(n, "[cg-mirror"):
				e.Kind = Mirror
			case strings.HasPrefix(n, "[cg:"):
				e.Kind = Partner
			}
			out = append(out, e)
		}
	}
	return out
}

// VivaLunda serves a club of Viva (primary) and Lunda (mirror).
func VivaLunda(t *testing.T, o Options) *Env {
	t.Helper()
	stub := vivastub.New(vivaUser, vivaPass, VivaStudio)
	stub.AddRoom(VivaRoom1, "Корт 1")
	stub.AddRoom(VivaRoom2, "Корт 2")
	stub.SetPricing(vivaPrices, vivaPricing)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	v := &VivaStub{Stub: stub, Server: srv, rooms: map[string]string{"court-1": VivaRoom1, "court-2": VivaRoom2}}
	l := NewLunda(t)
	spec := core.ClubSpec{ID: "test-club", Name: "Тестовый клуб", Timezone: lundaZoneName, SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "07:00-23:00"}, CRM: map[string]any{"studio": VivaStudio}, Mirror: o.Mirror}
	if spec.Mirror == nil {
		spec.Mirror = &core.MirrorSpec{}
	}
	for _, c := range []string{"court-1", "court-2"} {
		params := map[string]any{"viva_room": v.rooms[c], "price_list": vivaPrices}
		for k, val := range l.CourtParams(c) {
			params[k] = val
		}
		spec.Courts = append(spec.Courts, core.CourtSpec{ID: c, Name: "Корт " + c[len(c)-1:], Params: params})
	}
	crm := fmt.Sprintf("crm: { type: viva, username: %s, auth_url: %s/token, api_url: %s/api/v1 }\n", vivaUser, srv.URL, srv.URL)
	mk := func(t *testing.T) built {
		return build(t, crm, l.YAML(), spec, map[string]string{"CRM_PASSWORD": vivaPass}, srv.Client(), l.Stub, l.Server)
	}
	n := mk(t)
	return (&Env{Primary: v, Mirror: l, Courts: []string{"court-1", "court-2"},
		Location: n.b.Settings.Location, Partner: "booker", LundaStub: l.Stub}).with(n, mk)
}

// Raw: the exercise or break as the stub stores it.
func (v *VivaStub) Raw(t testing.TB, id string) string {
	r := v.Stub.Exercise(id)
	if r == nil {
		r = v.Stub.Break(id)
	}
	if r == nil {
		return ""
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Incomplete: Viva's driver has no incomplete answer a stub can give once
// (a page-sized list is asked again day by day), so the next list of
// exercises fails.
func (v *VivaStub) Incomplete(t testing.TB) (string, func()) {
	v.Stub.FailNext(http.MethodGet, "/exercises", http.StatusInternalServerError, "{}")
	return "failed", func() {}
}

var _ = contract.BookKey
