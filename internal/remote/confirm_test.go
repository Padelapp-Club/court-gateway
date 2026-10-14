package remote_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/remote"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Clubs of a Viva account checked against a CRM stub.

const (
	vStudio      = "00000000-0000-4000-8000-000000000001"
	vOtherStudio = "00000000-0000-4000-8000-000000000002"
	vRoom        = "00000000-0000-4000-8000-000000000011"
	vRoom2       = "00000000-0000-4000-8000-000000000012"
	vRoom3       = "00000000-0000-4000-8000-000000000013"
)

var vNow = time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)

type vivaInstallation struct {
	stub   *vivastub.Stub
	base   *core.Settings
	clubs  *core.Clubs
	syncer *remote.Syncer
}

func newVivaInstallation(t *testing.T) *vivaInstallation {
	t.Helper()
	stub := vivastub.New("review@example.test", "fixture-password", vStudio)
	stub.Now = func() time.Time { return vNow }
	stub.AddRoom(vRoom, "Court")
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	yaml := fmt.Sprintf("hub: {url: https://hub.example.test, installation: test-net, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=}\ncrm: {type: viva, username: review@example.test, auth_url: %s/token, api_url: %s/api/v1}\n", srv.URL, srv.URL)
	env := map[string]string{"CRM_PASSWORD": "fixture-password", "GATEWAY_HUB_SECRET": "fixture-hub-secret"}
	base, err := core.Load([]byte(yaml), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	base.Driver.Now = func() time.Time { return vNow }
	if base.Driver.Account, err = viva.NewAccount(base.Driver); err != nil {
		t.Fatal(err)
	}
	clubs := core.NewClubs(core.ClubsOptions{})
	return &vivaInstallation{stub: stub, base: base, clubs: clubs,
		syncer: remote.New(remote.Options{Base: base, Factories: drivers.Factories, Clubs: clubs})}
}

func vivaClub(studio string, rooms ...string) core.ClubSpec {
	s := core.ClubSpec{ID: "club-a", Name: "Club A", Timezone: "Etc/GMT-3", SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "00:00-24:00"}, CRM: map[string]any{"studio": studio}}
	for i, r := range rooms {
		s.Courts = append(s.Courts, core.CourtSpec{ID: fmt.Sprintf("court-%d", i+1), Name: "Court", Params: map[string]any{"viva_room": r}})
	}
	return s
}

func vivaConfig(version int64, s core.ClubSpec) tunnel.RemoteConfig {
	raw, _ := json.Marshal(s)
	return tunnel.RemoteConfig{Version: version, Clubs: []json.RawMessage{raw}}
}

func (in *vivaInstallation) call(method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(auth.WithPartnerName(r.Context(), "partner-a"))
	w := httptest.NewRecorder()
	in.clubs.TunnelHandler().ServeHTTP(w, r)
	return w
}

var vStart, vEnd = vNow.Add(24 * time.Hour), vNow.Add(25 * time.Hour)

func (in *vivaInstallation) occupancy() *httptest.ResponseRecorder {
	return in.call("GET", "/v1/clubs/club-a/occupancy?from="+vStart.Format(time.RFC3339)+"&to="+vEnd.Format(time.RFC3339), "")
}

