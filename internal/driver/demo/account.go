package demo

import (
	"context"
	"fmt"
	"strings"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// The demo account is a small club network: two studios with their own
// courts, so the multi-club path can be tried and tested without a CRM.
type demoStudio struct {
	id, name string
	courts   []string
}

var studios = []demoStudio{
	{id: "demo-north", name: "Демо Север", courts: []string{"Корт 1", "Корт 2"}},
	{id: "demo-south", name: "Демо Юг", courts: []string{"Корт A", "Корт B", "Корт C"}},
}

func studioByID(id string) *demoStudio {
	for i := range studios {
		if studios[i].id == id {
			return &studios[i]
		}
	}
	return nil
}

func studioIDs() string {
	ids := make([]string, len(studios))
	for i, s := range studios {
		ids[i] = s.id
	}
	return strings.Join(ids, ", ")
}

// Account is the demo CRM account.
type Account struct{}

var _ driver.Account = Account{}

// NewAccount is the driver.AccountFactory for crm.type "demo".
func NewAccount(cfg driver.Config) (driver.Account, error) {
	if err := cfg.CRM.RejectUnknown("crm", "studio"); err != nil {
		return nil, err
	}
	return Account{}, nil
}

// CheckLogin implements driver.Account: the demo CRM has no login.
func (Account) CheckLogin(context.Context) error { return nil }

// Discover implements driver.Account.
func (Account) Discover(_ context.Context, q driver.DiscoverQuery) (driver.Discovery, error) {
	var out driver.Discovery
	for _, s := range studios {
		out.Studios = append(out.Studios, driver.Studio{ID: s.id, Name: s.name})
	}
	if q.Studio == "" {
		out.Notices = append(out.Notices, driver.Notice{Code: "STUDIO_REQUIRED", Message: "в учебной CRM несколько студий: укажите studio, чтобы получить черновик клуба"})
		return out, nil
	}
	st := studioByID(q.Studio)
	if st == nil {
		return out, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("no studio %q in the demo CRM", q.Studio))
	}
	out.Studio, out.ClubName = st.id, st.name
	out.CRM = driver.Params{"studio": st.id}
	for _, c := range st.courts {
		out.Courts = append(out.Courts, driver.DiscoveredCourt{Name: c, Params: driver.Params{}})
	}
	for i := range out.Hours {
		out.Hours[i] = &[2]int{7 * 60, 23 * 60}
	}
	return out, nil
}
