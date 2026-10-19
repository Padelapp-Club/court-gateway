package lunda

// A constructor that fails sends nothing, and the WebSocket keep-alive
// treats a Close or a frame RFC 6455 forbids as a protocol error and stops
// its opening handshake with its context.

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

type savedStore []byte

func (s savedStore) LoadSession() ([]byte, error) { return s, nil }
func (s savedStore) SaveSession([]byte) error     { return nil }

type recordingRT struct{ requests atomic.Int64 }

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests.Add(1) // any request at all, whatever its context
	return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

// A constructor that returns an error sends nothing — no saved token
// to the rejected address above all: no client (and no keep-alive) is
// made before the whole config is known valid.
func TestInvalidConfigSendsNoToken(t *testing.T) {
	raw, _ := json.Marshal(session{V: 1, Token: "synthetic-review-token", Club: "synthetic-club"})
	rt := &recordingRT{}
	bad := driver.Params{"api_url": "http://outside.invalid/api"}
	many := make([]driver.CourtConfig, 2000) // a long validation: a keep-alive begun first would have run
	for i := range many {
		many[i] = driver.CourtConfig{ID: "court", Params: driver.Params{CourtKey: "synthetic-court"}}
	}
	four := []driver.CourtConfig{}
	for _, id := range []string{"one", "two", "three", "four"} {
		four = append(four, driver.CourtConfig{ID: id, Params: driver.Params{CourtKey: id}})
	}
	for i := 0; i < 20; i++ {
		if _, err := NewDriver(driver.Config{CRM: bad, HTTPClient: &http.Client{Transport: rt}, Sessions: savedStore(raw), Courts: many}); err == nil {
			t.Fatal("expected a validation failure")
		}
	}
	for i := 0; i < 2000; i++ {
		if _, err := NewDriver(driver.Config{CRM: bad, HTTPClient: &http.Client{Transport: rt}, Sessions: savedStore(raw), Courts: four}); err == nil {
			t.Fatal("expected a validation failure")
		}
		if _, err := NewAccount(driver.Config{CRM: bad, HTTPClient: &http.Client{Transport: rt}, Sessions: savedStore(raw)}); err == nil {
			t.Fatal("expected a validation failure")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := rt.requests.Load(); n != 0 {
		t.Fatalf("%d requests from constructors that returned an error", n)
	}
}

// wsPair is a wsConn over a pipe: the test is the server.
func wsPair(t *testing.T) (*wsConn, net.Conn, chan error) {
	a, b := net.Pipe()
	t.Cleanup(func() { b.Close() })
	w := &wsConn{conn: a, r: bufio.NewReader(a)}
	done := make(chan error, 1)
	go func() { done <- w.run(context.Background()) }()
	return w, b, done
}

// readClientFrame reads one (masked) frame of the client: its opcode and
// unmasked payload.
func readClientFrame(t *testing.T, b net.Conn) (byte, []byte) {
	t.Helper()
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(b, h[:]); err != nil {
		t.Fatalf("no frame from the client: %v", err)
	}
	if h[1]&0x80 == 0 {
		t.Fatal("a client frame without a mask")
	}
	var mask [4]byte
	_, _ = io.ReadFull(b, mask[:])
	p := make([]byte, h[1]&0x7f)
	_, _ = io.ReadFull(b, p)
	for i := range p {
		p[i] ^= mask[i%4]
	}
	return h[0] & 0x0f, p
}

func closeCode(p []byte) int {
	if len(p) < 2 {
		return -1
	}
	return int(binary.BigEndian.Uint16(p))
}

// A Close with a reserved or invalid status (or a bad reason) is a
// protocol error: the gateway answers Close 1002, never echoes the status,
// and drops the connection; a valid status is echoed.
func TestWSCloseStatusIsValidated(t *testing.T) {
	for name, tc := range map[string]struct {
		frame []byte
		want  int
	}{
		"reserved 1005":  {[]byte{0x88, 0x02, 0x03, 0xed}, 1002},
		"reserved 1006":  {[]byte{0x88, 0x02, 0x03, 0xee}, 1002},
		"reserved 1015":  {[]byte{0x88, 0x02, 0x03, 0xf7}, 1002},
		"reserved 1004":  {[]byte{0x88, 0x02, 0x03, 0xec}, 1002},
		"undefined 999":  {[]byte{0x88, 0x02, 0x03, 0xe7}, 1002},
		"undefined 2000": {[]byte{0x88, 0x02, 0x07, 0xd0}, 1002},
		"one byte":       {[]byte{0x88, 0x01, 0x03}, 1002},
		"bad reason":     {[]byte{0x88, 0x04, 0x03, 0xe8, 0xff, 0xfe}, 1002},
		"normal 1000":    {[]byte{0x88, 0x02, 0x03, 0xe8}, 1000},
		"going away":     {[]byte{0x88, 0x05, 0x03, 0xe9, 'b', 'y', 'e'}, 1001},
		"application":    {[]byte{0x88, 0x02, 0x0f, 0xa0}, 4000},
		"no status":      {[]byte{0x88, 0x00}, -1},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, done := wsPair(t)
			if _, err := b.Write(tc.frame); err != nil {
				t.Fatal(err)
			}
			op, p := readClientFrame(t, b)
			if op != 0x8 || closeCode(p) != tc.want {
				t.Fatalf("answered op %#x status %d, want Close %d", op, closeCode(p), tc.want)
			}
			if err := <-done; err == nil {
				t.Fatal("the connection was not dropped")
			}
		})
	}
}

