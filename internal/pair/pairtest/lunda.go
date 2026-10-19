package pairtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
)

// Obviously fake identifiers of the Lunda club: nothing here refers to a
// real club.
const (
	LundaClub     = "00000000-0000-4000-8000-00000000a1aa"
	LundaCourt1   = "00000000-0000-4000-8000-00000000a1c1"
	LundaCourt2   = "00000000-0000-4000-8000-00000000a1c2"
	LundaService  = "00000000-0000-4000-8000-00000000a1e1"
	LundaVisitor  = "00000000-0000-4000-8000-00000000a1e2"
	lundaZoneName = "Europe/Moscow"
)

// LundaStub is the mirror of a pair: Lunda's stub and its server.
type LundaStub struct {
	Stub   *lundastub.Stub
	Server *httptest.Server
	courts map[string]string // gateway court -> courtUid
}

// NewLunda starts the Lunda stub of a pair: two courts of the club.
func NewLunda(t *testing.T) *LundaStub {
	t.Helper()
	stub := lundastub.New(lundastub.Club{UID: LundaClub, Name: "Тестовый клуб в Лунде"})
	stub.Zone, _ = time.LoadLocation(lundaZoneName)
	stub.AddCourt(LundaCourt1, "Корт 1")
	stub.AddCourt(LundaCourt2, "Корт 2")
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return &LundaStub{Stub: stub, Server: srv, courts: map[string]string{"court-1": LundaCourt1, "court-2": LundaCourt2}}
}

// YAML is the installation's mirror section.
func (l *LundaStub) YAML() string {
	return "mirror: { type: lunda, api_url: " + l.Server.URL + "/api, service_client: " + LundaService + " }\n"
}

// CourtParams are the Lunda keys of a gateway court.
func (l *LundaStub) CourtParams(court string) map[string]any {
	return map[string]any{lunda.CourtKey: l.courts[court]}
}

func (l *LundaStub) Type() string { return "lunda" }

func (l *LundaStub) court(uid string) string {
	for c, u := range l.courts {
		if u == uid {
			return c
		}
	}
	return ""
}

// Book books as the club's staff: on a client of the club.
func (l *LundaStub) Book(t testing.TB, court string, start, end time.Time) string {
	return l.Stub.SeedBooking(lundastub.Booking{Court: l.courts[court], Begin: start, End: end, Customer: LundaVisitor, Price: 1500,
		Notes: "Клиент клуба PRIVACY-MARKER-COMMENT-a51f",
		Extra: map[string]any{"customer": map[string]any{"uid": LundaVisitor, "firstName": "PRIVACY-MARKER-FIRSTNAME-91c2",
			"phone": "PRIVACY-MARKER-PHONE-79990000000"}}})
}

// BookOnService books as the club's staff on the service client (the
// club's own tournaments), with notes.
func (l *LundaStub) BookOnService(court string, start, end time.Time, notes string) string {
	return l.Stub.SeedBooking(lundastub.Booking{Court: l.courts[court], Begin: start, End: end, Customer: LundaService, Price: 1500, Notes: notes})
}

func (l *LundaStub) Cancel(t testing.TB, id string) {
	l.Stub.EditBooking(id, func(b *lundastub.Booking) { b.Status = "CANCELLED" })
}

func (l *LundaStub) Move(t testing.TB, id string, start, end time.Time) {
	l.Stub.EditBooking(id, func(b *lundastub.Booking) { b.Begin, b.End = start, end })
}

func (l *LundaStub) Annotate(t testing.TB, id, note string) {
	l.Stub.EditBooking(id, func(b *lundastub.Booking) { b.Notes = note })
}

func (l *LundaStub) Fail(t testing.TB, write string) {
	switch write {
	case "create":
		l.Stub.FailNext(http.MethodPost, "/club/booking/booking-create", http.StatusBadRequest, `{"message":"refused by the stub"}`)
	case "delete":
		l.Stub.FailNext(http.MethodDelete, "/club/booking/booking-cancel", http.StatusBadRequest, `{"message":"refused by the stub"}`)
	default:
		t.Fatalf("no failure %q", write)
	}
}

// Raw: the booking as the stub stores it.
func (l *LundaStub) Raw(t testing.TB, id string) string {
	b := l.Stub.Booking(id)
	if b == nil {
		return ""
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Incomplete: a day of the schedule three days ahead cannot be read: the
// read is incomplete from that day on.
func (l *LundaStub) Incomplete(t testing.TB) (string, func()) {
	n := time.Now().In(l.Stub.Zone)
	day := time.Date(n.Year(), n.Month(), n.Day()+3, 0, 0, 0, 0, l.Stub.Zone).Format("2006-01-02")
	l.Stub.FailNextQuery(http.MethodGet, "/club/booking/slots-load", "date="+day, http.StatusServiceUnavailable, "{}")
	return "incomplete", func() {}
}

// Live: a booking on the service client is the gateway's by its notes.
func (l *LundaStub) Live(t testing.TB) []Entry {
	var out []Entry
	for _, b := range l.Stub.Bookings() {
		court := l.court(b.Court)
		if b.Status == "CANCELLED" || court == "" {
			continue
		}
		e := Entry{Handle: b.ID, Court: court, Start: b.Begin, End: b.End, Kind: Club, Note: b.Notes}
		if b.Customer == LundaService {
			switch n := strings.TrimSpace(b.Notes); {
			case strings.HasPrefix(n, "[cg-mirror"):
				e.Kind = Mirror
			case strings.HasPrefix(n, "[cg:"):
				e.Kind = Partner
			}
		}
		out = append(out, e)
	}
	return out
}

// CheckMirrorBookings fails t unless every mirror of the stub is a
// gateway booking as the gateway writes it to Lunda: PAID, no payment, net price 0.
func (l *LundaStub) CheckMirrorBookings(t testing.TB) {
	t.Helper()
	for _, b := range l.Stub.Bookings() {
		if b.Status == "CANCELLED" || !strings.HasPrefix(strings.TrimSpace(b.Notes), "[cg") {
			continue
		}
		if b.Status != "PAID" || len(b.Payments) != 0 || b.Price+b.Adjustment != 0 || b.Customer != LundaService || b.Goal != "GENERAL" {
			t.Errorf("a gateway booking in Lunda is not PAID at 0 without payments on the service client: %+v", b)
		}
	}
}
