package viva

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	// calls fail fast with that error. Any refusal of the login other than
	// a rejection is retried like an outage.
	loginRetry    = 5 * time.Second
	loginRetryMax = time.Minute
	// rejectedOutageRetry: a retry of a rejected login that met another
	// failure is not tried again before this.
	rejectedOutageRetry = time.Hour
	// restartRetryAfter: once the retries after a rejection are spent, a
	// restart of the gateway may try one login, if the last was this long
	// ago; rejectedCap bounds all rejected logins in a row, restarts
	// included, far below a lockout threshold (Keycloak's default: 30).
	restartRetryAfter = 4 * time.Hour
	rejectedCap       = 8
)

// rejectedRetries: a rejected login (Keycloak's invalid_grant: a wrong
// password, but also a lock or a limit that passes by itself) is tried
// again only these long after the first rejection, then not at all until
// the password is set again (setPassword: at once). The count is kept
// across restarts (driver.RejectionStore).
var rejectedRetries = []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour}

// maxBody caps how much of an upstream response is read.
const maxBody = 8 << 20

// client talks HTTP to VivaCRM. One client serves the installation's
// account and all its clubs. It keeps only the access token (in memory,
// until shortly before it expires), the refresh token of the same session
// (to renew the access token within the session, and to end it) and how
// its last login failed. The installation holds one Keycloak session: an
// expired access token is renewed with the refresh token, and only when
// the CRM refuses that is the session ended and a password login made.
type client struct {
	authURL   string
	logoutURL string // empty: no logout endpoint known (logoutURLOf)
	apiURL    string
	username  string
	password  string
	http      *http.Client
	now       func() time.Time
	log       *slog.Logger
	store     driver.RejectionStore // nil: rejections are not kept

	mu         sync.Mutex
	token      string
	expires    time.Time
	refresh    string    // of the session the last login opened
	refreshEnd time.Time // when refresh expires (zero: not said)
	earlyTried string    // the refresh token an early renewal failed with
	session    bool      // a login succeeded since the last endSession
	rejected   error     // the CRM rejected the login: no login before retryAt
	rej        driver.Rejections
	stopped    bool      // after rejected: no automatic login at all
	degraded   bool      // the store cannot count logins: none retried after a rejection
	reserved   bool      // the login being sent is counted in the store
	staleStore bool      // the store holds rejections of other credentials
	loginErr   error     // the last failed login, another failure
	failures   int       // logins failed in a row, rejections aside
	retryAt    time.Time // no login before this after rejected or loginErr
	// the studio list of one evaluation (studioList)
	studiosOf context.Context
	studios   *studioRead
}

type studioRead struct {
	once sync.Once
	list []vivaStudio
	err  error
}

type tokenResponse struct {
	AccessToken      string   `json:"access_token"`
	ExpiresIn        *float64 `json:"expires_in"`
	RefreshToken     string   `json:"refresh_token"`
	RefreshExpiresIn float64  `json:"refresh_expires_in"`
}

