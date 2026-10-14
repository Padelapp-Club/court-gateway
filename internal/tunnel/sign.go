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
// and still be corrected (Verifier.Time); further off, signatures are
// checked against its own clock and refused (readiness: CLOCK_SKEW).
const MaxClockSkew = time.Hour

// MaxTimeRoundTrip bounds a poll's time to its answer's headers for the
// hub's time on it to be taken: the hub sends them before it holds the
// poll, so a slower answer (a buffering proxy) would carry a late time.
const MaxTimeRoundTrip = 5 * time.Second

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

// message is what the hub signs of a request handed to the poll that sent
// nonce: every field but the seal, the body by its hash.
func (rq *Request) message(nonce string) []byte {
	return rq.Seal.message("request", func(c *canon) { c.str(nonce); rq.fields(c) })
}

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
	c.str(rc.Epoch)
	clubs := make([]string, len(rc.Clubs))
	for i, raw := range rc.Clubs {
		clubs[i] = string(raw)
	}
	c.list(clubs)
}

func seal(installation, registration string, now time.Time) Seal {
	return Seal{Installation: installation, Registration: registration, IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(SignedFor).UnixMilli()}
}

// Sign seals rq for a registration of an installation and for the poll
// that takes it, by the poll's nonce (the hub).
func (rq *Request) Sign(key ed25519.PrivateKey, installation, registration, nonce string, now time.Time) {
	rq.Seal = seal(installation, registration, now)
	rq.Signature = ed25519.Sign(key, rq.message(nonce))
}

// Sign seals rc for a registration of an installation (the hub), the clubs
// as the bytes sent (compact, HTML-escaped, as encoding/json sends them).
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
// time, the poll's nonce and the config version (and epoch) it announces.
func timeMessage(installation, registration, nonce, version string, ms int64) []byte {
	s := Seal{Installation: installation, Registration: registration, IssuedAt: ms, ExpiresAt: ms}
	return s.message("time", func(c *canon) { c.str(nonce); c.str(version) })
}

// SignTime is the HeaderHubTime value of a poll answer, "<unix ms> <base64
// signature>": the hub's time, bound to the nonce and to version.
func SignTime(key ed25519.PrivateKey, installation, registration, nonce, version string, now time.Time) string {
	ms := now.UnixMilli()
	return strconv.FormatInt(ms, 10) + " " + base64.StdEncoding.EncodeToString(ed25519.Sign(key, timeMessage(installation, registration, nonce, version, ms)))
}

// ErrBadSignature: a message is not signed by the pinned hub key for this
// gateway, or no longer valid.
var ErrBadSignature = errors.New("not signed by the hub for this gateway")

// ErrClockUnknown: the hub's time is not known, or this computer's clock is
// more than MaxClockSkew off it: a request is refused, not executed.
var ErrClockUnknown = errors.New("the hub's time is not known")

// ErrStaleConfig: a config of another epoch (hub run) than announced, or
// older than announced or applied since: it is pulled again.
var ErrStaleConfig = errors.New("the config is older than the one announced or applied")

// ErrSlowTime: the hub's time arrived too late to be taken (MaxTimeRoundTrip).
var ErrSlowTime = errors.New("the hub's time arrived too late to be taken")

// ErrUnconfirmedRun: the hub answered from another run (a restart) not yet
// confirmed as the current one (Verifier.Time): no request runs until it is.
var ErrUnconfirmedRun = errors.New("the hub's current run is not confirmed yet")

// Verifier checks the hub's signatures on the gateway against the hub's
// time as last signed (Time), and its config version announcements.
type Verifier struct {
	key          ed25519.PublicKey
	installation string
	registration string
	now          func() time.Time // this computer's clock

	mu     sync.Mutex
	synced bool            // the hub has signed its time for this process
	hubAt  time.Time       // the hub's time as last signed
	at     time.Time       // when it arrived, on time.Now: its monotonic reading carries hubAt on
	offset time.Duration   // the hub's clock minus now, then
	config int64           // the config version announced, or applied since
	epoch  string          // the hub run it belongs to
	gone   map[string]bool // the runs it replaced: never taken again
	seen   uint64          // how many times another run was seen (Ticket)
	unsure bool            // since then, which run is current is not confirmed
	next   string          // the run seen then, and its version
	nextV  int64
}

