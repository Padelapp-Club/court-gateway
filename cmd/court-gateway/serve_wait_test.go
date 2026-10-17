package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/drivers"
)

// slowCheck is a club's driver whose config check against the CRM is held
// until release, whatever its context says: a CRM call of the config
// syncer still running (a session being renewed into the state directory)
// when the service stops.
type slowCheck struct {
	driver.Driver
	entered, release chan struct{}
	once             *sync.Once
}

func (d slowCheck) CheckConfig(context.Context) error {
	d.once.Do(func() { close(d.entered) })
	<-d.release
	return nil
}

// serve returns only after its config syncer has: what the syncer still
// writes must not land in the state directory of a stopped service (as
// the status loop, ed1b5e2).
func TestServeWaitsForTheConfigSyncer(t *testing.T) {
	s := newStack(t)
	var out bytes.Buffer
	if rc := run(s.setupArgs(), &out, io.Discard, env{"CRM_PASSWORD": crmPass, "GATEWAY_JOIN_CODE": s.hub.JoinCode("club-one", "Клуб Один")}.get); rc != 0 {
		t.Fatalf("setup: %s", out.String())
	}
	entered, release := make(chan struct{}), make(chan struct{})
	viva := drivers.Factories["viva"]
	drivers.Factories["viva"] = func(cfg driver.Config) (driver.Driver, error) {
		d, err := viva(cfg)
		if err != nil {
			return nil, err
		}
		return slowCheck{Driver: d, entered: entered, release: release, once: &sync.Once{}}, nil
	}
	defer func() { drivers.Factories["viva"] = viva }()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, filepath.Join(s.dir, "gateway.yaml"), "", &contract.LogBuffer{}, env{}.get) }()
	defer func() { cancel(); <-done }()
	s.hub.MustOperator(t, http.StatusCreated, http.MethodPut, "/hub/installations/club-one/clubs/club-one", core.ClubSpec{
		ID: "club-one", Name: "Клуб Один", Timezone: "Europe/Moscow", SlotMinutes: 30, Hours: map[string]string{"mon-sun": "07:00-23:00"},
		CRM: map[string]any{"studio": studio}, Courts: []core.CourtSpec{{ID: "court-1", Name: "Корт 10", Params: map[string]any{"viva_room": roomA}}}})
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the config was never checked")
	}
	cancel()
	select {
	case rc := <-done:
		done <- rc // for the deferred wait
		t.Fatal("serve returned while its config syncer was still running")
	case <-time.After(time.Second):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case rc := <-done:
		done <- rc // for the deferred wait
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return once the syncer was done")
	}
}
