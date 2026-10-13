// Package auth is a self-contained HTTP middleware for partner API keys:
// bearer-key authentication with constant-time comparison, read/book scopes,
// a per-key token bucket on write requests and a cap on the requests one
// key has in flight. It knows nothing about the
// handler it protects, so it can be placed in front of the same API handler
// wherever that handler is served.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scope is a permission granted to a partner key.
type Scope string

const (
	// ScopeRead allows safe methods (GET, HEAD).
	ScopeRead Scope = "read"
	// ScopeBook allows unsafe methods (POST, PUT, PATCH, DELETE).
	ScopeBook Scope = "book"
)

// Partner is one API key holder.
type Partner struct {
	Name   string
	Key    string
	Scopes []Scope
	// WritesPerMinute is the token-bucket rate (and burst) for write
	// requests. Zero or negative disables the limit for this key.
	WritesPerMinute int
}

// ErrorWriter writes an error response. The middleware uses it for every
// refusal so the protected API keeps one error format.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, status int, code, message string)

// DefaultMaxInFlight is how many requests one key may have in flight at
// once when Options.MaxInFlight is not set.
const DefaultMaxInFlight = 16

// Options tune the middleware.
type Options struct {
	WriteError ErrorWriter
	Now        func() time.Time
	// MaxInFlight caps the requests of one key being served at once, reads
	// included, over all its clubs together. It bounds the load one key
	// puts on the server; a club's gateway is guarded by the hub's own
	// per-gateway cap.
	MaxInFlight int
}

type partner struct {
	name   string
	digest [sha256.Size]byte
	read   bool
	book   bool
	bucket *bucket
	slots  chan struct{} // one per request in flight
}

// Authenticator holds the configured keys.
type Authenticator struct {
	partners []*partner
	writeErr ErrorWriter
	now      func() time.Time
}

// New validates the partner list and builds an Authenticator.
func New(partners []Partner, opts Options) (*Authenticator, error) {
	if len(partners) == 0 {
		return nil, errors.New("auth: at least one partner is required")
	}
	a := &Authenticator{writeErr: opts.WriteError, now: opts.Now}
	if opts.MaxInFlight <= 0 {
		opts.MaxInFlight = DefaultMaxInFlight
	}
	if a.writeErr == nil {
		a.writeErr = defaultWriteError
	}
	if a.now == nil {
		a.now = time.Now
	}
	seen := map[[sha256.Size]byte]string{}
	names := map[string]bool{}
	for _, p := range partners {
		if p.Name == "" || p.Key == "" {
			return nil, errors.New("auth: partner name and key are required")
		}
		if names[p.Name] {
			return nil, fmt.Errorf("auth: duplicate partner name %q", p.Name)
		}
		names[p.Name] = true
		d := sha256.Sum256([]byte(p.Key))
		if other, dup := seen[d]; dup {
			return nil, fmt.Errorf("auth: partners %q and %q share a key", other, p.Name)
		}
		seen[d] = p.Name
		ip := &partner{name: p.Name, digest: d, slots: make(chan struct{}, opts.MaxInFlight)}
		for _, s := range p.Scopes {
			switch s {
			case ScopeRead:
				ip.read = true
			case ScopeBook:
				ip.book = true
			default:
				return nil, fmt.Errorf("auth: unknown scope %q", s)
			}
		}
		if p.WritesPerMinute > 0 {
			ip.bucket = newBucket(p.WritesPerMinute, a.now())
		}
		a.partners = append(a.partners, ip)
	}
	return a, nil
}

type ctxKey struct{}

// PartnerName returns the authenticated partner's name from the request
// context.
func PartnerName(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKey{}).(string)
	return v, ok && v != ""
}

// WithPartnerName returns a context carrying an authenticated partner name.
// Exposed for callers that authenticate by other means.
func WithPartnerName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, ctxKey{}, name)
}

// lookup compares the presented key with every configured key in constant
// time per key and without an early exit, so timing does not reveal which
// (or whether any) key matched a prefix.
func (a *Authenticator) lookup(key string) *partner {
	d := sha256.Sum256([]byte(key))
	var found *partner
	for _, p := range a.partners {
		if subtle.ConstantTimeCompare(d[:], p.digest[:]) == 1 {
			found = p
		}
	}
	return found
}

// Bearer returns the token of an "Authorization: Bearer" header.
func Bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	k := strings.TrimSpace(h[len(prefix):])
	return k, k != ""
}

func isWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// Wrap protects next.
func (a *Authenticator) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := Bearer(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			a.writeErr(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "missing or malformed Authorization: Bearer header")
			return
		}
		p := a.lookup(key)
		if p == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api", error="invalid_token"`)
			a.writeErr(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "unknown API key")
			return
		}
		write := isWrite(r.Method)
		if (write && !p.book) || (!write && !p.read) {
			a.writeErr(w, r, http.StatusForbidden, "FORBIDDEN", "the API key has no scope for this operation")
			return
		}
		if write && p.bucket != nil {
			if wait, allowed := p.bucket.take(a.now()); !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
				a.writeErr(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "too many write requests for this API key")
				return
			}
		}
		select {
		case p.slots <- struct{}{}:
			defer func() { <-p.slots }()
		default:
			w.Header().Set("Retry-After", "1")
			a.writeErr(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests in flight for this API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPartnerName(r.Context(), p.name)))
	})
}

// bucket is a classic token bucket: capacity tokens, refilled continuously
// at capacity per minute.
type bucket struct {
	mu       sync.Mutex
	capacity float64
	perSec   float64
	tokens   float64
	last     time.Time
}

func newBucket(perMinute int, now time.Time) *bucket {
	c := float64(perMinute)
	return &bucket{capacity: c, perSec: c / 60, tokens: c, last: now}
}

func (b *bucket) take(now time.Time) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+el*b.perSec)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	return time.Duration((1 - b.tokens) / b.perSec * float64(time.Second)), false
}

func defaultWriteError(w http.ResponseWriter, _ *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}
