package pairtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c/fitness1cstub"
)

// Obviously fake identifiers and secrets of the 1C:Fitness club.
const (
	F1CClub       = "00000000-0000-4000-8000-00000000c1aa"
	F1CRoom1      = "00000000-0000-4000-8000-00000000c1b1"
	F1CRoom2      = "00000000-0000-4000-8000-00000000c1b2"
	F1CService    = "00000000-0000-4000-8000-00000000c1e1"
	f1cUser       = "padelapp-ext"
	f1cPass       = "stub-password-1c-77aa"
	f1cKey        = "stub-apikey-1c-0d9e"
	F1CTechPhone  = "+79000000010"
	F1CMirrorTech = "+79000000011"
)

// Fitness1CStub is the primary of a 1C:Fitness + Lunda pair.
type Fitness1CStub struct {
	Stub   *fitness1cstub.Stub
	Server *httptest.Server
	rooms  map[string]string // gateway court -> room
}

func (f *Fitness1CStub) Type() string { return "fitness1c" }

// Book books as the club's admin: a rental of a client of the club.
func (f *Fitness1CStub) Book(t testing.TB, court string, start, end time.Time) string {
	return f.Stub.SeedRental(f.rooms[court], start, end, false, map[string]any{"client_name": "PRIVACY-MARKER-NAME-91c2", "phone": "PRIVACY-MARKER-PHONE-79990000001"})
}

func (f *Fitness1CStub) Cancel(t testing.TB, id string) { f.Stub.CancelRental(id) }

func (f *Fitness1CStub) Move(t testing.TB, id string, start, end time.Time) {
	for _, r := range f.Stub.RentalViews() {
		if r.ID == id {
			f.Stub.MoveRental(id, r.Room, r.Service, start, end)
		}
	}
}

// Annotate: a rental of 1C has no comment; nothing to write.
func (f *Fitness1CStub) Annotate(t testing.TB, id, note string) {}

func (f *Fitness1CStub) Fail(t testing.TB, write string) {
	switch write {
	case "create":
		f.Stub.Fail("POST /rental/", fitness1cstub.Failure{Code: 1118})
	case "delete":
		f.Stub.Fail("DELETE /rental/", fitness1cstub.Failure{Status: http.StatusInternalServerError})
	default:
		t.Fatalf("no failure %q", write)
	}
}

// Live: a rental is the gateway's by its client: the technical client's a
// partner's, the mirror client's a mirror.
func (f *Fitness1CStub) Live(t testing.TB) []Entry {
	var out []Entry
	for _, r := range f.Stub.RentalViews() {
		court := ""
		for c, room := range f.rooms {
			if strings.EqualFold(room, r.Room) {
				court = c
			}
		}
		if r.Canceled || court == "" {
			continue
		}
		e := Entry{Handle: r.ID, Court: court, Start: r.Start, End: r.End, Kind: Club}
		switch r.Phone {
		case F1CTechPhone:
			e.Kind = Partner
		case F1CMirrorTech:
			e.Kind = Mirror
		}
		out = append(out, e)
	}
	return out
}

// Fitness1CLunda serves a club of 1C:Fitness (primary) and Lunda (mirror).
func Fitness1CLunda(t *testing.T, o Options) *Env {
	t.Helper()
	zone, _ := time.LoadLocation(lundaZoneName)
	stub := fitness1cstub.New(f1cUser, f1cPass, f1cKey, F1CClub, "Тестовый клуб", zone)
	stub.AddService(fitness1cstub.Service{ID: F1CService, Title: "Аренда корта PadelApp", Seance: 30, Price: "0"})
	stub.AddRoom(F1CRoom1, "Корт 1")
	stub.AddRoom(F1CRoom2, "Корт 2")
	stub.AddClient(F1CTechPhone)
	stub.AddClient(F1CMirrorTech)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	f := &Fitness1CStub{Stub: stub, Server: srv, rooms: map[string]string{"court-1": F1CRoom1, "court-2": F1CRoom2}}
	l := NewLunda(t)
	spec := core.ClubSpec{ID: "test-club", Name: "Тестовый клуб", Timezone: lundaZoneName, SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "07:00-23:00"}, CRM: map[string]any{"studio": F1CClub},
		Services: map[string]map[string]any{"booking": {"service": F1CService}}, Mirror: o.Mirror}
	if spec.Mirror == nil {
		spec.Mirror = &core.MirrorSpec{}
	}
	for _, c := range []string{"court-1", "court-2"} {
		params := map[string]any{fitness1c.RoomKey: f.rooms[c]}
		for k, val := range l.CourtParams(c) {
			params[k] = val
		}
		spec.Courts = append(spec.Courts, core.CourtSpec{ID: c, Name: "Корт " + c[len(c)-1:], Params: params})
	}
	crm := fmt.Sprintf("crm: { type: fitness1c, base_url: %s, username: %s, client_phone: %q, partner: booker, mirror_client_phone: %q }\n",
		srv.URL, f1cUser, F1CTechPhone, F1CMirrorTech)
	env := map[string]string{"CRM_PASSWORD": f1cPass, fitness1c.APIKeyEnv: f1cKey}
	mk := func(t *testing.T) built { return build(t, crm, l.YAML(), spec, env, srv.Client(), l.Stub, l.Server) }
	n := mk(t)
	return (&Env{Primary: f, Mirror: l, Courts: []string{"court-1", "court-2"},
		Location: n.b.Settings.Location, Partner: "booker", LundaStub: l.Stub, MarkerlessPrimary: true}).with(n, mk)
}

// Raw: the rental as the stub stores it (RentalView: every field of it).
func (f *Fitness1CStub) Raw(t testing.TB, id string) string {
	for _, r := range f.Stub.RentalViews() {
		if r.ID == id {
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	return ""
}

// Incomplete: the next list of a technical client's rentals promises more
// than it holds (1C's pagination total): the read is incomplete.
func (f *Fitness1CStub) Incomplete(t testing.TB) (string, func()) {
	f.Stub.Fail("GET /appointments/", fitness1cstub.Failure{
		Body: `{"result":true,"data":[],"pagination_response":{"requested_offset":0,"page_size":30,"total_results":7}}`})
	return "incomplete", func() {}
}
