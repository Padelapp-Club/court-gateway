// Package driver defines the contract between the CRM-agnostic gateway core
// and a CRM-specific driver. The core never talks to a CRM directly: every
// read and write goes through the Driver interface, and every response the
// core sends is built from the neutral types declared here.
package driver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sentinel errors: drivers wrap them, the core maps them to API codes.
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
	// ErrStarted: the booking has already started; it is not cancelled.
	ErrStarted = errors.New("booking has already started")
	// ErrCourtNotConfigured: the booking's CRM room is no court of the club's config.
	ErrCourtNotConfigured = errors.New("booking is on a court that is not configured")
	// ErrClockUnknown: the time now is not known (Clock): no decision on it.
	ErrClockUnknown = errors.New("the hub's time is not known yet")
)

// Clock is the time of every booking decision: the hub's, as last signed
// (tunnel.Verifier.Now), so a club computer whose clock is off decides as
// the hub does. Until known, now is this computer's and nothing is decided.
type Clock func() (now time.Time, known bool)

// MisconfigError says, in Russian, what in a club's config does not match
// the CRM (a studio or a room that is not in the account, another time
// zone). Unlike an UpstreamError it does not pass by itself.
type MisconfigError struct {
	Problems []string
}

func (e *MisconfigError) Error() string { return strings.Join(e.Problems, "; ") }

// ConfigAlert says, in Russian, what in a club's config disagrees with the
// CRM while the club stays served; readiness reports Code until it is fixed.
type ConfigAlert struct {
	Code     string // a readiness reason, e.g. TIMEZONE_MISMATCH
	Problems []string
}

func (e *ConfigAlert) Error() string { return e.Code + ": " + strings.Join(e.Problems, "; ") }

// UpstreamKind classifies an upstream failure.
type UpstreamKind int

const (
	// UpstreamUnavailable: network error, timeout or 5xx.
	UpstreamUnavailable UpstreamKind = iota
	// UpstreamAuth: the CRM rejected the gateway's credentials.
	UpstreamAuth
	// UpstreamLogin: a login refused not for the credentials: retried like an outage.
	UpstreamLogin
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
}

func (e *UpstreamError) Error() string { return "upstream: " + e.Detail }

// Upstream builds an *UpstreamError.
func Upstream(kind UpstreamKind, detail string) error {
	return &UpstreamError{Kind: kind, Detail: detail}
}

// Capabilities are reported to partners as-is.
type Capabilities struct {
	Payments bool
	// Currency is the ISO 4217 code of the CRM's prices (PriceTable
	// amounts are its hundredths).
	Currency string
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
	// Complete is false when the CRM may not have returned everything.
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
	Occupancy(ctx context.Context, q OccupancyQuery) (Occupancy, error)
	PriceTable(ctx context.Context, court string) (PriceTable, error)
	// Create creates a booking. The core has already checked occupancy.
	Create(ctx context.Context, req CreateRequest) (Booking, error)
	Get(ctx context.Context, id string) (Booking, error)
	// Cancel cancels a booking created through the gateway by partner,
	// before it starts (ErrStarted after). A booking that is already gone
	// is not an error.
	Cancel(ctx context.Context, id string, partner string) error
	// MarkPaid marks a booking as paid (ErrNotSupported if unsupported).
	MarkPaid(ctx context.Context, id string, partner string) error
}

// CourtConfig is one court; the core does not interpret its driver Params.
type CourtConfig struct {
	ID     string
	Name   string
	Params Params
}

// Config is what a driver factory receives.
type Config struct {
	Timezone *time.Location
	// CRM holds every key of the `crm` section except `type`.
	CRM      Params
	Password string
	// Secrets are the values of the environment variables the crm.type
	// declares (SetupSpec.Secrets), by name; CRM_PASSWORD is also Password.
	Secrets map[string]string
	Courts   []CourtConfig
	// Services maps "booking"/"tournament" to driver-specific parameters.
	Services map[string]Params
	Logger   *slog.Logger
	// HTTPClient is optional; nil: the driver's own, through Proxy if set.
	HTTPClient *http.Client
	Proxy      func(*http.Request) (*url.URL, error)
	// Now is this computer's clock, for durations (a login token's
	// lifetime); time.Now when nil.
	Now func() time.Time
	// Clock is the time of booking decisions; nil takes Now as known
	// (tests and the local commands, which make none). See DecisionClock.
	Clock Clock
	// Account is the installation's CRM account, if any: its clubs' drivers
	// may share its session, so they all log in once.
	Account Account
	// Rejections, when set, keeps the account's rejected logins across
	// restarts (the service's state directory): the retry budget after a
	// rejection is the installation's, not one process's.
	Rejections RejectionStore
}

// DecisionClock is c.Clock, or c.Now (time.Now) taken as known.
func (c Config) DecisionClock() Clock {
	if c.Clock != nil {
		return c.Clock
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	return func() (time.Time, bool) { return now(), true }
}

// Factory builds a driver or returns a config error (in Russian).
type Factory func(Config) (Driver, error)

// Resourcer names the CRM resource behind a court (a Viva room), the same
// for every club and config. The core locks per resource from the
// occupancy check to the end of the create, so nothing books one interval
// twice through this gateway; without it the lock is per driver and court.
type Resourcer interface {
	Resource(court string) string
}

// ConfigChecker is implemented by drivers that can check a club's config
// against the CRM: CheckConfig returns a *MisconfigError when it does not
// match, a *ConfigAlert when it disagrees but can be served, any other
// error when the check could not be made.
type ConfigChecker interface {
	CheckConfig(ctx context.Context) error
}

// AccountClub: Studio is the club's own id in a CRM account of several
// clubs (see Account), as Studio.ID names it.
type AccountClub interface {
	Studio() string
}

// ZoneHorizon is how far ahead two zones must agree to count as the same
// for a club: the booking horizon, past at least one clock change.
const ZoneHorizon = 62 * 24 * time.Hour

// ZonesDiffer returns the first instant in [from, from+horizon] at which a
// and b are not as far from UTC, and whether there is one.
func ZonesDiffer(a, b *time.Location, from time.Time, horizon time.Duration) (time.Time, bool) {
	end := from.Add(horizon)
	for t := from; !t.After(end); {
		_, oa := t.In(a).Zone()
		_, ob := t.In(b).Zone()
		if oa != ob {
			return t, true
		}
		// the next instant either zone changes its offset
		next := end.Add(time.Nanosecond)
		for _, l := range []*time.Location{a, b} {
			if _, ze := t.In(l).ZoneBounds(); !ze.IsZero() && ze.Before(next) {
				next = ze
			}
		}
		t = next
	}
	return time.Time{}, false
}
