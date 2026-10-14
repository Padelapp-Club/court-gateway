package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The hub's signatures; the trust model is in the package comment.

// SignedFor is how long a signature is valid after the hub made it.
const SignedFor = 60 * time.Second

// clockSkew is how far a signature's window is stretched at both ends:
// the hub's time as the gateway knows it is a little off the hub's own.
const clockSkew = 30 * time.Second

// MaxClockSkew bounds how far this computer's clock may be off the hub's
// and still be corrected: the gateway checks signatures against the hub's
// time (Verifier.Time) when they are at most this far apart, and against
// its own clock otherwise, refusing them (readiness: CLOCK_SKEW).
const MaxClockSkew = time.Hour

// MaxTimeRoundTrip bounds how long a poll may take, from sending it to
// its answer's headers, for the hub's time on that answer to be taken:
// the hub sends the headers before it holds the poll, so a slower answer
// (a proxy that buffers it) would leave the time that much behind.
const MaxTimeRoundTrip = 5 * time.Second

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

// message is what the hub signs of a registration answer: every field,
// bound to the join code it answers.
func (rr *RegisterResponse) message(code string) []byte {
	var c canon
	c.str("court-gateway hub signature v1")
	c.str("registration")
	c.str(SecretSHA256(code))
	c.str(rr.Installation)
	c.str(rr.Name)
	c.str(SecretSHA256(rr.Secret))
	c.str(rr.HubKey)
	return c.Bytes()
}

// Sign signs the answer to join code code (the hub).
func (rr *RegisterResponse) Sign(key ed25519.PrivateKey, code string) {
	rr.Signature = ed25519.Sign(key, rr.message(code))
}

// Verify checks that key signed this answer to code, every field of it.
func (rr *RegisterResponse) Verify(key ed25519.PublicKey, code string) error {
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, rr.message(code), rr.Signature) {
		return fmt.Errorf("%w: the registration answer", ErrBadSignature)
	}
	return nil
}

// timeMessage is what the hub signs of its clock on a poll answer: the
// time, the poll's nonce and the config version it announces.
func timeMessage(installation, registration, nonce, version string, ms int64) []byte {
	s := Seal{Installation: installation, Registration: registration, IssuedAt: ms, ExpiresAt: ms}
	return s.message("time", func(c *canon) { c.str(nonce); c.str(version) })
}

// SignTime is the HeaderHubTime value the hub sets on a poll answer:
// its time now, bound to the poll's nonce and to the HeaderConfigVersion
// value version, "<unix ms> <base64 signature>".
func SignTime(key ed25519.PrivateKey, installation, registration, nonce, version string, now time.Time) string {
	ms := now.UnixMilli()
	return strconv.FormatInt(ms, 10) + " " + base64.StdEncoding.EncodeToString(ed25519.Sign(key, timeMessage(installation, registration, nonce, version, ms)))
}

// ErrBadSignature: a message is not signed by the pinned hub key for this
// gateway, or no longer valid.
var ErrBadSignature = errors.New("not signed by the hub for this gateway")

// ErrClockUnknown: the gateway does not know the hub's time yet, or this
// computer's clock is more than MaxClockSkew off it; nothing that depends
// on the time now is decided (a request is refused, not executed).
var ErrClockUnknown = errors.New("the hub's time is not known")

// ErrSlowTime: the hub's time arrived too late to be taken
// (MaxTimeRoundTrip).
var ErrSlowTime = errors.New("the hub's time arrived too late to be taken")

// Verifier checks the hub's signatures on the gateway, against the hub's
// time as last signed for it (Time), and the hub's announcements of its
// config version.
type Verifier struct {
	key          ed25519.PublicKey
	installation string
	registration string
	now          func() time.Time
	started      time.Time // this process, on now

	mu     sync.Mutex
	synced bool                 // the hub has signed its time for this process
	offset time.Duration        // the hub's clock minus now, as last signed
	config int64                // the highest config version applied or announced
	seen   map[string]time.Time // request id -> until when (hub time) it is remembered
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
	return &Verifier{key: key, installation: installation, registration: RegistrationOf(SecretSHA256(secret)), now: now, started: now(),
		seen: map[string]time.Time{}}, nil
}

