package hub

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/tunnel"
)

// Broker hands partner requests to the long polls of an installation's
// gateway and routes the replies back.
//
// Every registration of a gateway has a generation (Store numbers them,
// growing). Polls and requests carry the generation they were
// authenticated or routed under, and the broker serves only the newest one:
// once a registration is replaced or removed (Retire), its open polls end
// empty, it can no longer pick up a request, and a request routed to it is
// ErrOffline. A request it had already picked up ends at once with
// ErrReplaced: its reply could no longer be told from a stale one.
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
	// retired[inst]: generations below it are replaced or removed.
	retired map[string]uint64
}

type gwConn struct {
	gen      uint64
	dead     chan struct{} // closed, under Broker.mu, when gen is retired
	pending  chan *call    // unbuffered: a send succeeds only into a waiting poll
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
	// Polls is how many polls the gateway keeps open.
	Polls int
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
	// ErrReplaced: the registration that was running the request was
	// replaced or removed; whether the request took effect is unknown.
	ErrReplaced = errors.New("club gateway replaced while running the request")
)

// NewBroker builds a broker.
func NewBroker(pollWait time.Duration, now func() time.Time) *Broker {
	if now == nil {
		now = time.Now
	}
	return &Broker{now: now, pollWait: pollWait, grace: 3 * time.Second, conns: map[string]*gwConn{}, retired: map[string]uint64{}}
}

func isDead(c *gwConn) bool {
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}

// pollConn returns the connection of generation gen of inst, replacing an
// older one; nil when gen is retired or older than the current one.
// b.mu must be held.
func (b *Broker) pollConn(inst string, gen uint64) *gwConn {
	if gen < b.retired[inst] {
		return nil
	}
	c, ok := b.conns[inst]
	switch {
	case ok && c.gen == gen:
		return c
	case ok && c.gen > gen:
		return nil
	case ok:
		close(c.dead)
	}
	c = &gwConn{gen: gen, dead: make(chan struct{}), pending: make(chan *call), inflight: map[string]*call{}}
	b.conns[inst] = c
	return c
}

// Retire ends every generation of inst below gen: their polls return
// empty and requests routed to them are ErrOffline. The store calls it,
// under its own lock, when a registration is replaced or removed.
func (b *Broker) Retire(inst string, gen uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gen > b.retired[inst] {
		b.retired[inst] = gen
	}
	if c, ok := b.conns[inst]; ok && c.gen < gen {
		close(c.dead)
		delete(b.conns, inst)
	}
}

// Poll waits up to pollWait for a request for generation gen of inst. It
// returns nil when none came, the gateway went away or gen was retired.
func (b *Broker) Poll(ctx context.Context, inst string, gen uint64, meta ConnMeta) *tunnel.Request {
	b.mu.Lock()
	c := b.pollConn(inst, gen)
	if c == nil {
		b.mu.Unlock()
		return nil
	}
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
		if cl.abandoned || isDead(c) {
			return nil // nobody waits, or not for this registration: do not run it
		}
		c.inflight[cl.req.ID] = cl
		return &cl.req
	case <-c.dead:
		return nil
	case <-t.C:
		return nil
	case <-ctx.Done():
		return nil
	}
}

// Reply delivers a gateway response. It is false when no partner waits
// for that id from that club (late, duplicate or foreign reply).
func (b *Broker) Reply(club string, r tunnel.Response) bool {
	b.mu.Lock()
	c, ok := b.conns[club]
	if !ok {
		b.mu.Unlock()
		return false
	}
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

// Do sends req to generation gen of club's gateway and waits for the
// reply. pickup bounds the wait for a free poll, wait bounds the wait for
// the reply after pickup.
func (b *Broker) Do(ctx context.Context, club string, gen uint64, req tunnel.Request, pickup, wait time.Duration) (tunnel.Response, error) {
	if !b.Connected(club) {
		return tunnel.Response{}, ErrOffline
	}
	b.mu.Lock()
	c, ok := b.conns[club]
	b.mu.Unlock()
	if !ok || c.gen != gen {
		return tunnel.Response{}, ErrOffline
	}
	cl := &call{req: req, resp: make(chan tunnel.Response, 1)}
	pt := time.NewTimer(pickup)
	defer pt.Stop()
	select {
	case c.pending <- cl:
	case <-c.dead:
		return tunnel.Response{}, ErrOffline
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
	case <-c.dead:
		select {
		case r := <-cl.resp: // answered just before the retirement
			return r, nil
		default:
		}
		b.mu.Lock()
		_, picked := c.inflight[req.ID]
		b.mu.Unlock()
		drop()
		if !picked {
			// retired between the handoff and the poll recording the call:
			// the poll drops it, so it never ran
			return tunnel.Response{}, ErrOffline
		}
		// it runs on the replaced gateway, whose reply can no longer arrive
		return tunnel.Response{}, ErrReplaced
	case <-wt.C:
		drop()
		return tunnel.Response{}, ErrTimeout
	case <-ctx.Done():
		drop()
		return tunnel.Response{}, ctx.Err()
	}
}
