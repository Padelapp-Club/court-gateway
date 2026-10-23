package fitness1c

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// statusConfirmed is the status of a rental of a free service right after
// it is created; anything else (a temporary reservation that
// lapses in 15 minutes) is not a booking.
const statusConfirmed = "reserved"

// Compensation: removing a rental that must not exist, a few times.
const (
	compensationAttempts = 3
	compensationPause    = 500 * time.Millisecond
)

type created1C struct {
	Rental *struct {
		ID string `json:"id"`
	} `json:"rental"`
	Status              *string `json:"status"`
	TemporarilyReserved *bool   `json:"temporarily_reserved"`
}

// writer is the technical client, when it books for partner.
func (d *Driver) writer(partner string) bool {
	return d.c.phone != "" && d.c.partner == partner
}

// Create implements driver.Driver: a rental of the kind's service by the
// technical client, count = minutes / 30 (1C ignores duration). It is a
// booking only when 1C confirms it at once (status reserved, no temporary
// reservation); otherwise it is removed and the create refused, so no
// half-booking stays behind.
func (d *Driver) Create(ctx context.Context, req driver.CreateRequest) (driver.Booking, error) {
	c, ok := d.byCourt[req.Court]
	if !ok {
		return driver.Booking{}, fmt.Errorf("court %q: %w", req.Court, driver.ErrNotFound)
	}
	svc := d.services[req.Kind]
	if svc == "" {
		return driver.Booking{}, fmt.Errorf("no rental service is configured for %s bookings: %w", req.Kind, driver.ErrBookingNotConfigured)
	}
	if !d.writer(req.Partner) {
		return driver.Booking{}, fmt.Errorf("the 1C technical client books for another partner: %w", driver.ErrBookingNotConfigured)
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return driver.Booking{}, err
	}
	// the club may have changed the service since its config was checked:
	// a price or a payment basis would leave a debt or a 15-minute
	// reservation, another step another length (count x step)
	if err := d.bookingServiceNow(ctx, svc); err != nil {
		return driver.Booking{}, err
	}
	minutes := int(req.End.Sub(req.Start) / time.Minute)
	start := req.Start.In(loc)
	if minutes <= 0 || minutes%seance != 0 || start.Minute()%seance != 0 || start.Second() != 0 {
		return driver.Booking{}, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("1C books whole %d-minute seances on its grid", seance))
	}
	body := map[string]any{
		"date_time":  start.Format(dateTimeLayout),
		"service_id": svc,
		"room_id":    c.room,
		"count":      minutes / seance,
	}
	var out created1C
	err = d.c.as(ctx, gatewayClient, func(tok string) error {
		env, err := d.c.call(ctx, http.MethodPost, "/rental/", nil, body, tok, true)
		if err != nil {
			return err
		}
		if json.Unmarshal(env.Data, &out) != nil {
			return driver.Upstream(driver.UpstreamInvalid, "POST /rental/: data of an unexpected form")
		}
		return nil
	})
	if codeOf(err) == codeRoomBusy {
		return driver.Booking{}, fmt.Errorf("POST /rental/: %w", driver.ErrSlotUnavailable)
	}
	if err != nil {
		return driver.Booking{}, err
	}
	if out.Rental == nil || !uuidRe.MatchString(out.Rental.ID) {
		// the rental may exist without us knowing its id: the partner sees
		// an error and must re-read occupancy before retrying
		return driver.Booking{}, driver.Upstream(driver.UpstreamInvalid, "POST /rental/: answer has no valid rental id")
	}
	id := strings.ToLower(out.Rental.ID)
	if out.TemporarilyReserved == nil || *out.TemporarilyReserved || out.Status == nil || *out.Status != statusConfirmed {
		d.compensate(ctx, gatewayClient, id, "not confirmed without payment")
		return driver.Booking{}, driver.Upstream(driver.UpstreamRejected,
			"POST /rental/: 1C made a temporary reservation, not a booking: the service must not require a payment basis")
	}
	d.log.Info("rental created", "court", req.Court, "count", minutes/seance)
	return driver.Booking{ID: ownID(id), Court: req.Court, Start: req.Start, End: req.End, Status: driver.StatusActive}, nil
}