// Time takes the hub's signed time from the answer to a request sent at
// sent (header, the HeaderHubTime value, signed with the request's nonce
// and the announced config version): from then on signatures are checked
// against the hub's clock, so this computer's may be off, and a config
// older than version is refused. An answer slower than MaxTimeRoundTrip
// is not taken (ErrSlowTime).
func (v *Verifier) Time(nonce, header, version string, sent time.Time) error {
	msText, sigText, _ := strings.Cut(header, " ")
	ms, err1 := strconv.ParseInt(msText, 10, 64)
	sig, err2 := base64.StdEncoding.DecodeString(sigText)
	if err1 != nil || err2 != nil || !ed25519.Verify(v.key, timeMessage(v.installation, v.registration, nonce, version, ms), sig) {
		return fmt.Errorf("%w: the hub's time", ErrBadSignature)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	received := v.now()
	if rtt := received.Sub(sent); rtt > MaxTimeRoundTrip {
		return fmt.Errorf("%w: %s after the request", ErrSlowTime, rtt.Round(time.Millisecond))
	}
	// the hub signed before this arrived, so the offset may come out low by
	// the answer's travel time, never high: the start below is never later
	// than the true one
	v.synced, v.offset = true, time.UnixMilli(ms).Sub(received)
	if n, err := strconv.ParseInt(version, 10, 64); err == nil {
		v.config = max(v.config, n)
	}
	return nil
}

// Offset is how far the hub's clock is ahead of this computer's, as the
// hub last signed it (0 before it has).
func (v *Verifier) Offset() time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.offset
}

// Now is the hub's time now, as the hub last signed it, and whether it
// is known (ErrClockUnknown); when it is not, Now is this computer's own.
// It is the clock of every booking decision (driver.Clock).
func (v *Verifier) Now() (time.Time, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now, _ := v.clock()
	return now, v.known()
}

// known: the hub signed its time, and it is at most MaxClockSkew off this
// computer's; v.mu must be held.
func (v *Verifier) known() bool { return v.synced && v.offset.Abs() <= MaxClockSkew }

// clock is the hub's time now and at this process's start, as far as it
// is known; v.mu must be held.
func (v *Verifier) clock() (now, started time.Time) {
	var off time.Duration
	if v.known() {
		off = v.offset
	}
	return v.now().Add(off), v.started.Add(off)
}

// check verifies a seal: signed by the pinned key for this registration,
// and valid now on the hub's clock, or on this computer's when that is
// not known, unless needClock (ErrClockUnknown).
func (v *Verifier) check(s Seal, msg []byte, needClock bool) error {
	issued, expires := time.UnixMilli(s.IssuedAt), time.UnixMilli(s.ExpiresAt)
	switch {
	case !ed25519.Verify(v.key, msg, s.Signature):
		return fmt.Errorf("%w: bad signature", ErrBadSignature)
	case s.Installation != v.installation || s.Registration != v.registration:
		return fmt.Errorf("%w: signed for another installation or registration", ErrBadSignature)
	case !expires.After(issued) || expires.Sub(issued) > SignedFor:
		return fmt.Errorf("%w: invalid validity window", ErrBadSignature)
	}
	v.mu.Lock()
	now, _ := v.clock()
	known := v.known()
	v.mu.Unlock()
	switch {
	case needClock && !known:
		return ErrClockUnknown
	case now.After(expires.Add(clockSkew)) || now.Before(issued.Add(-clockSkew)):
		return fmt.Errorf("%w: expired or not yet valid (check this computer's clock)", ErrBadSignature)
	}
	return nil
}

// Request accepts rq once: signed for this gateway, valid now, issued
// after this process started by the hub's clock (what it saw before a
// restart is forgotten, and the hub signs a request as a poll of this
// process takes it), and not seen before within its validity. Until the
// hub's time is known it accepts nothing (ErrClockUnknown).
func (v *Verifier) Request(rq *Request) error {
	if err := v.check(rq.Seal, rq.Seal.message("request", rq.fields), true); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now, started := v.clock()
	if rq.IssuedAt < started.UnixMilli() { // in the seal's own unit
		return fmt.Errorf("%w: issued before this gateway started", ErrBadSignature)
	}
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

// Config accepts a club configuration signed for this gateway, valid
// now, and not older than one already applied or announced (Time).
// Replaying it within its validity gives the same configuration.
func (v *Verifier) Config(rc *RemoteConfig) error {
	if err := v.check(rc.Seal, rc.Seal.message("config", rc.fields), false); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if rc.Version < v.config {
		return fmt.Errorf("%w: config version %d is older than %d, applied or announced", ErrBadSignature, rc.Version, v.config)
	}
	v.config = rc.Version
	return nil
}
