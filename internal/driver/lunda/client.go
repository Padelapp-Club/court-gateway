package lunda

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/core"
	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// Operations of the Lunda club admin API the driver calls.
const (
	opQRStart     = "auth/qr/start"
	opQRStatus    = "auth/qr/status"
	opRefresh     = "auth/refresh"
	opSendPush    = "profile/send-push"
	opAuth        = "profile/auth"
	opSwitchClub  = "profile/switch-club"
	opCurrent     = "profile/current"
	opCourts      = "court/settings/load-list"
	opSlots       = "club/booking/slots-load"
	opTariffs     = "club/settings/tariff/load-list"
	opTariff      = "club/settings/tariff/load"
	opCalendar    = "club/settings/calendar/load"
	opVisitors    = "club/visitor/list"
	opBookingLoad = "club/booking/booking-load"
	opCreate      = "club/booking/booking-create"
	opAdjust      = "club/booking/update-courts-price-adjustment"
	opNotes       = "club/booking/booking-notes-update"
	opCancel      = "club/booking/booking-cancel"
)

// endpoint is one operation as the admin client calls it.
type endpoint struct {
	method string
	auth   bool // sent with the session token (X-Club-Auth-Token)
}

// endpoints are every call the driver makes, and the only ones it can make
// (client.call refuses any other): each is a call of the club admin client
// itself (REQ-394), as listed from its bundle and seen in its traffic on
// 2026-10-17. There is no payment, refund or external-payment operation
// here, and there must never be one: the gateway does not touch the
// club's till (add-ext-payment would write cash that is not in it).
//
// [UNVERIFIED] profile/current is seen in the admin's traffic after a
// login and polled by it, but is not among the bundle's generated
// operations; its method is taken to be GET.
var endpoints = map[string]endpoint{
	opQRStart:     {http.MethodPost, false},
	opQRStatus:    {http.MethodPost, false},
	opRefresh:     {http.MethodPost, true},
	opSendPush:    {http.MethodPost, false},
	opAuth:        {http.MethodPost, false},
	opSwitchClub:  {http.MethodPost, true},
	opCurrent:     {http.MethodGet, true},
	opCourts:      {http.MethodGet, true},
	opSlots:       {http.MethodGet, true},
	opTariffs:     {http.MethodGet, true},
	opTariff:      {http.MethodGet, true},
	opCalendar:    {http.MethodGet, true},
	opVisitors:    {http.MethodPost, true}, // a search: POST, but reads
	opBookingLoad: {http.MethodGet, true},
	// the writes: the only operations that change the club's data
	opCreate: {http.MethodPost, true},
	opAdjust: {http.MethodPost, true},
	opNotes:  {http.MethodPost, true},
	opCancel: {http.MethodDelete, true},
}

// Operations returns the operations the driver may call, as
// "METHOD /path" below the API base.
func Operations() []string {
	out := make([]string, 0, len(endpoints))
	for op, ep := range endpoints {
		out = append(out, ep.method+" /"+op)
	}
	return out
}

// maxBody caps how much of an answer is read.
const maxBody = 8 << 20

// renewTimeout bounds a renewal: it is made under its own context, so the
// end of the request that brought refresh-required does not cut it short.
const renewTimeout = 20 * time.Second

// session is what the driver keeps of a login: the token and the club it
// is for. It is saved through driver.SessionStore as JSON (a secret: the
// store keeps it readable by the owner only) and never logged.
type session struct {
	V        int    `json:"v"`
	Token    string `json:"token"`
	Club     string `json:"club"`
	ClubName string `json:"club_name,omitempty"`
}

// client talks HTTP to Lunda. One client serves the installation's account
// and all its clubs, with one session. A token Lunda refused is lost: it
// is not sent again, nor taken again from the store, and while there is
// no other no call at all is made — writes above all (REQ-395).
type client struct {
	apiURL string
	http   *http.Client
	ua     string
	store  driver.SessionStore // nil: the session lives in memory only
	log    *slog.Logger
	now    func() time.Time

	mu    sync.Mutex
	sess  *session
	lost  string // the token Lunda refused
	renew string // the token Lunda asked to renew, while the renewal is not done

	renewMu sync.Mutex // one renewal at a time
}

func newClient(apiURL string, hc *http.Client, store driver.SessionStore, log *slog.Logger, now func() time.Time) *client {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	// Lunda ties a QR login to the cookie qr/start sets (X_SESSION_ID): a
	// qr/status without it answers EXPIRED at once (seen live 2026-10-17).
	// The client keeps cookies as the admin's browser does; they are never
	// logged.
	jc := *hc
	if jar, err := cookiejar.New(nil); err == nil {
		jc.Jar = jar
	}
	c := &client{apiURL: apiURL, http: &jc, store: store, log: log, now: now,
		ua: "PadelApp court-gateway/" + asciiOnly(driver.Version) + " (club gateway)"}
	c.mu.Lock()
	c.sess = c.loadLocked()
	c.mu.Unlock()
	return c
}

