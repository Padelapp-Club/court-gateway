// Package tunnel carries partner requests from the hub to a gateway over a
// connection that the GATEWAY opens. The gateway never listens: it keeps a
// few HTTPS long-poll requests open to the hub, the hub answers a poll with
// a queued partner request, and the gateway posts the response back.
//
// Why long-polling over plain HTTPS rather than WebSocket: it needs nothing
// but the standard library, it passes every corporate proxy and middlebox
// that passes ordinary HTTPS, HTTP/2 multiplexes all polls over one TCP
// connection, and every exchange is a self-contained request with its own
// timeout, so a broken network path is noticed by the next request instead
// of by a keep-alive protocol layered on top.
//
// Trust: the gateway authenticates the hub by its TLS certificate (system
// roots, or a CA file for self-hosted hubs and tests) and the hub
// authenticates the gateway by its per-gateway secret (Bearer). A request
// that arrives in a poll response is therefore known to come from the hub;
// that is what "requests from the hub are signed" means for this design.
package tunnel

// Endpoints on the hub.
const (
	PathRegister  = "/tunnel/register"
	PathPoll      = "/tunnel/poll"
	PathReply     = "/tunnel/reply"
	PathWhoami    = "/tunnel/whoami"
	PathHeartbeat = "/tunnel/heartbeat"
)

// Headers a gateway sends with every poll, and the hub's update hint.
const (
	HeaderVersion    = "X-Gateway-Version"
	HeaderBuild      = "X-Gateway-Build" // BuildRelease or BuildSelf
	HeaderPlatform   = "X-Gateway-Platform"
	HeaderUpdateHint = "X-Update-Hint"
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
	// MaxResponseBody caps a gateway response body.
	MaxResponseBody = 4 << 20
	// maxEnvelope bounds an encoded envelope: base64 grows the body by 4/3.
	maxEnvelope = MaxResponseBody*4/3 + 64<<10
)

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
	// Partner is the partner the hub authenticated.
	Partner   string `json:"partner,omitempty"`
	TimeoutMS int    `json:"timeout_ms"`
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
type RegisterResponse struct {
	Club     string `json:"club"`
	ClubName string `json:"club_name"`
	Secret   string `json:"secret"`
}

// Whoami is the hub's view of the calling gateway.
type Whoami struct {
	Club      string `json:"club"`
	Connected bool   `json:"connected"`
	LastSeen  string `json:"last_seen,omitempty"`
	Latest    string `json:"latest_version,omitempty"`
}

// forwardedHeaders are the only request/response headers carried through
// the tunnel. Authorization never is: partner keys stay on the hub.
var forwardedHeaders = []string{"Content-Type", "Accept", "Retry-After"}

// ForwardedHeaders returns the header names carried through the tunnel.
func ForwardedHeaders() []string { return append([]string(nil), forwardedHeaders...) }
