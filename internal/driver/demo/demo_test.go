package demo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// A booking that has started is not cancelled.
func TestCancelRefusedOnceTheBookingStarted(t *testing.T) {
	now := time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)
	d, err := NewDriver(driver.Config{Courts: []driver.CourtConfig{{ID: "court-1"}}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Create(context.Background(), driver.CreateRequest{Court: "court-1", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Partner: "p"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := d.Cancel(context.Background(), b.ID, "p"); !errors.Is(err, driver.ErrStarted) {
		t.Fatalf("cancel at the start: %v", err)
	}
}