// A frame RFC 6455 forbids — a fragmented ping, reserved bits, a
// reserved opcode, a masked server frame, a control frame of more than 125
// bytes, a continuation of nothing — is answered with Close 1002 and the
// connection dropped; never a pong. A valid ping is answered with its pong.
func TestWSForbiddenFramesAreProtocolErrors(t *testing.T) {
	for name, frame := range map[string][]byte{
		"fragmented ping":     {0x09, 0x01, 'x'},
		"reserved bit":        {0xc9, 0x01, 'x'},
		"reserved opcode":     {0x83, 0x00},
		"reserved control":    {0x8b, 0x00},
		"masked server frame": {0x89, 0x81, 1, 2, 3, 4, 'x'},
		"long ping":           {0x89, 0x7e, 0x00, 0x7e},
		"lone continuation":   {0x80, 0x00},
		"data inside data":    {0x01, 0x00, 0x81, 0x00},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, done := wsPair(t)
			if _, err := b.Write(frame); err != nil {
				t.Fatal(err)
			}
			op, p := readClientFrame(t, b)
			if op != 0x8 || closeCode(p) != 1002 {
				t.Fatalf("answered op %#x status %d, want Close 1002", op, closeCode(p))
			}
			if err := <-done; !errors.Is(err, errProtocol) {
				t.Fatalf("ended with %v", err)
			}
		})
	}
	_, b, _ := wsPair(t)
	if _, err := b.Write([]byte{0x89, 0x02, 'h', 'i'}); err != nil {
		t.Fatal(err)
	}
	if op, p := readClientFrame(t, b); op != 0xA || string(p) != "hi" {
		t.Fatalf("a valid ping answered op %#x %q", op, p)
	}
}

// A stop during the opening handshake ends it at once, not at the
// handshake's deadline; EndSession waits no longer than its context.
func TestHandshakeStopsWithItsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	entered := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1024)
		_, _ = c.Read(buf) // the upgrade request, never answered
		close(entered)
		_, _ = io.Copy(io.Discard, c)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := dialWS(ctx, "ws://"+ln.Addr().String()+wsPath, "synthetic-ticket"); done <- err }()
	<-entered
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil || time.Since(start) > time.Second {
			t.Fatalf("after %s: %v", time.Since(start), err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handshake is still waiting after its context ended")
	}

	// EndSession honours its context even when the keep-alive takes long
	c := newClient("http://127.0.0.1:1/api", "ws://127.0.0.1:1"+wsPath, &http.Client{}, nil, nil, nil)
	block := make(chan struct{})
	c.ka.cancel, c.ka.done = func() {}, make(chan struct{})
	go func() { <-block; close(c.ka.done) }()
	defer close(block)
	ectx, ecancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer ecancel()
	a := &Account{c: c}
	if err := a.EndSession(ectx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EndSession: %v", err)
	}
}
