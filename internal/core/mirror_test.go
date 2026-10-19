package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
)

const hubYAML = "hub: { url: https://hub.example.test, installation: test-net, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= }\n"

func env(k string) string {
	switch k {
	case core.DefaultHubSecretEnv:
		return "test-hub-secret"
	case core.PasswordEnv:
		return "stub-password"
	}
	return ""
}

const (
	studio = "00000000-0000-4000-8000-0000000000aa"
	room1  = "00000000-0000-4000-8000-0000000000b1"
	lc1    = "00000000-0000-4000-8000-0000000000c1"
)

func club(mirror *core.MirrorSpec, params map[string]any) core.ClubSpec {
	return core.ClubSpec{ID: "test-club", Name: "Клуб", Timezone: "Europe/Moscow", SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "07:00-23:00"}, CRM: map[string]any{"studio": studio},
		Courts: []core.CourtSpec{{ID: "court-1", Name: "Корт 1", Params: params}}, Mirror: mirror}
}

// gateway.yaml takes a second CRM of the clubs: Lunda only, never the
// primary's own type.
func TestLoadMirror(t *testing.T) {
	s, err := core.Load([]byte(hubYAML+"crm: { type: viva, username: a@example.test }\nmirror: { type: lunda, service_client: abc }\n"), env)
	if err != nil {
		t.Fatal(err)
	}
	if s.Mirror == nil || s.Mirror.CRMType != "lunda" || s.Mirror.Driver.CRM["service_client"] != "abc" {
		t.Fatalf("mirror: %+v", s.Mirror)
	}
	for _, tc := range []struct{ yaml, want string }{
		{"crm: { type: viva }\nmirror: { service_client: abc }\n", "mirror.type: параметр обязателен"},
		{"crm: { type: viva }\nmirror: { type: yclients }\n", "может быть только lunda"},
		{"crm: { type: lunda }\nmirror: { type: lunda }\n", "совпадает с основной"},
	} {
		if _, err := core.Load([]byte(hubYAML+tc.yaml), env); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.yaml, err)
		}
	}
}

// A club's mirror section turns the pair on; each court's keys go to the
// CRM they belong to.
func TestWithClubSplitsCourts(t *testing.T) {
	base, err := core.Load([]byte(hubYAML+"crm: { type: viva, username: a@example.test }\nmirror: { type: lunda, service_client: abc }\n"), env)
	if err != nil {
		t.Fatal(err)
	}
	keys := drivers.ClubCRMKeys("viva")
	s, err := base.WithClub(club(&core.MirrorSpec{}, map[string]any{"viva_room": room1, "lunda_court": lc1}), keys)
	if err != nil {
		t.Fatal(err)
	}
	if s.MirrorClub == nil || s.MirrorClub.MaxRemovals != core.DefaultMirrorMaxRemovals || s.MirrorClub.MaxCreates != core.DefaultMirrorMaxCreates {
		t.Fatalf("mirror club: %+v", s.MirrorClub)
	}
	if p := s.Driver.Courts[0].Params; p["viva_room"] != room1 || p["lunda_court"] != nil {
		t.Fatalf("the primary's court: %v", p)
	}
	if p := s.Mirror.Driver.Courts[0].Params; p["lunda_court"] != lc1 || p["viva_room"] != nil {
		t.Fatalf("the mirror's court: %v", p)
	}
	if base.Mirror.Driver.Courts != nil {
		t.Fatal("WithClub changed the installation's settings")
	}
	// a Lunda court without the club's mirror section
	if _, err := base.WithClub(club(nil, map[string]any{"viva_room": room1, "lunda_court": lc1}), keys); err == nil || !strings.Contains(err.Error(), "раздела mirror у клуба нет") {
		t.Fatalf("lunda_court without mirror: %v", err)
	}
	// a club without a mirror section is served by the primary alone
	s, err = base.WithClub(club(nil, map[string]any{"viva_room": room1}), keys)
	if err != nil || s.MirrorClub != nil {
		t.Fatalf("a club without a mirror: %v %+v", err, s.MirrorClub)
	}
	// a mirror section where the installation has no second CRM
	plain, _ := core.Load([]byte(hubYAML+"crm: { type: viva, username: a@example.test }\n"), env)
	if _, err := plain.WithClub(club(&core.MirrorSpec{}, map[string]any{"viva_room": room1}), keys); err == nil || !strings.Contains(err.Error(), "нет второй CRM") {
		t.Fatalf("a mirror section without the installation's: %v", err)
	}
}

