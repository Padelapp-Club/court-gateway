package demo_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// demoEnv is the suite's environment for one demo club served by handler.
func demoEnv(handler http.Handler, club string, others []string, srv *core.Server, logs func() string) *contract.Env {
	d := srv.Driver().(*demo.Driver)
	return &contract.Env{
		Handler:      handler,
		Club:         club,
		OtherClubs:   others,
		ReadKey:      contract.ReadKey,
		BookKey:      contract.BookKey,
		OtherBookKey: contract.OtherBookKey,
		Courts:       []string{"court-1", "court-2"},
		Location:     srv.Settings().Location,
		SlotMinutes:  30,
		SeedForeign: func(t *testing.T, court string, start, end time.Time) string {
			return d.Seed(court, start, end)
		},
		Price: func(day time.Time) contract.PriceCase {
			// 2000/h base; 10:00-11:30 is never in the evening surcharge.
			s := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
			return contract.PriceCase{Court: "court-1", Start: s, End: s.Add(90 * time.Minute), Total: 3000}
		},
		Logs: logs,
		// The demo driver keeps no personal data; the markers are still
		// sent in a note and must not come back.
		PrivacyMarkers: []string{"PRIVACY-MARKER-NOTE-7f3a"},
	}
}

var demoClub = testkit.Club("demo-club", "Демо-клуб", "Корт 1", "Корт 2")

// The suite against the club's API with the partner middleware in front,
// no hub: what the API itself promises.
func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		b := contract.Build(t, testkit.DemoCRM, demoClub, nil, "demo", demo.New, nil)
		return demoEnv(b.Handler(), "demo-club", nil, b.Server, b.Logs.String)
	})
}

// The suite through the hub (partner -> hub -> outbound tunnel -> gateway
// -> driver, over real TLS) against one club of an installation that
// serves two clubs configured on the hub: the other club must not change
// anything a partner sees.
func TestContractMultiClubThroughHub(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		h := testkit.StartHub(t, "poll_wait: 2s\n"+testkit.OperatorYAML+testkit.HubPartnersYAML, testkit.HubEnv(), hub.Options{})
		club := func(id, studio string, courts ...string) core.ClubSpec {
			c := testkit.Club(id, "Клуб "+id, courts...)
			c.CRM = map[string]any{"studio": studio}
			return c
		}
		in := testkit.StartInstallation(t, h, "demo-net", testkit.InstallationOptions{
			CRMYAML: testkit.DemoCRM, CRMType: "demo", Factory: demo.New, Account: demo.NewAccount,
			Clubs: []core.ClubSpec{club("club-north", "demo-north", "Корт 1", "Корт 2"), club("club-south", "demo-south", "Корт A", "Корт B")},
		})
		srv, _ := in.Clubs.Server("club-south")
		return demoEnv(h.Forwarder(t), "club-south", []string{"club-north"}, srv, func() string { return in.Logs.String() + h.Logs.String() })
	})
}
