package fitness1c

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// PriceTable implements driver.Driver. The client API of 1C names one
// price per rental service (GET /rental_services/, the price of one
// seance) and no prices by time of day: the price per hour is that price
// over the seance, the whole day, every day. The service is crm.price_service,
// or the one the grid is read by. [UNVERIFIED] whether a club's 1C prices
// by time of day in a way this API does not show.
func (d *Driver) PriceTable(ctx context.Context, courtID string) (driver.PriceTable, error) {
	if _, ok := d.byCourt[courtID]; !ok {
		return driver.PriceTable{}, fmt.Errorf("court %q: %w", courtID, driver.ErrNotFound)
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return driver.PriceTable{}, err
	}
	list, err := listServices(ctx, d.c, d.studio)
	if err != nil {
		return driver.PriceTable{}, err
	}
	svc, err := d.priceServiceOf(list)
	if err != nil {
		return driver.PriceTable{}, driver.Upstream(driver.UpstreamInvalid, "rental services: "+err.Error())
	}
	perSeance, ok := kopecks(svc.Price)
	if !ok || perSeance <= 0 {
		return driver.PriceTable{}, driver.Upstream(driver.UpstreamInvalid, "rental services: the price service has no price")
	}
	return driver.PriceTable{
		Base: []driver.PriceRule{{Days: driver.AllWeekdays, From: 0, To: 1440, PerHour: perSeance * 60 / int64(svc.step())}},
		Zone: loc,
	}, nil
}

// priceServiceOf is the configured price service, or the grid's.
func (d *Driver) priceServiceOf(list []rentalService) (rentalService, error) {
	if d.priceService == "" {
		return pickGridService(list, d.occupancyService)
	}
	for _, s := range list {
		if s.id() == d.priceService {
			if s.step() == 0 {
				return s, fmt.Errorf("crm.price_service: у услуги %s нет seance_length", d.priceService)
			}
			return s, nil
		}
	}
	return rentalService{}, fmt.Errorf("crm.price_service: услуги %s нет среди услуг аренды клуба в 1С", d.priceService)
}

// kopecks reads 1C's price: a string ("875", "875.50") or a number, in
// rubles.
func kopecks(v any) (int64, bool) {
	var f float64
	switch p := v.(type) {
	case string:
		n, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(p), ",", "."), 64)
		if err != nil {
			return 0, false
		}
		f = n
	case float64:
		f = p
	case json.Number:
		n, err := p.Float64()
		if err != nil {
			return 0, false
		}
		f = n
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f > 1e9 {
		return 0, false
	}
	return int64(math.Round(f * 100)), true
}
