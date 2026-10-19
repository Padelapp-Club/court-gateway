package lunda

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// The admin keeps a WebSocket open beside its calls: its schedule
// registers a CLUB_BOOKING ticket (club/websocket/ticket/register) and
// listens on wss://app.lundapadel.ru/api-ws/websocket-listen?ticket=…,
// reconnecting every 3 s up to 10 times with a new ticket each time
// (react-use-websocket, in its bundle). Seen live 2026-10-17: sessions
// that registered no ticket were refused ~20 s after the login (403
// "missing security details"); one with a ticket and the WebSocket open
// lived 5 minutes, one with a ticket and no WebSocket 4+ minutes. Which of
// the two is necessary is not separated, so the gateway does both, as the
// admin does, for as long as it has a session: a keep-alive. Its frames
// are dropped (nothing reads them yet); neither a ticket nor a
// frame is logged; its failure fails no call (WARN once per outage).
// Through a proxy (Config.Proxy) the WebSocket is dialled directly.

const (
	// wsPath is the admin's WebSocket on its host.
	wsPath = "/api-ws/websocket-listen"
	// defaultWSURL is the WebSocket of the admin at club.lundapadel.ru.
	defaultWSURL = "wss://app.lundapadel.ru" + wsPath
	// wsOrigin is the Origin the admin's browser sends.
	wsOrigin = "https://club.lundapadel.ru"
	// wsFastRetries is how many reconnects in a row are tried wsRetry
	// apart; then one every wsSlowRetry, for as long as there is a session.
	wsFastRetries = 10
	// wsHandshake bounds the dial and the opening handshake.
	wsHandshake = 15 * time.Second
	// wsGUID is RFC 6455's key suffix.
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// Retry intervals of the keep-alive, and how long a connection must stay
// open to count as up (variables for tests): one that closes sooner counts
// as a failed reconnect, toward the backoff and the outage's WARN.
var (
	wsRetry     = 3 * time.Second
	wsSlowRetry = time.Minute
	wsStable    = 30 * time.Second
)

// wsURLOf is the WebSocket beside an API base: the admin's for the default
// one, else the same host at wsPath (ws:// for http://), as the stub serves
// it.
func wsURLOf(apiURL string) string {
	if apiURL == DefaultAPIURL {
		return defaultWSURL
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return defaultWSURL
	}
	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}
	return scheme + "://" + u.Host + wsPath
}

// keepAlive is the state of a client's keep-alive: running from the first
// session on, until stop.
type keepAlive struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	kick    chan struct{} // a new session: reconnect now
	stopped bool
}

// startKeepAlive starts the keep-alive if it is not running (and was not
// stopped); with relogin, a running one is told a new login's session is
// there: it reconnects with a ticket of that session (a renewal of the
// same session does not: the admin's WebSocket stays open across it).
func (c *client) startKeepAlive(relogin bool) {
	k := &c.ka
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return
	}
	if k.cancel != nil {
		if !relogin {
			return
		}
		select {
		case k.kick <- struct{}{}:
		default:
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	k.cancel, k.done, k.kick = cancel, make(chan struct{}), make(chan struct{}, 1)
	go c.keepAliveLoop(ctx, k.done, k.kick)
}

// stopKeepAliveCtx stops the keep-alive for good and waits for it to end,
// no longer than ctx allows (its error then).
func (c *client) stopKeepAliveCtx(ctx context.Context) error {
	k := &c.ka
	k.mu.Lock()
	k.stopped = true
	cancel, done := k.cancel, k.done
	k.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// errNoKeepSession: no session to keep (none yet, or lost: alarmed apart).
var errNoKeepSession = errors.New("no session")

func (c *client) keepAliveLoop(ctx context.Context, done chan struct{}, kick chan struct{}) {
	defer close(done)
	var mu sync.Mutex // fails and down: the loop's, and the timer's that finds a connection up
	fails, down := 0, false
	for ctx.Err() == nil {
		// a new session (kick) ends the connection of the old one: its
		// ticket is the old session's, and the new one needs its own
		cctx, endConn := context.WithCancel(ctx)
		kicked := make(chan bool, 1)
		go func() {
			select {
			case <-kick:
				kicked <- true
				endConn()
			case <-cctx.Done():
				kicked <- false
			}
		}()
		var stable *time.Timer
		opened, err := c.listen(cctx, func() {
			stable = time.AfterFunc(wsStable, func() { // open for wsStable: the outage, if any, is over
				mu.Lock()
				defer mu.Unlock()
				if down {
					c.log.Info("the Lunda keep-alive connection is up again")
				}
				fails, down = 0, false
			})
		})
		endConn()
		lived := stable != nil && !stable.Stop()
		wasKicked := <-kicked
		mu.Lock()
		if wasKicked {
			fails = 0
			mu.Unlock()
			continue
		}
		if ctx.Err() != nil {
			mu.Unlock()
			return
		}
		wait := wsRetry
		switch {
		case lived: // closed after it was up: reconnect after wsRetry
		case errors.Is(err, errNoKeepSession):
			fails, wait = 0, wsSlowRetry
		default: // not opened, or closed soon after: a failure
			if opened {
				err = fmt.Errorf("it closed within %s of opening: %w", wsStable, err)
			}
			if !down {
				c.log.Warn("the Lunda keep-alive connection is down: the gateway keeps trying (calls go on without it)", "error", err)
				down = true
			}
			if fails++; fails > wsFastRetries {
				wait = wsSlowRetry
			}
		}
		mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-kick:
			t.Stop()
			fails = 0
		case <-t.C:
		}
	}
}

// listen registers a ticket and keeps the WebSocket open until it closes,
// calling onOpen once it is open; opened tells whether it opened. The error
// never holds the ticket.
func (c *client) listen(ctx context.Context, onOpen func()) (opened bool, err error) {
	if _, err := c.current(); err != nil {
		return false, errNoKeepSession
	}
	status, res, err := c.call(ctx, opTicket, url.Values{"ticketType": {"CLUB_BOOKING"}, "relatedEntityUid": {""}}, nil)
	if errors.Is(err, driver.ErrNoSession) {
		return false, errNoKeepSession // the loss is alarmed by the calls
	}
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("GET /%s answered HTTP %d", opTicket, status)
	}
	var r struct {
		Ticket string `json:"ticket"`
	}
	if json.Unmarshal(res, &r) != nil || !validToken(r.Ticket) {
		return false, fmt.Errorf("GET /%s: the answer has no ticket", opTicket)
	}
	ws, err := dialWS(ctx, c.wsURL, r.Ticket)
	if err != nil {
		return false, err
	}
	onOpen()
	return true, ws.run(ctx)
}