// accessToken returns a cached token, renews it within the session or logs
// in, but not before retryAt after a failed login, nor at all once the
// retries after a rejection are spent: then the error is returned at once
// instead. A rejected login stays the error until a login passes, even
// when a retry of it meets another failure. A refresh token about to
// expire is renewed early, while the access token still serves.
func (c *client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && now.Before(c.expires) {
		if c.refresh != "" && c.earlyTried != c.refresh && !c.refreshEnd.IsZero() &&
			!now.Before(c.refreshEnd.Add(-tokenSafety)) && now.Before(c.refreshEnd) {
			if tok, err := c.renewLocked(ctx); err == nil {
				return tok, nil
			}
			c.earlyTried = c.refresh // the access token serves until it expires
		}
		return c.token, nil
	}
	switch {
	case c.rejected != nil && (c.stopped || now.Before(c.retryAt)):
		return "", c.rejected
	case now.Before(c.retryAt):
		return "", c.loginErr
	}
	// the session's refresh token is kept until it expires, so it can be
	// renewed or, failing that, ended before a login opens another
	if c.refresh != "" && (c.refreshEnd.IsZero() || now.Before(c.refreshEnd)) {
		tok, err := c.renewLocked(ctx)
		var ue *driver.UpstreamError
		switch {
		case err == nil:
			c.loginErr, c.failures, c.retryAt = nil, 0, time.Time{}
			return tok, nil
		case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ue) && ue.Kind == driver.UpstreamUnavailable:
			// the CRM not reached: the session may well be there, renew later
			if ctx.Err() == nil {
				c.failures++
				c.loginErr, c.retryAt = err, now.Add(min(loginRetry<<min(c.failures-1, 10), loginRetryMax))
			}
			return "", err
		}
		// refused (the session expired or was ended): no word on the
		// password; end what may be left of the session, then log in
		c.log.Info("the CRM did not renew the session: logging in again", "error", err.Error())
	}
	if c.refresh != "" { // whatever is left of the session goes before the next
		if err := c.logout(ctx, c.refresh); err != nil {
			c.log.Warn("the previous CRM session could not be ended: it expires by itself", "error", err.Error())
		}
	}
	c.refresh, c.refreshEnd = "", time.Time{}
	if !c.reserveLocked(now) {
		return "", c.rejected
	}
	tok, err := c.loginLocked(ctx)
	reserved := c.reserved
	c.reserved = false
	var ue *driver.UpstreamError
	switch {
	case err == nil:
		if c.rejected != nil {
			c.log.Info("the CRM accepted the login again", "rejected_logins", c.rej.Count)
		}
		c.clearRejectionsLocked()
		c.loginErr, c.failures, c.retryAt = nil, 0, time.Time{}
	case errors.As(err, &ue) && ue.Kind == driver.UpstreamAuth:
		if !reserved { // counted here: no store counted it before
			if c.rej.Count == 0 {
				c.rej.First = now
			}
			c.rej.Key, c.rej.Count, c.rej.Last = c.credentialKey(), c.rej.Count+1, now
		}
		c.rejected = err
		c.scheduleLocked(false)
		if c.stopped {
			c.log.Error("the CRM rejected the login: no more logins until set-password", "error", ue.Detail, "rejected_logins", c.rej.Count)
		} else {
			c.log.Error("the CRM rejected the login: it is tried again at next_login, or at once after set-password",
				"error", ue.Detail, "rejected_logins", c.rej.Count, "next_login", c.retryAt.UTC().Format(time.RFC3339))
		}
	case c.rejected != nil: // the retry of a rejected login met another failure
		if reserved {
			c.releaseLocked()
		}
		c.retryAt = now.Add(rejectedOutageRetry)
		return "", c.rejected
	default:
		if reserved {
			c.releaseLocked()
		}
		if ctx.Err() == nil {
			c.failures++
			c.loginErr, c.retryAt = err, now.Add(min(loginRetry<<min(c.failures-1, 10), loginRetryMax))
		}
	}
	return tok, err
}

// scheduleLocked sets when a rejected login is tried again: rejectedRetries
// after the first rejection, then never (stopped); at a start (restart)
// with those spent, once at once if the last was restartRetryAfter ago,
// up to rejectedCap rejections in a row.
func (c *client) scheduleLocked(restart bool) {
	n := c.rej.Count
	c.stopped = false
	switch {
	case c.degraded: // no store counts the logins: none tried again by itself
		c.retryAt, c.stopped = time.Time{}, true
	case n <= len(rejectedRetries):
		c.retryAt = c.rej.First.Add(rejectedRetries[n-1])
	case restart && n < rejectedCap && !c.now().Before(c.rej.Last.Add(restartRetryAfter)):
		c.retryAt = c.now()
	default:
		c.retryAt, c.stopped = time.Time{}, true
	}
}

// loadRejections takes up the rejections a previous run of the gateway
// kept for these credentials.
func (c *client) loadRejections() {
	if c.store == nil {
		return
	}
	r, err := c.store.LoadRejections()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.degraded = true
		c.log.Error("the CRM login attempts kept in the state directory cannot be read: a rejected login will not be tried again by itself", "error", err)
		return
	}
	if r.Count <= 0 || r.Key != c.credentialKey() {
		c.staleStore = r.Count > 0
		return
	}
	c.rej = r
	c.rejected = driver.Upstream(driver.UpstreamAuth, fmt.Sprintf("login rejected %d times in a row (kept since %s)", r.Count, r.Last.UTC().Format(time.RFC3339)))
	c.scheduleLocked(true)
}

