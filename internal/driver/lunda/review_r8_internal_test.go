package lunda

// Regressions of review round r8 (keep-alive follow-ups 4, 6 and 7, and
// mutation m8).

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// clientFrames reads the client's frames off b until it ends: their
// opcodes.
func clientFrames(b net.Conn) chan []byte {
	out := make(chan []byte, 1)
	go func() {
		var ops []byte
		for {
			var h [2]byte
			if _, err := io.ReadFull(b, h[:]); err != nil {
				out <- ops
				return
			}
			if _, err := io.CopyN(io.Discard, b, 4+int64(h[1]&0x7f)); err != nil {
				out <- ops
				return
			}
			ops = append(ops, h[0]&0x0f)
		}
	}()
	return out
}

// r8-4: after a Close nothing is sent — not a second Close (the stop and
// the loop's own Close racing), nor a pong.
func TestWSSendsOneCloseOnly(t *testing.T) {
	a, b := net.Pipe()
	w := &wsConn{conn: a, r: bufio.NewReader(a)}
	frames := clientFrames(b)
	if err := w.write(0x8, []byte{0x03, 0xea}); err != nil {
		t.Fatal(err)
	}
	if err := w.write(0x8, []byte{0x03, 0xe8}); !errors.Is(err, errCloseSent) {
		t.Fatalf("a second close: %v", err)
	}
	if err := w.write(0xA, []byte("x")); !errors.Is(err, errCloseSent) {
		t.Fatalf("a pong after the close: %v", err)
	}
	a.Close()
	if ops := <-frames; len(ops) != 1 || ops[0] != 0x8 {
		t.Fatalf("frames sent: %v", ops)
	}
}

// r8-6: a data frame longer than any bound is discarded as it streams
// in, not refused: the connection stays and the next ping is answered.
// Only a 64-bit length with its top bit set is a protocol error.
func TestWSLongDataFrameIsDiscarded(t *testing.T) {
	_, b, done := wsPair(t)
	const n = 3 << 20
	go func() {
		frame := append([]byte{0x82, 0x7f, 0, 0, 0, 0, 0, n >> 16 & 0xff, n >> 8 & 0xff, n & 0xff}, make([]byte, n)...)
		_, _ = b.Write(append(frame, 0x89, 0x02, 'h', 'i'))
	}()
	if op, p := readClientFrame(t, b); op != 0xA || string(p) != "hi" {
		t.Fatalf("after a long data frame: op %#x %q", op, p)
	}
	select {
	case err := <-done:
		t.Fatalf("the connection ended: %v", err)
	default:
	}

	_, b, done = wsPair(t)
	if _, err := b.Write([]byte{0x82, 0x7f, 0x80, 0, 0, 0, 0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	if op, p := readClientFrame(t, b); op != 0x8 || closeCode(p) != 1002 {
		t.Fatalf("a length with its top bit set: op %#x status %d", op, closeCode(p))
	}
	if err := <-done; !errors.Is(err, errProtocol) {
		t.Fatalf("ended with %v", err)
	}
}

// stopAtAnswer is the raw connection of a WebSocket dial: once the
// upgrade's answer has been read, it ends the dial's context and waits
// until the handshake watch has closed it — a stop landing between the
// answer and the end of the watch.
type stopAtAnswer struct {
	net.Conn
	stop   context.CancelFunc
	closed chan struct{}
	once   sync.Once
}

func (c *stopAtAnswer) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if strings.Contains(string(p[:n]), "\r\n\r\n") {
		c.stop()
		select {
		case <-c.closed:
		case <-time.After(5 * time.Second):
		}
	}
	return n, err
}

func (c *stopAtAnswer) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// m8: a stop that lands after the upgrade's answer was read, but while the
// handshake watch is still on, has closed the connection: dialWS says the
// opening was stopped and returns no connection (never a closed one,
// which would log a false "up again").
func TestWSStopAtTheUpgradeReturnsNoConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := net.Pipe()
	defer b.Close()
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(b))
		if err != nil {
			return
		}
		sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + wsGUID))
		_, _ = io.WriteString(b, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+
			base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	}()
	restore := wsDialer
	wsDialer = func(context.Context, string, string) (net.Conn, error) {
		return &stopAtAnswer{Conn: a, stop: cancel, closed: make(chan struct{})}, nil
	}
	defer func() { wsDialer = restore }()
	ws, err := dialWS(ctx, "ws://lunda.test"+wsPath, "synthetic-ticket")
	if ws != nil || err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("dial: %v, %v", ws, err)
	}
}