// asciiOnly keeps a header value printable ASCII.
func asciiOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, s)
}

// loadLocked reads the store's session (nil when there is none, or it does
// not read: the error is logged, never the content).
func (c *client) loadLocked() *session {
	if c.store == nil {
		return nil
	}
	raw, err := c.store.LoadSession()
	if err != nil {
		c.log.Error("the Lunda session in the state directory cannot be read", "error", err)
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	var s session
	if json.Unmarshal(raw, &s) != nil || !validToken(s.Token) || !uidRe.MatchString(s.Club) {
		c.log.Error("the Lunda session in the state directory is malformed: log the gateway in again (court-gateway login)")
		return nil
	}
	return &s
}

// adopt makes s the session and saves it. A session that cannot be saved
// still serves this process; the error says the next one will not have it.
func (c *client) adopt(s session) error {
	s.V = 1
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess, c.renew = &s, ""
	if c.lost != "" && c.lost != s.Token {
		c.lost = ""
	}
	if c.store == nil {
		return nil
	}
	raw, _ := json.Marshal(s)
	if err := c.store.SaveSession(raw); err != nil {
		c.log.Error("the Lunda session could not be saved to the state directory: a restart will need a new login", "error", err)
		return fmt.Errorf("the Lunda session could not be saved: %w", err)
	}
	return nil
}

// hasSession reports whether a session is held or saved (it may still be
// refused).
func (c *client) hasSession() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil && c.sess.Token != c.lost {
		return true
	}
	s := c.loadLocked()
	return s != nil && s.Token != c.lost
}

// current is the session the calls go with: the one held, or else the
// one the store holds now — `court-gateway login` in another process may
// have saved it — unless that is the one Lunda refused.
func (c *client) current() (session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil && c.sess.Token != c.lost {
		return *c.sess, nil
	}
	if s := c.loadLocked(); s != nil && s.Token != c.lost {
		if c.lost != "" {
			c.log.Info("a new Lunda session was found in the state directory: the gateway uses it")
		}
		c.sess, c.lost, c.renew = s, "", ""
		return *s, nil
	}
	if c.lost != "" {
		return session{}, noSession("Lunda ended the gateway's session")
	}
	return session{}, noSession("nobody has logged the gateway in to Lunda")
}

// replaced reports whether a session other than tok's is at hand — renewed
// meanwhile, or saved to the store by a new login — and takes it.
func (c *client) replaced(tok string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil && c.sess.Token != tok && c.sess.Token != c.lost {
		return true
	}
	if s := c.loadLocked(); s != nil && s.Token != tok && s.Token != c.lost {
		c.sess, c.renew = s, ""
		c.log.Info("a new Lunda session was found in the state directory: the gateway uses it")
		return true
	}
	return false
}

// markLost records that Lunda refused tok: nothing is sent with it again.
func (c *client) markLost(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lost == tok {
		return
	}
	c.lost, c.renew = tok, ""
	c.log.Error("Lunda refused the gateway's session: nothing is written to Lunda until a member of the club's staff logs the gateway in again (court-gateway login)")
}

// noSession is the error of a call without a live session.
func noSession(detail string) error {
	return fmt.Errorf("%w: %w", driver.Upstream(driver.UpstreamAuth, detail), driver.ErrNoSession)
}

// call sends one operation with the session and returns its HTTP status
// and, for a 2xx, the result (the "result" of Lunda's envelope). A 401 or
// 403 that is not a refusal of the operation alone (profile/current still
// passes) loses the session: a session at hand other than the refused
// one (renewed meanwhile, or saved by a new login) is tried once,
// otherwise the error is an UpstreamAuth naming driver.ErrNoSession.
// refresh-required on any answer renews the session first. 5xx and
// network failures are UpstreamUnavailable; other statuses are the
// caller's. Neither the token nor a body ever goes into an error.
func (c *client) call(ctx context.Context, op string, query url.Values, params any) (int, json.RawMessage, error) {
	return c.callGuarded(ctx, op, query, params, nil)
}

