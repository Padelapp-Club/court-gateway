package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/auth"
	"github.com/Padelapp-Club/court-gateway/internal/core"
)

// ClientConfig configures the gateway side of the tunnel.
type ClientConfig struct {
	HubURL string
	Secret string
	// HubKey and Installation are pinned at registration: the client acts
	// only on requests and configs that key signed for this installation
	// and this secret's registration.
	HubKey       ed25519.PublicKey
	Installation string
	// Verifier, when set, is the one the client checks with (built from
	// HubKey, Installation and Secret otherwise): its Now is the hub's
	// time, for the clubs' booking decisions.
	Verifier *Verifier
	// Polls is the number of concurrent long-poll requests (default 4).
	Polls int
	// Handler serves requests that arrive through the tunnel.
	Handler http.Handler
	// Version, Build and Platform are reported to the hub on every poll.
	Version, Build, Platform string
	// Instance identifies this process to the hub (default: random).
	Instance string
	Logger   *slog.Logger
	// OnHint is called when the hub says a newer release exists.
	OnHint func(version string)
	// OnConfigVersion is called with the hub's club configuration version
	// from every answer whose signed time signs it (polls).
	OnConfigVersion func(version int64)
	// Transport defaults to NewTransport(nil, nil).
	Transport http.RoundTripper
	// Backoff after a failed poll grows from MinBackoff to MaxBackoff with
	// full jitter (defaults 1s and 60s).
	MinBackoff, MaxBackoff time.Duration
	// PollTimeout caps one poll request; it must exceed the hub's wait
	// (default 60s).
	PollTimeout time.Duration
}

// Client keeps the gateway connected to the hub.
type Client struct {
	cfg      ClientConfig
	http     *http.Client
	log      *slog.Logger
	verify   *Verifier
	refused  atomic.Int64 // requests and configs refused for their signature
	okPolls  atomic.Int32
	lastHint atomic.Value // string
	authFail atomic.Bool
	dup      atomic.Bool
	badKey   atomic.Bool // the hub's last signed time failed the pinned key
	mu       sync.Mutex
	state    string // "connected" / "disconnected", for transition logs
}

// ErrUnauthorized means the hub rejected the gateway secret.
var ErrUnauthorized = errors.New("the hub rejected the gateway secret")

// ErrDuplicate: the hub serves another running copy of this gateway, one
// with the same secret; this copy stays idle until that one is gone.
var ErrDuplicate = errors.New("another copy of this gateway, with the same secret, is connected to the hub")

// ErrRedirected: the hub answered with a redirect. No request to the hub
// follows one (core.RefuseRedirects): hub.url must be the hub itself.
var ErrRedirected = errors.New("the hub answered with a redirect, which is not followed: hub.url must be the hub's own address")

// answerError is the error for a hub answer with status code, other than
// a success; what names the request.
func answerError(status int, what string) error {
	switch {
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusConflict:
		return ErrDuplicate
	case core.IsRedirect(status):
		return fmt.Errorf("HTTP %d to %s: %w", status, what, ErrRedirected)
	}
	return fmt.Errorf("hub answered HTTP %d to %s", status, what)
}

// hubError is answerError, noting a rejected secret and a refused copy
// for Unauthorized and Duplicate.
func (c *Client) hubError(status int, what string) error {
	err := answerError(status, what)
	switch {
	case errors.Is(err, ErrUnauthorized):
		c.authFail.Store(true)
	case errors.Is(err, ErrDuplicate):
		c.dup.Store(true)
	}
	return err
}

// hubClient is the HTTP client of every request to the hub.
func hubClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, CheckRedirect: core.RefuseRedirects}
}

// LoadRoots returns the roots the hub's certificate is checked against:
// nil (the system roots) without caFile, otherwise ONLY the certificates
// in caFile. That pins the hub's CA: a TLS-inspecting proxy that this
// machine trusts can then not stand in for the hub.
func LoadRoots(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pool := x509.NewCertPool()
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("hub.ca_file: %w", err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("hub.ca_file: в файле %s нет сертификатов PEM", caFile)
	}
	return pool, nil
}

// NewTransport builds the HTTPS transport used to reach the hub: proxy
// from the environment (clubs behind a corporate proxy), HTTP/2 with
// pings so a silently dead path is noticed, TLS 1.2+ verified against
// roots (nil means the system roots).
func NewTransport(roots *x509.CertPool, proxy core.ProxyFunc) *http.Transport {
	t := proxy.Transport()
	t.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	t.ForceAttemptHTTP2 = true
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 20 * time.Second, PingTimeout: 10 * time.Second}
	t.MaxIdleConnsPerHost = 64
	return t
}

