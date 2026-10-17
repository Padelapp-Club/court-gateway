package doctor_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/doctor"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/service"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

const (
	studio      = "00000000-0000-4000-8000-000000000001"
	otherStudio = "00000000-0000-4000-8000-000000000002"
	room        = "00000000-0000-4000-8000-000000000011"
)

// doctorAgainst runs doctor for a Viva installation whose hub serves cfg
// (one club, club-a) and reports who about the running service.
func doctorAgainst(t *testing.T, clubStudio string, who tunnel.Whoami) doctor.Report {
	return doctorAgainstHub(t, clubStudio, who, 0)
}

// doctorAgainstHub is doctorAgainst with a hub whose clock is ahead of
// this computer's by ahead; it signs its time on whoami, as the hub does.
func doctorAgainstHub(t *testing.T, clubStudio string, who tunnel.Whoami, ahead time.Duration) doctor.Report {
	rep, _ := doctorWithService(t, clubStudio, who, ahead, nil)
	return rep
}

// managerSays is what the service manager answers doctor.
var managerSays = func() service.State { return service.Stopped }

// doctorWithService is doctorAgainstHub with the status file a running
// service wrote (svc; nil: no service runs); it returns the CRM too.
func doctorWithService(t *testing.T, clubStudio string, who tunnel.Whoami, ahead time.Duration, svc *core.HealthStatus) (doctor.Report, *vivastub.Stub) {
	t.Helper()
	stub := vivastub.New("review@example.test", "fixture-password", studio)
	stub.AddRoom(room, "Court")
	crm := httptest.NewServer(stub)
	t.Cleanup(crm.Close)
	spec := core.ClubSpec{ID: "club-a", Name: "Club A", Timezone: "Etc/GMT-3", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "00:00-24:00"},
		CRM: map[string]any{"studio": clubStudio}, Courts: []core.CourtSpec{{ID: "court-1", Name: "Court", Params: map[string]any{"viva_room": room}}}}
	raw, _ := json.Marshal(spec)
	who.Installation, who.Connected, who.ConfigVersion, who.Clubs = "inst-a", true, 1, []string{"club-a"}
	pub, key, _ := ed25519.GenerateKey(nil)
	reg := tunnel.RegistrationOf(tunnel.SecretSHA256("fixture-hub-secret"))
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/live":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "live"})
		case tunnel.PathWhoami:
			if nonce := r.Header.Get(tunnel.HeaderNonce); nonce != "" {
				w.Header().Set(tunnel.HeaderHubTime, tunnel.SignTime(key, "inst-a", reg, nonce, "", time.Now().Add(ahead)))
			}
			_ = json.NewEncoder(w).Encode(who)
		case tunnel.PathConfig:
			rc := tunnel.RemoteConfig{Version: 1, Clubs: []json.RawMessage{raw}}
			rc.Sign(key, "inst-a", reg, time.Now().Add(ahead))
			_ = json.NewEncoder(w).Encode(rc)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(hub.Close)
	p := filepath.Join(t.TempDir(), "gateway.yaml")
	yaml := fmt.Sprintf("hub: {url: %s, installation: inst-a, key: %s}\ncrm: {type: viva, username: review@example.test, auth_url: %s/token, api_url: %s/api/v1}\n",
		hub.URL, core.EncodeHubKey(pub), crm.URL, crm.URL)
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if svc != nil {
		if err := core.WriteHealthStatus(filepath.Join(filepath.Dir(p), "state"), *svc); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"CRM_PASSWORD": "fixture-password", "GATEWAY_HUB_SECRET": "fixture-hub-secret"}
	return doctor.Run(context.Background(), doctor.Options{ConfigPath: p, Getenv: func(k string) string { return env[k] },
		Factories: drivers.Factories, Accounts: drivers.Accounts, Version: "1.0.0", Timeout: 3 * time.Second, ServiceState: managerSays}), stub
}

func hubConfig(rep doctor.Report) doctor.Check {
	for _, c := range rep.Checks {
		if c.Code == doctor.CodeHubConfig {
			return c
		}
	}
	return doctor.Check{}
}

