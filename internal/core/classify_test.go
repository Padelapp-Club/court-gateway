package core

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// A club that has not finished setting up bookings is "try later" (503),
// not "never" (501): partners keep the club and retry.
func TestBookingNotConfiguredIsNotNotSupported(t *testing.T) {
	err := fmt.Errorf("%w (%w)", &driver.ConfigAlert{Code: "SERVICE_CLIENT_MISSING"}, driver.ErrBookingNotConfigured)
	if status, code, _ := classify(err); status != http.StatusServiceUnavailable || code != "BOOKING_NOT_CONFIGURED" {
		t.Fatalf("classify = %d %s", status, code)
	}
	if status, code, _ := classify(driver.ErrNotSupported); status != http.StatusNotImplemented || code != "NOT_SUPPORTED" {
		t.Fatalf("classify = %d %s", status, code)
	}
}