// NewClient validates cfg.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.HubURL == "" || cfg.Secret == "" || cfg.Handler == nil {
		return nil, errors.New("tunnel: hub URL, secret and handler are required")
	}
	v := cfg.Verifier
	if v == nil {
		var err error
		if v, err = NewVerifier(cfg.HubKey, cfg.Installation, cfg.Secret, nil); err != nil {
			return nil, err
		}
	}
	cfg.HubURL = strings.TrimRight(cfg.HubURL, "/")
	if cfg.Polls <= 0 {
		cfg.Polls = 4
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = 60 * time.Second
		if cfg.MaxBackoff < cfg.MinBackoff {
			cfg.MaxBackoff = cfg.MinBackoff
		}
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 60 * time.Second
	}
	if cfg.Transport == nil {
		cfg.Transport = NewTransport(nil, nil)
	}
	if cfg.Build == "" {
		cfg.Build = BuildSelf
	}
	if cfg.Instance == "" {
		var b [16]byte
		_, _ = crand.Read(b[:])
		cfg.Instance = hex.EncodeToString(b[:])
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{cfg: cfg, http: hubClient(cfg.Transport), log: log, verify: v}, nil
}

// Connected reports whether at least one poll is currently healthy.
func (c *Client) Connected() bool { return c.okPolls.Load() > 0 }

// Unauthorized reports whether the hub's last answer rejected the secret.
func (c *Client) Unauthorized() bool { return c.authFail.Load() }

// Duplicate reports whether the hub's last answer refused this copy
// because another one with the same secret is connected.
func (c *Client) Duplicate() bool { return c.dup.Load() }

// Refused counts the requests and configs refused because the hub did
// not sign them for this gateway (heartbeats report it).
func (c *Client) Refused() int64 { return c.refused.Load() }

// ClockOffset is how far the hub's clock was ahead of this computer's
// when the hub last signed it (0 before it has); beyond MaxClockSkew the
// gateway refuses what the hub signs and reports CLOCK_SKEW.
func (c *Client) ClockOffset() time.Duration {
	c.verify.mu.Lock()
	defer c.verify.mu.Unlock()
	return c.verify.offset
}

// HubKeyMismatch reports whether the hub's last signed time failed the
// pinned key: the hub signs with another key (a new one, or not the hub).
func (c *Client) HubKeyMismatch() bool { return c.badKey.Load() }

// Run keeps Polls pollers running until ctx is done.
func (c *Client) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < c.cfg.Polls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.poller(ctx)
		}()
	}
	wg.Wait()
}

func (c *Client) note(ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case ok && c.state != "connected" && c.Connected():
		c.state = "connected"
		c.log.Info("hub connected", "hub", c.cfg.HubURL)
	case !ok && c.state != "disconnected" && !c.Connected():
		c.state = "disconnected"
		switch {
		case errors.Is(err, ErrUnauthorized):
			c.log.Error("hub rejected the gateway secret; run setup again with a new join code", "hub", c.cfg.HubURL)
		case errors.Is(err, ErrDuplicate):
			c.log.Error("another copy of this gateway, with the same secret, is connected to the hub; this copy stays idle "+
				"until that one stops — run only one", "hub", c.cfg.HubURL)
		default:
			c.log.Warn("hub unreachable, reconnecting", "hub", c.cfg.HubURL, "error", err)
		}
	}
}