// reserveLocked counts a password login in the store before it is sent,
// so neither a crash nor a lost answer frees it; false when it must not be
// sent: the store cannot count it and a login was rejected before.
func (c *client) reserveLocked(now time.Time) bool {
	if c.store == nil {
		return true // nothing kept (the commands): counted after the answer
	}
	if !c.degraded {
		next := c.rej
		if next.Count == 0 {
			next.First = now
		}
		next.Key, next.Count, next.Last = c.credentialKey(), next.Count+1, now
		err := c.store.SaveRejections(next)
		if err == nil {
			c.rej, c.reserved = next, true
			return true
		}
		c.degraded = true
		c.log.Error("the CRM login attempts cannot be kept in the state directory: a rejected login will not be tried again by itself", "error", err)
	}
	return c.rejected == nil
}

// releaseLocked takes back a reserved login that ended neither accepted
// nor rejected (the CRM not reached, another refusal).
func (c *client) releaseLocked() {
	if c.rej.Count--; c.rej.Count <= 0 {
		c.rej = driver.Rejections{}
	}
	c.saveRejectionsLocked()
}

func (c *client) saveRejectionsLocked() {
	if c.store == nil {
		return
	}
	if err := c.store.SaveRejections(c.rej); err != nil {
		c.log.Warn("the CRM logins rejected could not be kept: a restart counts from none", "error", err)
	}
}

func (c *client) clearRejectionsLocked() {
	had := c.rej.Count > 0 || c.staleStore
	c.rejected, c.rej, c.stopped, c.staleStore = nil, driver.Rejections{}, false, false
	if had {
		c.saveRejectionsLocked()
	}
}

// credentialKey names the credentials rejections are about: a hash of the
// token endpoint, the username and the password as a JSON array (no two
// differ only in where one field ends), never the password itself.
func (c *client) credentialKey() string {
	fields, _ := json.Marshal([]string{"court-gateway crm login", c.authURL, c.username, c.password})
	sum := sha256.Sum256(fields)
	return hex.EncodeToString(sum[:16])
}

// setPassword replaces the password, as set-password writes it to .env,
// and ends the wait after any failed login: the next call logs in, or
// renews the session it still has.
func (c *client) setPassword(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.password, c.token = p, ""
	c.rejected, c.rej, c.stopped = nil, driver.Rejections{}, false
	c.saveRejectionsLocked() // none kept, whatever credentials they were about
	c.loginErr, c.failures, c.retryAt = nil, 0, time.Time{}
}

// rejectedPassword reports whether logins are stopped after the CRM
// rejected the password.
func (c *client) rejectedPassword() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejected != nil
}

// nextLogin is when a rejected login is tried again (zero when none was
// rejected, or when none will be until the password is set again).
func (c *client) nextLogin() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejected == nil || c.stopped {
		return time.Time{}
	}
	return c.retryAt
}

// studioList returns the account's studios: read once per evaluation
// (driver.Evaluation), under its context but within the time limit of the
// club that reads it, so the clubs of one readiness round or one config
// share it and a hanging read does not hold the round; never used for
// another evaluation. Outside an evaluation it reads with ctx.
func (c *client) studioList(ctx context.Context) ([]vivaStudio, error) {
	ev := driver.Evaluation(ctx)
	if ev == nil {
		return listStudios(ctx, c)
	}
	c.mu.Lock()
	if c.studiosOf != ev {
		c.studiosOf, c.studios = ev, &studioRead{}
	}
	r := c.studios
	c.mu.Unlock()
	r.once.Do(func() {
		rctx := ev
		if d, ok := ctx.Deadline(); ok {
			var cancel context.CancelFunc
			rctx, cancel = context.WithDeadline(ev, d)
			defer cancel()
		}
		r.list, r.err = listStudios(rctx, c)
	})
	return r.list, r.err
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
	return c.tokenLocked(ctx, "login", url.Values{
		"grant_type": {"password"},
		"client_id":  {ClientID},
		"username":   {c.username},
		"password":   {c.password},
	})
}

// renewLocked renews the access token within the session (the refresh
// grant). Its refusal says nothing about the password: an invalid_grant
// here is an expired or ended session.
func (c *client) renewLocked(ctx context.Context) (string, error) {
	return c.tokenLocked(ctx, "session renewal", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {c.refresh},
	})
}

