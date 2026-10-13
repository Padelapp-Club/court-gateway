package driver

import (
	"context"
	"sync"
)

// An evaluation is one pass over the clubs of an installation: one
// readiness round, or one application of the hub's config. Its clubs share
// one read of the account (Shared), made under the round's own context,
// not under that of the club that happens to ask first (which carries that
// club's time limit). Nothing is kept after it: the only state a driver
// keeps across requests is its login.

type evaluationKey struct{}

type evaluation struct {
	ctx  context.Context
	once sync.Once
	val  any
	err  error
}

// WithEvaluation starts an evaluation, or keeps the one ctx already has.
func WithEvaluation(ctx context.Context) context.Context {
	if _, ok := ctx.Value(evaluationKey{}).(*evaluation); ok {
		return ctx
	}
	ev := &evaluation{}
	ev.ctx = context.WithValue(ctx, evaluationKey{}, ev)
	return ev.ctx
}

// Shared returns read's result, made once per evaluation with the
// evaluation's context and shared by every caller in it; outside an
// evaluation it just reads with ctx.
func Shared[T any](ctx context.Context, read func(context.Context) (T, error)) (T, error) {
	ev, ok := ctx.Value(evaluationKey{}).(*evaluation)
	if !ok {
		return read(ctx)
	}
	ev.once.Do(func() { ev.val, ev.err = read(ev.ctx) })
	v, _ := ev.val.(T)
	return v, ev.err
}
