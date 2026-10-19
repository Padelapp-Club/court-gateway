package fitness1c

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// apiPrefix is the path of the client API v3 under the club's instance.
const apiPrefix = "/hs/api/v3"

// maxBody caps how much of an answer is read.
const maxBody = 8 << 20

// authPause is how long no request goes out after 1C refused the user's
// login (HTTP 401 without a client token): the same login would be refused
// again, and a stream of refused logins may lock the 1C user. A new
// password (SetPassword) ends the pause at once.
const authPause = 2 * time.Minute

// Codes of 1C's application errors the driver branches on (D14 runs, §27).
const (
	codeRoomBusy        = 1060 // the room is taken at that time
	codeAlreadyCanceled = 1037 // the rental is already cancelled
)

// clientAuthCodes are the application errors after which a client token
// is issued again, once: 4003 wrong credentials, 1010 client not found.
var clientAuthCodes = map[int]bool{4003: true, 1010: true}

// apiError is 1C's application error: HTTP 400 with a numeric code. The
// message 1C adds may name a client: it is never kept.
type apiError struct {
	Code int
	up   *driver.UpstreamError
}

func (e *apiError) Error() string { return e.up.Error() }
func (e *apiError) Unwrap() error { return e.up }

func codeOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return 0
}

// envelope is every answer of 1C: {result, data} or {result: false, error}.
type envelope struct {
	Result     *bool           `json:"result"`
	Data       json.RawMessage `json:"data"`
	Error      *json.Number    `json:"error"`
	Pagination *struct {
		Total *int `json:"total_results"`
	} `json:"pagination_response"`
}

// client talks to one 1C instance with the installation's login: HTTP Basic
// of the 1C user and the application key in the apikey header, in every
// request (1C has no session). Client tokens (usertoken) of the technical
// client are kept in memory only.
type client struct {
	base     string // the instance address with apiPrefix
	username string
	apiKey   string
	phone    string // the technical client; "" when the gateway does not write
	partner  string // the partner whose bookings the technical client makes
	http     *http.Client
	log      *slog.Logger
	now      func() time.Time

	mu        sync.Mutex
	password  string
	stopUntil time.Time // no request before (a refused login)
	token     string    // the technical client's usertoken

	// the technical client of a pair's mirrors ("" when none) and its token
	mirrorPhone string
	mirrorToken string
}

func (c *client) setPassword(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.password, c.stopUntil = p, time.Time{}
}

func (c *client) stopped() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopUntil, c.now().Before(c.stopUntil)
}

// call sends one request and returns the envelope of a successful answer.
// Every failure is classified: network and 5xx are UpstreamUnavailable, a
// refused login UpstreamAuth, 1C's numeric error an *apiError, an answer
// that is not {result: true, data} UpstreamInvalid. retry repeats the
// request once when no answer came at all (a lost answer: 1C is idempotent
// per client and slot, D14 §27).
func (c *client) call(ctx context.Context, method, path string, query url.Values, body any, usertoken string, retry bool) (*envelope, error) {
	op := method + " " + path
	if until, stop := c.stopped(); stop && usertoken == "" {
		return nil, driver.Upstream(driver.UpstreamAuth, fmt.Sprintf("%s: 1C refused the login, next attempt after %s", op, until.UTC().Format(time.RFC3339)))
	}
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	send := func() (*http.Response, error) {
		var rd io.Reader
		if raw != nil {
			rd = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		req.SetBasicAuth(c.username, c.password)
		c.mu.Unlock()
		req.Header.Set("apikey", c.apiKey)
		req.Header.Set("Accept", "application/json")
		if raw != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if usertoken != "" {
			req.Header.Set("usertoken", usertoken)
		}
		return c.http.Do(req)
	}
	// the answer is only had once its body is read: a body cut off after
	// the headers is as lost as no answer
	exchange := func() (*http.Response, []byte, error) {
		resp, err := send()
		if err != nil {
			return nil, nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		return resp, data, err
	}
	resp, data, err := exchange()
	if err != nil && retry && ctx.Err() == nil {
		c.log.Warn("1C: no answer, one retry", "op", op)
		resp, data, err = exchange()
	}
	if err != nil {
		return nil, networkError(op, err)
	}
	switch s := resp.StatusCode; {
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		// IIS answers a refused Basic login with an HTML page: known by
		// the status, not the body
		if usertoken == "" {
			c.mu.Lock()
			c.stopUntil = c.now().Add(authPause)
			c.mu.Unlock()
		}
		return nil, driver.Upstream(driver.UpstreamAuth, fmt.Sprintf("%s: 1C refused the login (HTTP %d)", op, s))
	case core.IsRedirect(s):
		// the login goes nowhere but the configured instance
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("%s: 1C redirected (HTTP %d), not followed", op, s))
	}
	var env envelope
	jsonErr := json.Unmarshal(data, &env)
	if jsonErr == nil && env.Error != nil {
		n, err := env.Error.Int64()
		if err == nil {
			return nil, &apiError{Code: int(n), up: &driver.UpstreamError{Kind: driver.UpstreamRejected, Detail: fmt.Sprintf("%s: 1C error %d", op, n)}}
		}
	}
	switch s := resp.StatusCode; {
	case s == http.StatusNotFound:
		return nil, driver.Upstream(driver.UpstreamRejected, op+": HTTP 404 (check crm.base_url)")
	case s == http.StatusTooManyRequests || s >= 500:
		return nil, driver.Upstream(driver.UpstreamUnavailable, fmt.Sprintf("%s: HTTP %d", op, s))
	case s != http.StatusOK:
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("%s: HTTP %d", op, s))
	}
	if jsonErr != nil || env.Result == nil || !*env.Result || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, driver.Upstream(driver.UpstreamInvalid, op+": answer is not {result: true, data}")
	}
	return &env, nil
}

