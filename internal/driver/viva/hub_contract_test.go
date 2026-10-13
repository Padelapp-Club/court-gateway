package viva_test

import (
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/testkit"
)

// The privacy suite (and the rest of the contract) through the hub: a club
// booking full of personal data sits in the CRM stub, and nothing of it may
// reach a partner or a log line on the gateway or the hub.
func TestContractThroughHub(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env {
		f := newFixture(t, fixtureOpts{services: bookingService})
		env := f.env()
		env.Handler, env.Logs = testkit.ThroughHub(t, f.built)
		return env
	})
}
