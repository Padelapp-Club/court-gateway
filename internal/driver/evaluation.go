package driver

import (
	"context"
	"sync"
)

// An evaluation is one pass over the clubs of an installation: one
// readiness round, or one application of the hub's config. Within it the
// clubs of one CRM account may share a read (Once), so the account is
// asked once, not once per club. Nothing is kept after it: the only state
// a driver keeps across requests is its login.

type evaluationKey struct{}

type evaluation struct {
	mu   sync.Mutex
	vals map[string]*shared
}

type shared struct {
	once sync.Once
	val  any
	err  error
}

// WithEvaluation starts an evaluation, or keeps the one ctx already has.
func WithEvaluation(ctx context.Context) context.Context {
	if _, ok := ctx.Value(evaluationKey{}).(*evaluation); ok {
		return ctx
	}
	return context.WithValue(ctx, evaluationKey{}, &evaluation{vals: map[string]*shared{}})
}

// Once returns read's result, made once per key within ctx's evaluation
// and shared by every caller in it; outside an evaluation it just reads.
func Once[T any](ctx context.Context, key string, read func() (T, error)) (T, error) {
	ev, ok := ctx.Value(evaluationKey{}).(*evaluation)
	if !ok {
		return read()
	}
	ev.mu.Lock()
	s := ev.vals[key]
	if s == nil {
		s = &shared{}
		ev.vals[key] = s
	}
	ev.mu.Unlock()
	s.once.Do(func() { s.val, s.err = read() })
	v, _ := s.val.(T)
	return v, s.err
}
