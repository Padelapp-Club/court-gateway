package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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
	// Polls is the number of concurrent long-poll requests (default 4).
	Polls int
	// Handler serves requests that arrive through the tunnel.
	Handler http.Handler
	// Version, Build and Platform are reported to the hub on every poll.
	Version, Build, Platform string
	Logger                   *slog.Logger
	// OnHint is called when the hub says a newer release exists.
	OnHint func(version string)
	// OnConfigVersion is called with the hub's club configuration version
	// from every poll and heartbeat answer that carries one.
	OnConfigVersion func(version int64)
	// Transport defaults to NewTransport(nil).
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
	okPolls  atomic.Int32
	lastHint atomic.Value // string
	authFail atomic.Bool
	mu       sync.Mutex
	state    string // "connected" / "disconnected", for transition logs
}

// ErrUnauthorized means the hub rejected the gateway secret.
var ErrUnauthorized = errors.New("the hub rejected the gateway secret")

// LoadRoots returns the system root pool plus the certificates in caFile.
func LoadRoots(caFile string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile == "" {
		return pool, nil
	}
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
func NewTransport(roots *x509.CertPool) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
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
		cfg.Transport = NewTransport(nil)
	}
	if cfg.Build == "" {
		cfg.Build = BuildSelf
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{cfg: cfg, http: &http.Client{Transport: cfg.Transport}, log: log}, nil
}

// Connected reports whether at least one poll is currently healthy.
func (c *Client) Connected() bool { return c.okPolls.Load() > 0 }

// Unauthorized reports whether the hub's last answer rejected the secret.
func (c *Client) Unauthorized() bool { return c.authFail.Load() }

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
		if errors.Is(err, ErrUnauthorized) {
			c.log.Error("hub rejected the gateway secret; run setup again with a new join code", "hub", c.cfg.HubURL)
		} else {
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
		rq, err := c.poll(ctx, func() {
			failures = 0
			c.authFail.Store(false)
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
		resp := c.Execute(ctx, *rq)
		c.reply(ctx, resp)
	}
}

// backoff returns a full-jitter delay: uniform in [0, min(max, min*2^n)).
// A rejected secret waits the maximum: retrying faster cannot help.
func (c *Client) backoff(failures int, unauthorized bool) time.Duration {
	ceil := c.cfg.MaxBackoff
	if !unauthorized {
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// poll returns the next request, nil when the hub had none, or an error.
// connected is called as soon as the hub accepted the poll (headers
// received), before the long wait for the body.
func (c *Client) poll(ctx context.Context, connected func()) (*Request, error) {
	pctx, cancel := context.WithTimeout(ctx, c.cfg.PollTimeout)
	defer cancel()
	req, err := c.newRequest(pctx, http.MethodPost, PathPoll, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, cleanError(err)
	}
	defer resp.Body.Close()
	c.noteConfigVersion(resp.Header)
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
	case http.StatusUnauthorized:
		c.authFail.Store(true)
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("hub answered HTTP %d to a poll", resp.StatusCode)
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

func (c *Client) noteConfigVersion(h http.Header) {
	if c.cfg.OnConfigVersion == nil {
		return
	}
	v := h.Get(HeaderConfigVersion)
	if v == "" {
		return
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
		c.cfg.OnConfigVersion(n)
	}
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
	resp, err := c.http.Do(req)
	if err != nil {
		return out, cleanError(err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		c.authFail.Store(true)
		return out, ErrUnauthorized
	default:
		return out, fmt.Errorf("hub answered HTTP %d to a config request", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxConfigBody)).Decode(&out); err != nil {
		return RemoteConfig{}, errors.New("hub sent a malformed config")
	}
	return out, nil
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

// Execute runs one tunnelled request against the local handler. Exposed
// for tests.
func (c *Client) Execute(ctx context.Context, rq Request) Response {
	fail := func(status int, code, msg string) Response {
		b, _ := json.Marshal(map[string]string{"code": code, "message": msg, "request_id": rq.RequestID})
		return Response{ID: rq.ID, Status: status, Header: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: b}
	}
	if !allowed(rq.Method, rq.Path) {
		return fail(http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	}
	if len(rq.Body) > MaxRequestBody {
		return fail(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body is too large")
	}
	timeout := min(max(time.Duration(rq.TimeoutMS)*time.Millisecond, time.Second), MaxRequestTimeout)
	if rq.TimeoutMS <= 0 {
		timeout = 30 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
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