// doctor does not call a club config fine when the service refused it or
// does not serve it, whatever the versions say, nor when the CRM does not
// match it.
func TestDoctorReportsAConfigTheServiceDoesNotRun(t *testing.T) {
	served := func(ids ...string) tunnel.Whoami { return tunnel.Whoami{AppliedConfigVersion: 1, ClubsServed: ids} }
	refused := served("club-a") // the previous config of the club kept
	refused.ConfigError = "клуб «club-a»: комната не из студии"
	for name, tc := range map[string]struct {
		studio string
		who    tunnel.Whoami
		want   doctor.Status
	}{
		"fine":                        {studio, served("club-a"), doctor.OK},
		"refused at the same version": {studio, refused, doctor.Fail},
		"not served":                  {studio, served(), doctor.Fail},
		"studio not in the account":   {otherStudio, served("club-a"), doctor.Fail},
	} {
		t.Run(name, func(t *testing.T) {
			rep := doctorAgainst(t, tc.studio, tc.who)
			if c := hubConfig(rep); c.Status != tc.want || rep.OK != (tc.want == doctor.OK) {
				t.Fatalf("HUB_CONFIG %s (%s), ok=%v; want %s", c.Status, c.Message, rep.OK, tc.want)
			}
		})
	}
}

// doctor checks the hub's signatures against the hub's own time, which it
// learns first, as the service does: a club computer some minutes off
// the hub is no reason to call the club config unsigned.
func TestDoctorChecksTheConfigOnTheHubsClock(t *testing.T) {
	for _, ahead := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		rep := doctorAgainstHub(t, studio, tunnel.Whoami{AppliedConfigVersion: 1, ClubsServed: []string{"club-a"}}, ahead)
		if c := hubConfig(rep); c.Status != doctor.OK {
			t.Fatalf("hub %s ahead: HUB_CONFIG %s (%s)", ahead, c.Status, c.Message)
		}
	}
}

// Over plain http (loopback only) doctor does not claim a checked
// certificate.
func TestDoctorSaysLoopbackHasNoTLS(t *testing.T) {
	rep := doctorAgainst(t, studio, tunnel.Whoami{AppliedConfigVersion: 1, ClubsServed: []string{"club-a"}})
	for _, c := range rep.Checks {
		if c.Code == doctor.CodeHubReach && (c.Status != doctor.OK || c.Message != "хаб отвечает (loopback без TLS)") {
			t.Fatalf("%s: %s %s", c.Code, c.Status, c.Message)
		}
	}
}

