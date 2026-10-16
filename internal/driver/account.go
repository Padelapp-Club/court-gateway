package driver

import "context"

// An installation of the gateway is one CRM account. The account may hold
// several clubs (a club network); each club is configured on the hub and
// served by its own Driver. Before any club is configured the gateway
// still has the account: it can check the login and read the CRM to draft
// a club configuration for the operator.

// Account is the CRM account of an installation, built from the
// installation-level config only (the `crm` section and the password; no
// courts, no club).
type Account interface {
	// CheckLogin checks the CRM login.
	CheckLogin(ctx context.Context) error
	// Discover reads the CRM and returns draft material for a club config.
	// It never returns personal data: rooms, price lists and hours only.
	Discover(ctx context.Context, q DiscoverQuery) (Discovery, error)
}

// PasswordSetter is implemented by accounts that take a new CRM password
// while running (the gateway re-reads .env after set-password): it ends a
// stop after a rejected login. PasswordRejected reports that stop: the
// CRM rejected the password, and nothing logs in until it changes.
type PasswordSetter interface {
	SetPassword(password string)
	PasswordRejected() bool
}

// SessionEnder is implemented by accounts whose CRM login opens a session
// on the CRM's side that outlives the process: EndSession ends the session
// of the last login, and the next call logs in again. The commands that log
// in only to check the login (setup, set-password, doctor) end their
// session, so the CRM sees one session of the installation, the service's.
type SessionEnder interface {
	EndSession(ctx context.Context) error
}

// EndSession ends acct's CRM session if it has one to end.
func EndSession(ctx context.Context, acct Account) error {
	if se, ok := acct.(SessionEnder); ok {
		return se.EndSession(ctx)
	}
	return nil
}

// AccountFactory builds an Account. Config carries CRM, Password, Logger,
// HTTPClient and Now; Courts and Services are empty.
type AccountFactory func(Config) (Account, error)

// DiscoverQuery asks for draft material.
type DiscoverQuery struct {
	// Studio is the CRM's own id of a club within the account (VivaCRM
	// calls it a studio). Empty asks the driver to list the studios it can
	// see.
	Studio string
}

// Studio is one club of the account as the CRM names it.
type Studio struct {
	ID   string
	Name string
}

// DiscoveredCourt is a court candidate.
type DiscoveredCourt struct {
	Name string
	// Params are the driver-specific keys of the court entry.
	Params Params
}

// PriceListInfo names a price list of a studio.
type PriceListInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Notice is something the operator must look at in a draft.
type Notice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Discovery is draft material for one club.
type Discovery struct {
	// Studios the driver could see; empty with StudiosReason when it
	// cannot list them.
	Studios       []Studio
	StudiosReason string
	// The rest is filled only when a studio was asked for (or the account
	// has exactly one).
	Studio     string
	ClubName   string
	CRM        Params // club-level keys of the `crm` section (e.g. the studio)
	Courts     []DiscoveredCourt
	PriceLists []PriceListInfo
	// Timezone is an IANA zone when the CRM tells it ("" otherwise).
	Timezone string
	// Hours per ISO weekday (0 = Monday) in minutes; nil where unknown.
	Hours   [7]*[2]int
	Notices []Notice
}
