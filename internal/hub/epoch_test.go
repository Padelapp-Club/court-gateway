package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/demo"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva"
	"github.com/Padelapp-Club/court-gateway/internal/driver/viva/vivastub"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
	"github.com/Padelapp-Club/court-gateway/internal/remote"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// crossing delays and reorders the gateway's real exchanges with the hub,
// and answers a config pull with bytes the hub sent earlier: it never
// makes or signs a message. Once armed, the first whoami is held before it
// leaves, the second one's answer before it is delivered.
type crossing struct {
	base http.RoundTripper

	mu      sync.Mutex
	armed   bool
	whoamis int
	replay  []byte

	firstHeld, secondHeld chan struct{}
	sendFirst, sendSecond chan struct{}
}

func newCrossing(base http.RoundTripper) *crossing {
	return &crossing{base: base, firstHeld: make(chan struct{}), secondHeld: make(chan struct{}),
		sendFirst: make(chan struct{}), sendSecond: make(chan struct{})}
}

func (x *crossing) RoundTrip(r *http.Request) (*http.Response, error) {
	x.mu.Lock()
	replay, n := x.replay, 0
	if x.armed && r.URL.Path == tunnel.PathWhoami {
		x.whoamis++
		n = x.whoamis
	}
	x.mu.Unlock()
	if replay != nil && r.URL.Path == tunnel.PathConfig {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(replay)), Request: r}, nil
	}
	if n == 1 {
		close(x.firstHeld)
		if err := wait(r.Context(), x.sendFirst); err != nil {
			return nil, err
		}
	}
	resp, err := x.base.RoundTrip(r)
	if err != nil || n != 2 {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	close(x.secondHeld)
	if err := wait(r.Context(), x.sendSecond); err != nil {
		return nil, err
	}
	return resp, nil
}

