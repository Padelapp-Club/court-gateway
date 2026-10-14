package viva

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

const (
	// tokenSafety is subtracted from the token lifetime so a token is
	// never used in its last seconds.
	tokenSafety = 30 * time.Second
	// After a login that failed for another reason than the password (the
	// CRM unreachable, 5xx, a timeout) the next attempt waits loginRetry,
	// doubling with every failure in a row up to loginRetryMax; until then
	// calls fail fast with that error. A rejected login (Keycloak's
	// invalid_grant: a wrong password) is not tried again at all until the
	// password changes (setPassword), so a wrong password cannot get the
	// account locked; any other refusal of the login is retried.
	loginRetry    = 5 * time.Second
	loginRetryMax = time.Minute
)

// maxBody caps how much of an upstream response is read.
const maxBody = 8 << 20

// client talks HTTP to VivaCRM. One client serves the installation's
// account and all its clubs. It keeps only the access token (in memory,
// until shortly before it expires) and how its last login failed.
type client struct {
	authURL  string
	apiURL   string
	username string
	password string
	http     *http.Client
	now      func() time.Time

	mu       sync.Mutex
	token    string
	expires  time.Time
	rejected error     // the CRM rejected the password: no login until it changes
	loginErr error     // the last failed login, another failure
	failures int       // logins failed in a row, rejections aside
	retryAt  time.Time // no login before this after loginErr
}

type tokenResponse struct {
	AccessToken string   `json:"access_token"`
	ExpiresIn   *float64 `json:"expires_in"`
}

// accessToken returns a cached token or logs in. After a rejected login
// it never logs in again (until setPassword); after another failure not
// before retryAt. Either way the error is returned at once instead.
func (c *client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	switch {
	case c.token != "" && now.Before(c.expires):
		return c.token, nil
	case c.rejected != nil:
		return "", c.rejected
	case now.Before(c.retryAt):
		return "", c.loginErr
	}
	tok, err := c.loginLocked(ctx)
	var ue *driver.UpstreamError
	switch {
	case err == nil:
		c.loginErr, c.failures, c.retryAt = nil, 0, time.Time{}
	case errors.As(err, &ue) && ue.Kind == driver.UpstreamAuth:
		c.rejected = err
	case ctx.Err() == nil:
		c.failures++
		c.loginErr, c.retryAt = err, now.Add(min(loginRetry<<min(c.failures-1, 10), loginRetryMax))
	}
	return tok, err
}

// setPassword replaces the password, as set-password writes it to .env,
// and ends the wait after any failed login: the next call logs in.
func (c *client) setPassword(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.password, c.token = p, ""
	c.rejected, c.loginErr, c.failures, c.retryAt = nil, nil, 0, time.Time{}
}

// rejectedPassword reports whether logins are stopped after the CRM
// rejected the password.
func (c *client) rejectedPassword() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejected != nil
}

// studioList returns the account's studios: read once per evaluation
// (driver.Shared), so the clubs of one readiness round or one config share
// it, and never reused after it.
func (c *client) studioList(ctx context.Context) ([]vivaStudio, error) {
	return driver.Shared(ctx, func(ctx context.Context) ([]vivaStudio, error) { return listStudios(ctx, c) })
}

// invalidate drops the cached token if it is still the one that failed.
func (c *client) invalidate(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == tok {
		c.token = ""
	}
}

func (c *client) loginLocked(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {ClientID},
		"username":   {c.username},
		"password":   {c.password},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", driver.Upstream(driver.UpstreamUnavailable, "build login request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", networkError("login", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	switch {
	case resp.StatusCode == http.StatusOK:
	case core.IsRedirect(resp.StatusCode):
		return "", redirected("login", resp.StatusCode)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		var kc struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &kc) == nil && kc.Error == "invalid_grant" {
			return "", driver.Upstream(driver.UpstreamAuth, fmt.Sprintf("login rejected with HTTP %d (invalid_grant)", resp.StatusCode))
		}
		return "", driver.Upstream(driver.UpstreamLogin, fmt.Sprintf("login refused with HTTP %d", resp.StatusCode))
	default:
		return "", driver.Upstream(driver.UpstreamUnavailable, fmt.Sprintf("login answered HTTP %d", resp.StatusCode))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" || tr.ExpiresIn == nil {
		return "", driver.Upstream(driver.UpstreamInvalid, "login response has no access_token/expires_in")
	}
	c.token = tr.AccessToken
	c.expires = c.now().Add(time.Duration(*tr.ExpiresIn*float64(time.Second)) - tokenSafety)
	return c.token, nil
}

func networkError(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", op, context.DeadlineExceeded)
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return fmt.Errorf("%s: %w", op, context.DeadlineExceeded)
	}
	// The url.Error text holds the request URL; it carries no secrets
	// (credentials go in the body or a header), but keep only the op.
	return driver.Upstream(driver.UpstreamUnavailable, op+": network error")
}

// redirected is the error for a 3xx answer: redirects are not followed
// (core.RefuseRedirects).
func redirected(op string, status int) error {
	return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("%s answered with a redirect (HTTP %d), which is not followed", op, status))
}

// do sends an authenticated API request. A 401 drops the token, logs in
// again and retries exactly once. 5xx and network failures become
// UpstreamUnavailable; every other status is returned to the caller.
func (c *client) do(ctx context.Context, method, path string, query url.Values, payload any) (int, []byte, error) {
	var raw []byte
	if payload != nil {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			return 0, nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return 0, nil, err
		}
		u := c.apiURL + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		var body io.Reader
		if raw != nil {
			body = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, body)
		if err != nil {
			return 0, nil, driver.Upstream(driver.UpstreamUnavailable, "build request")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if raw != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, nil, networkError(method+" "+pathTemplate(path), err)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if rerr != nil {
			return 0, nil, networkError(method+" "+pathTemplate(path), rerr)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			c.invalidate(tok)
			if attempt == 0 {
				continue
			}
			return 0, nil, driver.Upstream(driver.UpstreamAuth, method+" "+pathTemplate(path)+": token rejected after re-login")
		}
		if core.IsRedirect(resp.StatusCode) {
			return 0, nil, redirected(method+" "+pathTemplate(path), resp.StatusCode)
		}
		if resp.StatusCode >= 500 {
			return 0, nil, driver.Upstream(driver.UpstreamUnavailable,
				fmt.Sprintf("%s %s answered HTTP %d", method, pathTemplate(path), resp.StatusCode))
		}
		return resp.StatusCode, data, nil
	}
}

// pathTemplate removes ids from a path for logs: /exercises/<id> -> /exercises/{id}.
func pathTemplate(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if uuidRe.MatchString(s) {
			parts[i] = "{id}"
		}
	}
	return strings.Join(parts, "/")
}