// The hub checks the limits of a club's mirror section.
func TestValidateMirrorSpec(t *testing.T) {
	n := func(v int) *int { return &v }
	for _, tc := range []struct {
		m  core.MirrorSpec
		ok bool
	}{
		{core.MirrorSpec{}, true},
		{core.MirrorSpec{MaxRemovals: n(10), MaxCreates: n(200)}, true},
		{core.MirrorSpec{MaxRemovals: n(0)}, false},
		{core.MirrorSpec{MaxCreates: n(1001)}, false},
	} {
		m := tc.m
		err := core.ValidateClubSpec(club(&m, map[string]any{"viva_room": room1}), drivers.AllClubCRMKeys())
		if (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.m, err)
		}
	}
}

// A primary that cannot keep mirrors is refused as a config error (F13).
func TestPairNeedsAMirrorer(t *testing.T) {
	base, err := core.Load([]byte(hubYAML+"crm: { type: demo }\nmirror: { type: lunda, service_client: abc }\n"), env)
	if err != nil {
		t.Fatal(err)
	}
	spec := club(&core.MirrorSpec{}, map[string]any{"lunda_court": lc1})
	spec.CRM = nil
	s, err := base.WithClub(spec, drivers.ClubCRMKeys("demo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.BuildDriver(s, drivers.Factories); err == nil || !strings.Contains(err.Error(), "не умеет держать отражения") {
		t.Fatalf("a pair with the demo CRM: %v", err)
	}
}

// 1C in a pair needs a mirror client of its own (F15): none, or the
// partners' one, is a config error.
func TestFitness1CMirrorClient(t *testing.T) {
	env1c := func(k string) string {
		if k == "FITNESS1C_APIKEY" {
			return "stub-key"
		}
		return env(k)
	}
	spec := club(&core.MirrorSpec{}, map[string]any{"fitness1c_room": room1, "lunda_court": lc1})
	spec.Services = map[string]map[string]any{"booking": {"service": "00000000-0000-4000-8000-0000000000e1"}}
	for _, tc := range []struct{ extra, want string }{
		{"", "crm.mirror_client_phone: у 1С в паре"},
		{", mirror_client_phone: '+79000000010'", "совпадает с техническим клиентом"},
	} {
		base, err := core.Load([]byte(hubYAML+"crm: { type: fitness1c, base_url: https://1c.example.test, username: u, client_phone: '+79000000010', partner: padelapp"+tc.extra+" }\nmirror: { type: lunda, service_client: abc }\n"), env1c)
		if err != nil {
			t.Fatal(err)
		}
		s, err := base.WithClub(spec, drivers.ClubCRMKeys("fitness1c"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := core.BuildDriver(s, drivers.Factories); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.extra, err)
		}
	}
}

// Readiness names a failure of the mirror as the mirror's.
func TestMirrorReadyReason(t *testing.T) {
	err := &driver.MirrorError{Err: driver.Upstream(driver.UpstreamAuth, "no session")}
	if r := core.ReadyReason(err); r != "MIRROR_CRM_AUTH_FAILED" {
		t.Fatalf("reason: %s", r)
	}
}

// The lock of a pair's court is each of its rooms: a writer of either
// waits.
func TestLockResources(t *testing.T) {
	unlock, err := core.LockResources(context.Background(), []string{"viva room-1", "lunda court-1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := core.LockResources(ctx, []string{"lunda court-1"}); err == nil {
		t.Fatal("the mirror's room was taken while the pair held it")
	}
	unlock()
	again, err := core.LockResources(context.Background(), []string{"lunda court-1", "viva room-1"})
	if err != nil {
		t.Fatal(err)
	}
	again()
}