func wait(ctx context.Context, ch chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// epochGateway is a tunnel client of club's installation at h, with its
// syncer, driven by the test instead of a poll loop.
func epochGateway(t *testing.T, h *testkit.Hub, club, secret string, rt http.RoundTripper) (*tunnel.Client, *remote.Syncer, *core.Clubs) {
	t.Helper()
	env := map[string]string{core.DefaultHubSecretEnv: secret}
	base, err := core.Load([]byte("hub: {url: "+h.URL()+", installation: "+club+", key: "+core.EncodeHubKey(h.Server().Store().HubKey())+"}\n"+testkit.DemoCRM),
		func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	clubs := core.NewClubs(core.ClubsOptions{})
	syncer := remote.New(remote.Options{Base: base, Clubs: clubs, Factories: map[string]driver.Factory{"demo": demo.New}})
	cfg := h.ClientConfig(club, secret, clubs.TunnelHandler(), nil)
	cfg.Transport, cfg.OnConfigVersion = rt, syncer.Notify
	c, err := tunnel.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, syncer, clubs
}

func clubName(t *testing.T, clubs *core.Clubs, id string) string {
	t.Helper()
	srv, ok := clubs.Server(id)
	if !ok {
		t.Fatalf("club %s not served", id)
	}
	return srv.Settings().ClubName
}

// A hub run, once replaced, is never taken again, whatever the order the
// gateway sent its requests in. Here the gateway's earlier request reaches
// the restarted hub and announces its run; a later request's answer from
// the hub before the restart arrives after that. The config that hub
// signed, replayed as it sent it and still within its validity, is
// refused, and the restarted hub's stays applied.
func TestReplacedHubRunIsNotTakenAgain(t *testing.T) {
	h := testkit.StartHub(t, "poll_wait: 1s\n"+testkit.OperatorYAML, testkit.HubEnv(), hub.Options{})
	secret := h.Register("club-a", "Before restart")
	x := newCrossing(h.Transport())
	c, syncer, clubs := epochGateway(t, h, "club-a", secret, x)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pull := func() (tunnel.RemoteConfig, error) {
		rc, err := c.FetchConfig(ctx)
		if err == nil {
			_ = syncer.Apply(ctx, rc)
		}
		return rc, err
	}
	if _, err := c.Whoami(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := pull()
	if err != nil {
		t.Fatal(err)
	}

	x.mu.Lock()
	x.armed = true
	x.mu.Unlock()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := c.Whoami(ctx); first <- err }()
	<-x.firstHeld // sent, not yet on the wire
	go func() { _, err := c.Whoami(ctx); second <- err }()
	<-x.secondHeld // answered by the hub before the restart
	_, old := h.Do(t, 5*time.Second, http.MethodGet, tunnel.PathConfig, secret, nil)

	h.Restart()
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/club-a/clubs/club-a", testkit.Club("club-a", "After restart", "Court 1"))
	close(x.sendFirst)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	after, err := pull()
	if err != nil || after.Epoch == before.Epoch || clubName(t, clubs, "club-a") != "After restart" {
		t.Fatalf("the restarted hub's config: %+v %v", after, err)
	}

	close(x.sendSecond)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if b := syncer.Behind(); b != 0 {
		t.Errorf("behind %s after the replaced run's answer", b)
	}
	x.mu.Lock()
	x.replay = old
	x.mu.Unlock()
	if rc, err := pull(); !errors.Is(err, tunnel.ErrStaleConfig) {
		t.Errorf("the replaced run's config (version %d) pulled again: %v", rc.Version, err)
	}
	if name := clubName(t, clubs, "club-a"); name != "After restart" {
		t.Errorf("served %q after the replaced run's answer", name)
	}
	x.mu.Lock()
	x.replay = nil
	x.mu.Unlock()
	if _, err := c.Whoami(ctx); err != nil {
		t.Fatal(err)
	}
	if rc, err := pull(); err != nil || rc.Epoch != after.Epoch || rc.Version != after.Version {
		t.Errorf("the restarted hub's config, pulled again: %+v %v", rc, err)
	}
}

// A hub restored from a backup made before a change is a new run with a
// lower config version: the gateway follows it back to that version.
func TestRestoredHubIsFollowedToItsLowerVersion(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	club := demoClub("club-r", "demo-north", "Корт 1")
	in := demoInstallation(t, h, "net-r", club)
	file := filepath.Join(h.StateDir, "installations", "net-r.json")
	backup, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	changed := club
	changed.Name = "Изменённый клуб"
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/net-r/clubs/club-r", changed)
	testkit.WaitFor(t, "the change applied", 10*time.Second, func() bool { return in.Syncer.State().ConfigVersion == 2 })

	if err := os.WriteFile(file, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	h.Restart()
	testkit.WaitFor(t, "the restored config applied", 15*time.Second, func() bool {
		srv, ok := in.Clubs.Server("club-r")
		return in.Syncer.State().ConfigVersion == 1 && ok && srv.Settings().ClubName == club.Name && in.Syncer.Behind() == 0
	})
}

// A late answer that announces an older version of the hub's run does
// not mark the applied, current config as behind: the syncer is told the
// version the gateway took, not the one the late answer carries.
func TestLateAnnouncementDoesNotMarkTheConfigBehind(t *testing.T) {
	h := testkit.StartHub(t, "poll_wait: 1s\n"+testkit.OperatorYAML, testkit.HubEnv(), hub.Options{})
	secret := h.Register("club-a", "Before change")
	x := newCrossing(h.Transport())
	c, syncer, _ := epochGateway(t, h, "club-a", secret, x)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	x.mu.Lock()
	x.armed = true
	x.mu.Unlock()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := c.Whoami(ctx); first <- err }()
	<-x.firstHeld
	go func() { _, err := c.Whoami(ctx); second <- err }()
	<-x.secondHeld // announces the version before the change
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/club-a/clubs/club-a", testkit.Club("club-a", "After change", "Court 1"))
	close(x.sendFirst)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	rc, err := c.FetchConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.Apply(ctx, rc); err != nil || syncer.Behind() != 0 {
		t.Fatalf("version %d applied: %v, behind %s", rc.Version, err, syncer.Behind())
	}
	close(x.sendSecond)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if b := syncer.Behind(); b != 0 {
		t.Errorf("behind %s after a late announcement of an older version", b)
	}
}

const (
	vivaStudio = "00000000-0000-4000-8000-000000000001"
	roomA      = "00000000-0000-4000-8000-000000000011"
	roomB      = "00000000-0000-4000-8000-000000000012"
)

// vivaClub books its one court, court-1, in a room of a Viva studio.
func vivaClub(room string) core.ClubSpec {
	return core.ClubSpec{ID: "club-a", Name: "Club A", Timezone: "Europe/Moscow", SlotMinutes: 30,
		Hours: map[string]string{"mon-sun": "00:00-24:00"}, CRM: map[string]any{"studio": vivaStudio},
		Courts: []core.CourtSpec{{ID: "court-1", Name: "Court 1", Params: map[string]any{"viva_room": room}}}}
}

// vivaInstallation is installation test-net on h, serving vivaClub(room)
// on a Viva stub with rooms A and B.
func vivaInstallation(t *testing.T, h *testkit.Hub, room string) (*testkit.Installation, *vivastub.Stub) {
	t.Helper()
	stub := vivastub.New("review@example.test", "fixture-password", vivaStudio)
	stub.AddRoom(roomA, "Room A")
	stub.AddRoom(roomB, "Room B")
	crm := httptest.NewServer(stub)
	t.Cleanup(crm.Close)
	return testkit.StartInstallation(t, h, "test-net", testkit.InstallationOptions{
		CRMYAML: fmt.Sprintf("crm: {type: viva, username: review@example.test, auth_url: %s/token, api_url: %s/api/v1}\n", crm.URL, crm.URL),
		Env:     map[string]string{"CRM_PASSWORD": "fixture-password"}, CRMType: "viva", Factory: viva.New, Account: viva.NewAccount,
		Clubs: []core.ClubSpec{vivaClub(room)},
	}), stub
}

// book books court-1 through h, hours after a day from now: the status and
// the room of the booking in the CRM ("" when none was made).
func book(t *testing.T, h *testkit.Hub, stub *vivastub.Stub, hours int) (int, string) {
	t.Helper()
	start := time.Now().UTC().Truncate(time.Hour).Add(time.Duration(24+hours) * time.Hour)
	body := fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
	resp, data := h.Do(t, 10*time.Second, http.MethodPost, "/v1/clubs/club-a/bookings", contract.BookKey, strings.NewReader(body))
	var b struct{ ID string }
	_ = json.Unmarshal(data, &b)
	room, _ := stub.Break(strings.TrimPrefix(b.ID, "b-"))["room"].(map[string]any)
	id, _ := room["id"].(string)
	return resp.StatusCode, id
}

// A hub restarted after its clock was set right, back or forward by
// minutes, is followed at once: the restarted run's config, which moves
// court-1 to room B, is applied, and court-1 is booked in room B.
func TestRestartAfterTheHubsClockIsSetRight(t *testing.T) {
	for name, clock := range map[string][2]time.Duration{"back": {2 * time.Minute, 0}, "forward": {0, 2 * time.Minute}} {
		t.Run(name, func(t *testing.T) {
			var offset atomic.Int64
			offset.Store(int64(clock[0]))
			h := startOperatorHub(t, hub.Options{Now: func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }})
			in, stub := vivaInstallation(t, h, roomA)
			h.Stop()
			offset.Store(int64(clock[1]))
			h.Restart()
			h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/test-net/clubs/club-a", vivaClub(roomB))
			testkit.WaitFor(t, "the restarted hub's config", 5*time.Second, func() bool {
				return in.Syncer.State().ConfigVersion == 2 && in.Syncer.Behind() == 0
			})
			if status, room := book(t, h, stub, 0); status != http.StatusCreated || room != roomB {
				t.Fatalf("court-1 booked: %d, in room %q", status, room)
			}
		})
	}
}

// A hub whose clock was once far ahead, set right, then restarted, is
// followed without restarting the gateway.
func TestRestartAfterTheHubsClockWasOnceFarAhead(t *testing.T) {
	var offset atomic.Int64
	h := startOperatorHub(t, hub.Options{Now: func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }})
	in := demoInstallation(t, h, "net-c", demoClub("club-c", "demo-north", "Корт 1"))
	offset.Store(int64(365 * 24 * time.Hour))
	if _, err := in.Gateway.Client.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	offset.Store(0)
	h.Restart()
	changed := demoClub("club-c", "demo-north", "Корт 1")
	changed.Name = "После перезапуска"
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/net-c/clubs/club-c", changed)
	testkit.WaitFor(t, "the restarted hub's config", 5*time.Second, func() bool {
		srv, ok := in.Clubs.Server("club-c")
		return ok && srv.Settings().ClubName == changed.Name && in.Syncer.Behind() == 0
	})
}

