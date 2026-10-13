package demo_test

import (
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
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
