package core

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/Padelapp-Club/court-gateway/internal/driver"
)

// createLocks serialises the occupancy check and the create of a booking
// per CRM resource. It is one per process: every club and every config of
// the installation (one CRM account) shares it, so neither a second
// partner nor a config reload in the middle of a create opens a window for
// a double booking.
var createLocks = keyedLocks{m: map[string]*keyedLock{}}

// createLockWait bounds the wait for another create of the same court:
// then the partner gets 503 BUSY and retries.
var createLockWait = 10 * time.Second

// minWriteTime: with less left of the request (or of half its time) a
// create does not start the write (503 BUSY): cut off, its outcome is unknown.
const minWriteTime = 10 * time.Second

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

// resourceKeys names everything a booking of court occupies: the room of
// each CRM of a pair (driver.MultiResourcer), else resourceKey.
func resourceKeys(d driver.Driver, court string) []string {
	if r, ok := d.(driver.MultiResourcer); ok {
		if keys := r.Resources(court); len(keys) > 0 {
			return keys
		}
	}
	return []string{resourceKey(d, court)}
}

// lockAll takes the locks of keys in one order (sorted, each once), so two
// writers that need some of the same keys never wait for each other in a
// circle; on failure it releases what it took.
func (k *keyedLocks) lockAll(ctx context.Context, keys []string) (func(), error) {
	keys = slices.Compact(slices.Sorted(slices.Values(keys)))
	var held []func()
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i]()
		}
	}
	for _, key := range keys {
		unlock, err := k.lock(ctx, key)
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, unlock)
	}
	return release, nil
}

// LockResources takes the process-wide create lock of every CRM resource
// in keys (a pair's court: its room in each CRM), the same lock a
// partner's create of the court holds from its occupancy check to its
// write: the reconciliation loop of a pair writes under it, so neither
// writes into a check the other made. The returned func unlocks.
func LockResources(ctx context.Context, keys []string) (func(), error) {
	return createLocks.lockAll(ctx, keys)
}
