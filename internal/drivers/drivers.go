// Package drivers lists the CRM drivers built into the binary.
package drivers

import (
	"sort"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
)

// Factories build the driver of one club, by crm.type.
var Factories = map[string]driver.Factory{"viva": viva.New, "demo": demo.New}

// Accounts build the CRM account of an installation (login check and
// discovery), by crm.type.
var Accounts = map[string]driver.AccountFactory{"viva": viva.NewAccount, "demo": demo.NewAccount}

// clubCRMKeys are the keys of the crm section that a club configured on the
// hub may set, by crm.type. Every other key (the login, the CRM endpoints)
// belongs to the installation and comes only from its gateway.yaml.
var clubCRMKeys = map[string][]string{"viva": viva.ClubCRMKeys, "demo": demo.ClubCRMKeys}

// ClubCRMKeys returns the club-level crm keys of a CRM type (none for an
// unknown type).
func ClubCRMKeys(crmType string) []string { return clubCRMKeys[crmType] }

// AllClubCRMKeys returns the club-level crm keys of every driver: what the
// hub accepts in a club, not knowing the installation's CRM type.
func AllClubCRMKeys() []string {
	seen := map[string]bool{}
	var out []string
	for _, keys := range clubCRMKeys {
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}