func (c *Client) poller(ctx context.Context) {
	failures := 0
	healthy := false
	setHealthy := func(v bool, err error) {
		if v != healthy {
			healthy = v
			if v {
				c.okPolls.Add(1)
			} else {
				c.okPolls.Add(-1)
			}
		}
		c.note(v, err)
	}
	defer func() {
		if healthy {
			c.okPolls.Add(-1)
		}
	}()
	for ctx.Err() == nil {
		rq, nonce, err := c.poll(ctx, func() {
			failures = 0
			c.authFail.Store(false)
			c.dup.Store(false)
			setHealthy(true, nil)
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			setHealthy(false, err)
			failures++
			if !sleep(ctx, c.backoff(failures, errors.Is(err, ErrUnauthorized))) {
				return
			}
			continue
		}
		if rq == nil {
			continue
		}
		resp := c.Execute(ctx, *rq, nonce)
		c.reply(ctx, resp)
	}
}

// backoff returns a full-jitter delay: uniform in [0, min(max, min*2^n)).
// A rejected secret waits the maximum: retrying faster cannot help. A
// refused duplicate backs off like a failure: it may be the restart of a
// copy whose last polls the hub has not yet seen end.
func (c *Client) backoff(failures int, refused bool) time.Duration {
	ceil := c.cfg.MaxBackoff
	if !refused {
		ceil = c.cfg.MinBackoff
		for i := 1; i < failures && ceil < c.cfg.MaxBackoff; i++ {
			ceil *= 2
		}
		if ceil > c.cfg.MaxBackoff {
			ceil = c.cfg.MaxBackoff
		}
	}
	return c.cfg.MinBackoff/2 + rand.N(ceil)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Client) newRequest(ctx context.Context, method, p string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.HubURL+p, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Secret)
	req.Header.Set(HeaderVersion, c.cfg.Version)
	req.Header.Set(HeaderBuild, c.cfg.Build)
	req.Header.Set(HeaderPlatform, c.cfg.Platform)
	req.Header.Set(HeaderPolls, strconv.Itoa(c.cfg.Polls))
	req.Header.Set(HeaderInstance, c.cfg.Instance)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// poll returns the next request and the poll's nonce, nil when the hub
// had none, or an error. connected is called as soon as the hub accepted
// the poll (headers received), before the long wait for the body.
func (c *Client) poll(ctx context.Context, connected func()) (*Request, string, error) {
	pctx, cancel := context.WithTimeout(ctx, c.cfg.PollTimeout)
	defer cancel()
	req, err := c.newRequest(pctx, http.MethodPost, PathPoll, nil)
	if err != nil {
		return nil, "", err
	}
	rq, err := c.pollAnswer(req, connected)
	return rq, req.Header.Get(HeaderNonce), err
}

func (c *Client) pollAnswer(req *http.Request, connected func()) (*Request, error) {
	resp, err := c.doTimed(req)
	if err != nil {
		return nil, cleanError(err)
	}
	defer resp.Body.Close()
	if h := resp.Header.Get(HeaderUpdateHint); h != "" && c.cfg.OnHint != nil {
		if prev, _ := c.lastHint.Load().(string); prev != h {
			c.lastHint.Store(h)
			c.cfg.OnHint(h)
		}
	}
	switch resp.StatusCode {
	case http.StatusNoContent:
		connected()
		return nil, nil
	case http.StatusOK:
		connected()
	default:
		return nil, c.hubError(resp.StatusCode, "a poll")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEnvelope))
	if err != nil {
		return nil, cleanError(err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil // the wait ended without a request
	}
	var rq Request
	if err := json.Unmarshal(data, &rq); err != nil {
		return nil, fmt.Errorf("hub sent a malformed request: %w", err)
	}
	if rq.ID == "" {
		return nil, errors.New("hub sent a request without id")
	}
	return &rq, nil
}

// doTimed sends req with a fresh nonce and takes the hub's signed time
// from the answer (Verifier.Time), and the config version it announces
// when the hub signed it (OnConfigVersion).
func (c *Client) doTimed(req *http.Request) (*http.Response, error) {
	var nonce [16]byte
	_, _ = crand.Read(nonce[:])
	req.Header.Set(HeaderNonce, hex.EncodeToString(nonce[:]))
	sent := c.verify.now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if h := resp.Header.Get(HeaderHubTime); h != "" {
		err := c.verify.Time(req.Header.Get(HeaderNonce), h, resp.Header.Get(HeaderConfigVersion), sent)
		c.badKey.Store(errors.Is(err, ErrBadSignature))
		switch {
		case errors.Is(err, ErrBadSignature):
			c.refused.Add(1)
			c.log.Error("the hub's time refused: not signed by the hub for this gateway", "error", err)
		case err != nil:
			c.log.Debug("the hub's time not taken", "error", err)
		}
		n, perr := strconv.ParseInt(resp.Header.Get(HeaderConfigVersion), 10, 64)
		if (err == nil || errors.Is(err, ErrSlowTime)) && perr == nil && n >= 0 && c.cfg.OnConfigVersion != nil {
			c.cfg.OnConfigVersion(n)
		}
	}
	return resp, nil
}

// MaxConfigBody bounds the club configuration of an installation.
const MaxConfigBody = 8 << 20

// FetchConfig pulls the installation's club configuration from the hub.
func (c *Client) FetchConfig(ctx context.Context) (RemoteConfig, error) {
	var out RemoteConfig
	req, err := c.newRequest(ctx, http.MethodGet, PathConfig, nil)
	if err != nil {
		return out, err
	}
	status, err := exchange(c.http.Do, req, MaxConfigBody, &out)
	switch {
	case status == 0:
		return out, err
	case status != http.StatusOK:
		return out, c.hubError(status, "a config request")
	case err != nil:
		return RemoteConfig{}, errors.New("hub sent a malformed config")
	}
	if err := c.verify.Config(&out); err != nil {
		if !errors.Is(err, ErrStaleConfig) {
			c.refused.Add(1)
			c.log.Error("club config refused: not signed by the hub for this gateway", "error", err)
		}
		return RemoteConfig{}, err
	}
	return out, nil
}

// Release tells the hub that this copy has stopped, so the copy started
// after it (a restart, an update) is served at once rather than refused
// as a duplicate. Call it after Run has returned.
func (c *Client) Release(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodPost, PathRelease, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return cleanError(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return answerError(resp.StatusCode, "a release")
	}
	return nil
}

func (c *Client) reply(ctx context.Context, r Response) {
	body, err := json.Marshal(r)
	if err != nil {
		return
	}
	for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, err := c.newRequest(rctx, http.MethodPost, PathReply, body)
		if err != nil {
			cancel()
			return
		}
		resp, err := c.http.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			cancel()
			if core.IsRedirect(resp.StatusCode) {
				break // not delivered, and a retry would not be either
			}
			if resp.StatusCode < 500 {
				return // delivered, or the partner is gone (410)
			}
		} else {
			cancel()
		}
		if !sleep(ctx, time.Duration(200*(attempt+1))*time.Millisecond) {
			return
		}
	}
	c.log.Warn("could not deliver a reply to the hub", "request_id", r.ID)
}

