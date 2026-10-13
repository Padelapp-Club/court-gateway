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
	// loginRetry: after a failed login the next attempt waits this long;
	// until then every call fails fast with the same error.
	loginRetry = time.Minute
	// authFailureTTL: a rejected login (wrong password) is not retried for
	// this long, so a wrong password cannot get the account locked.
	authFailureTTL = 10 * time.Minute
)

// maxBody caps how much of an upstream response is read.
const maxBody = 8 << 20

// client talks HTTP to VivaCRM. One client serves the installation's
// account and all its clubs. It keeps only the access token (in memory,
// until shortly before it expires) and the last login failure.
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
	loginErr error     // the last failed login
	retryAt  time.Time // no login before this
}

type tokenResponse struct {
	AccessToken string   `json:"access_token"`
	ExpiresIn   *float64 `json:"expires_in"`
}

// accessToken returns a cached token or logs in. A failed login is not
// repeated before retryAt: the error is returned at once instead.
func (c *client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && now.Before(c.expires) {
		return c.token, nil
	}
	if now.Before(c.retryAt) {
		return "", c.loginErr
	}
	tok, err := c.loginLocked(ctx)
	if err != nil && ctx.Err() == nil {
		wait := loginRetry
		var ue *driver.UpstreamError
		if errors.As(err, &ue) && ue.Kind == driver.UpstreamAuth {
			wait = authFailureTTL
		}
		c.loginErr, c.retryAt = err, now.Add(wait)
	}
	return tok, err
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
		return "", driver.Upstream(driver.UpstreamAuth, fmt.Sprintf("login rejected with HTTP %d", resp.StatusCode))
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
