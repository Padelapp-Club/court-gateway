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

// AccountFactory builds an Account. Config carries CRM, Password, Logger,
// HTTPClient and Now; Courts and Services are empty.
type AccountFactory func(Config) (Account, error)

// DiscoverQuery asks for draft material.
type DiscoverQuery struct {
	// Studio is the CRM's own id of a club within the account (VivaCRM
	// calls it a studio). Empty asks the driver to list the studios it can
	// see.
	Studio string
	// Days of future occupancy to scan for courts (default 28).
	Days int
}

// Studio is one club of the account as the CRM names it.
type Studio struct {
	ID   string
	Name string
}

// DiscoveredCourt is a court candidate.
type DiscoveredCourt struct {
	Name     string
	Bookings int
	// Params are the driver-specific keys of the court entry.
	Params Params
}

// PriceListInfo names a price list of a studio.
type PriceListInfo struct {
	ID   string
	Name string
}

// Notice is something the operator must look at in a draft.
type Notice struct {
	Code    string
	Message string
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
