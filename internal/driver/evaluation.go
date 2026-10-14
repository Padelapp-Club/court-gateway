package driver

import "context"

// An evaluation is one pass over the clubs of an installation: one
// readiness round, or one application of the hub's config. Its clubs share
// one read of the account (a driver keeps it for that evaluation), made
// under the evaluation's own context, not under that of the club that
// happens to ask first (which carries that club's time limit).

type evaluationKey struct{}

type evaluation struct{ ctx context.Context }

// WithEvaluation starts an evaluation, or keeps the one ctx already has.
func WithEvaluation(ctx context.Context) context.Context {
	if Evaluation(ctx) != nil {
		return ctx
	}
	ev := &evaluation{}
	ev.ctx = context.WithValue(ctx, evaluationKey{}, ev)
	return ev.ctx
}

// Evaluation is the context of the evaluation ctx is in (WithEvaluation),
// nil outside one: it names the evaluation, and a read its clubs share is
// made under it.
func Evaluation(ctx context.Context) context.Context {
	if ev, ok := ctx.Value(evaluationKey{}).(*evaluation); ok {
		return ev.ctx
	}
	return nil
}