// callGuarded is call for a write whose precondition may have changed by
// the time it is sent again with another session (a cancel of a booking
// that has started meanwhile): guard, when set, is asked before that
// retry, and an error from it is returned instead — the write is not
// sent again.
func (c *client) callGuarded(ctx context.Context, op string, query url.Values, params any, guard func(context.Context) error) (int, json.RawMessage, error) {
	ep, ok := endpoints[op]
	if !ok {
		return 0, nil, fmt.Errorf("lunda: %s is not an operation the driver may call", op)
	}
	body, err := envelope(params)
	if err != nil {
		return 0, nil, err
	}
	for attempt := 0; ; attempt++ {
		var tok string
		if ep.auth {
			s, err := c.current()
			if err != nil {
				return 0, nil, err
			}
			tok = s.Token
		}
		status, hdr, data, err := c.send(ctx, op, ep, query, body, tok)
		if err != nil {
			return 0, nil, err
		}
		if ep.auth && op != opRefresh && (renewAsked(hdr) || c.renewDue(tok)) {
			c.renewSession(ctx, tok)
		}
		switch {
		case ep.auth && (status == http.StatusUnauthorized || status == http.StatusForbidden):
			if status == http.StatusForbidden && op != opCurrent && c.alive(ctx, tok) {
				return 0, nil, driver.Upstream(driver.UpstreamRejected, ep.method+" /"+op+": forbidden for the staff member the gateway is logged in as")
			}
			if attempt == 0 && c.replaced(tok) {
				if guard != nil {
					if err := guard(ctx); err != nil {
						return 0, nil, err
					}
				}
				continue
			}
			c.markLost(tok)
			return 0, nil, noSession(fmt.Sprintf("%s /%s: Lunda refused the session (HTTP %d)", ep.method, op, status))
		case core.IsRedirect(status):
			return 0, nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("%s /%s answered with a redirect (HTTP %d), which is not followed", ep.method, op, status))
		case status >= 500:
			return 0, nil, driver.Upstream(driver.UpstreamUnavailable, fmt.Sprintf("%s /%s answered HTTP %d", ep.method, op, status))
		case status >= 200 && status < 300:
			res, ok := unwrap(data)
			if !ok {
				return 0, nil, driver.Upstream(driver.UpstreamInvalid, ep.method+" /"+op+": the answer is not a JSON object")
			}
			return status, res, nil
		}
		return status, nil, nil
	}
}

// alive asks profile/current with tok, without the loss machinery: true
// when Lunda takes the session.
func (c *client) alive(ctx context.Context, tok string) bool {
	status, _, _, err := c.send(ctx, opCurrent, endpoints[opCurrent], nil, nil, tok)
	return err == nil && status == http.StatusOK
}

// exchange sends a login-time operation with an explicit token (none, or
// the login's before a club is chosen): no session is used, lost or
// renewed. It returns the result of a 2xx, or the status of a refusal
// (4xx); 5xx and network failures are errors.
func (c *client) exchange(ctx context.Context, op, tok string, params any) (int, json.RawMessage, error) {
	ep, ok := endpoints[op]
	if !ok {
		return 0, nil, fmt.Errorf("lunda: %s is not an operation the driver may call", op)
	}
	body, err := envelope(params)
	if err != nil {
		return 0, nil, err
	}
	status, _, data, err := c.send(ctx, op, ep, nil, body, tok)
	switch {
	case err != nil:
		return 0, nil, err
	case core.IsRedirect(status):
		return 0, nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("%s /%s answered with a redirect (HTTP %d), which is not followed", ep.method, op, status))
	case status >= 500:
		return 0, nil, driver.Upstream(driver.UpstreamUnavailable, fmt.Sprintf("%s /%s answered HTTP %d", ep.method, op, status))
	case status >= 200 && status < 300:
		res, ok := unwrap(data)
		if !ok {
			return 0, nil, driver.Upstream(driver.UpstreamInvalid, ep.method+" /"+op+": the answer is not a JSON object")
		}
		return status, res, nil
	}
	return status, nil, nil
}

// envelope wraps parameters as the admin client sends them:
// {"parameters": {...}}; nil sends no body.
func envelope(params any) ([]byte, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(map[string]any{"parameters": params})
}

// unwrap returns the "result" of an answer, or the answer itself when it
// has none. [UNVERIFIED] Every answer seen (auth, profile, booking) wraps
// its data in "result"; the schedule's was recorded without the envelope,
// so both are read.
func unwrap(data []byte) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	if trimmed[0] == '{' {
		var env map[string]json.RawMessage
		if json.Unmarshal(trimmed, &env) != nil {
			return nil, false
		}
		if r, ok := env["result"]; ok {
			return r, true
		}
	} else if !json.Valid(trimmed) {
		return nil, false
	}
	return json.RawMessage(trimmed), true
}