// wsConn is a client WebSocket (RFC 6455): frames are read and dropped,
// pings answered, a close echoed.
type wsConn struct {
	conn      net.Conn
	r         *bufio.Reader
	wmu       sync.Mutex
	closeSent bool // under wmu: after a Close, nothing more is sent
}

// wsDialer and wsRoots are the WebSocket's TCP dial and TLS roots: the
// system's, replaced only by tests.
var (
	wsDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	wsRoots *x509.CertPool
)

// dialWS opens the WebSocket at rawURL with the ticket, as the admin's
// browser does (its Origin and User-Agent).
func dialWS(ctx context.Context, rawURL, ticket string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") || u.Host == "" {
		return nil, errors.New("the WebSocket address is not ws:// or wss://")
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), map[string]string{"wss": "443", "ws": "80"}[u.Scheme])
	}
	hctx, cancel := context.WithTimeout(ctx, wsHandshake)
	defer cancel()
	raw, err := wsDialer(hctx, "tcp", addr)
	if err != nil {
		return nil, errors.New("the WebSocket host cannot be reached")
	}
	// until the upgrade is done, the end of ctx (a stop) closes the
	// connection at once: no waiting out wsHandshake on a stalled peer.
	// It closes the raw connection it was given, never conn, which the
	// TLS wrapper replaces concurrently.
	unwatch := context.AfterFunc(hctx, func() { raw.Close() })
	defer unwatch()
	conn := raw
	if u.Scheme == "wss" {
		tc := tls.Client(raw, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12, RootCAs: wsRoots})
		if err := tc.HandshakeContext(hctx); err != nil {
			conn.Close()
			return nil, errors.New("the WebSocket TLS handshake failed")
		}
		conn = tc
	}
	if dl, ok := hctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var kb [16]byte
	_, _ = rand.Read(kb[:])
	key := base64.StdEncoding.EncodeToString(kb[:])
	q := u.Query()
	q.Set("ticket", ticket)
	req := "GET " + u.EscapedPath() + "?" + q.Encode() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n" +
		"Origin: " + wsOrigin + "\r\nUser-Agent: " + userAgent + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, errors.New("the WebSocket request could not be sent")
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, errors.New("the WebSocket answer does not read")
	}
	resp.Body.Close()
	sum := sha1.Sum([]byte(key + wsGUID))
	switch {
	case resp.StatusCode != http.StatusSwitchingProtocols:
		conn.Close()
		return nil, fmt.Errorf("the WebSocket answered HTTP %d", resp.StatusCode)
	case !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]):
		conn.Close()
		return nil, errors.New("the WebSocket answer is not an upgrade to this key")
	}
	if !unwatch() { // ctx ended during the upgrade: the connection is closed
		return nil, errors.New("the WebSocket opening was stopped")
	}
	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, r: br}, nil
}

