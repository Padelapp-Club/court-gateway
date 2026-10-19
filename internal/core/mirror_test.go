package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

type twoResources struct {
	driver.Driver
	keys []string
}

func (d twoResources) Resources(string) []string { return append([]string(nil), d.keys...) }
func TestPairLocksBothResources(t *testing.T) {
	d := twoResources{keys: []string{"primary-test", "mirror-test"}}
	unlock, e := lockResources(context.Background(), d, "court")
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	for _, key := range d.keys {
		ctx, stop := context.WithTimeout(context.Background(), time.Millisecond)
		f, e := createLocks.lock(ctx, key)
		stop()
		if e == nil {
			f()
			t.Fatal("resource not locked", key)
		}
	}
}
func TestMirrorConfig(t *testing.T) {
	base := strings.Replace(hubInstallation, "type: demo", "type: viva", 1)
	for _, suffix := range []string{"mirror: {type: viva}\n", "mirror: {type: lunda, max_removals: -1}\n", "mirror: {type: lunda, max_creates: 0}\n"} {
		if _, e := Load([]byte(base+suffix), env(goodEnv)); e == nil {
			t.Fatal("invalid mirror accepted", suffix)
		}
	}
	s, e := Load([]byte(base+"mirror: {type: lunda, service_client: club-service}\n"), env(goodEnv))
	if e != nil {
		t.Fatal(e)
	}
	if s.Mirror.MaxRemovals != 3 || s.Mirror.MaxCreates != 50 {
		t.Fatal("defaults")
	}
	c := s.MirrorConfig()
	if c.Password != "" || len(c.Secrets) != 0 || c.CRM["username"] != nil {
		t.Fatal("primary credentials sent to mirror")
	}
	if _, e := Load([]byte(hubInstallation+"mirror: {type: lunda}\n"), env(goodEnv)); e == nil {
		t.Fatal("unsupported primary accepted")
	}
}

type reconcileDriver struct {
	slowDriver
	calls int
}

func (d *reconcileDriver) Reconcile(context.Context) error { d.calls++; return nil }
func TestReconcileCurrentClubsOnly(t *testing.T) {
	a, b := &reconcileDriver{}, &reconcileDriver{}
	c := NewClubs(ClubsOptions{})
	server := func(d driver.Driver) *Server {
		return NewServer(&Settings{ClubID: "club", Driver: driver.Config{}}, d, nil)
	}
	c.Set("", []*Server{server(a)})
	c.Reconcile(context.Background())
	c.Set("", []*Server{server(b)})
	c.Reconcile(context.Background())
	if a.calls != 1 || b.calls != 1 {
		t.Fatal("stale club reconciled")
	}
}
