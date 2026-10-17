package core_test

import (
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// noPrices is the demo driver declaring that its CRM gives no prices. Its
// PriceTable still answers, so what the test sees is the core keeping to
// the declaration, not the driver's ErrNotSupported.
type noPrices struct{ driver.Driver }

func (d noPrices) Capabilities() driver.Capabilities {
	c := d.Driver.Capabilities()
	c.NoPrices = true
	return c
}

// A CRM without prices passes the whole contract with Price nil: the club
// says "prices": false and /prices and /quote answer NOT_SUPPORTED.
func TestContractWithoutPrices(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		b := contract.Build(t, testkit.DemoCRM, testkit.Club("demo-club", "Демо-клуб", "Корт 1", "Корт 2"), nil, "demo",
			func(c driver.Config) (driver.Driver, error) {
				d, err := demo.New(c)
				return noPrices{d}, err
			}, nil)
		d := b.Driver.(noPrices).Driver.(*demo.Driver)
		return &contract.Env{Handler: b.Handler(), Club: "demo-club", ReadKey: contract.ReadKey, BookKey: contract.BookKey, OtherBookKey: contract.OtherBookKey,
			Courts: []string{"court-1", "court-2"}, Location: b.Settings.Location, SlotMinutes: 30,
			SeedForeign: func(t *testing.T, court string, s, e time.Time) string { return d.Seed(court, s, e) },
			Logs:        b.Logs.String, PrivacyMarkers: []string{"PRIVACY-MARKER-NOTE-7f3a"}}
	})
}
