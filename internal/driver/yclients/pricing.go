package yclients

import (
	"context"
	"fmt"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// PriceTable implements driver.Driver: not supported. A YCLIENTS club's
// prices of sale live in PadelApp (the backend adapter read none either),
// and
// YCLIENTS prices the time of day with services in effect at some hours,
// which no single read turns into a week of rules. What a booking costs
// in YCLIENTS is what its services cost there (Create).
func (d *Driver) PriceTable(_ context.Context, court string) (driver.PriceTable, error) {
	if _, ok := d.byCourt[court]; !ok {
		return driver.PriceTable{}, fmt.Errorf("court %q: %w", court, driver.ErrNotFound)
	}
	return driver.PriceTable{}, driver.ErrNotSupported
}