// cleanError keeps the cause of a transport error; the URL in *url.Error
// is the hub address, which is not secret, but the cause is what helps.
func cleanError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, ue.URL, ue.Err)
	}
	return err
}

func allowed(method, p string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	if p == "" || path.Clean(p) != p || strings.Contains(p, "%") {
		return false
	}
	if p == PathDiscover {
		return method == http.MethodPost
	}
	return strings.HasPrefix(p, "/v1/")
}

// scopeAllows: a partner reads with read, writes with book (as the hub's
// auth.Authenticator decides).
func scopeAllows(scopes []string, method string) bool {
	need := string(auth.ScopeBook)
	if method == http.MethodGet || method == http.MethodHead {
		need = string(auth.ScopeRead)
	}
	return slices.Contains(scopes, need)
}

// Execute runs one tunnelled request against the local handler, if the
// hub signed it for this gateway and for the poll that sent nonce.
// Exposed for tests.
func (c *Client) Execute(ctx context.Context, rq Request, nonce string) Response {
	fail := func(status int, code, msg string) Response {
		b, _ := json.Marshal(map[string]string{"code": code, "message": msg, "request_id": rq.RequestID})
		return Response{ID: rq.ID, Status: status, Header: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: b}
	}
	err := c.verify.Request(&rq, nonce)
	if errors.Is(err, ErrClockUnknown) {
		r := fail(http.StatusServiceUnavailable, "CLOCK_UNKNOWN", "the club's gateway does not know the hub's time yet, retry")
		r.Header["Retry-After"] = "5"
		return r
	}
	if err != nil {
		c.refused.Add(1)
		c.log.Error("request refused: not signed by the hub for this gateway", "request_id", rq.RequestID, "error", err)
		return fail(http.StatusBadGateway, "SIGNATURE_REJECTED", "the club's gateway did not accept the hub's signature")
	}
	if rq.Partner != "" && !scopeAllows(rq.Scopes, rq.Method) {
		return fail(http.StatusForbidden, "FORBIDDEN", "the API key has no scope for this operation")
	}
	// the gateway API (/gateway/...) is the hub operator's, never a partner's
	if !allowed(rq.Method, rq.Path) || (rq.Partner != "" && strings.HasPrefix(rq.Path, "/gateway/")) {
		return fail(http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	}
	if len(rq.Body) > MaxRequestBody {
		return fail(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body is too large")
	}
	rctx, cancel := context.WithTimeout(ctx, ExecTimeout(rq.TimeoutMS))
	defer cancel()
	if rq.Partner != "" {
		rctx = auth.WithPartnerName(rctx, rq.Partner)
	}
	if rq.RequestID != "" {
		rctx = core.WithRequestID(rctx, rq.RequestID)
	}
	u := &url.URL{Path: rq.Path, RawQuery: rq.Query}
	hr, err := http.NewRequestWithContext(rctx, rq.Method, u.RequestURI(), bytes.NewReader(rq.Body))
	if err != nil {
		return fail(http.StatusBadRequest, "INVALID_REQUEST", "malformed request")
	}
	hr.RemoteAddr = "hub"
	for _, h := range RequestHeaders {
		if v := rq.Header[h]; v != "" {
			hr.Header.Set(h, v)
		}
	}
	rec := &recorder{header: http.Header{}, limit: MaxResponseBody}
	c.cfg.Handler.ServeHTTP(rec, hr)
	if rec.overflow {
		return fail(http.StatusBadGateway, "RESPONSE_TOO_LARGE", "the gateway response is too large")
	}
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	out := Response{ID: rq.ID, Status: rec.status, Body: rec.buf.Bytes(), Header: map[string]string{}}
	for _, h := range ResponseHeaders {
		if v := rec.header.Get(h); v != "" {
			out.Header[h] = v
		}
	}
	return out
}

// recorder is a bounded http.ResponseWriter.
type recorder struct {
	header   http.Header
	status   int
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.buf.Len()+len(b) > r.limit {
		r.overflow = true
		return 0, errors.New("response too large")
	}
	return r.buf.Write(b)
}