// send makes one HTTP request. The token goes in X-Club-Auth-Token only;
// the error of a failed request names the operation, never the URL's
// query (it may carry a client's search) or the body.
func (c *client) send(ctx context.Context, op string, ep endpoint, query url.Values, body []byte, tok string) (int, http.Header, []byte, error) {
	u := c.apiURL + "/" + op
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, ep.method, u, rd)
	if err != nil {
		return 0, nil, nil, driver.Upstream(driver.UpstreamUnavailable, "build "+ep.method+" /"+op+" request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("X-Request-ID", requestID())
	if tok != "" {
		req.Header.Set("X-Club-Auth-Token", tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, networkError(ep.method+" /"+op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, nil, networkError(ep.method+" /"+op, err)
	}
	return resp.StatusCode, resp.Header, data, nil
}

// requestID is a random UUID v4, as the admin client sends in X-Request-ID.
func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func networkError(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", op, context.DeadlineExceeded)
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return fmt.Errorf("%s: %w", op, context.DeadlineExceeded)
	}
	// the url.Error text holds the URL and its query: keep only the op
	return driver.Upstream(driver.UpstreamUnavailable, op+": network error")
}

// renewAsked reports Lunda's request to renew the session.
func renewAsked(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get("refresh-required")), "true")
}

// renewDue reports a renewal of tok that Lunda asked for and that did not
// go through yet (it is tried again with the next call, not on a timer).
func (c *client) renewDue(tok string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.renew != "" && c.renew == tok
}

// renewSession renews the session tok belongs to, as the admin client does
// when an answer carries refresh-required: auth/refresh with a new key
// pair, the token answered encrypted with it. A renewal Lunda refuses
// loses the session (unless another is at hand); one that fails otherwise
// is tried again with the next call. Only one renewal runs at a time, and
// none for a token already renewed.
func (c *client) renewSession(ctx context.Context, tok string) {
	c.renewMu.Lock()
	defer c.renewMu.Unlock()
	c.mu.Lock()
	s := c.sess
	if s == nil || s.Token != tok || c.lost == tok {
		c.mu.Unlock()
		return
	}
	cur := *s
	c.renew = tok
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renewTimeout)
	defer cancel()
	key, err := newKeyPair()
	if err != nil {
		c.log.Error("no key pair for the Lunda session renewal", "error", err)
		return
	}
	status, res, err := c.exchange(rctx, opRefresh, tok, map[string]any{"publicKey": key.public})
	switch {
	case err != nil:
		c.log.Warn("the Lunda session renewal failed: it is tried again with the next call", "error", err)
		return
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if !c.replaced(tok) {
			c.markLost(tok)
		}
		return
	case status != http.StatusOK && status != http.StatusCreated:
		c.log.Warn("Lunda refused the session renewal: it is tried again with the next call", "status", status)
		return
	}
	var r struct {
		Token []string `json:"token"`
	}
	if json.Unmarshal(res, &r) != nil {
		c.log.Warn("the Lunda session renewal answered no token: it is tried again with the next call")
		return
	}
	next, err := key.decrypt(r.Token)
	if err != nil {
		c.log.Warn("the Lunda session renewal answered a token that does not decrypt: it is tried again with the next call", "error", err)
		return
	}
	cur.Token = next
	if err := c.adopt(cur); err == nil {
		c.log.Info("the Lunda session was renewed")
	}
}

// profileCurrent checks the session: profile/current answers 200.
func profileCurrent(ctx context.Context, c *client) error {
	status, _, err := c.call(ctx, opCurrent, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opCurrent, status))
	}
	return nil
}

// lundaCourt is a court of the club as court/settings/load-list lists it.
// [UNVERIFIED] the fields: uid (courtUid as the schedule names it) and
// name; both spellings of the id are read.
type lundaCourt struct {
	UID      string `json:"uid"`
	CourtUID string `json:"courtUid"`
	Name     string `json:"name"`
}

func (c lundaCourt) uid() string {
	if c.UID != "" {
		return c.UID
	}
	return c.CourtUID
}

// courtList reads the club's courts. A list that does not read is an
// error, never a shorter list.
func courtList(ctx context.Context, c *client) ([]lundaCourt, error) {
	status, res, err := c.call(ctx, opCourts, nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, driver.Upstream(driver.UpstreamRejected, fmt.Sprintf("GET /%s answered HTTP %d", opCourts, status))
	}
	var list []lundaCourt
	if err := decodeList(res, &list); err != nil {
		return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opCourts+": the answer is not a list of courts")
	}
	for _, ct := range list {
		if !uidRe.MatchString(ct.uid()) {
			return nil, driver.Upstream(driver.UpstreamInvalid, "GET /"+opCourts+": a court without a valid id")
		}
	}
	return list, nil
}

// listKeys are where a list answer may hold its items when it is not a
// bare array. [UNVERIFIED] which one Lunda uses for each list.
var listKeys = []string{"items", "content", "list", "data"}

// decodeList decodes a JSON array, or the array under one of listKeys.
func decodeList(raw json.RawMessage, out any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return json.Unmarshal(trimmed, out)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return err
	}
	for _, k := range listKeys {
		if v, ok := obj[k]; ok && len(bytes.TrimSpace(v)) > 0 && bytes.TrimSpace(v)[0] == '[' {
			return json.Unmarshal(v, out)
		}
	}
	return errors.New("no list in the answer")
}
