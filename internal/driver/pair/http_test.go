package pair_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c/fitness1cstub"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda/lundastub"
	"github.com/Padelapp-Club/court-gateway/internal/driver/pair"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients"
	"github.com/Padelapp-Club/court-gateway/internal/driver/yclients/yclientsstub"
)

const studio = "00000000-0000-4000-8000-0000000000aa"
const room = "00000000-0000-4000-8000-0000000000b1"
const room2 = "00000000-0000-4000-8000-0000000000b2"
const service = "service-client"

var now = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

type httpPair struct {
	d         *pair.Driver
	v         *vivastub.Stub
	l         *lundastub.Stub
	s         *core.Settings
	factories map[string]driver.Factory
	h         http.Handler
}

func newHTTPPair(t *testing.T) *httpPair {
	t.Helper()
	v := vivastub.New("user", "password", studio)
	v.Now = func() time.Time { return now }
	v.AddRoom(room, "one")
	v.AddRoom(room2, "two")
	vs := httptest.NewServer(v)
	t.Cleanup(vs.Close)
	l := lundastub.New(lundastub.Club{UID: "club", Name: "Тест"})
	l.Now = func() time.Time { return now }
	l.Zone = time.UTC
	l.TimeZoneID = "UTC"
	l.AddCourt("lc1", "one")
	l.AddCourt("lc2", "two")
	ls := httptest.NewServer(l)
	t.Cleanup(ls.Close)
	s, e := core.Load([]byte(fmt.Sprintf("hub: {url: https://hub.example.test, installation: test-net, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=}\ncrm: {type: viva, username: user, auth_url: %s/token, api_url: %s/api/v1}\nmirror: {type: lunda, api_url: %s/api, service_client: %s}\n", vs.URL, vs.URL, ls.URL, service)), func(k string) string {
		if k == "CRM_PASSWORD" {
			return "password"
		}
		if k == core.DefaultHubSecretEnv {
			return "hub-secret"
		}
		return ""
	})
	if e != nil {
		t.Fatal(e)
	}
	s.StateDir = t.TempDir()
	store := core.SessionStore(filepath.Join(s.StateDir, "mirror"))
	raw, _ := json.Marshal(map[string]any{"v": 1, "token": l.IssueSession(), "club": "club"})
	if e := store.SaveSession(raw); e != nil {
		t.Fatal(e)
	}
	s.Driver.Now = func() time.Time { return now }
	s, e = s.WithClub(core.ClubSpec{ID: "test-club", Name: "Тест", Timezone: "UTC", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "00:00-24:00"}, CRM: map[string]any{"studio": studio}, Courts: []core.CourtSpec{{ID: "court-1", Name: "one", Params: map[string]any{"viva_room": room, "lunda_court": "lc1"}}, {ID: "court-2", Name: "two", Params: map[string]any{"viva_room": room2, "lunda_court": "lc2"}}}}, viva.ClubCRMKeys)
	if e != nil {
		t.Fatal(e)
	}
	factories := map[string]driver.Factory{"viva": viva.New, "lunda": lunda.New}
	d, e := core.BuildDriver(s, factories)
	if e != nil {
		t.Fatal(e)
	}
	p := d.(*pair.Driver)
	t.Cleanup(p.Close)
	srv := core.NewServer(s, d, nil)
	h := srv.APIHandler()
	return &httpPair{d: p, v: v, l: l, s: s, factories: factories, h: h}
}
func (f *httpPair) seedV(id string, start time.Time, note string) string {
	return f.v.SeedExercise(map[string]any{"id": id, "timeFrom": start.Format(time.RFC3339), "timeTo": start.Add(time.Hour).Format(time.RFC3339), "room": map[string]any{"id": room}, "studio": map[string]any{"id": studio}, "clientsCount": 1, "comment": note, "canceled": false})
}
func (f *httpPair) reconcile(t *testing.T) {
	t.Helper()
	if e := f.d.Reconcile(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func (f *httpPair) count() int {
	n := 0
	for _, b := range f.l.Bookings() {
		if b.Status != "CANCELLED" {
			n++
		}
	}
	return n
}
func TestHTTPVivaLundaLifecycle(t *testing.T) {
	f := newHTTPPair(t)
	at := now.Add(24 * time.Hour)
	id := f.seedV("", at, "private-name-phone")
	f.reconcile(t)
	f.reconcile(t)
	if f.count() != 1 {
		t.Fatal("missing or duplicate mirror")
	}
	b := f.l.Bookings()[0]
	if b.Status != "PAID" || len(b.Payments) != 0 || !strings.HasPrefix(b.Notes, "[cg-mirror:viva:e-"+id+"]") {
		t.Fatalf("bad mirror %+v", b)
	}
	occ, e := f.d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: at, To: at.Add(time.Hour)})
	if e != nil || !occ.Complete || len(occ.Intervals) != 2 {
		t.Fatal(occ, e)
	}
	f.v.SeedExercise(map[string]any{"id": id, "timeFrom": at.Format(time.RFC3339), "timeTo": at.Add(time.Hour).Format(time.RFC3339), "room": map[string]any{"id": room}, "canceled": true})
	f.reconcile(t)
	if f.count() != 0 {
		t.Fatal("cancelled source left mirror")
	}
	lid := f.l.SeedBooking(lundastub.Booking{Court: "lc1", Begin: at, End: at.Add(time.Hour), Customer: "club-client", Notes: "private-name-phone"})
	f.reconcile(t)
	occ, e = f.d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: at, To: at.Add(time.Hour)})
	if e != nil || len(occ.Intervals) != 2 {
		t.Fatal(occ, e)
	}
	var bid string
	for _, iv := range occ.Intervals {
		if strings.HasPrefix(iv.ID, "b-") {
			bid = iv.ID
		}
	}
	if bid == "" {
		t.Fatal("Lunda did not reflect to a Viva break")
	}
	f.l.EditBooking(lid, func(b *lundastub.Booking) { b.Status = "CANCELLED" })
	f.reconcile(t)
	if b := f.v.Break(strings.TrimPrefix(bid, "b-")); b != nil && b["canceled"] != true {
		t.Fatal("Viva mirror survived cancellation")
	}
}
func TestHTTPPairCreateCancelAndRetry(t *testing.T) {
	f := newHTTPPair(t)
	req := driver.CreateRequest{Court: "court-1", Start: now.Add(24 * time.Hour), End: now.Add(25 * time.Hour), Kind: driver.KindGame, Partner: "booker"}
	b, e := f.d.Create(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	if f.count() != 1 || f.v.Break(strings.TrimPrefix(b.ID, "b-")) == nil {
		t.Fatal("not in both")
	}
	f.reconcile(t)
	if f.count() != 1 {
		t.Fatal("PadelApp was mirrored")
	}
	// Rebuild from CRM markers, with no local booking registry.
	f.d.Close()
	d, e := core.BuildDriver(f.s, f.factories)
	if e != nil {
		t.Fatal(e)
	}
	f.d = d.(*pair.Driver)
	t.Cleanup(f.d.Close)
	f.l.FailNext("DELETE", "/club/booking/booking-cancel", 503, `{}`)
	if e = f.d.Cancel(context.Background(), b.ID, "booker"); e == nil {
		t.Fatal("partial cancel reported success")
	}
	if e = f.d.Cancel(context.Background(), b.ID, "booker"); e != nil {
		t.Fatal(e)
	}
	if f.count() != 0 {
		t.Fatal("retry did not cancel remaining half")
	}
}
func TestHTTPPairRefusalRollsBack(t *testing.T) {
	f := newHTTPPair(t)
	f.l.FailNext("POST", "/club/booking/booking-create", 409, `{}`)
	_, e := f.d.Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: now.Add(24 * time.Hour), End: now.Add(25 * time.Hour), Kind: driver.KindGame, Partner: "booker"})
	if e == nil {
		t.Fatal("refusal accepted")
	}
	o, e := f.d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: now.Add(24 * time.Hour), To: now.Add(25 * time.Hour)})
	if e != nil || len(o.Intervals) != 0 {
		t.Fatal("half booking", o, e)
	}
}
func TestHTTPOwnershipClassAndDamagedMarker(t *testing.T) {
	f := newHTTPPair(t)
	at := now.Add(24 * time.Hour)
	vID := f.seedV("", at, "[cg-mirror:lunda:gone]")
	lID := f.l.SeedBooking(lundastub.Booking{Court: "lc2", Begin: at, End: at.Add(time.Hour), Customer: "club-client", Notes: "[cg-mirror:viva:gone]"})
	damaged := f.l.SeedBooking(lundastub.Booking{Court: "lc1", Begin: at.Add(3 * time.Hour), End: at.Add(4 * time.Hour), Customer: service, Notes: "[cg-mirror:viva:cut"})
	_ = f.d.Reconcile(context.Background())
	_ = f.d.Reconcile(context.Background())
	if f.v.Exercise(vID)["canceled"] == true || f.l.Booking(lID).Status == "CANCELLED" || f.l.Booking(damaged).Status == "CANCELLED" {
		t.Fatal("club or damaged record cancelled")
	}
}
func TestHTTPMappingValidation(t *testing.T) {
	for _, key := range []string{"lunda_court", "viva_room"} {
		t.Run(key, func(t *testing.T) {
			f := newHTTPPair(t)
			f.s.Driver.Courts[1].Params[key] = f.s.Driver.Courts[0].Params[key]
			if d, e := core.BuildDriver(f.s, f.factories); e == nil {
				d.(*pair.Driver).Close()
				t.Fatal("duplicate mapping accepted")
			}
		})
	}
}
func TestHTTPSharedCreateLock(t *testing.T) {
	f := newHTTPPair(t)
	at := now.Add(24 * time.Hour)
	f.seedV("", at, "")
	// The API's occupancy check refuses a slot occupied in either CRM even
	// before reconciliation has copied the club booking.
	body := fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`, at.Format(time.RFC3339), at.Add(time.Hour).Format(time.RFC3339))
	req := httptest.NewRequest("POST", "/v1/clubs/test-club/bookings", strings.NewReader(body))
	core.SetPartner(req, "booker")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestHTTPUnmappedMirrorAlarm(t *testing.T) {
	f := newHTTPPair(t)
	f.l.AddCourt("outside", "not mapped")
	id := f.l.SeedBooking(lundastub.Booking{Court: "outside", Begin: now.Add(24 * time.Hour), End: now.Add(25 * time.Hour), Customer: service, Notes: "[cg-mirror:viva:gone]"})
	e := f.d.Reconcile(context.Background())
	if e == nil || !strings.Contains(e.Error(), "MIRROR_UNMAPPED") {
		t.Fatal(e)
	}
	if f.l.Booking(id).Status == "CANCELLED" {
		t.Fatal("unmapped mirror deleted")
	}
}
func TestHTTPPartialLundaWriteCompensates(t *testing.T) {
	f := newHTTPPair(t)
	f.l.FailNext("POST", "/club/booking/update-courts-price-adjustment", 503, `{}`)
	_, e := f.d.Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: now.Add(24 * time.Hour), End: now.Add(25 * time.Hour), Kind: driver.KindGame, Partner: "booker"})
	if e == nil {
		t.Fatal("partial create accepted")
	}
	o, e := f.d.Occupancy(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: now.Add(24 * time.Hour), To: now.Add(25 * time.Hour)})
	if e != nil || len(o.Intervals) != 0 {
		t.Fatal("partial write left half", o, e)
	}
}

// The same smoke scenario runs over all supported HTTP stub pairs. T13
// can extend this factory with its fault traces without changing the loop.
func pairFor(t *testing.T, kind string) *httpPair {
	f := newHTTPPair(t)
	if kind == "viva" {
		return f
	}
	f.d.Close()
	s := f.s
	s.CRMType = kind
	for i := range s.Driver.Courts {
		s.Driver.Courts[i].Params = driver.Params{"lunda_court": fmt.Sprint("lc", i+1)}
	}
	switch kind {
	case "fitness1c":
		const svc = "00000000-0000-4000-8000-0000000000c1"
		st := fitness1cstub.New("user", "password", "key", studio, "club", time.UTC)
		st.Now = func() time.Time { return now }
		st.AddRoom(room, "one")
		st.AddRoom(room2, "two")
		st.AddClient("+79990000001")
		st.AddClient("+79990000002")
		st.AddService(fitness1cstub.Service{ID: svc, Seance: 30, Price: "0"})
		srv := httptest.NewServer(st)
		t.Cleanup(srv.Close)
		s.Driver.CRM = driver.Params{"base_url": srv.URL, "username": "user", "studio": studio, "client_phone": "+79990000001", "partner": "booker", "mirror_client_phone": "+79990000002", "mirror_service": svc}
		s.Driver.Secrets = map[string]string{fitness1c.APIKeyEnv: "key"}
		s.Driver.Services = map[string]driver.Params{"booking": {"service": svc}}
		s.Driver.Courts[0].Params["fitness1c_room"] = room
		s.Driver.Courts[1].Params["fitness1c_room"] = room2
		f.factories[kind] = fitness1c.New
	case "yclients":
		st := yclientsstub.New("partner-token", "user-token", "user", "password", "101", time.UTC)
		st.DefaultDay = yclientsstub.Day{Working: true, Slots: [][2]string{{"00:00", "00:00"}}}
		for i := range 2 {
			st.AddStaff(yclientsstub.Staff{ID: fmt.Sprint(201 + i), Name: "court", Bookable: true, Prepaid: "allowed", Services: []yclientsstub.Service{{ID: 301, Title: "hour", SeanceLength: 3600, Price: 1000}}})
		}
		srv := httptest.NewServer(st)
		t.Cleanup(srv.Close)
		s.Driver.CRM = driver.Params{"api_url": srv.URL + "/api/v1", "company_id": "101", "client_phone": "79000000000"}
		s.Driver.Password = ""
		s.Driver.Secrets = map[string]string{yclients.PartnerTokenEnv: "partner-token", yclients.UserTokenEnv: "user-token"}
		s.Driver.Services = nil
		s.Driver.Courts[0].Params["staff_id"] = "201"
		s.Driver.Courts[1].Params["staff_id"] = "202"
		f.factories[kind] = yclients.New
	}
	d, e := core.BuildDriver(s, f.factories)
	if e != nil {
		t.Fatal(e)
	}
	f.d = d.(*pair.Driver)
	t.Cleanup(f.d.Close)
	return f
}
func TestHTTPThreePairSmoke(t *testing.T) {
	for _, kind := range []string{"viva", "yclients", "fitness1c"} {
		t.Run(kind, func(t *testing.T) {
			f := pairFor(t, kind)
			at := now.Add(24 * time.Hour)
			q := driver.OccupancyQuery{Courts: []string{"court-1"}, From: at, To: at.Add(time.Hour)}
			b, e := f.d.Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: at, End: at.Add(time.Hour), Kind: driver.KindGame, Partner: "booker"})
			if e != nil {
				t.Fatal(e)
			}
			o, e := f.d.Occupancy(context.Background(), q)
			if e != nil || len(o.Intervals) != 2 || !o.Complete {
				t.Fatal(o, e)
			}
			f.reconcile(t)
			f.reconcile(t)
			if e := f.d.Cancel(context.Background(), b.ID, "booker"); e != nil {
				t.Fatal(e)
			}
			id := f.l.SeedBooking(lundastub.Booking{Court: "lc1", Begin: at, End: at.Add(time.Hour), Customer: "club-client"})
			f.reconcile(t)
			f.reconcile(t)
			o, e = f.d.Occupancy(context.Background(), q)
			if e != nil || len(o.Intervals) != 2 {
				t.Fatal(o, e)
			}
			f.l.EditBooking(id, func(b *lundastub.Booking) { b.Status = "CANCELLED" })
			f.reconcile(t)
			f.reconcile(t)
			o, e = f.d.Occupancy(context.Background(), q)
			if e != nil || len(o.Intervals) != 0 {
				t.Fatal(o, e)
			}
		})
	}
}
