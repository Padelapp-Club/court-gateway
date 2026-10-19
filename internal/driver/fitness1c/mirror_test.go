package fitness1c_test

import (
	"context"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
	"github.com/Padelapp-Club/court-gateway/internal/driver/fitness1c"
	"testing"
	"time"
)

func TestMirrorTechnicalClientIsolation(t *testing.T) {
	const phone = "+79990000002"
	f := newFixture(t, fixtureOpts{extraCRM: ", mirror_client_phone: '" + phone + "'", clubCRM: map[string]any{"mirror_service": padelSvc}})
	f.stub.AddClient(phone)
	m := driver.Mirrorer(f.drv)
	start := time.Now().Add(48 * time.Hour).In(moscow).Truncate(time.Hour)
	req := driver.BlockRequest{Interval: driver.Interval{Court: "court-1", Start: start, End: start.Add(time.Hour)}, Source: "lunda", SourceID: "l-source"}
	if e := m.CreateBlock(context.Background(), req); e != nil {
		t.Fatal(e)
	}
	q := driver.OccupancyQuery{Courts: []string{"court-1"}, From: start, To: start.Add(time.Hour)}
	inv, e := m.Inventory(context.Background(), q)
	if e != nil || !inv.Complete || len(inv.Items) != 1 {
		t.Fatal(inv, e)
	}
	block := inv.Items[0]
	if block.Class != driver.MirrorBooking || block.SourceID != "" {
		t.Fatal("not markerless mirror", block)
	}
	if e := f.drv.Cancel(context.Background(), block.ID, partner); e == nil {
		t.Fatal("PadelApp cancelled a mirror")
	}
	if e := m.DeleteBlock(context.Background(), block); e != nil {
		t.Fatal(e)
	}
	b, e := f.drv.Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: start, End: start.Add(time.Hour), Kind: driver.KindGame, Partner: partner})
	if e != nil {
		t.Fatal(e)
	}
	inv, e = m.Inventory(context.Background(), q)
	if e != nil || len(inv.Items) != 1 || inv.Items[0].Class != driver.PartnerBooking {
		t.Fatal(inv, e)
	}
	forged := block
	forged.ID = b.ID
	if e := m.DeleteBlock(context.Background(), forged); e == nil {
		t.Fatal("mirror deletion cancelled PadelApp client")
	}
}

func TestMirrorClientMustDiffer(t *testing.T) {
	cfg := driver.Config{CRM: driver.Params{"base_url": "http://127.0.0.1:1", "username": stubUser, "studio": club1C, "client_phone": clientPhone, "partner": partner, "mirror_client_phone": "79990000001", "mirror_service": padelSvc}, Password: stubPass, Secrets: map[string]string{fitness1c.APIKeyEnv: stubKey}, Courts: []driver.CourtConfig{{ID: "court-1", Params: driver.Params{fitness1c.RoomKey: room1}}}}
	if _, e := fitness1c.New(cfg); e == nil {
		t.Fatal("same technical client accepted")
	}
}

func TestMirrorClientRecognitionSurvivesServiceEdit(t *testing.T) {
	const phone = "+79990000002"
	f := newFixture(t, fixtureOpts{extraCRM: ", mirror_client_phone: '" + phone + "'", clubCRM: map[string]any{"mirror_service": padelSvc}})
	f.stub.AddClient(phone)
	start := time.Now().Add(48 * time.Hour).In(moscow).Truncate(time.Hour)
	f.stub.SeedClientRental(phone, room1, gridSvc, start, start.Add(time.Hour))
	inv, e := f.drv.Inventory(context.Background(), driver.OccupancyQuery{Courts: []string{"court-1"}, From: start, To: start.Add(time.Hour)})
	if e != nil || !inv.Complete || len(inv.Items) != 1 || inv.Items[0].Class != driver.MirrorBooking {
		t.Fatal("mirror client was treated as a club booking after a service edit", inv, e)
	}
}