// NewVerifier verifies with the pinned hub key for one registration (its
// secret) of an installation; now (default time.Now) is this computer's
// clock. The hub's time is carried on by the monotonic clock, never by now.
func NewVerifier(key ed25519.PublicKey, installation, secret string, now func() time.Time) (*Verifier, error) {
	if len(key) != ed25519.PublicKeySize || installation == "" || secret == "" {
		return nil, errors.New("tunnel: the hub key, the installation and the secret are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{key: key, installation: installation, registration: RegistrationOf(SecretSHA256(secret)), now: now, gone: map[string]bool{}}, nil
}

// Ticket is taken when a request is sent, for Time: its answer confirms
// which hub run is current only when no other run was seen since.
func (v *Verifier) Ticket() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.seen
}

// Time takes the hub's signed time (header, signed with the request's
// nonce and version, "<number> <epoch>") from the answer to a request sent
// at sent with ticket (Ticket), unless slower than MaxTimeRoundTrip
// (ErrSlowTime). The version is taken anyway: its order never comes from
// the hub's clock. Within an epoch it only goes up. Another epoch (a hub
// restart or restore) is not taken on its own, a late answer may be an
// ended run's: until an answer to a request sent after it was seen names
// the current epoch, the run is unconfirmed. That epoch is taken (a run
// that ended cannot answer a nonce made after another run answered), the
// others seen are replaced, and a replaced epoch never returns.
func (v *Verifier) Time(nonce, header, version string, sent time.Time, ticket uint64) error {
	msText, sigText, _ := strings.Cut(header, " ")
	ms, err1 := strconv.ParseInt(msText, 10, 64)
	sig, err2 := base64.StdEncoding.DecodeString(sigText)
	if err1 != nil || err2 != nil || !ed25519.Verify(v.key, timeMessage(v.installation, v.registration, nonce, version, ms), sig) {
		return fmt.Errorf("%w: the hub's time", ErrBadSignature)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	number, epoch, _ := strings.Cut(version, " ")
	n, err := strconv.ParseInt(number, 10, 64)
	switch {
	case err != nil || v.gone[epoch]:
	case v.unsure && ticket == v.seen: // asked after the other run was seen
		if epoch != v.next {
			v.gone[v.next] = true
		}
		if epoch != v.epoch {
			v.gone[v.epoch], v.config = true, n
		}
		v.config, v.epoch, v.unsure = max(v.config, n), epoch, false
	case epoch == v.epoch:
		v.config = max(v.config, n)
	case v.unsure && epoch == v.next:
		v.nextV = max(v.nextV, n)
	default:
		v.seen++
		v.unsure, v.next, v.nextV = true, epoch, n
	}
	received := v.now()
	if rtt := received.Sub(sent); rtt > MaxTimeRoundTrip {
		return fmt.Errorf("%w: %s after the request", ErrSlowTime, rtt.Round(time.Millisecond))
	}
	hub := time.UnixMilli(ms)
	v.synced, v.hubAt, v.at, v.offset = true, hub, time.Now(), hub.Sub(received)
	return nil
}

// Now is the hub's time now as last signed and whether it is known; when
// not, it is this computer's. It is the booking decisions' driver.Clock.
func (v *Verifier) Now() (time.Time, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.clock(), v.known()
}

// known: the hub signed its time, and it is at most MaxClockSkew off this
// computer's; v.mu must be held.
func (v *Verifier) known() bool { return v.synced && v.offset.Abs() <= MaxClockSkew }

// clock is the hub's time now, as far as it is known: the last signed one
// and the monotonic time since; v.mu must be held.
func (v *Verifier) clock() time.Time {
	if v.known() {
		return v.hubAt.Add(time.Since(v.at))
	}
	return v.now()
}

// Announced is the config version and epoch taken from the hub's signed
// time (Time), or applied since (Config); while the hub's run is not
// confirmed, the other run's: a config is pending.
func (v *Verifier) Announced() (int64, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.unsure {
		return v.nextV, v.next
	}
	return v.config, v.epoch
}

// Confirmed reports whether the hub's current run is confirmed (Time).
func (v *Verifier) Confirmed() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return !v.unsure
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
	now, known := v.clock(), v.known()
	v.mu.Unlock()
	switch {
	case needClock && !known:
		return ErrClockUnknown
	case now.After(expires.Add(clockSkew)) || now.Before(issued.Add(-clockSkew)):
		return fmt.Errorf("%w: expired or not yet valid (check this computer's clock)", ErrBadSignature)
	}
	return nil
}

// Request accepts rq when the hub signed it for this gateway, valid now,
// and for the poll that carried it (nonce, fresh on every poll), so a
// request runs once with nothing kept to remember it. Until the hub's
// time is known it accepts nothing (ErrClockUnknown), nor while its run
// is not confirmed (ErrUnconfirmedRun), nor a request of a run other than
// the current one.
func (v *Verifier) Request(rq *Request, nonce string) error {
	if err := v.check(rq.Seal, rq.message(nonce), true); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	switch {
	case v.unsure:
		return ErrUnconfirmedRun
	case rq.run != v.epoch:
		return fmt.Errorf("%w: signed in another hub run than the current one", ErrBadSignature)
	}
	return nil
}

// Config accepts a club configuration signed for this gateway, valid now,
// of the epoch announced (Time) and confirmed, and not older than
// announced or applied since (ErrStaleConfig); a replay within its
// validity gives the same one.
func (v *Verifier) Config(rc *RemoteConfig) error {
	if err := v.check(rc.Seal, rc.Seal.message("config", rc.fields), false); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.unsure || rc.Epoch != v.epoch || rc.Version < v.config {
		return fmt.Errorf("%w: version %d, %d announced or applied", ErrStaleConfig, rc.Version, v.config)
	}
	v.config = rc.Version
	return nil
}
