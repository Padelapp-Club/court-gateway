package drivers

import (
	"slices"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Every built-in CRM type is served whole: a driver, an account, a setup,
// and its resource keys reach the hub.
func TestEveryRegisteredCRMIsComplete(t *testing.T) {
	studio, room := ResourceKeys()
	for typ, c := range driver.Registered() {
		if Factories[typ] == nil || Accounts[typ] == nil {
			t.Errorf("%s: no factory or account", typ)
		}
		if _, ok := driver.SetupFor(typ); !ok {
			t.Errorf("%s: no setup", typ)
		}
		for _, k := range c.StudioKeys {
			if !slices.Contains(studio, k) {
				t.Errorf("%s: studio key %s not among the hub's", typ, k)
			}
		}
		for _, k := range c.CourtKeys {
			if !slices.Contains(room, k) {
				t.Errorf("%s: court key %s not among the hub's", typ, k)
			}
		}
		for _, k := range c.ClubCRMKeys {
			if !slices.Contains(AllClubCRMKeys(), k) || !slices.Contains(ClubCRMKeys(typ), k) {
				t.Errorf("%s: club key %s lost", typ, k)
			}
		}
	}
	if !slices.Contains(room, "viva_room") || !slices.Contains(studio, "studio") {
		t.Fatalf("resource keys: %v %v", studio, room)
	}
}

func TestRegisterTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a second registration of viva did not panic")
		}
	}()
	driver.Register("viva", driver.CRM{Driver: Factories["viva"], Account: Accounts["viva"]})
}
