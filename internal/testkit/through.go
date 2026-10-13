package testkit

import (
	"net/http"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/contract"
	"github.com/Padelapp-Club/court-gateway/internal/hub"
)

// HubPartnersYAML gives the contract suite's three keys to a hub, with
// access to every club.
const HubPartnersYAML = `partners:
  - { name: reader, key_env: TEST_READ_KEY, scopes: [read], clubs: ["*"] }
  - { name: booker, key_env: TEST_BOOK_KEY, scopes: [read, book], clubs: ["*"] }
  - { name: other, key_env: TEST_OTHER_KEY, scopes: [read, book], clubs: ["*"] }
`

// ThroughHub puts a hub in front of a built gateway: the gateway connects
// out to the hub over TLS, and the returned handler forwards every request
// over real HTTPS to the hub. The contract suite run against that handler
// checks the whole partner -> hub -> tunnel -> gateway -> CRM path. logs
// returns the gateway's and the hub's logs together.
func ThroughHub(t *testing.T, b *contract.Built) (handler http.Handler, logs func() string) {
	t.Helper()
	h := StartHub(t, "poll_wait: 2s\n"+HubPartnersYAML, contract.KeyEnv(), hub.Options{})
	secret := h.Register(b.Settings.ClubID, b.Settings.ClubName)
	gw := StartGateway(t, ClientConfig(h.URL(), h.Transport(), secret, b.Clubs.TunnelHandler(), nil))
	WaitFor(t, "gateway connected", 10*time.Second, func() bool {
		return gw.Client.Connected() && h.Server().Broker().Connected(b.Settings.ClubID)
	})
	handler = h.Forwarder(t)
	return handler, func() string { return b.Logs.String() + h.Logs.String() }
}
