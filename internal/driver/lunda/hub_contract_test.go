package lunda_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// The contract, privacy included, through the hub for an installation with
// two clubs configured on the hub, both on the one Lunda session of the
// installation's account (one on courts 1 and 2, the other on court 9).
func TestContractMultiClubThroughHub(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		clk := &clock{t: time.Now()}
		stub := newStub(clk)
		srv := httptest.NewServer(stub)
		t.Cleanup(srv.Close)
		assertOnlyAllowedCalls(t, stub)
		store := &memStore{}
		store.set(lunda.EncodeSession(stub.IssueSession(), clubUID, clubName))
		h := testkit.StartHub(t, "poll_wait: 2s\n"+testkit.OperatorYAML+testkit.HubPartnersYAML, testkit.HubEnv(), hub.Options{})
		main := testClub()
		other := core.ClubSpec{ID: "other-club", Name: "Другой клуб", Timezone: "Europe/Moscow", SlotMinutes: 60, Hours: main.Hours,
			Courts: []core.CourtSpec{{ID: "court-1", Name: "Корт", Params: map[string]any{lunda.CourtKey: courtOther}}}}
		hook := func(c *driver.Config) { c.HTTPClient = srv.Client(); c.Sessions = store; c.Now = clk.now }
		in := testkit.StartInstallation(t, h, "lunda-net", testkit.InstallationOptions{
			CRMYAML: crmYAML(srv.URL, true), CRMType: "lunda", Factory: lunda.New, Account: func(cfg driver.Config) (driver.Account, error) {
				a, err := lunda.NewAccount(cfg)
				if err == nil {
					t.Cleanup(func() { _ = driver.EndSession(context.Background(), a) })
				}
				return a, err
			},
			Hook: hook, Clubs: []core.ClubSpec{main, other},
		})
		s, _ := in.Clubs.Server("test-club")
		return &contract.Env{
			Handler: h.Forwarder(t), Club: "test-club", OtherClubs: []string{"other-club"},
			ReadKey: contract.ReadKey, BookKey: contract.BookKey, OtherBookKey: contract.OtherBookKey,
			Courts: []string{"court-1", "court-2"}, Location: s.Settings().Location, SlotMinutes: 30,
			SeedForeign: func(t *testing.T, court string, start, end time.Time) string {
				return "l-" + seedClubBooking(stub, courtUID(court), start, end)
			},
			Price:          priceCase,
			Logs:           func() string { return in.Logs.String() + h.Logs.String() },
			PrivacyMarkers: privacyMarkers,
		}
	})
}