// get decodes the data of a GET into out.
func (c *client) get(ctx context.Context, path string, query url.Values, usertoken string, out any) (*envelope, error) {
	env, err := c.call(ctx, http.MethodGet, path, query, nil, usertoken, false)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET "+path+": data of an unexpected form")
	}
	return env, nil
}

// techClient is one of the installation's technical clients in 1C.
type techClient int

const (
	// gatewayClient makes the partners' bookings (crm.client_phone).
	gatewayClient techClient = iota
	// mirrorClient makes the mirrors of a pair (crm.mirror_client_phone):
	// 1C keeps no comment on a rental, so a mirror is told by its client.
	mirrorClient
)

// tokenOf is a technical client's token: kept until 1C refuses it,
// issued again by phone (GET /client_usertoken/, no secret needed).
func (c *client) tokenOf(ctx context.Context, who techClient, fresh bool) (string, error) {
	c.mu.Lock()
	phone, slot := c.phone, &c.token
	if who == mirrorClient {
		phone, slot = c.mirrorPhone, &c.mirrorToken
	}
	tok := *slot
	c.mu.Unlock()
	if tok != "" && !fresh {
		return tok, nil
	}
	if phone == "" {
		return "", fmt.Errorf("the 1C technical client is not configured: %w", driver.ErrBookingNotConfigured)
	}
	var data struct {
		UserToken string `json:"usertoken"`
	}
	if _, err := c.get(ctx, "/client_usertoken/", url.Values{"phone": {phone}}, "", &data); err != nil {
		return "", err
	}
	if data.UserToken == "" {
		return "", driver.Upstream(driver.UpstreamInvalid, "GET /client_usertoken/: no usertoken")
	}
	c.mu.Lock()
	*slot = data.UserToken
	c.mu.Unlock()
	return data.UserToken, nil
}

// as runs f with a technical client's token; when 1C refuses the token,
// it is issued again and f runs once more.
func (c *client) as(ctx context.Context, who techClient, f func(tok string) error) error {
	tok, err := c.tokenOf(ctx, who, false)
	if err != nil {
		return err
	}
	err = f(tok)
	var ue *driver.UpstreamError
	refused := clientAuthCodes[codeOf(err)] || (errors.As(err, &ue) && ue.Kind == driver.UpstreamAuth && codeOf(err) == 0)
	if !refused {
		return err
	}
	if tok, err = c.tokenOf(ctx, who, true); err != nil {
		return err
	}
	return f(tok)
}

func networkError(op string, err error) error {
	// never the *url.Error itself: its text is the whole URL, query included
	// (the technical client's phone)
	for _, ce := range []error{context.DeadlineExceeded, context.Canceled} {
		if errors.Is(err, ce) {
			return fmt.Errorf("%s: %w", op, ce)
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return driver.Upstream(driver.UpstreamUnavailable, op+": timeout")
	}
	return driver.Upstream(driver.UpstreamUnavailable, op+": "+strings.TrimSpace(classOf(err)))
}

// classOf names a transport failure without the URL (it carries no
// secret, but keeps logs short).
func classOf(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}
