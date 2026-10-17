package fitness1c_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// The same driver-agnostic suite as every other driver, against the 1C
// stand-in: the club's API with the partner middleware in front, no hub.
func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		clock := newClock()
		f := newFixture(t, fixtureOpts{now: clock.Now})
		return f.env(clock)
	})
}

// The suite through the hub for an installation with two clubs of one 1C
// instance configured on the hub: the login is the installation's, each
// club adds its own club_id, rooms and service.
func TestContractMultiClubThroughHub(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		stub := newStub()
		srv := httptest.NewServer(stub)
		t.Cleanup(srv.Close)
		h := testkit.StartHub(t, "poll_wait: 2s\n"+testkit.OperatorYAML+testkit.HubPartnersYAML, testkit.HubEnv(), hub.Options{})
		main := testClub(fixtureOpts{})
		other := core.ClubSpec{ID: "other-club", Name: "Другой клуб", Timezone: "Europe/Moscow", SlotMinutes: 60, Hours: main.Hours,
			CRM:      map[string]any{"studio": club1C},
			Courts:   []core.CourtSpec{{ID: "court-1", Name: "Корт", Params: map[string]any{fitness1c.RoomKey: roomOther}}},
			Services: bookingService}
		in := testkit.StartInstallation(t, h, "fitness1c-net", testkit.InstallationOptions{
			CRMYAML: crmYAML(srv.URL, ""),
			Env:     installEnv(fixtureOpts{}), CRMType: "fitness1c", Factory: fitness1c.New, Account: fitness1c.NewAccount,
			Hook:  func(c *driver.Config) { c.HTTPClient = srv.Client() },
			Clubs: []core.ClubSpec{main, other},
		})
		s, _ := in.Clubs.Server("test-club")
		return &contract.Env{
			Handler: h.Forwarder(t), Club: "test-club", OtherClubs: []string{"other-club"},
			ReadKey: contract.ReadKey, BookKey: contract.BookKey, OtherBookKey: contract.OtherBookKey,
			Courts: []string{"court-1", "court-2"}, Location: s.Settings().Location, SlotMinutes: 30,
			SeedForeign: func(t *testing.T, court string, start, end time.Time) string {
				stub.SeedRental(roomFor(court), start, end, false, clubPrivate)
				return stretchID(roomFor(court), start, end)
			},
			Price:          priceCase,
			Logs:           func() string { return in.Logs.String() + h.Logs.String() },
			PrivacyMarkers: privacyMarkers,
		}
	})
}
