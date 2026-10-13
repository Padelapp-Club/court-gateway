// Package driver defines the contract between the CRM-agnostic gateway core
// and a CRM-specific driver. The core never talks to a CRM directly: every
// read and write goes through the Driver interface, and every response the
// core sends is built from the neutral types declared here.
package driver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Sentinel errors. Drivers wrap them (fmt.Errorf("...: %w", ErrX)) and the
// core maps them to API error codes with errors.Is.
var (
	// ErrNotSupported: the CRM has no such capability (e.g. payment marks).
	ErrNotSupported = errors.New("not supported by this CRM")
	// ErrNotOwned: the booking exists but was not created through the
	// gateway by the calling partner, so the gateway refuses to touch it.
	ErrNotOwned = errors.New("booking is not owned by the caller")
	// ErrSlotUnavailable: the requested interval overlaps existing occupancy.
	ErrSlotUnavailable = errors.New("slot is unavailable")
	// ErrNotFound: no such booking/court/price list.
	ErrNotFound = errors.New("not found")
	// ErrUpstream: the CRM failed or answered something unusable. Use
	// *UpstreamError to say how exactly.
	ErrUpstream = errors.New("upstream CRM error")
)

// UpstreamKind classifies an upstream failure.
type UpstreamKind int

const (
	// UpstreamUnavailable: network error, timeout or 5xx.
	UpstreamUnavailable UpstreamKind = iota
	// UpstreamAuth: the CRM rejected the gateway's credentials.
	UpstreamAuth
	// UpstreamInvalid: the CRM answered with a body the driver cannot
	// interpret. The core must never turn this into "everything is free".
	UpstreamInvalid
	// UpstreamRejected: the CRM refused the request (4xx other than auth).
	UpstreamRejected
)

// UpstreamError carries a classified upstream failure. Detail must never
// contain secrets or personal data: it is logged.
type UpstreamError struct {
	Kind   UpstreamKind
	Detail string
	Err    error
}

func (e *UpstreamError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("upstream: %s: %v", e.Detail, e.Err)
	}
	return "upstream: " + e.Detail
}

// Is makes errors.Is(err, ErrUpstream) true for every UpstreamError.
func (e *UpstreamError) Is(target error) bool { return target == ErrUpstream }

// Unwrap exposes the cause.
func (e *UpstreamError) Unwrap() error { return e.Err }

// Upstream builds an *UpstreamError.
func Upstream(kind UpstreamKind, detail string, err error) error {
	return &UpstreamError{Kind: kind, Detail: detail, Err: err}
}

// Capabilities are reported to partners as-is.
type Capabilities struct {
	// Payments: the driver can mark a booking as paid.
	Payments bool
}

// Kind of a booking requested by a partner.
type Kind string

const (
	KindGame       Kind = "game"
	KindTournament Kind = "tournament"
)

// Interval is one occupied stretch of a court. It deliberately has no room
// for anything but the id, the court and the time span.
type Interval struct {
	ID    string
	Court string // gateway court id
	Start time.Time
	End   time.Time
}

// Occupancy is the answer to an occupancy query.
type Occupancy struct {
	// Complete is false when the driver knows the CRM could not return the
	// whole picture (e.g. the CRM returns nothing for past dates).
	Complete  bool
	Intervals []Interval
}

// OccupancyQuery asks for intervals overlapping [From, To).
type OccupancyQuery struct {
	Courts []string // gateway court ids, never empty
	From   time.Time
	To     time.Time
}

// CreateRequest asks the driver to create a booking.
type CreateRequest struct {
	Court   string
	Start   time.Time
	End     time.Time
	Kind    Kind
	Note    string // free text from the partner: written, never read back or logged
	Partner string // name of the calling partner, used for the ownership marker
}

// BookingStatus of a booking read back.
type BookingStatus string

const (
	StatusActive    BookingStatus = "active"
	StatusCancelled BookingStatus = "cancelled"
)

// Booking is what the core may expose about a single booking.
type Booking struct {
	ID     string
	Court  string
	Start  time.Time
	End    time.Time
	Status BookingStatus
}

// Driver is implemented by every CRM driver.
type Driver interface {
	// Capabilities are static for the lifetime of the driver.
	Capabilities() Capabilities
	// Ready checks that the CRM is reachable with the configured
	// credentials (login plus a narrow read).
	Ready(ctx context.Context) error
	// Occupancy returns intervals overlapping the query range.
	Occupancy(ctx context.Context, q OccupancyQuery) (Occupancy, error)
	// PriceTable returns the price rules of a court.
	PriceTable(ctx context.Context, court string) (PriceTable, error)
	// Create creates a booking. The core has already checked occupancy.
	Create(ctx context.Context, req CreateRequest) (Booking, error)
	// Get reads one booking by id.
	Get(ctx context.Context, id string) (Booking, error)
	// Cancel cancels a booking created through the gateway by partner.
	// A booking that is already gone is not an error.
	Cancel(ctx context.Context, id string, partner string) error
	// MarkPaid marks a booking as paid (ErrNotSupported if unsupported).
	MarkPaid(ctx context.Context, id string, partner string) error
}

// CourtConfig is one court from the gateway config. Params holds the
// driver-specific keys of the court entry; the core does not interpret them.
type CourtConfig struct {
	ID     string
	Name   string
	Params Params
}

// Config is what a driver factory receives.
type Config struct {
	Timezone    *time.Location
	SlotMinutes int
	// CRM holds every key of the `crm` section except `type`.
	CRM Params
	// Password is the CRM password taken from the environment.
	Password string
	Courts   []CourtConfig
	// Services maps "booking"/"tournament" to driver-specific parameters.
	Services map[string]Params
	Logger   *slog.Logger
	// HTTPClient is optional; drivers create their own when nil.
	HTTPClient *http.Client
	// Now is optional; drivers use time.Now when nil.
	Now func() time.Time
}

// Factory builds a driver or returns a config error with a message for the
// club administrator (in Russian).
type Factory func(Config) (Driver, error)

// LoginChecker is implemented by drivers that can check the CRM login on
// its own, separately from a data read (used by `doctor`).
type LoginChecker interface {
	CheckLogin(ctx context.Context) error
}
