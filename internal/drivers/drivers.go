// Package drivers lists the CRM drivers built into the binary. A driver
// registers itself (driver.Register, driver.RegisterSetup) from its own
// package's init; adding one is one import line here.
package drivers

import (
	"sort"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	_ "github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c"
	_ "github.com/Padelapp-Club/court-gateway/internal/driver/lunda"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	_ "github.com/Padelapp-Club/court-gateway/internal/driver/yclients"
)

// DefaultCRM is the crm.type setup takes when none is given: the CRM of
// the first installations, kept so their instructions stay true.
const DefaultCRM = "viva"

// The drivers that predate self-registration are registered here.
func init() {
	driver.Register("viva", driver.CRM{Driver: viva.New, Account: viva.NewAccount, ClubCRMKeys: viva.ClubCRMKeys,
		StudioKeys: []string{viva.StudioKey}, CourtKeys: []string{viva.RoomKey}})
	driver.RegisterSetup("viva", driver.SetupSpec{
		Name:  "Viva",
		Needs: "логин и пароль администратора Viva",
		Fields: []driver.SetupField{
			{Key: "username", Flag: "crm-username", Env: "CRM_USERNAME", Prompt: "Логин администратора Viva (e-mail)", Required: true},
			{Key: "auth_url", Flag: "crm-auth-url"},
			{Key: "api_url", Flag: "crm-api-url"},
		},
		Secrets: []driver.SetupSecret{{Env: "CRM_PASSWORD", Prompt: "Пароль администратора Viva", Required: true}},
	})
	driver.Register("demo", driver.CRM{Driver: demo.New, Account: demo.NewAccount, ClubCRMKeys: demo.ClubCRMKeys,
		StudioKeys: []string{"studio"}})
	driver.RegisterSetup("demo", driver.SetupSpec{Name: "демо-CRM", Needs: "только код подключения (демо-CRM, не для клубов)", Hidden: true})

	for t, c := range driver.Registered() {
		Factories[t], Accounts[t] = c.Driver, c.Account
	}
}

// Factories build the driver of one club, by crm.type.
var Factories = map[string]driver.Factory{}

// Accounts build the CRM account of an installation (login check and
// discovery), by crm.type.
var Accounts = map[string]driver.AccountFactory{}

// ClubCRMKeys returns the club-level crm keys of a CRM type (none for an
// unknown type). Every other key (the login, the CRM endpoints) belongs to
// the installation and comes only from its gateway.yaml.
func ClubCRMKeys(crmType string) []string { return driver.Registered()[crmType].ClubCRMKeys }

// ResourceKeys returns the keys of every driver that hold the CRM
// resources a club takes — crm keys of a whole studio, court params of one
// room: what the hub keeps for one club, not knowing the CRM type. Each
// key is its own namespace (the hub keeps key and value), so ids of two
// CRMs never collide unless two drivers share a key name.
func ResourceKeys() (studio, room []string) {
	var s, r []string
	for _, c := range driver.Registered() {
		s = append(s, c.StudioKeys...)
		r = append(r, c.CourtKeys...)
	}
	return unique(s), unique(r)
}

// AllClubCRMKeys returns the club-level crm keys of every driver: what the
// hub accepts in a club, not knowing the installation's CRM type.
func AllClubCRMKeys() []string {
	var out []string
	for _, c := range driver.Registered() {
		out = append(out, c.ClubCRMKeys...)
	}
	return unique(out)
}

func unique(keys []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