// With the hub reachable and its club config received, an installation
// the operator has not given a club yet is told so on the occupancy line,
// not that the hub is unreachable.
func TestDoctorOccupancyWithNoClubOnTheHub(t *testing.T) {
	h := testkit.StartHub(t, testkit.OperatorYAML, testkit.HubEnv(), hub.Options{})
	h.MustOperator(t, http.StatusCreated, http.MethodPost, "/hub/installations", map[string]string{"installation_id": "test-net"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rr, err := tunnel.Register(ctx, h.Transport(), h.URL(), h.JoinCode("test-net", "Сеть"), "test")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(p, []byte("hub: {url: "+h.URL()+", installation: test-net, key: "+rr.HubKey+"}\n"+testkit.DemoCRM), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GATEWAY_HUB_SECRET": rr.Secret}
	rep := doctor.Run(context.Background(), doctor.Options{ConfigPath: p, Getenv: func(k string) string { return env[k] },
		Factories: drivers.Factories, Accounts: drivers.Accounts, Version: "1.0.0", HubTransport: h.Transport(), Timeout: 10 * time.Second, ServiceState: managerSays})
	if c := hubConfig(rep); c.Status != doctor.OK {
		t.Fatalf("HUB_CONFIG %s (%s)", c.Status, c.Message)
	}
	var occupancy doctor.Check
	for _, c := range rep.Checks {
		if c.Code == doctor.CodeOccupancy {
			occupancy = c
		}
	}
	if occupancy.Status != doctor.Skip || !strings.Contains(occupancy.Message, "клубов пока нет") {
		t.Errorf("occupancy: %s %s", occupancy.Status, occupancy.Message)
	}
}

func check(rep doctor.Report, code string) doctor.Check {
	for _, c := range rep.Checks {
		if c.Code == code {
			return c
		}
	}
	return doctor.Check{}
}

// While the service runs, doctor reports the service's own CRM checks from
// its status file and logs in to the CRM not at all: the installation holds
// one CRM session, the service's. A service that cannot log in is a failed
// CRM_LOGIN with its reason, even when doctor's own login would pass (the
// CRM may refuse one login and accept the next).
func TestDoctorReportsTheServicesCRMState(t *testing.T) {
	served := tunnel.Whoami{AppliedConfigVersion: 1, ClubsServed: []string{"club-a"}}
	checked := time.Now().Add(-30 * time.Second)
	running := func(crm string, clubs ...string) *core.HealthStatus {
		st := &core.HealthStatus{Time: time.Now(), Version: "1.0.0", Hub: "connected", Ready: crm == "ok", CRM: crm, CRMClubs: clubs, CRMCheckedAt: checked}
		if crm != "ok" {
			st.Reason = crm
		}
		return st
	}
	from := "по данным службы, проверка " + checked.Local().Format("15:04")
	retryAt := checked.Add(15 * time.Minute)
	retrying := running("CRM_AUTH_FAILED")
	retrying.CRMNextLogin = retryAt
	future, stale, legacy := running("ok"), running("ok"), running("")
	future.Time = time.Now().Add(24 * time.Hour)
	stale.CRMCheckedAt = time.Now().Add(-24 * time.Hour)
	// a status of a version without the crm fields
	legacy.Reason, legacy.CRMCheckedAt = "CRM_AUTH_FAILED", time.Time{}
	aheadCheck := running("ok")
	aheadCheck.CRMCheckedAt = time.Now().Add(time.Hour)
	// incomplete, unknown or contradicting itself
	noVersion, unknownResult, contradicting, okButFailed := running("ok"), running("garbage", "club-a"), running("ok"), running("CRM_AUTH_FAILED")
	noVersion.Version = ""
	contradicting.Ready, contradicting.Reason = false, "CRM_UNREACHABLE"
	okButFailed.Ready = true
	for name, tc := range map[string]struct {
		svc              *core.HealthStatus
		login, occupancy doctor.Status
		message, hint    string
	}{
		"ready":                       {running("ok"), doctor.OK, doctor.OK, from, ""},
		"password rejected":           {running("CRM_AUTH_FAILED"), doctor.Fail, doctor.Skip, from + ": вход в CRM не удался: CRM_AUTH_FAILED; сама служба больше не пробует войти", "set-password"},
		"rejected with a club":        {running("CRM_AUTH_FAILED", "club-a"), doctor.Fail, doctor.Skip, "CRM_AUTH_FAILED", "set-password"},
		"rejected, retried later":     {retrying, doctor.Fail, doctor.Skip, "следующая попытка в " + retryAt.Local().Format("15:04"), "set-password"},
		"CRM unreachable":             {running("CRM_UNREACHABLE"), doctor.Fail, doctor.Skip, "CRM_UNREACHABLE", "интернет"},
		"a club's answer is rejected": {running("CRM_REJECTED", "club-a"), doctor.OK, doctor.Fail, from, ""},
		"legacy status, CRM failed":   {legacy, doctor.Fail, doctor.Skip, "CRM_AUTH_FAILED", "set-password"},
		"no check yet":                {running(""), doctor.Warn, doctor.Skip, "нет свежих данных службы: служба ещё не сообщила", "через минуту"},
		"status from the future":      {future, doctor.Warn, doctor.Skip, "нет свежих данных службы: время данных службы впереди", "через минуту"},
		"old CRM check, fresh file":   {stale, doctor.Warn, doctor.Skip, "нет свежих данных службы: последняя проверка CRM", "через минуту"},
		"CRM check from the future":   {aheadCheck, doctor.Warn, doctor.Skip, "нет свежих данных службы: время проверки CRM", "через минуту"},
		"status without its version":  {noVersion, doctor.Warn, doctor.Skip, "данные службы неполные", "через минуту"},
		"unknown CRM result":          {unknownResult, doctor.Warn, doctor.Skip, "неизвестный результат", "через минуту"},
		"a driver's config alert":     {running("SERVICE_CLIENT_MISSING", "club-a"), doctor.OK, doctor.Warn, from, ""},
		"ok CRM, CRM reason":          {contradicting, doctor.Warn, doctor.Skip, "расходится", "через минуту"},
		"failed CRM, ready":           {okButFailed, doctor.Warn, doctor.Skip, "расходится", "через минуту"},
	} {
		t.Run(name, func(t *testing.T) {
			rep, stub := doctorWithService(t, studio, served, 0, tc.svc)
			login := check(rep, doctor.CodeCRMLogin)
			if login.Status != tc.login || !strings.Contains(login.Message, tc.message) || !strings.Contains(login.Hint, tc.hint) {
				t.Fatalf("CRM_LOGIN %s %q (%s)", login.Status, login.Message, login.Hint)
			}
			if c := check(rep, doctor.CodeOccupancy); c.Status != tc.occupancy {
				t.Fatalf("CRM_OCCUPANCY %s %q", c.Status, c.Message)
			}
			if rep.OK != (tc.login == doctor.OK && tc.occupancy != doctor.Fail) {
				t.Fatalf("ok=%v", rep.OK)
			}
			if n := stub.LoginAttempts(); n != 0 || len(stub.Calls()) != 0 {
				t.Fatalf("doctor called the CRM beside the service: %d logins, calls %v", n, stub.Calls())
			}
		})
	}
}

// Without a running service (no status file, or one too old to be a
// running service's) doctor logs in itself, once for all its checks, and
// ends that session.
func TestDoctorWithoutTheServiceEndsItsSession(t *testing.T) {
	served := tunnel.Whoami{AppliedConfigVersion: 1, ClubsServed: []string{"club-a"}}
	stopped := &core.HealthStatus{Time: time.Now().Add(-time.Hour), CRM: "CRM_AUTH_FAILED", CRMCheckedAt: time.Now().Add(-time.Hour)}
	for name, svc := range map[string]*core.HealthStatus{"no status file": nil, "stopped service": stopped} {
		t.Run(name, func(t *testing.T) {
			rep, stub := doctorWithService(t, studio, served, 0, svc)
			login := check(rep, doctor.CodeCRMLogin)
			if !rep.OK || login.Status != doctor.OK || !strings.HasPrefix(login.Message, "проверка doctor") || check(rep, doctor.CodeOccupancy).Status != doctor.OK {
				t.Fatalf("CRM_LOGIN %s %q; ok=%v", login.Status, login.Message, rep.OK)
			}
			if stub.Logins() != 1 || stub.OpenSessions() != 0 {
				t.Fatalf("%d logins, %d sessions left open", stub.Logins(), stub.OpenSessions())
			}
		})
	}
}

// No status file of a running service: doctor logs in itself only when
// the service manager says the service is stopped or not installed. When
// it runs, or the manager cannot tell, doctor does not log in and the
// report is not a success.
func TestDoctorAsksTheServiceManager(t *testing.T) {
	served := tunnel.Whoami{AppliedConfigVersion: 1, ClubsServed: []string{"club-a"}}
	defer func() { managerSays = func() service.State { return service.Stopped } }()
	for name, tc := range map[string]struct {
		says   service.State
		logins int
		status doctor.Status
	}{
		"running":         {service.Running, 0, doctor.Warn},
		"stopped":         {service.Stopped, 1, doctor.OK},
		"cannot be asked": {service.Unknown, 0, doctor.Warn},
	} {
		t.Run(name, func(t *testing.T) {
			managerSays = func() service.State { return tc.says }
			rep, stub := doctorWithService(t, studio, served, 0, nil)
			login := check(rep, doctor.CodeCRMLogin)
			if login.Status != tc.status || stub.LoginAttempts() != tc.logins || rep.OK != (tc.status == doctor.OK) || stub.OpenSessions() != 0 {
				t.Fatalf("CRM_LOGIN %s %q, ok=%v, %d logins, %d sessions open", login.Status, login.Message, rep.OK, stub.LoginAttempts(), stub.OpenSessions())
			}
		})
	}
}
