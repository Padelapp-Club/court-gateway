package lunda_test

import (
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
)

// The same driver-agnostic suite as every other driver, against the stub:
// a session saved by a login, a service client chosen.
func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) *contract.Env { return newFixture(t, fixtureOpts{}).env() })
}