// bookingServiceNow reads the rental services and checks the booking
// service as CheckConfig does: present, free, no payment basis, the proven
// step.
func (d *Driver) bookingServiceNow(ctx context.Context, svc string) error {
	list, err := listServices(ctx, d.c, d.studio)
	if err != nil {
		return err
	}
	for _, s := range list {
		if s.id() != svc {
			continue
		}
		if s.PaymentRequired == nil || *s.PaymentRequired || !free(s.Price) || s.step() != seance {
			return fmt.Errorf("the booking service is no longer free, without a payment basis, of 30-minute seances: %w", driver.ErrBookingNotConfigured)
		}
		return nil
	}
	return fmt.Errorf("the booking service is not among the club's rental services: %w", driver.ErrBookingNotConfigured)
}

// compensate removes a rental of technical client who that must not
// exist, up to three times; the last failure is logged as an error with
// the rental's id (no personal data) for removal by hand.
func (d *Driver) compensate(ctx context.Context, who techClient, id, why string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	for attempt := 1; attempt <= compensationAttempts; attempt++ {
		err := d.deleteRental(ctx, who, id)
		if err == nil {
			d.log.Warn("1C: rental removed", "why", why, "rental", id)
			return
		}
		d.log.Warn("1C: removing a rental failed", "why", why, "rental", id, "attempt", attempt, "error", err)
		if attempt < compensationAttempts {
			select {
			case <-ctx.Done():
			case <-time.After(compensationPause * time.Duration(attempt)):
			}
		}
	}
	d.log.Error("1C: a rental stayed in 1C, remove it by hand", "why", why, "rental", id)
}

// deleteRental cancels a rental of technical client who; 1037 "already
// cancelled" is success.
func (d *Driver) deleteRental(ctx context.Context, who techClient, id string) error {
	err := d.c.as(ctx, who, func(tok string) error {
		_, err := d.c.call(ctx, http.MethodDelete, "/rental/", url.Values{"rental_id": {id}}, nil, tok, false)
		return err
	})
	if codeOf(err) == codeAlreadyCanceled {
		return nil
	}
	return err
}

// Cancel implements driver.Driver. A rental is cancelled only when it is
// among the technical client's appointments of a gateway service, the
// technical client books for the caller, its court is configured and it
// has not started. A busy stretch of the grid is never ours. A rental that
// is already cancelled counts as cancelled.
func (d *Driver) Cancel(ctx context.Context, id string, partner string) error {
	if room, _, ok := parseForeignID(id); ok {
		if _, configured := d.byRoom[room]; configured {
			return driver.ErrNotOwned
		}
		return driver.ErrNotFound
	}
	rid, ok := parseOwnID(id)
	if !ok {
		return driver.ErrNotFound
	}
	if !d.writer(partner) {
		return driver.ErrNotOwned
	}
	loc, err := d.zoneNow(ctx)
	if err != nil {
		return err
	}
	own, complete, err := d.ownRentals(ctx, loc)
	if err != nil {
		return err
	}
	for _, r := range own {
		if r.id != rid {
			continue
		}
		if _, ok := d.byRoom[r.room]; !ok {
			return driver.ErrCourtNotConfigured
		}
		now, known := d.clock()
		switch {
		case !known:
			return driver.ErrClockUnknown
		case r.started || !r.Start.After(now):
			return driver.ErrStarted
		}
		if err := d.deleteRental(ctx, gatewayClient, rid); err != nil {
			return err
		}
		d.log.Info("rental cancelled")
		return nil
	}
	// not among the planned and running: gone already, ended, or not ours
	r, s, err := d.readRental(ctx, rid, loc)
	if err != nil {
		return err
	}
	if *r.Canceled {
		return nil
	}
	if now, known := d.clock(); known && !s.Start.After(now) && r.Service != nil && d.ours(r.Service.ID) {
		return driver.ErrStarted // an ended rental is not listed; started is the answer either way
	}
	if !complete {
		return driver.Upstream(driver.UpstreamInvalid, "the technical client's appointments were not all read: ownership unknown")
	}
	return driver.ErrNotOwned
}

// MarkPaid implements driver.Driver: the gateway's rentals are of a free
// service, there is nothing to mark.
func (d *Driver) MarkPaid(context.Context, string, string) error { return driver.ErrNotSupported }
