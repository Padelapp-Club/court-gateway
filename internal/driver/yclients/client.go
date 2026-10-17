package yclients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

const (
	accept = "application/vnd.yclients.v2+json"
	// requestTimeout bounds one request (the D11 adapter's 20 s).
	requestTimeout = 20 * time.Second
	// retry429 is how often a 429 is retried, after Retry-After (at most
	// retryAfterMax) or 1, 2, 4 s.
	retry429      = 3
	retryAfterMax = 60 * time.Second
	maxBody       = 8 << 20
	// A login YCLIENTS refused (a wrong login or password) is tried again
	// only rejectedRetry after, doubling up to rejectedRetryMax, so the
	// account is not locked; a login that failed otherwise, loginRetry
	// after, doubling up to loginRetryMax. SetPassword clears both.
	rejectedRetry    = 15 * time.Minute
	rejectedRetryMax = 4 * time.Hour
	loginRetry       = 5 * time.Second
	loginRetryMax    = time.Minute
	// reloginAfter: a user token YCLIENTS refuses is replaced by a new
	// login only when the last login is this old, so a user without the
	// rights (401 on every call) does not log in at every request.
	reloginAfter = time.Minute
)

// minInterval spaces the process's requests: YCLIENTS allows 200 a minute
// per IP for every branch together; the gateway keeps to 180.
var minInterval = time.Minute / 180

// throttle spaces every request of the process (all clients: the limit
// is the IP's).
var throttle struct {
	mu   sync.Mutex
	last time.Time
}

func wait(ctx context.Context) error {
	throttle.mu.Lock()
	next := throttle.last.Add(minInterval)
	now := time.Now()
	if next.Before(now) {
		next = now
	}
	throttle.last = next
	throttle.mu.Unlock()
	return sleep(ctx, time.Until(next))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type authKind int

const (
	partnerAuth authKind = iota // Bearer <partner>
	userAuth                    // Bearer <partner>, User <user>
)

// client talks HTTP to YCLIENTS for an installation. It keeps only the
// user token of the last login (none with a configured user token) and how
// the last login failed.
type client struct {
	apiURL  string
	partner string
	login   string
	http    *http.Client
	now     func() time.Time
	log     *slog.Logger

	mu        sync.Mutex
	fixedUser string // YCLIENTS_USER_TOKEN: never replaced
	password  string
	token     string    // from the last login
	loggedIn  time.Time // when token was issued
	rejected  error     // the last login was refused: none before retryAt
	rejects   int
	loginErr  error // the last login failed otherwise
	failures  int
	retryAt   time.Time
}

func newClient(apiURL, partner, user, login, password string, hc *http.Client, now func() time.Time, log *slog.Logger) *client {
	return &client{apiURL: apiURL, partner: partner, fixedUser: user, login: login, password: password, http: hc, now: now, log: log}
}

// response is YCLIENTS' envelope {success, data, meta} with the status.
type response struct {
	status  int
	success *bool
	data    json.RawMessage
	meta    json.RawMessage
}

type envelope struct {
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
	Meta    json.RawMessage `json:"meta"`
}

// message is meta.message of an error answer, for the log: YCLIENTS
// writes there what it refused, never the caller's data.
func (r response) message() string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(r.meta, &m) == nil && m.Message != "" {
		return m.Message
	}
	return "без текста"
}

// do sends one request. 2xx answers come back decoded (success=false is a
// rejection); 404 and other 4xx except 401/403/429 come back as they are,
// for the caller to judge; everything else is an *driver.UpstreamError.
// A user-authorized request whose token the CRM refuses logs in again once
// (login mode only).
func (c *client) do(ctx context.Context, method, path string, q url.Values, body any, auth authKind) (response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return response{}, err
		}
	}
	for relogin := auth == userAuth; ; relogin = false {
		header := "Bearer " + c.partner
		var used string
		if auth == userAuth {
			tok, err := c.userToken(ctx)
			if err != nil {
				return response{}, err
			}
			used = tok
			header += ", User " + tok
		}
		r, err := c.send(ctx, method, path, q, payload, header)
		if err != nil {
			return r, err
		}
		switch r.status {
		case http.StatusUnauthorized:
			if relogin && c.dropToken(used) {
				continue
			}
			return r, driver.Upstream(driver.UpstreamAuth, fmt.Sprintf("%s %s: HTTP 401 (key not recognized): %s", method, path, r.message()))
		case http.StatusForbidden:
			return r, driver.Upstream(driver.UpstreamAuth, fmt.Sprintf("%s %s: HTTP 403 (the application has no rights): %s", method, path, r.message()))
		}
		return r, nil
	}
}