// A hub restarted twice in a row is followed to its last run.
func TestTwoHubRestartsInARow(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	in := demoInstallation(t, h, "net-t", demoClub("club-t", "demo-north", "Корт 1"))
	h.Restart()
	h.Restart()
	changed := demoClub("club-t", "demo-north", "Корт 1")
	changed.Name = "После двух перезапусков"
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/net-t/clubs/club-t", changed)
	testkit.WaitFor(t, "the last run's config", 10*time.Second, func() bool {
		srv, ok := in.Clubs.Server("club-t")
		return ok && srv.Settings().ClubName == changed.Name && in.Syncer.Behind() == 0
	})
	if r := call(t, h, http.MethodGet, "/v1/clubs/club-t/courts", contract.ReadKey, ""); r.status != http.StatusOK {
		t.Fatalf("courts after two restarts: %d %s", r.status, r.body)
	}
}

// holdPolls delays the answers to the gateway's polls while armed, until
// released; it never makes or changes a message.
type holdPolls struct {
	base          http.RoundTripper
	armed         atomic.Bool
	once          sync.Once
	held, release chan struct{}
}

func (x *holdPolls) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := x.base.RoundTrip(r)
	if err != nil || r.URL.Path != tunnel.PathPoll || !x.armed.Load() {
		return resp, err
	}
	x.once.Do(func() { close(x.held) })
	if err := wait(r.Context(), x.release); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// A hub restarted during the gateway's first polls: their answers, the