func (in *vivaInstallation) create() *httptest.ResponseRecorder {
	return in.call("POST", "/v1/clubs/club-a/bookings",
		fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`, vStart.Format(time.RFC3339), vEnd.Format(time.RFC3339)))
}

func blocked(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusServiceUnavailable && strings.Contains(w.Body.String(), "CLUB_UNAVAILABLE")
}

// The studio list is read for each check, never reused from an earlier
// one: after a room leaves the studio, a running driver sees it as a
// freshly started one does.
func TestStudioListIsNotReusedAcrossChecks(t *testing.T) {
	in := newVivaInstallation(t)
	st, err := in.base.WithClub(vivaClub(vStudio, vRoom), viva.ClubCRMKeys)
	if err != nil {
		t.Fatal(err)
	}
	d, err := viva.NewDriver(st.Driver)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	in.stub.RemoveRoom(vRoom)
	if err := d.CheckConfig(context.Background()); core.ReadyReason(err) != "CONFIG_MISMATCH" {
		t.Fatalf("after the room left the studio: %v", err)
	}
}

// A club whose config the CRM could not confirm (one failed GET /studios)
// is not served: a studio B paired with a room of studio A must neither
// answer "free" nor write into that room over a client's booking. The
// next readiness round asks again and finds the mismatch.
func TestUnconfirmedClubIsNotServed(t *testing.T) {
	in := newVivaInstallation(t)
	in.stub.AddStudio(vOtherStudio, "Studio B", map[string]string{})
	existing := in.stub.SeedExercise(map[string]any{"room": map[string]any{"id": vRoom}, "timeFrom": vStart.Format(time.RFC3339),
		"timeTo": vEnd.Format(time.RFC3339), "masterServiceBooking": map[string]any{"client": map[string]any{"name": "fixture-client"}}})
	in.stub.FailNext("GET", "/studios", 503, `{}`)
	if err := in.syncer.Apply(context.Background(), vivaConfig(1, vivaClub(vOtherStudio, vRoom))); err == nil {
		t.Fatal("an unconfirmed club was reported as applied")
	}
	if st := in.syncer.State(); !strings.Contains(st.ClubErrors["club-a"], "CRM_UNREACHABLE") || len(st.Clubs) != 0 {
		t.Fatalf("state: %+v", st)
	}
	if w := in.occupancy(); !blocked(w) {
		t.Fatalf("occupancy: %d %s", w.Code, w.Body.String())
	}
	if w := in.create(); !blocked(w) {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if in.stub.Exercise(existing) == nil {
		t.Fatal("the client's booking is gone")
	}
	_ = in.syncer.Ready(context.Background())
	if st := in.syncer.State(); !strings.Contains(st.ClubErrors["club-a"], "courts[0].viva_room") || len(st.Clubs) != 0 || st.ConfigVersion != 1 {
		t.Fatalf("after the readiness round: %+v", st)
	}
	if w := in.create(); !blocked(w) {
		t.Fatalf("create after the round: %d %s", w.Code, w.Body.String())
	}
}

// A changed club the CRM could not confirm keeps its previous confirmed
// config; once the CRM answers, the readiness round serves the new one
// without a new config version from the hub.
func TestUnconfirmedChangeKeepsThePreviousConfig(t *testing.T) {
	in := newVivaInstallation(t)
	in.stub.AddRoom(vRoom2, "Court 2")
	if err := in.syncer.Apply(context.Background(), vivaConfig(1, vivaClub(vStudio, vRoom))); err != nil {
		t.Fatal(err)
	}
	in.stub.FailNext("GET", "/studios", 503, `{}`)
	if err := in.syncer.Apply(context.Background(), vivaConfig(2, vivaClub(vStudio, vRoom, vRoom2))); err == nil {
		t.Fatal("an unconfirmed change was reported as applied")
	}
	srv, ok := in.clubs.Server("club-a")
	if !ok || len(srv.Settings().Courts) != 1 {
		t.Fatalf("the previous config is not served: %v", ok)
	}
	if err := in.syncer.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv, _ = in.clubs.Server("club-a")
	if st := in.syncer.State(); len(srv.Settings().Courts) != 2 || st.ConfigError != "" || st.ConfigVersion != 2 {
		t.Fatalf("after the readiness round: %d courts, %+v", len(srv.Settings().Courts), st)
	}
}

// A court the newer config removed is dropped from the previous config
// the club keeps while the newer one is not confirmed: it is no longer
// bookable, the other courts are.
func TestRetainedConfigDropsTheRemovedCourts(t *testing.T) {
	in := newVivaInstallation(t)
	in.stub.AddRoom(vRoom2, "Court 2")
	if err := in.syncer.Apply(context.Background(), vivaConfig(1, vivaClub(vStudio, vRoom, vRoom2))); err != nil {
		t.Fatal(err)
	}
	in.stub.FailNext("GET", "/studios", 503, `{}`)
	if err := in.syncer.Apply(context.Background(), vivaConfig(2, vivaClub(vStudio, vRoom))); err == nil {
		t.Fatal("an unconfirmed change was reported as applied")
	}
	srv, ok := in.clubs.Server("club-a")
	if !ok || len(srv.Settings().Courts) != 1 || srv.Settings().Courts[0].ID != "court-1" {
		t.Fatalf("the previous config, less court-2, is not served: %v", ok)
	}
	w := in.call("POST", "/v1/clubs/club-a/bookings",
		fmt.Sprintf(`{"court":"court-2","start":%q,"end":%q,"kind":"game"}`, vStart.Format(time.RFC3339), vEnd.Format(time.RFC3339)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("create on the removed court: %d %s", w.Code, w.Body.String())
	}
	if w := in.create(); w.Code != http.StatusCreated {
		t.Fatalf("create on the kept court: %d %s", w.Code, w.Body.String())
	}
}

// A club whose only court the newer, unconfirmed config moves to another
// room has no court left of its previous config: it is unavailable, not
// served on the old room.
func TestRetainedConfigWithNoCourtLeft(t *testing.T) {
	in := newVivaInstallation(t)
	in.stub.AddRoom(vRoom2, "Court 2")
	if err := in.syncer.Apply(context.Background(), vivaConfig(1, vivaClub(vStudio, vRoom))); err != nil {
		t.Fatal(err)
	}
	in.stub.FailNext("GET", "/studios", 503, `{}`)
	if err := in.syncer.Apply(context.Background(), vivaConfig(2, vivaClub(vStudio, vRoom2))); err == nil {
		t.Fatal("an unconfirmed change was reported as applied")
	}
	if w := in.create(); !blocked(w) {
		t.Fatalf("create on the moved court: %d %s", w.Code, w.Body.String())
	}
}

// Through the hub, a court the newer, unconfirmed config removed or moved
// to another room is neither booked, read nor cancelled by the previous
// config the club keeps: its driver knows only the courts left, so no
// write reaches the CRM for it; the court left is still booked.
func TestRetainedConfigDoesNotReachTheDroppedCourts(t *testing.T) {
	for name, next := range map[string]core.ClubSpec{
		"removed": vivaClub(vStudio, vRoom),
		"moved":   vivaClub(vStudio, vRoom, vRoom3),
	} {
		t.Run(name, func(t *testing.T) {
			stub := vivastub.New("review@example.test", "fixture-password", vStudio)
			stub.AddRoom(vRoom, "Court 1")
			stub.AddRoom(vRoom2, "Court 2")
			stub.AddRoom(vRoom3, "Court 3")
			crm := httptest.NewServer(stub)
			t.Cleanup(crm.Close)
			h := testkit.StartHub(t, "poll_wait: 1s\n"+testkit.OperatorYAML+testkit.HubPartnersYAML, testkit.HubEnv(), hub.Options{})
			in := testkit.StartInstallation(t, h, "test-net", testkit.InstallationOptions{
				CRMYAML: fmt.Sprintf("crm: {type: viva, username: review@example.test, auth_url: %s/token, api_url: %s/api/v1}\n", crm.URL, crm.URL),
				Env:     map[string]string{"CRM_PASSWORD": "fixture-password"}, CRMType: "viva", Factory: viva.New, Account: viva.NewAccount,
				Clubs: []core.ClubSpec{vivaClub(vStudio, vRoom, vRoom2)},
			})
			start := time.Now().UTC().Truncate(time.Hour).Add(48 * time.Hour)
			book := func(court string, at time.Time) (*http.Response, []byte) {
				body := fmt.Sprintf(`{"court":%q,"start":%q,"end":%q,"kind":"game"}`, court, at.Format(time.RFC3339), at.Add(time.Hour).Format(time.RFC3339))
				return h.Do(t, 10*time.Second, "POST", "/v1/clubs/club-a/bookings", contract.BookKey, strings.NewReader(body))
			}
			resp, data := book("court-2", start)
			var b struct{ ID string }
			if json.Unmarshal(data, &b); resp.StatusCode != http.StatusCreated {
				t.Fatalf("create: %d %s", resp.StatusCode, data)
			}
			stub.FailNext("GET", "/studios", 503, `{}`)
			h.MustOperator(t, http.StatusOK, "PUT", "/hub/installations/test-net/clubs/club-a", next)
			testkit.WaitFor(t, "the change refused", 10*time.Second, func() bool { return in.Syncer.State().ConfigError != "" })
			if srv, ok := in.Clubs.Server("club-a"); !ok || len(srv.Settings().Courts) != 1 || srv.Settings().Courts[0].ID != "court-1" {
				t.Fatalf("the previous config, less court-2, is not served: %v", ok)
			}
			calls := len(stub.Calls())
			for _, method := range []string{"GET", "DELETE"} {
				if resp, data := h.Do(t, 10*time.Second, method, "/v1/clubs/club-a/bookings/"+b.ID, contract.BookKey, nil); resp.StatusCode != http.StatusConflict ||
					!strings.Contains(string(data), "COURT_NOT_CONFIGURED") {
					t.Errorf("%s of the court-2 booking: %d %s", method, resp.StatusCode, data)
				}
			}
			if resp, data := book("court-2", start.Add(2*time.Hour)); resp.StatusCode != http.StatusNotFound {
				t.Errorf("create on court-2: %d %s", resp.StatusCode, data)
			}
			for _, call := range stub.Calls()[calls:] {
				if !strings.HasPrefix(call, "GET ") {
					t.Errorf("CRM write for court-2: %s", call)
				}
			}
			if rec := stub.Break(strings.TrimPrefix(b.ID, "b-")); rec == nil || rec["canceled"] == true {
				t.Errorf("the court-2 booking in the CRM: %v", rec)
			}
			if resp, data := book("court-1", start); resp.StatusCode != http.StatusCreated {
				t.Errorf("create on court-1: %d %s", resp.StatusCode, data)
			}
		})
	}
}

// A room the club creates in the CRM after the config named it: the
// refusal is not final, the next readiness round serves the club (no new
// config version needed). A room created just before the config arrives
// is seen at once, whatever an earlier round read.
func TestRefusedClubIsServedOnceTheCRMMatches(t *testing.T) {
	in := newVivaInstallation(t)
	if err := in.syncer.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	in.stub.AddRoom(vRoom2, "Court 2")
	if err := in.syncer.Apply(context.Background(), vivaConfig(1, vivaClub(vStudio, vRoom, vRoom2))); err != nil {
		t.Fatalf("a room created before the config: %v", err)
	}
	const vRoom3 = "00000000-0000-4000-8000-000000000013"
	if err := in.syncer.Apply(context.Background(), vivaConfig(2, vivaClub(vStudio, vRoom, vRoom3))); err == nil {
		t.Fatal("a room missing in the CRM was accepted")
	}
	in.stub.AddRoom(vRoom3, "Court 3")
	if err := in.syncer.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv, _ := in.clubs.Server("club-a")
	if st := in.syncer.State(); st.ConfigError != "" || st.ConfigVersion != 2 || !strings.HasSuffix(srv.Driver().(*viva.Driver).Resource("court-2"), " "+vRoom3) {
		t.Fatalf("after the readiness round: %+v", st)
	}
}

// A served club that stops matching its CRM (its room leaves the studio)
// is blocked — no occupancy, no booking — from the readiness round that
// sees it until one that does not.
func TestMismatchFoundByReadinessBlocksTheClub(t *testing.T) {
	in := newVivaInstallation(t)
	if err := in.syncer.Apply(context.Background(), vivaConfig(1, vivaClub(vStudio, vRoom))); err != nil {
		t.Fatal(err)
	}
	if w := in.occupancy(); w.Code != http.StatusOK {
		t.Fatalf("occupancy: %d %s", w.Code, w.Body.String())
	}
	in.stub.RemoveRoom(vRoom)
	if err := in.syncer.Ready(context.Background()); core.ReadyReason(err) != "CONFIG_MISMATCH" {
		t.Fatalf("ready: %v", err)
	}
	if w := in.occupancy(); !blocked(w) {
		t.Fatalf("occupancy: %d %s", w.Code, w.Body.String())
	}
	if w := in.create(); !blocked(w) {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	in.stub.FailNext("GET", "/studios", 503, `{}`)
	_ = in.syncer.Ready(context.Background())
	if w := in.occupancy(); !blocked(w) {
		t.Fatalf("occupancy after a round that could not check: %d %s", w.Code, w.Body.String())
	}
	in.stub.AddRoom(vRoom, "Court")
	if err := in.syncer.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := in.occupancy(); w.Code != http.StatusOK {
		t.Fatalf("occupancy after the room is back: %d %s", w.Code, w.Body.String())
	}
}

// A club whose changed config is not served keeps its previous one only
// while no other club's config books its room: once the room is given to
// club B, A is unavailable (not served on the old room) and B books it.
// The change fails the CRM check (a new court on a room missing in the
// CRM) or does not load at all (not a room id).
func TestPreviousConfigIsDroppedWhenItsRoomIsReassigned(t *testing.T) {
	for name, a := range map[string]core.ClubSpec{"check fails": vivaClub(vStudio, vRoom, vRoom3), "does not load": vivaClub(vStudio, "not-a-room")} {
		t.Run(name, func(t *testing.T) {
			in := newVivaInstallation(t)
			if err := in.syncer.Apply(context.Background(), vivaConfig(1, vivaClub(vStudio, vRoom))); err != nil {
				t.Fatal(err)
			}
			b := vivaClub(vStudio, vRoom)
			b.ID = "club-b"
			_ = in.syncer.Apply(context.Background(), vivaConfig(2, a))
			if _, ok := in.clubs.Server("club-a"); !ok {
				t.Fatal("the previous config is not kept while nobody else books its room")
			}
			rawA, _ := json.Marshal(a)
			rawB, _ := json.Marshal(b)
			_ = in.syncer.Apply(context.Background(), tunnel.RemoteConfig{Version: 3, Clubs: []json.RawMessage{rawA, rawB}})
			if st := in.syncer.State(); !strings.Contains(st.ClubErrors["club-a"], "ROOM_CONFLICT") {
				t.Fatalf("state: %+v", st)
			}
			_ = in.syncer.Ready(context.Background())
			if w := in.create(); !blocked(w) {
				t.Fatalf("create through club A on the room of club B: %d %s", w.Code, w.Body.String())
			}
			if w := in.call("GET", "/v1/clubs/club-b/occupancy?from="+vStart.Format(time.RFC3339)+"&to="+vEnd.Format(time.RFC3339), ""); w.Code != http.StatusOK {
				t.Fatalf("club B: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
