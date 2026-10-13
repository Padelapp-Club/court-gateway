package demo_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

const demoConfig = `club: { id: demo-club, name: "Демо-клуб" }
crm: { type: demo }
venue:
  timezone: Europe/Moscow
  slot_minutes: 30
  hours: { mon-fri: "07:00-23:00", sat-sun: "08:00-23:00" }
courts:
  - { id: court-1, name: "Корт 1" }
  - { id: court-2, name: "Корт 2" }
` + contract.PartnersYAML

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		b := contract.Build(t, demoConfig, contract.KeyEnv(), "demo", demo.New, nil)
		d := b.Driver.(*demo.Driver)
		return &contract.Env{
			Handler:      b.Server.Handler(),
			Club:         "demo-club",
			ReadKey:      contract.ReadKey,
			BookKey:      contract.BookKey,
			OtherBookKey: contract.OtherBookKey,
			Courts:       []string{"court-1", "court-2"},
			Location:     b.Settings.Location,
			SlotMinutes:  30,
			SeedForeign: func(t *testing.T, court string, start, end time.Time) string {
				return d.Seed(court, start, end)
			},
			Price: func(day time.Time) contract.PriceCase {
				// 2000/h base; 10:00-11:30 is never in the evening surcharge.
				s := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
				return contract.PriceCase{Court: "court-1", Start: s, End: s.Add(90 * time.Minute), Total: 3000}
			},
			Logs: b.Logs.String,
			// The demo driver keeps no personal data; the markers are
			// still sent in a note and must not come back.
			PrivacyMarkers: []string{"PRIVACY-MARKER-NOTE-7f3a"},
		}
	})
}

// The same suite through the hub: partner -> hub -> outbound tunnel ->
// gateway -> driver, over real TLS. Keys, scopes and rate limits live on
// the hub here; the gateway has no port of its own.
func TestContractThroughHub(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		b := contract.Build(t, demoConfig, contract.KeyEnv(), "demo", demo.New, nil)
		d := b.Driver.(*demo.Driver)
		handler, logs := testkit.ThroughHub(t, b)
		return &contract.Env{
			Handler:      handler,
			Club:         "demo-club",
			ReadKey:      contract.ReadKey,
			BookKey:      contract.BookKey,
			OtherBookKey: contract.OtherBookKey,
			Courts:       []string{"court-1", "court-2"},
			Location:     b.Settings.Location,
			SlotMinutes:  30,
			SeedForeign: func(t *testing.T, court string, start, end time.Time) string {
				return d.Seed(court, start, end)
			},
			Price: func(day time.Time) contract.PriceCase {
				s := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
				return contract.PriceCase{Court: "court-1", Start: s, End: s.Add(90 * time.Minute), Total: 3000}
			},
			Logs:           logs,
			PrivacyMarkers: []string{"PRIVACY-MARKER-NOTE-7f3a"},
		}
	})
}

// The suite through the hub against one club of an installation that
// serves two clubs configured on the hub (one CRM account, a club
// network): the other club must not change anything a partner sees.
func TestContractMultiClubThroughHub(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		h := testkit.StartHub(t, "poll_wait: 2s\n"+testkit.OperatorYAML+testkit.HubPartnersYAML, testkit.HubEnv(), hub.Options{})
		club := func(id, studio string, courts ...string) core.ClubSpec {
			c := core.ClubSpec{ID: id, Name: "Клуб " + id, Timezone: "Europe/Moscow", SlotMinutes: 30,
				Hours: map[string]string{"mon-fri": "07:00-23:00", "sat-sun": "08:00-23:00"}, CRM: map[string]any{"studio": studio}}
			for i, n := range courts {
				c.Courts = append(c.Courts, core.CourtSpec{ID: fmt.Sprintf("court-%d", i+1), Name: n})
			}
			return c
		}
		in := testkit.StartInstallation(t, h, "demo-net", testkit.InstallationOptions{
			CRMYAML: "crm: { type: demo }\n", CRMType: "demo", Factory: demo.New, Account: demo.NewAccount,
			Clubs: []core.ClubSpec{club("club-north", "demo-north", "Корт 1", "Корт 2"), club("club-south", "demo-south", "Корт A", "Корт B")},
		})
		srv, _ := in.Clubs.Server("club-south")
		d := srv.Driver().(*demo.Driver)
		return &contract.Env{
			Handler:      h.Forwarder(t),
			Club:         "club-south",
			OtherClubs:   []string{"club-north"},
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
				s := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, day.Location())
				return contract.PriceCase{Court: "court-1", Start: s, End: s.Add(90 * time.Minute), Total: 3000}
			},
			Logs:           func() string { return in.Logs.String() + h.Logs.String() },
			PrivacyMarkers: []string{"PRIVACY-MARKER-NOTE-7f3a"},
		}
	})
}
