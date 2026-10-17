package driver

import (
	"context"
	"errors"
	"time"
)

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
// stop after a rejected login at once. PasswordRejected reports that stop:
// the CRM rejected the login, and it is tried again only rarely, next at
// NextLogin (zero without a stop).
type PasswordSetter interface {
	SetPassword(password string)
	PasswordRejected() bool
	NextLogin() time.Time
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

// Rejections are an account's logins the CRM rejected in a row. Key tells
// which credentials they are about (a hash, never the password itself):
// rejections of other credentials do not count.
type Rejections struct {
	Key   string    `json:"key"`
	Count int       `json:"count"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// RejectionStore keeps Rejections; Load of nothing kept is the zero value.
type RejectionStore interface {
	LoadRejections() (Rejections, error)
	SaveRejections(Rejections) error
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

// SetupField is an installation-level key of the `crm` section that setup
// asks for (or takes from a flag or the environment) and writes to
// gateway.yaml. It is never a secret: secrets are SetupSecrets.
type SetupField struct {
	Key  string // the crm key, e.g. "username"
	Flag string // the setup flag without dashes, e.g. "crm-username"
	Env  string // optional environment fallback, e.g. "CRM_USERNAME"
	// Prompt is the interactive question, in Russian; empty: flag only
	// (an endpoint override for test doubles).
	Prompt   string
	Required bool
}

// SetupSecret is a CRM secret setup writes to .env (mode 0600), never to
// gateway.yaml. The gateway reads it from the environment, keeps it out of
// logs and hands it to the driver in Config.Secrets.
type SetupSecret struct {
	Env      string // e.g. "CRM_PASSWORD"
	Prompt   string // the interactive question, in Russian
	Required bool
}

// SetupSpec is what setup needs for one crm.type: the driver's own words
// for the administrator, the keys and the secrets. Drivers register theirs
// with RegisterSetup, so setup and the config loader know no CRM by name.
type SetupSpec struct {
	// Name is the CRM as a club knows it ("Viva").
	Name string
	// Needs completes «Понадобятся … и код подключения.», in Russian.
	Needs   string
	Fields  []SetupField
	Secrets []SetupSecret
	// Hidden types are never offered to a club (the demo CRM).
	Hidden bool
	// Interactive: the CRM has no password; a person logs the gateway in
	// (InteractiveLogin), at setup and again with `court-gateway login`
	// whenever the CRM ends the session.
	Interactive bool
}

var setups = map[string]SetupSpec{}

// RegisterSetup declares the setup of a crm.type; a driver package calls
// it from init. A second registration of a type panics.
func RegisterSetup(crmType string, spec SetupSpec) {
	if _, dup := setups[crmType]; dup {
		panic("driver: setup of crm.type " + crmType + " registered twice")
	}
	setups[crmType] = spec
}

// SetupFor returns the registered setup of a crm.type.
func SetupFor(crmType string) (SetupSpec, bool) {
	s, ok := setups[crmType]
	return s, ok
}

// SecretEnvs are the environment variables holding a crm.type's secrets.
func SecretEnvs(crmType string) []string {
	var out []string
	for _, s := range setups[crmType].Secrets {
		out = append(out, s.Env)
	}
	return out
}

// LoginMethod is a way a person logs an interactive account in.
type LoginMethod string

const (
	// LoginQR: a member of the club's staff scans a QR code in the CRM's
	// mobile app and confirms there.
	LoginQR LoginMethod = "qr"
	// LoginCode: the CRM sends a one-time code to the staff member's phone
	// (by its own channel), and it is typed in.
	LoginCode LoginMethod = "code"
)

// Errors of an interactive login.
var (
	// ErrLoginExpired: the QR code or the code expired before the person
	// acted; start again (a QR is reissued).
	ErrLoginExpired = errors.New("the login challenge expired")
	// ErrLoginDeclined: the person declined the login in the CRM's app, or
	// the code was wrong.
	ErrLoginDeclined = errors.New("the login was declined")
	// ErrNoSession: an interactive account has no session: nobody logged
	// the gateway in yet, or the CRM ended the session. Wrapped in an
	// *UpstreamError of kind UpstreamAuth where a call needs one.
	ErrNoSession = errors.New("no CRM session: log the gateway in (court-gateway login)")
)

// Challenge is what the person logging the gateway in acts on. It is shown
// to that person only: neither the QR payload nor anything typed is ever
// logged.
type Challenge struct {
	Method LoginMethod
	// QR is the exact text the QR code encodes (LoginQR).
	QR string
	// Sent says, in Russian, where the code went (LoginCode), e.g. «в
	// Telegram или пушем в приложение».
	Sent string
	// Expires is when the challenge stops working (zero: unknown).
	Expires time.Time
}

// LoginState is where an interactive login stands.
type LoginState struct {
	// Done: the CRM accepted the login. With several clubs the session is
	// not usable before SelectClub.
	Done bool
	// Clubs the person may act for, when the CRM tells (more than one
	// needs SelectClub).
	Clubs []Studio
}

// InteractiveLogin is implemented by accounts whose CRM has no password and
// is entered by a person. A successful login (and every renewal of the
// session the CRM asks for later) is saved to Config.Sessions, so the
// session survives restarts; an account built with a saved session uses it
// without a person.
type InteractiveLogin interface {
	// LoginMethods the CRM offers, the preferred first.
	LoginMethods() []LoginMethod
	// StartLogin starts a login: for LoginCode, phone is the staff
	// member's number in international form (+7…); ignored for LoginQR.
	StartLogin(ctx context.Context, method LoginMethod, phone string) (Challenge, error)
	// PollLogin (LoginQR) asks whether the person confirmed; not Done
	// while waiting, ErrLoginExpired or ErrLoginDeclined when it is over.
	PollLogin(ctx context.Context) (LoginState, error)
	// ConfirmLogin (LoginCode) sends the code the person typed.
	ConfirmLogin(ctx context.Context, code string) (LoginState, error)
	// SelectClub picks the club of the session (LoginState.Clubs), then
	// saves it.
	SelectClub(ctx context.Context, id string) error
	// HasSession: a session is saved (it may still be refused).
	HasSession() bool
}

// SessionStore keeps an interactive account's CRM session between
// processes: setup and `court-gateway login` save it, the service uses and
// renews it. The bytes are the driver's own (a secret: the store keeps
// them readable by the owner only). Load of nothing saved is nil, nil;
// Save(nil) removes the session.
type SessionStore interface {
	LoadSession() ([]byte, error)
	SaveSession([]byte) error
}

// Client is a client of the club as a person choosing it sees it. It is
// shown in the terminal of the person setting the gateway up and never
// logged.
type Client struct {
	ID   string
	Name string
	// Hint tells clients of one name apart (a masked phone), may be empty.
	Hint string
}

// ClientPicker is implemented by accounts whose bookings are made on a
// client of the club the club chooses at setup (a service client): its id
// is the crm key ClientKey of the installation. Without it the driver
// reads, but does not book.
type ClientPicker interface {
	ClientKey() string
	FindClients(ctx context.Context, pattern string) ([]Client, error)
}