// tokenLocked asks the token endpoint (op names the grant in errors) and
// keeps the tokens of the answer.
func (c *client) tokenLocked(ctx context.Context, op string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", driver.Upstream(driver.UpstreamUnavailable, "build "+op+" request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", networkError(op, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	switch {
	case resp.StatusCode == http.StatusOK:
	case core.IsRedirect(resp.StatusCode):
		return "", redirected(op, resp.StatusCode)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		var kc struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(body, &kc) == nil && kc.Error == "invalid_grant" {
			// Keycloak's reason, as a fixed label: its text is never
			// passed on (an upstream may echo a token in it)
			return "", driver.Upstream(driver.UpstreamAuth, fmt.Sprintf("%s rejected with HTTP %d (invalid_grant: %s)", op, resp.StatusCode, rejectionLabel(kc.Description)))
		}
		return "", driver.Upstream(driver.UpstreamLogin, fmt.Sprintf("%s refused with HTTP %d", op, resp.StatusCode))
	default:
		return "", driver.Upstream(driver.UpstreamUnavailable, fmt.Sprintf("%s answered HTTP %d", op, resp.StatusCode))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" || tr.ExpiresIn == nil {
		return "", driver.Upstream(driver.UpstreamInvalid, op+" response has no access_token/expires_in")
	}
	now := c.now()
	c.token, c.session = tr.AccessToken, true
	c.expires = now.Add(time.Duration(*tr.ExpiresIn*float64(time.Second)) - tokenSafety)
	if tr.RefreshToken != "" || form.Get("grant_type") == "password" {
		c.refresh, c.refreshEnd = tr.RefreshToken, time.Time{}
		if tr.RefreshExpiresIn > 0 { // its real end: renewed early before it (accessToken)
			c.refreshEnd = now.Add(time.Duration(tr.RefreshExpiresIn * float64(time.Second)))
		}
	}
	return c.token, nil
}

// endSession ends the Keycloak session of the last login, if one is open,
// and forgets its tokens: an OpenID Connect logout with the login's
// refresh token and the client id, at the logout endpoint next to the
// token endpoint. Keycloak ends a session only by its refresh token (or an
// id token, which a password login without the openid scope does not get);
// revoking the access token would not end the session. So a login answered
// without a refresh token leaves a session that only expires. The error
// never holds a token.
func (c *client) endSession(ctx context.Context) error {
	c.mu.Lock()
	open, refresh := c.session, c.refresh
	c.token, c.refresh, c.session = "", "", false
	c.mu.Unlock()
	switch {
	case !open:
		return nil
	case refresh == "":
		return errors.New("the login answer had no refresh_token, so the session cannot be ended and expires by itself")
	case c.logoutURL == "":
		return errors.New("auth_url does not end in /token, so no logout endpoint is known and the session expires by itself")
	}
	return c.logout(ctx, refresh)
}

// logout ends the session of refresh (see endSession).
func (c *client) logout(ctx context.Context, refresh string) error {
	if c.logoutURL == "" {
		return errors.New("no logout endpoint known")
	}
	form := url.Values{"client_id": {ClientID}, "refresh_token": {refresh}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.logoutURL, strings.NewReader(form.Encode()))
	if err != nil {
		return driver.Upstream(driver.UpstreamUnavailable, "build logout request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return networkError("logout", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("logout answered HTTP %d", resp.StatusCode))
	}
	return nil
}

// logoutURLOf is the OpenID Connect logout endpoint next to a token
// endpoint (Keycloak: .../protocol/openid-connect/token and .../logout);
// empty when the token endpoint's path does not end in /token.
func logoutURLOf(authURL string) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return ""
	}
	base, ok := strings.CutSuffix(u.Path, "/token")
	if !ok {
		return ""
	}
	u.Path, u.RawPath, u.RawQuery = base+"/logout", "", ""
	return u.String()
}

// rejectionLabel names Keycloak's error_description of an invalid_grant
// with a fixed label; any other text is "other".
func rejectionLabel(d string) string {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "invalid user credentials":
		return "invalid_credentials"
	case "account disabled", "user is disabled":
		return "account_disabled"
	case "account temporarily disabled":
		return "account_temporarily_locked"
	case "account is not fully set up":
		return "account_not_set_up"
	case "session not active", "token is not active", "invalid refresh token", "stale token", "session doesn't have required client":
		return "session_not_active"
	}
	return "other"
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
