package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// createLocks serialises the occupancy check and the create of a booking
// per CRM resource. It is one per process: every club and every config of
// the installation (one CRM account) shares it, so neither a second
// partner nor a config reload in the middle of a create opens a window for
// a double booking.
var createLocks = keyedLocks{m: map[string]*keyedLock{}}

type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	ch    chan struct{} // holds a token while locked
	users int
}

// lock waits for key until ctx is done; the returned func unlocks.
func (k *keyedLocks) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	l := k.m[key]
	if l == nil {
		l = &keyedLock{ch: make(chan struct{}, 1)}
		k.m[key] = l
	}
	l.users++
	k.mu.Unlock()
	release := func() {
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
	select {
	case l.ch <- struct{}{}:
		return func() { <-l.ch; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// resourceKey names what a booking of court occupies in the CRM.
func resourceKey(d driver.Driver, court string) string {
	if r, ok := d.(driver.Resourcer); ok {
		return r.Resource(court)
	}
	return fmt.Sprintf("%p %s", d, court)
}
