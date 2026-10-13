package viva_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// The contract, privacy included, through the hub for an installation with
// two clubs configured on the hub: the CRM login is the installation's,
// each club adds its own studio, courts and services. The suite runs
// against one club while the other is served by the same gateway.
func TestContractMultiClubThroughHub(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		stub := vivastub.New(stubUser, stubPass, studio)
		stub.AddRoom(room1, "Room one")
		stub.AddRoom(room2, "Room two")
		stub.AddRoom(roomOther, "Room elsewhere")
		stub.SetPricing(priceList, pricingJSON)
		srv := httptest.NewServer(stub)
		t.Cleanup(srv.Close)
		h := testkit.StartHub(t, "poll_wait: 2s\n"+testkit.OperatorYAML+testkit.HubPartnersYAML, testkit.HubEnv(), hub.Options{})
		hours := map[string]string{"mon-fri": "07:00-23:00", "sat-sun": "08:00-23:00"}
		main := core.ClubSpec{ID: "test-club", Name: "Тестовый клуб", Timezone: "Europe/Moscow", SlotMinutes: 30, Hours: hours,
			CRM: map[string]any{"studio": studio},
			Courts: []core.CourtSpec{
				{ID: "court-1", Name: "Корт 1", Params: map[string]any{"viva_room": room1, "price_list": priceList}},
				{ID: "court-2", Name: "Корт 2", Params: map[string]any{"viva_room": room2, "price_list": priceList}},
			},
			Services: map[string]map[string]any{"booking": {"direction": 12, "type": 3}}}
		other := core.ClubSpec{ID: "other-club", Name: "Другой клуб", Timezone: "Europe/Moscow", SlotMinutes: 60, Hours: hours,
			CRM:    map[string]any{"studio": studio},
			Courts: []core.CourtSpec{{ID: "court-1", Name: "Корт", Params: map[string]any{"viva_room": roomOther}}}}
		in := testkit.StartInstallation(t, h, "viva-net", testkit.InstallationOptions{
			CRMYAML: "crm: { type: viva, username: " + stubUser + ", auth_url: " + srv.URL + "/token, api_url: " + srv.URL + "/api/v1 }\n",
			Env:     map[string]string{"CRM_PASSWORD": stubPass}, CRMType: "viva", Factory: viva.New, Account: viva.NewAccount,
			Hook:  func(c *driver.Config) { c.HTTPClient = srv.Client() },
			Clubs: []core.ClubSpec{main, other},
		})
		f := &fixture{stub: stub, srv: srv}
		s, _ := in.Clubs.Server("test-club")
		return &contract.Env{
			Handler: h.Forwarder(t), Club: "test-club", OtherClubs: []string{"other-club"},
			ReadKey: contract.ReadKey, BookKey: contract.BookKey, OtherBookKey: contract.OtherBookKey,
			Courts: []string{"court-1", "court-2"}, Location: s.Settings().Location, SlotMinutes: 30,
			SeedForeign: func(t *testing.T, court string, start, end time.Time) string {
				return "e-" + f.seedClubBooking(roomFor(court), start, end)
			},
			Price: func(day time.Time) contract.PriceCase {
				st := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
				total := int64(3000)
				if wd := st.Weekday(); wd == time.Saturday || wd == time.Sunday {
					total = 2250
				}
				return contract.PriceCase{Court: "court-1", Start: st, End: st.Add(90 * time.Minute), Total: total}
			},
			Logs:           func() string { return in.Logs.String() + h.Logs.String() },
			PrivacyMarkers: privacyMarkers,
		}
	})
}
