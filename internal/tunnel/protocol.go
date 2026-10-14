// Package tunnel carries partner requests from the hub to a gateway over a
// connection that the GATEWAY opens. The gateway never listens: it keeps a
// few HTTPS long-poll requests open to the hub, the hub answers a poll with
// a queued partner request, and the gateway posts the response back.
//
// Plain HTTPS requests pass every proxy that passes HTTPS, and each one has
// its own timeout, so a broken path is noticed by the next request.
//
// Trust: the gateway reaches the hub over TLS (system roots, or only the
// CA of hub.ca_file) and follows no redirect; the hub authenticates the
// gateway by its secret (Bearer). What the gateway acts on — requests and
// club configurations — is signed by the hub's own ed25519 key, which the
// gateway pins at registration after checking it out of band (the key a
// release embeds, or the operator's fingerprint). So a TLS-inspecting
// proxy the club's computer trusts can read the traffic and the gateway
// secret, but cannot make the gateway write to its CRM (sign.go).
package tunnel

import (
	"encoding/json"
	"time"
)

// Endpoints on the hub.
const (
	PathRegister  = "/tunnel/register"
	PathPoll      = "/tunnel/poll"
	PathReply     = "/tunnel/reply"
	PathWhoami    = "/tunnel/whoami"
	PathHeartbeat = "/tunnel/heartbeat"
	// PathConfig returns the installation's club configuration.
	PathConfig = "/tunnel/config"
	// PathRelease: a gateway that stops tells the hub, so that the copy
	// started after it is not refused as a duplicate.
	PathRelease = "/tunnel/release"
)

// PathDiscover is served by the GATEWAY (through the tunnel): the hub's
// operator API asks the gateway to read its CRM and draft a club config.
const PathDiscover = "/gateway/discover"

// Headers a gateway sends with every poll, and the hub's update hint.
const (
	HeaderVersion  = "X-Gateway-Version"
	HeaderBuild    = "X-Gateway-Build" // BuildRelease or BuildSelf
	HeaderPlatform = "X-Gateway-Platform"
	// HeaderPolls is how many polls the gateway keeps open.
	HeaderPolls = "X-Gateway-Polls"
	// HeaderInstance is a random id of the running gateway process. The
	// hub serves one instance per registration and refuses another copy
	// that runs with the same secret (409 DUPLICATE_GATEWAY).
	HeaderInstance   = "X-Gateway-Instance"
	HeaderUpdateHint = "X-Update-Hint"
	// HeaderConfigVersion is set by the hub on poll and heartbeat answers:
	// the current version of the installation's club configuration. A
	// gateway that runs another version pulls PathConfig.
	HeaderConfigVersion = "X-Config-Version"
	// HeaderNonce is a fresh random value of each poll, and HeaderHubTime
	// the hub's answer to it: its clock, signed with the nonce and the
	// HeaderConfigVersion value (SignTime), so an old answer cannot be
	// passed off as new.
	HeaderNonce   = "X-Gateway-Nonce"
	HeaderHubTime = "X-Hub-Time"
)

// Build kinds reported by a gateway.
const (
	BuildRelease = "release"
	BuildSelf    = "selfbuilt"
)

// Limits.
const (
	// MaxRequestBody caps a partner request body forwarded to a gateway.
	MaxRequestBody = 1 << 20
	// MaxRequestTimeout caps the time a gateway gives one request; a longer
	// timeout from the hub is cut to it.
	MaxRequestTimeout = 3 * time.Minute
	// MaxResponseBody caps a gateway response body.
	MaxResponseBody = 4 << 20
	// maxEnvelope bounds an encoded envelope: base64 grows the body by 4/3.
	maxEnvelope = MaxResponseBody*4/3 + 64<<10
)

// ExecTimeout is the time a gateway gives a request whose TimeoutMS is
// timeoutMS: within [1s, MaxRequestTimeout], 30s when unset.
func ExecTimeout(timeoutMS int) time.Duration {
	if timeoutMS <= 0 {
		return 30 * time.Second
	}
	return min(max(time.Duration(timeoutMS)*time.Millisecond, time.Second), MaxRequestTimeout)
}

// Request is a partner request handed to a gateway in a poll response.
type Request struct {
	// ID correlates the reply with the waiting partner request.
	ID string `json:"id"`
	// RequestID is the id the partner sees in X-Request-ID and errors.
	RequestID string            `json:"request_id"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     string            `json:"query,omitempty"`
	Header    map[string]string `json:"header,omitempty"`
	Body      []byte            `json:"body,omitempty"`
	// Partner is the partner the hub authenticated, Scopes its scopes.
	Partner   string   `json:"partner,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	TimeoutMS int      `json:"timeout_ms"`
	Seal
}

// Response is what a gateway posts back.
type Response struct {
	ID     string            `json:"id"`
	Status int               `json:"status"`
	Header map[string]string `json:"header,omitempty"`
	Body   []byte            `json:"body,omitempty"`
}

// RegisterRequest exchanges a one-time join code for a gateway secret.
type RegisterRequest struct {
	Code    string `json:"code"`
	Version string `json:"version,omitempty"`
}

// RegisterResponse carries the long-lived secret. It is shown once.
// HubKey is the hub's public signing key (base64), which the gateway pins;
// Signature is that key's signature of the whole answer for the join code
// it answers (Sign, Verify).
type RegisterResponse struct {
	Installation string `json:"installation_id"`
	Name         string `json:"name"`
	Secret       string `json:"secret"`
	HubKey       string `json:"hub_key"`
	Signature    []byte `json:"signature,omitempty"`
}

// Whoami is the hub's view of the calling gateway.
type Whoami struct {
	Installation string `json:"installation_id"`
	Connected    bool   `json:"connected"`
	LastSeen     string `json:"last_seen,omitempty"`
	// ConfigVersion is the hub's current club configuration version;
	// Clubs are the clubs in it.
	ConfigVersion int64    `json:"config_version"`
	Clubs         []string `json:"clubs"`
	// Applied* is what the gateway reported in its last heartbeat.
	AppliedConfigVersion int64    `json:"applied_config_version"`
	ConfigError          string   `json:"config_error,omitempty"`
	ClubsServed          []string `json:"clubs_served"`
}

// RemoteConfig is the club configuration of an installation (PathConfig).
// Clubs are core.ClubSpec objects, kept raw here so the tunnel does not
// depend on the core.
type RemoteConfig struct {
	Version int64             `json:"version"`
	Clubs   []json.RawMessage `json:"clubs"`
	// NextKey is the hub's successor signing key (base64), announced under
	// the current key's signature before the hub switches to it.
	NextKey string `json:"next_key,omitempty"`
	Seal
}

// The only headers carried through the tunnel, each way: RequestHeaders
// from the partner to the gateway, ResponseHeaders back. Authorization
// never is: partner keys stay on the hub.
var (
	RequestHeaders  = []string{"Content-Type", "Accept"}
	ResponseHeaders = []string{"Content-Type", "Retry-After", "Cache-Control", "Location"}
)