// first run's, arrive after the restart, and the gateway follows the
// restarted run, its config served and not behind.
func TestRestartDuringTheFirstPolls(t *testing.T) {
	h := testkit.StartHub(t, "poll_wait: 1s\n"+testkit.OperatorYAML, testkit.HubEnv(), hub.Options{})
	secret := h.Register("club-a", "Before restart")
	x := &holdPolls{base: h.Transport(), held: make(chan struct{}), release: make(chan struct{})}
	x.armed.Store(true)
	c, syncer, clubs := epochGateway(t, h, "club-a", secret, x)
	syncer.SetFetch(c.FetchConfig)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	wg.Add(2)
	go func() { defer wg.Done(); c.Run(ctx) }()
	go func() { defer wg.Done(); syncer.Run(ctx) }()
	<-x.held // answered by the first run, not delivered
	h.Restart()
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/club-a/clubs/club-a", testkit.Club("club-a", "After restart", "Court 1"))
	x.armed.Store(false)
	close(x.release)
	testkit.WaitFor(t, "the restarted hub's config", 10*time.Second, func() bool {
		srv, ok := clubs.Server("club-a")
		return ok && srv.Settings().ClubName == "After restart" && syncer.Behind() == 0
	})
}

// holdWhoami delays the answer to the next whoami while armed, until
// released; it never makes or changes a message.
type holdWhoami struct {
	base          http.RoundTripper
	armed         atomic.Bool
	held, release chan struct{}
}

func (x *holdWhoami) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := x.base.RoundTrip(r)
	if err != nil || r.URL.Path != tunnel.PathWhoami || !x.armed.CompareAndSwap(true, false) {
		return resp, err
	}
	close(x.held)
	if err := wait(r.Context(), x.release); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// An answer of the first hub run, to a request sent before the restarted