func (c *client) send(ctx context.Context, method, path string, q url.Values, payload []byte, authorization string) (response, error) {
	u := c.apiURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	for attempt := 0; ; attempt++ {
		if err := wait(ctx); err != nil {
			return response{}, err
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return response{}, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", authorization)
		res, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return response{}, ctx.Err()
			}
			// the error names the URL, never the headers (the tokens)
			return response{}, driver.Upstream(driver.UpstreamUnavailable, fmt.Sprintf("%s %s: no answer", method, path))
		}
		data, rerr := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
		res.Body.Close()
		if rerr != nil {
			return response{}, driver.Upstream(driver.UpstreamUnavailable, fmt.Sprintf("%s %s: answer cut off", method, path))
		}
		if len(data) > maxBody {
			return response{}, driver.Upstream(driver.UpstreamInvalid, fmt.Sprintf("%s %s: answer too large", method, path))
		}
		r := response{status: res.StatusCode}
		var env envelope
		if len(bytes.TrimSpace(data)) > 0 && json.Unmarshal(data, &env) == nil {
			r.success, r.data, r.meta = env.Success, env.Data, env.Meta
		}
		switch {
		case res.StatusCode == http.StatusTooManyRequests && attempt < retry429:
			delay := time.Duration(1<<attempt) * time.Second
			if s, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && s >= 0 {
				delay = time.Duration(s) * time.Second
			}
			delay = min(delay, retryAfterMax)
			c.log.Warn("yclients rate limit, retrying", "path", path, "after", delay.String())
			if err := sleep(ctx, delay); err != nil {
				return response{}, err
			}
			continue
		case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
			return r, driver.Upstream(driver.UpstreamUnavailable, fmt.Sprintf("%s %s: HTTP %d", method, path, res.StatusCode))
		case res.StatusCode >= 200 && res.StatusCode < 300:
			if res.StatusCode != http.StatusNoContent && len(bytes.TrimSpace(data)) > 0 && r.success == nil && r.data == nil {
				return r, driver.Upstream(driver.UpstreamInvalid, fmt.Sprintf("%s %s: answer is not a YCLIENTS envelope", method, path))
			}
			if r.success != nil && !*r.success {
				return r, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("%s %s: success=false: %s", method, path, r.message()))
			}
		}
		return r, nil
	}
}

// userToken is the configured user token, or that of the last login, or a
// new login's, but not before retryAt after a failed login.
func (c *client) userToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fixedUser != "" {
		return c.fixedUser, nil
	}
	if c.token != "" {
		return c.token, nil
	}
	if c.now().Before(c.retryAt) {
		if c.rejected != nil {
			return "", c.rejected
		}
		return "", c.loginErr
	}
	tok, err := c.loginLocked(ctx)
	if err != nil {
		return "", err
	}
	c.token, c.loggedIn = tok, c.now()
	return tok, nil
}

// dropToken forgets the login's token the CRM refused, so the request is
// sent again with a new login's; false when it is not to be sent again: a
// configured token, or a login younger than reloginAfter. A token another
// request already replaced is not dropped, the request goes with the new.
func (c *client) dropToken(used string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fixedUser != "" {
		return false
	}
	if c.token != used {
		return true
	}
	if c.now().Sub(c.loggedIn) < reloginAfter {
		return false
	}
	c.token = ""
	return true
}

// loginLocked: POST /auth with the staff member's login and password,
// partner-authorized. A 401, 403 or 422 is a refusal of the credentials.
func (c *client) loginLocked(ctx context.Context) (string, error) {
	r, err := c.send(ctx, http.MethodPost, "/auth", nil, mustJSON(map[string]string{"login": c.login, "password": c.password}), "Bearer "+c.partner)
	var tok struct {
		UserToken string `json:"user_token"`
	}
	var ue *driver.UpstreamError
	switch {
	case err == nil && r.status >= 200 && r.status < 300 && json.Unmarshal(r.data, &tok) == nil && tok.UserToken != "":
		c.rejected, c.rejects, c.loginErr, c.failures, c.retryAt = nil, 0, nil, 0, time.Time{}
		c.log.Info("yclients login passed")
		return tok.UserToken, nil
	case err == nil && r.status >= 200 && r.status < 300:
		// [UNVERIFIED] how YCLIENTS answers a login that needs a second
		// factor: an answer without a token is taken as that.
		return "", c.refuse(driver.Upstream(driver.UpstreamAuth, "POST /auth: no user_token in the answer (a second factor? set "+UserTokenEnv+")"))
	case err == nil && (r.status == http.StatusUnauthorized || r.status == http.StatusForbidden || r.status == http.StatusUnprocessableEntity || r.status == http.StatusBadRequest):
		return "", c.refuse(driver.Upstream(driver.UpstreamAuth, fmt.Sprintf("POST /auth: HTTP %d: the login was refused: %s", r.status, r.message())))
	case err == nil:
		err = driver.Upstream(driver.UpstreamLogin, fmt.Sprintf("POST /auth: HTTP %d", r.status))
	case errors.As(err, &ue) && ue.Kind == driver.UpstreamRejected:
		return "", c.refuse(driver.Upstream(driver.UpstreamAuth, "POST /auth: the login was refused"))
	case errors.As(err, &ue):
		err = driver.Upstream(driver.UpstreamLogin, ue.Detail)
	}
	c.failures++
	c.loginErr = err
	c.retryAt = c.now().Add(min(loginRetry<<(c.failures-1), loginRetryMax))
	c.log.Warn("yclients login failed", "retry_in", c.retryAt.Sub(c.now()).Round(time.Second).String())
	return "", err
}

func (c *client) refuse(err error) error {
	c.rejects++
	c.rejected = err
	c.retryAt = c.now().Add(min(rejectedRetry<<(c.rejects-1), rejectedRetryMax))
	c.log.Warn("yclients login refused", "retry_in", c.retryAt.Sub(c.now()).Round(time.Second).String())
	return err
}

func (c *client) setPassword(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.password, c.token = p, ""
	c.rejected, c.rejects, c.loginErr, c.failures, c.retryAt = nil, 0, nil, 0, time.Time{}
}

func (c *client) loginRefused() (bool, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejected == nil {
		return false, time.Time{}
	}
	return true, c.retryAt
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