// errProtocol is a frame RFC 6455 forbids: it is answered with Close 1002
// (protocol error) and the connection is dropped.
var errProtocol = errors.New("the WebSocket sent a frame the protocol forbids")

// closeCodeOK: a Close status a peer may send (RFC 6455 §7.4): the defined
// 1000–1003 and 1007–1011, those registered since (1012–1014), and the
// ones for libraries and applications (3000–4999). 1004–1006 and 1015
// are reserved, never on the wire.
func closeCodeOK(code uint16) bool {
	switch {
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1014, code >= 3000 && code <= 4999:
		return true
	}
	return false
}

// run reads frames until the connection ends or ctx is done (then it says
// goodbye with a close frame). A ping is answered with a pong; a valid
// close is answered with its status; a frame readFrame refuses, a close
// with an invalid status or reason, and a data frame out of its message's
// sequence with Close 1002, and the connection is dropped. Not checked:
// the UTF-8 of text messages (they are discarded unread) and that lengths
// are minimally encoded.
func (w *wsConn) run(ctx context.Context) (err error) {
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		select {
		case <-ctx.Done():
			_ = w.write(0x8, []byte{0x03, 0xe8}) // 1000: normal closure
			w.conn.Close()
		case <-ended:
		}
	}()
	defer w.conn.Close()
	defer func() { // before the connection is closed
		if errors.Is(err, errProtocol) {
			_ = w.write(0x8, []byte{0x03, 0xea}) // 1002: protocol error
		}
	}()
	inMessage := false // a fragmented data message is being read
	for {
		fin, op, payload, err := w.readFrame()
		if err != nil {
			return err
		}
		switch op {
		case 0x8: // close: a valid status is echoed, none answered with none
			if len(payload) == 1 || len(payload) >= 2 && (!closeCodeOK(binary.BigEndian.Uint16(payload)) || !utf8.Valid(payload[2:])) {
				return errProtocol
			}
			_ = w.write(0x8, payload[:min(len(payload), 2)])
			return errors.New("the WebSocket was closed by Lunda")
		case 0x9: // ping
			if err := w.write(0xA, payload); err != nil {
				return errors.New("the WebSocket pong could not be sent")
			}
		case 0xA: // pong: dropped
		case 0x1, 0x2, 0x0: // data, dropped: a message begins only after the last ended, continues only within one
			if inMessage != (op == 0x0) {
				return errProtocol
			}
			inMessage = !fin
		}
	}
}

// readFrame reads one frame; data frames, of any length, are discarded
// unread as they stream in, control frames returned with their payload.
// errProtocol: reserved bits set, a reserved opcode, a masked frame (a
// server's never are), a fragmented control frame or one longer than
// 125, a 64-bit length with its top bit set. The error never holds a
// frame.
func (w *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	ended := errors.New("the WebSocket connection ended")
	var h [2]byte
	if _, err := io.ReadFull(w.r, h[:]); err != nil {
		return false, 0, nil, ended
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0f
	control := op&0x8 != 0
	n := uint64(h[1] & 0x7f)
	switch {
	case h[0]&0x70 != 0, // RSV1–3: no extension was agreed
		op > 0x2 && op < 0x8, op > 0xA, // reserved opcodes
		h[1]&0x80 != 0,     // masked: a server's frames never are
		control && !fin,    // control frames are never fragmented
		control && n > 125: // nor longer than 125
		return false, 0, nil, errProtocol
	}
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(w.r, b[:]); err != nil {
			return false, 0, nil, ended
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(w.r, b[:]); err != nil {
			return false, 0, nil, ended
		}
		if n = binary.BigEndian.Uint64(b[:]); n > math.MaxInt64 {
			return false, 0, nil, errProtocol
		}
	}
	if !control {
		if _, err := io.CopyN(io.Discard, w.r, int64(n)); err != nil {
			return false, 0, nil, ended
		}
		return fin, op, nil, nil
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(w.r, payload); err != nil {
		return false, 0, nil, ended
	}
	return fin, op, payload, nil
}

// errCloseSent: a Close went out already; nothing is sent after it
// (RFC 6455 §5.5.1), a second Close least of all.
var errCloseSent = errors.New("the WebSocket close was sent already")

// write sends one final frame, masked as a client's must be; after a
// Close, nothing. Its payloads are at most 125 bytes: a close status, or
// the payload of a ping readFrame bounded.
func (w *wsConn) write(op byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.closeSent {
		return errCloseSent
	}
	w.closeSent = op == 0x8
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	frame := make([]byte, 0, 6+len(payload))
	frame = append(frame, 0x80|op, 0x80|byte(len(payload)))
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := w.conn.Write(frame)
	return err
}