// run was seen, arrives after it was seen and before it is confirmed: it
// does not confirm the ended run. The next exchange confirms the restarted
// run and the gateway serves it.
func TestLateAnswerOfAnEndedRunDoesNotConfirmIt(t *testing.T) {
	h := testkit.StartHub(t, hubYAML, testkit.HubEnv(), hub.Options{})
	secret := h.Register("club-a", "Club A")
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/club-a/clubs/club-a", testkit.Club("club-a", "Club A", "Court 1"))
	x := &holdWhoami{base: h.Transport(), held: make(chan struct{}), release: make(chan struct{})}
	c, syncer, _ := epochGateway(t, h, "club-a", secret, x)
	syncer.SetFetch(c.FetchConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	who := func() {
		t.Helper()
		if _, err := c.Whoami(ctx); err != nil {
			t.Fatal(err)
		}
	}
	who()
	rc, err := c.FetchConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = syncer.Apply(ctx, rc)
	x.armed.Store(true)
	late := make(chan error, 1)
	go func() { _, err := c.Whoami(ctx); late <- err }()
	<-x.held // answered by the first run, not delivered yet
	h.Restart()
	who() // the restarted run seen
	close(x.release)
	if err := <-late; err != nil {
		t.Fatal(err)
	}
	who() // an exchange sent after the restarted run was seen
	var wg sync.WaitGroup
	defer wg.Wait()
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	wg.Add(2)
	go func() { defer wg.Done(); c.Run(rctx) }()
	go func() { defer wg.Done(); syncer.Run(rctx) }()
	var last partnerResp
	for i := 0; i < 40; i++ {
		if last = call(t, h, http.MethodGet, "/v1/clubs/club-a/courts", contract.ReadKey, ""); last.status == http.StatusOK {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Errorf("the restarted run is not served: %d %s, refused %d", last.status, last.body, c.Refused())
}

// vivaGateway is a gateway of installation test-net on h, reaching it
// through wrap(the hub's transport), serving vivaClub(roomA) on a Viva
// stub. No syncer runs: pull takes the config the hub announces now, as
// a syncer does.
func vivaGateway(t *testing.T, h *testkit.Hub, wrap func(http.RoundTripper) http.RoundTripper) (*vivastub.Stub, *tunnel.Verifier, *testkit.Gateway, func() tunnel.RemoteConfig) {
	t.Helper()
	stub := vivastub.New("review@example.test", "fixture-password", vivaStudio)
	stub.AddRoom(roomA, "Room A")
	stub.AddRoom(roomB, "Room B")
	crm := httptest.NewServer(stub)
	t.Cleanup(crm.Close)
	h.MustOperator(t, http.StatusCreated, http.MethodPost, "/hub/installations", map[string]string{"installation_id": "test-net"})
	h.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/test-net/clubs/club-a", vivaClub(roomA))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	rr, err := tunnel.Register(ctx, h.Transport(), h.URL(), h.JoinCode("test-net", "Сеть"), "test")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CRM_PASSWORD": "fixture-password", core.DefaultHubSecretEnv: rr.Secret}
	yaml := fmt.Sprintf("hub: {url: %s, installation: test-net, key: %s}\ncrm: {type: viva, username: review@example.test, auth_url: %s/token, api_url: %s/api/v1}\n", h.URL(), rr.HubKey, crm.URL, crm.URL)
	base, err := core.Load([]byte(yaml), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	v, err := tunnel.NewVerifier(h.Server().Store().HubKey(), "test-net", rr.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	base.Driver.Clock = v.Now
	if base.Driver.Account, err = viva.NewAccount(base.Driver); err != nil {
		t.Fatal(err)
	}
	clubs := core.NewClubs(core.ClubsOptions{})
	syncer := remote.New(remote.Options{Base: base, Clubs: clubs, Factories: map[string]driver.Factory{"viva": viva.New}})
	cfg := h.ClientConfig("test-net", rr.Secret, clubs.TunnelHandler(), nil)
	cfg.Verifier, cfg.OnConfigVersion, cfg.PollTimeout = v, syncer.Notify, 30*time.Second
	if wrap != nil {
		cfg.Transport = wrap(cfg.Transport)
	}
	g := testkit.StartGateway(t, cfg)
	t.Cleanup(g.Stop)
	return stub, v, g, func() tunnel.RemoteConfig {
		t.Helper()
		if _, err := g.Client.Whoami(ctx); err != nil {
			t.Fatal(err)
		}
		rc, err := g.Client.FetchConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := syncer.Apply(ctx, rc); err != nil {
			t.Fatal(err)
		}
		return rc
	}
}

// holdBody delays the body of the first poll answer that carries a
// request until released; it never makes or changes a message.
type holdBody struct {
	base          http.RoundTripper
	once          sync.Once
	held, release chan struct{}
}

func (x *holdBody) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := x.base.RoundTrip(r)
	if err != nil || r.URL.Path != tunnel.PathPoll || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) > 0 {
		x.once.Do(func() {
			close(x.held)
			_ = wait(r.Context(), x.release)
		})
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

// A booking the first hub run signed, its poll answer delivered only after
// the restarted run's config (court-1 moved to room B) is applied, is not
// run: it is refused, and nothing reaches the CRM.
func TestRequestOfAReplacedHubRunIsNotRun(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	x := &holdBody{held: make(chan struct{}), release: make(chan struct{})}
	stub, _, g, pull := vivaGateway(t, h, func(rt http.RoundTripper) http.RoundTripper { x.base = rt; return x })
	before := pull()
	testkit.WaitFor(t, "connected", 5*time.Second, g.Client.Connected)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		start := time.Now().UTC().Truncate(time.Hour).Add(48 * time.Hour)
		body := fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URL()+"/v1/clubs/club-a/bookings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+contract.BookKey)
		if resp, err := (&http.Client{Transport: h.Transport()}).Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-x.held // signed by the first run, not yet delivered
	cancel()
	<-done
	h.Restart()
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/test-net/clubs/club-a", vivaClub(roomB))
	if after := pull(); after.Epoch == before.Epoch {
		t.Fatal("the hub's run did not change")
	}
	close(x.release)
	written := func() bool {
		return slices.ContainsFunc(stub.Calls(), func(call string) bool { return !strings.HasPrefix(call, "GET ") })
	}
	testkit.WaitFor(t, "the first run's booking answered", 5*time.Second, func() bool { return g.Client.Refused() > 0 || written() })
	if written() || g.Client.Refused() == 0 {
		t.Fatalf("the first run's booking run: CRM calls %q, refused %d", stub.Calls(), g.Client.Refused())
	}
}

// A booking of the restarted hub run, which moved court-1 to room B, is
// not served by the clubs of the first run's config: it gets 503
// CONFIG_PENDING until its run's config is applied, then court-1 is booked
// in room B.
func TestRequestWaitsForItsHubRunsConfig(t *testing.T) {
	h := startOperatorHub(t, hub.Options{})
	stub, v, _, pull := vivaGateway(t, h, nil)
	before := pull()
	h.Restart()
	h.MustOperator(t, http.StatusOK, http.MethodPut, "/hub/installations/test-net/clubs/club-a", vivaClub(roomB))
	testkit.WaitFor(t, "the restarted run confirmed", 5*time.Second, func() bool {
		_, run := v.Announced()
		return run != before.Epoch && v.Confirmed()
	})
	start := time.Now().UTC().Truncate(time.Hour).Add(48 * time.Hour)
	body := fmt.Sprintf(`{"court":"court-1","start":%q,"end":%q,"kind":"game"}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
	resp, data := h.Do(t, 10*time.Second, http.MethodPost, "/v1/clubs/club-a/bookings", contract.BookKey, strings.NewReader(body))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(data), "CONFIG_PENDING") {
		t.Errorf("before the run's config is applied: %d %s", resp.StatusCode, data)
	}
	for _, call := range stub.Calls() {
		if !strings.HasPrefix(call, "GET ") {
			t.Errorf("CRM write: %s", call)
		}
	}
	pull()
	if status, room := book(t, h, stub, 0); status != http.StatusCreated || room != roomB {
		t.Fatalf("court-1 booked: %d, in room %q", status, room)
	}
}
