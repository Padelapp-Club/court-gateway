package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"
)

// The hub signs every request it hands a gateway, and every club
// configuration it sends, with its own ed25519 key. The gateway pins the
// hub's public key when it registers, after checking it out of band
// (setup: the key a release embeds, or the operator's fingerprint), and
// acts on nothing that key did not sign for it. So whoever stands between
// the two — a TLS-inspecting proxy the club's computer trusts — can read
// the traffic and the gateway secret, but cannot make the gateway write to
// its CRM, enrollment included.

// SignedFor is how long a signature is valid after the hub made it.
const SignedFor = 60 * time.Second

// clockSkew is how far the gateway's clock may be off the hub's.
const clockSkew = 30 * time.Second

// maxSeen bounds the request ids the gateway remembers against replay.
const maxSeen = 1 << 16

// Seal is the hub's signature and what it binds the message to: the
// installation, its current registration and a validity window.
type Seal struct {
	Installation string `json:"installation,omitempty"`
	// Registration names the registration (RegistrationOf): a message
	// signed for an earlier one is refused after a new join code.
	Registration string `json:"registration,omitempty"`
	// IssuedAt and ExpiresAt are unix milliseconds on the hub's clock.
	IssuedAt  int64  `json:"issued_at,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Signature []byte `json:"signature,omitempty"`
}

// SecretSHA256 is the hex SHA-256 of a gateway secret, which is all the
// hub stores of it.
func SecretSHA256(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// RegistrationOf names a registration by the hash of its secret: both
// sides can compute it, and a new join code changes it.
func RegistrationOf(secretSHA256 string) string {
	sum := sha256.Sum256([]byte("court-gateway-registration:" + secretSHA256))
	return hex.EncodeToString(sum[:16])
}

// canon writes fields so that no two different field lists give the same
// bytes: each value is length-prefixed, each list count-prefixed.
type canon struct{ bytes.Buffer }

func (c *canon) str(s string) { c.WriteString(strconv.Itoa(len(s)) + ":" + s + ",") }
func (c *canon) num(n int64)  { c.str(strconv.FormatInt(n, 10)) }
func (c *canon) list(ss []string) {
	c.num(int64(len(ss)))
	for _, s := range ss {
		c.str(s)
	}
}

func (s Seal) message(kind string, body func(*canon)) []byte {
	var c canon
	c.str("court-gateway hub signature v1")
	c.str(kind)
	c.str(s.Installation)
	c.str(s.Registration)
	c.num(s.IssuedAt)
	c.num(s.ExpiresAt)
	body(&c)
	return c.Bytes()
}

// fields are every field of the request but the seal; the body by its hash.
func (rq *Request) fields(c *canon) {
	c.str(rq.ID)
	c.str(rq.RequestID)
	c.str(rq.Method)
	c.str(rq.Path)
	c.str(rq.Query)
	var header []string
	for _, k := range slices.Sorted(maps.Keys(rq.Header)) {
		header = append(header, k, rq.Header[k])
	}
	c.list(header)
	sum := sha256.Sum256(rq.Body)
	c.str(hex.EncodeToString(sum[:]))
	c.str(rq.Partner)
	c.list(rq.Scopes)
	c.num(int64(rq.TimeoutMS))
}

func (rc *RemoteConfig) fields(c *canon) {
	c.num(rc.Version)
	clubs := make([]string, len(rc.Clubs))
	for i, raw := range rc.Clubs {
		clubs[i] = string(raw)
	}
	c.list(clubs)
}

func seal(installation, registration string, now time.Time) Seal {
	return Seal{Installation: installation, Registration: registration, IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(SignedFor).UnixMilli()}
}

// Sign seals rq for a registration of an installation (the hub).
func (rq *Request) Sign(key ed25519.PrivateKey, installation, registration string, now time.Time) {
	rq.Seal = seal(installation, registration, now)
	rq.Signature = ed25519.Sign(key, rq.Seal.message("request", rq.fields))
}

// Sign seals rc for a registration of an installation (the hub). The
// clubs are signed as the bytes that are sent: first written the way
// encoding/json sends a raw message (compact, HTML-escaped).
func (rc *RemoteConfig) Sign(key ed25519.PrivateKey, installation, registration string, now time.Time) {
	for i, raw := range rc.Clubs {
		var compact, escaped bytes.Buffer
		if json.Compact(&compact, raw) == nil {
			json.HTMLEscape(&escaped, compact.Bytes())
			rc.Clubs[i] = escaped.Bytes()
		}
	}
	rc.Seal = seal(installation, registration, now)
	rc.Signature = ed25519.Sign(key, rc.Seal.message("config", rc.fields))
}

// ErrBadSignature: a message is not signed by the pinned hub key for this
// gateway, or no longer valid.
var ErrBadSignature = errors.New("not signed by the hub for this gateway")

// Verifier checks the hub's signatures on the gateway.
type Verifier struct {
	key          ed25519.PublicKey
	installation string
	registration string
	now          func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // request id -> until when it is remembered
}

// NewVerifier verifies with the pinned hub key for one registration (its
// secret) of an installation; now defaults to time.Now.
func NewVerifier(key ed25519.PublicKey, installation, secret string, now func() time.Time) (*Verifier, error) {
	if len(key) != ed25519.PublicKeySize || installation == "" || secret == "" {
		return nil, errors.New("tunnel: the hub key, the installation and the secret are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{key: key, installation: installation, registration: RegistrationOf(SecretSHA256(secret)), now: now, seen: map[string]time.Time{}}, nil
}

func (v *Verifier) check(s Seal, msg []byte) error {
	now := v.now()
	issued, expires := time.UnixMilli(s.IssuedAt), time.UnixMilli(s.ExpiresAt)
	switch {
	case !ed25519.Verify(v.key, msg, s.Signature):
		return fmt.Errorf("%w: bad signature", ErrBadSignature)
	case s.Installation != v.installation || s.Registration != v.registration:
		return fmt.Errorf("%w: signed for another installation or registration", ErrBadSignature)
	case !expires.After(issued) || expires.Sub(issued) > SignedFor:
		return fmt.Errorf("%w: invalid validity window", ErrBadSignature)
	case now.After(expires.Add(clockSkew)) || now.Before(issued.Add(-clockSkew)):
		return fmt.Errorf("%w: expired or not yet valid (check this computer's clock)", ErrBadSignature)
	}
	return nil
}

// Request accepts rq once: signed for this gateway, valid now, and not
// seen before within its validity.
func (v *Verifier) Request(rq *Request) error {
	if err := v.check(rq.Seal, rq.Seal.message("request", rq.fields)); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if _, dup := v.seen[rq.ID]; dup {
		return fmt.Errorf("%w: replayed request", ErrBadSignature)
	}
	if len(v.seen) >= maxSeen {
		maps.DeleteFunc(v.seen, func(_ string, until time.Time) bool { return now.After(until) })
		if len(v.seen) >= maxSeen {
			return fmt.Errorf("%w: too many requests within the validity window", ErrBadSignature)
		}
	}
	v.seen[rq.ID] = time.UnixMilli(rq.ExpiresAt).Add(clockSkew)
	return nil
}

// Config accepts a club configuration signed for this gateway and valid
// now. Replaying it within its validity gives the same configuration.
func (v *Verifier) Config(rc *RemoteConfig) error {
	return v.check(rc.Seal, rc.Seal.message("config", rc.fields))
}
