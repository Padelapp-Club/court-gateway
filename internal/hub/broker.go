package hub

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Broker hands partner requests to the long polls of the club's gateway
// and routes the replies back.
type Broker struct {
	now      func() time.Time
	pollWait time.Duration
	// grace keeps a gateway "connected" for a moment between two polls.
	grace time.Duration
	// pickedUp, when set, runs between a poll taking a call and recording
	// it in flight (tests widen that gap with it).
	pickedUp func()

	mu    sync.Mutex
	conns map[string]*gwConn
}

type gwConn struct {
	pending  chan *call // unbuffered: a send succeeds only into a waiting poll
	inflight map[string]*call
	active   int
	lastSeen time.Time
	lastEnd  time.Time
	meta     ConnMeta
}

// ConnMeta is what a gateway reports about itself on every poll.
type ConnMeta struct {
	Version  string
	Build    string
	Platform string
}

type call struct {
	req  tunnel.Request
	resp chan tunnel.Response
	// abandoned is set, under Broker.mu, when the sender gave up after the
	// handoff; a poll that sees it drops the call instead of recording it.
	abandoned bool
}

// Broker errors.
var (
	ErrOffline = errors.New("club gateway is not connected")
	// ErrBusy: the gateway is connected but every poll is serving a
	// request; none picked this one up in time.
	ErrBusy    = errors.New("club gateway is busy")
	ErrTimeout = errors.New("club gateway did not answer in time")
)

// NewBroker builds a broker.
func NewBroker(pollWait time.Duration, now func() time.Time) *Broker {
	if now == nil {
		now = time.Now
	}
	return &Broker{now: now, pollWait: pollWait, grace: 3 * time.Second, conns: map[string]*gwConn{}}
}

func (b *Broker) conn(club string) *gwConn {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.conns[club]
	if !ok {
		c = &gwConn{pending: make(chan *call), inflight: map[string]*call{}}
		b.conns[club] = c
	}
	return c
}

// Poll waits up to pollWait for a request for club. It returns nil when
// none came or the gateway went away.
func (b *Broker) Poll(ctx context.Context, club string, meta ConnMeta) *tunnel.Request {
	c := b.conn(club)
	b.mu.Lock()
	c.active++
	c.lastSeen = b.now()
	c.meta = meta
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		c.active--
		c.lastSeen = b.now()
		c.lastEnd = time.Now() // real time: grace is about wall-clock gaps
		b.mu.Unlock()
	}()
	t := time.NewTimer(b.pollWait)
	defer t.Stop()
	select {
	case cl := <-c.pending:
		if b.pickedUp != nil {
			b.pickedUp()
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if cl.abandoned {
			return nil // nobody waits: do not run it, do not keep it
		}
		c.inflight[cl.req.ID] = cl
		return &cl.req
	case <-t.C:
		return nil
	case <-ctx.Done():
		return nil
	}
}

// Reply delivers a gateway response. It is false when no partner waits
// for that id from that club (late, duplicate or foreign reply).
func (b *Broker) Reply(club string, r tunnel.Response) bool {
	c := b.conn(club)
	b.mu.Lock()
	cl, ok := c.inflight[r.ID]
	delete(c.inflight, r.ID)
	b.mu.Unlock()
	if !ok {
		return false
	}
	cl.resp <- r // buffered, never blocks
	return true
}

// Connected reports whether club's gateway has a poll open, is serving a
// request it picked up, or had a poll open a moment ago.
func (b *Broker) Connected(club string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.conns[club]
	if !ok {
		return false
	}
	return c.active > 0 || len(c.inflight) > 0 || (!c.lastEnd.IsZero() && time.Since(c.lastEnd) < b.grace)
}

// ConnInfo is the broker's view of one gateway.
type ConnInfo struct {
	Connected bool
	LastSeen  time.Time
	Meta      ConnMeta
}

// Info returns the broker's view of club's gateway.
func (b *Broker) Info(club string) ConnInfo {
	connected := b.Connected(club)
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.conns[club]
	if !ok {
		return ConnInfo{}
	}
	return ConnInfo{Connected: connected, LastSeen: c.lastSeen, Meta: c.meta}
}

// Do sends req to club's gateway and waits for the reply. pickup bounds the
// wait for a free poll, wait bounds the wait for the reply after pickup.
func (b *Broker) Do(ctx context.Context, club string, req tunnel.Request, pickup, wait time.Duration) (tunnel.Response, error) {
	if !b.Connected(club) {
		return tunnel.Response{}, ErrOffline
	}
	c := b.conn(club)
	cl := &call{req: req, resp: make(chan tunnel.Response, 1)}
	pt := time.NewTimer(pickup)
	defer pt.Stop()
	select {
	case c.pending <- cl:
	case <-pt.C:
		if !b.Connected(club) {
			return tunnel.Response{}, ErrOffline
		}
		return tunnel.Response{}, ErrBusy
	case <-ctx.Done():
		return tunnel.Response{}, ctx.Err()
	}
	wt := time.NewTimer(wait)
	defer wt.Stop()
	drop := func() {
		b.mu.Lock()
		cl.abandoned = true
		delete(c.inflight, req.ID)
		b.mu.Unlock()
	}
	select {
	case r := <-cl.resp:
		return r, nil
	case <-wt.C:
		drop()
		return tunnel.Response{}, ErrTimeout
	case <-ctx.Done():
		drop()
		return tunnel.Response{}, ctx.Err()
	}
}
