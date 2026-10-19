package pairtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

// Obviously fake identifiers and secrets of the YCLIENTS branch.
const (
	YCompany      = "9900001"
	YStaff1       = "9910001"
	YStaff2       = "9910002"
	yPartnerToken = "stub-partner-token-6b1f0c2d"
	yUserToken    = "stub-user-token-91aa3e70"
	yServicePhone = "79000000000"
)

// YclientsStub is the primary of a YCLIENTS + Lunda pair.
type YclientsStub struct {
	Stub   *yclientsstub.Stub
	Server *httptest.Server
	staff  map[string]string // gateway court -> staff
}

func (y *YclientsStub) Type() string { return "yclients" }

func (y *YclientsStub) id(t testing.TB, handle string) int64 {
	n, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		t.Fatalf("not a record id: %q", handle)
	}
	return n
}

// Book books as the club's admin: a record of a client of the club.
func (y *YclientsStub) Book(t testing.TB, court string, start, end time.Time) string {
	id := y.Stub.Seed(yclientsstub.Record{StaffID: y.staff[court], Start: start, Length: int(end.Sub(start) / time.Second),
		Comment: "Клиент PRIVACY-MARKER-COMMENT-a51f", ClientName: "PRIVACY-MARKER-NAME-91c2", ClientPhone: "79990000001",
		Services: []map[string]any{{"id": 1, "cost": 2000}}})
	return strconv.FormatInt(id, 10)
}

func (y *YclientsStub) Cancel(t testing.TB, handle string) { y.Stub.Delete(y.id(t, handle)) }

func (y *YclientsStub) Move(t testing.TB, handle string, start, end time.Time) {
	y.Stub.EditRecord(y.id(t, handle), func(r *yclientsstub.Record) { r.Start, r.Length = start, int(end.Sub(start)/time.Second) })
}

func (y *YclientsStub) Annotate(t testing.TB, handle, note string) {
	y.Stub.EditRecord(y.id(t, handle), func(r *yclientsstub.Record) { r.Comment = note })
}

func (y *YclientsStub) Fail(t testing.TB, write string) {
	switch write {
	case "create":
		y.Stub.FailNext("/api/v1/book_record", http.StatusBadRequest)
	case "delete":
		y.Stub.FailNext("/api/v1/record/", http.StatusInternalServerError)
	default:
		t.Fatalf("no failure %q", write)
	}
}

// Live: a record of the service client is the gateway's by its comment's
// marker. The records of one booking or mirror (a set tag) are one entry.
func (y *YclientsStub) Live(t testing.TB) []Entry {
	var out []Entry
	sets := map[string]int{} // set tag -> index in out
	for _, r := range y.Stub.Records() {
		court := ""
		for c, s := range y.staff {
			if s == r.StaffID {
				court = c
			}
		}
		if r.Deleted || court == "" {
			continue
		}
		e := Entry{Handle: strconv.FormatInt(r.ID, 10), Court: court, Start: r.Start, End: r.Start.Add(time.Duration(r.Length) * time.Second), Kind: Club, Note: r.Comment}
		if r.ClientPhone == yServicePhone {
			switch n := strings.TrimSpace(r.Comment); {
			case strings.HasPrefix(n, "[cg-mirror"):
				e.Kind = Mirror
			case strings.HasPrefix(n, "[cg:"):
				e.Kind = Partner
			}
		}
		if i := strings.Index(r.Comment, "[cg-set:"); i >= 0 && e.Kind != Club {
			key := court + r.Comment[i:]
			if j, ok := sets[key]; ok {
				if e.Start.Before(out[j].Start) {
					out[j].Start = e.Start
				}
				if e.End.After(out[j].End) {
					out[j].End = e.End
				}
				continue
			}
			sets[key] = len(out)
			e.Note = strings.TrimSpace(r.Comment[:i])
		}
		out = append(out, e)
	}
	return out
}

// YclientsLunda serves a club of YCLIENTS (primary) and Lunda (mirror).
func YclientsLunda(t *testing.T, o Options) *Env {
	t.Helper()
	clock := o.clock(t)
	zone, _ := time.LoadLocation(lundaZoneName)
	stub := yclientsstub.New(yPartnerToken, yUserToken, "", "", YCompany, zone)
	services := func(base int64) []yclientsstub.Service {
		return []yclientsstub.Service{
			{ID: base + 1, Title: "Аренда 30 мин", SeanceLength: 1800, Price: 1000},
			{ID: base + 2, Title: "Аренда 1 ч", SeanceLength: 3600, Price: 2000},
			{ID: base + 3, Title: "Аренда 1,5 ч", SeanceLength: 5400, Price: 3000},
			{ID: base + 4, Title: "Аренда 2 ч", SeanceLength: 7200, Price: 4000},
		}
	}
	stub.AddStaff(yclientsstub.Staff{ID: YStaff1, Name: "Корт 1", Bookable: true, Prepaid: "allowed", Services: services(31000)})
	stub.AddStaff(yclientsstub.Staff{ID: YStaff2, Name: "Корт 2", Bookable: true, Prepaid: "allowed", Services: services(32000)})
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	y := &YclientsStub{Stub: stub, Server: srv, staff: map[string]string{"court-1": YStaff1, "court-2": YStaff2}}
	l := NewLunda(t)
	l.Stub.Now = clock
	spec := core.ClubSpec{ID: "test-club", Name: "Тестовый клуб", Timezone: lundaZoneName, SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "07:00-23:00"},
		CRM:   map[string]any{"company_id": YCompany, "client_phone": "+7 900 000-00-00"}, Mirror: o.Mirror}
	if spec.Mirror == nil {
		spec.Mirror = &core.MirrorSpec{}
	}
	for _, c := range []string{"court-1", "court-2"} {
		params := map[string]any{yclients.RoomKey: y.staff[c]}
		for k, val := range l.CourtParams(c) {
			params[k] = val
		}
		spec.Courts = append(spec.Courts, core.CourtSpec{ID: c, Name: "Корт " + c[len(c)-1:], Params: params})
	}
	crm := fmt.Sprintf("crm: { type: yclients, api_url: %s/api/v1 }\n", srv.URL)
	env := map[string]string{yclients.PartnerTokenEnv: yPartnerToken, yclients.UserTokenEnv: yUserToken}
	mk := func(t *testing.T) built {
		return build(t, crm, l.YAML(), spec, env, srv.Client(), l.Stub, l.Server, clock)
	}
	n := mk(t)
	return (&Env{Primary: y, Mirror: l, Courts: []string{"court-1", "court-2"},
		Location: n.b.Settings.Location, Partner: "booker", LundaStub: l.Stub}).with(n, mk, clock)
}

// Raw: the record as the stub stores it.
func (y *YclientsStub) Raw(t testing.TB, handle string) string {
	r, ok := y.Stub.Record(y.id(t, handle))
	if !ok {
		return ""
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Incomplete: YCLIENTS's driver tells an incomplete list of records by its
// total_count and refuses it (an error, not an incomplete answer): every
// list promises a record more than it has until undone.
func (y *YclientsStub) Incomplete(t testing.TB) (string, func()) {
	y.Stub.LieTotal(1)
	return "failed", func() { y.Stub.LieTotal(0) }
}
