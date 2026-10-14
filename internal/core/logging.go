package core

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
)

// sensitiveKeyParts: an attribute whose key contains any of these is
// replaced by "[redacted]" whatever its value.
var sensitiveKeyParts = []string{
	"password", "secret", "token", "authorization", "key",
	"note", "comment", "client", "phone", "email", "name", "title",
}

const redacted = "[redacted]"

// NewLogger builds a JSON logger that redacts sensitive attributes and every
// occurrence of the given secret values.
func NewLogger(w io.Writer, level slog.Level, secrets []string) *slog.Logger {
	inner := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(NewRedactingHandler(inner, secrets))
}

// RedactingHandler wraps a slog.Handler.
type RedactingHandler struct {
	inner   slog.Handler
	secrets *secretList // shared with the handlers derived from it
}

type secretList struct {
	mu sync.RWMutex
	v  []string
}

func (l *secretList) add(s string) {
	if len(s) < 4 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !slices.Contains(l.v, s) {
		l.v = append(l.v, s)
	}
}

// NewRedactingHandler wraps inner.
func NewRedactingHandler(inner slog.Handler, secrets []string) *RedactingHandler {
	h := &RedactingHandler{inner: inner, secrets: &secretList{}}
	for _, v := range secrets {
		h.secrets.add(v)
	}
	return h
}

// AddSecret makes a logger from NewLogger redact s from now on, as it
// does its secrets (a password the running service takes from .env).
func AddSecret(log *slog.Logger, s string) {
	if h, ok := log.Handler().(*RedactingHandler); ok {
		h.secrets.add(s)
	}
}

func (h *RedactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, h.scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(h.attr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = h.attr(a)
	}
	return &RedactingHandler{inner: h.inner.WithAttrs(out), secrets: h.secrets}
}

func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{inner: h.inner.WithGroup(name), secrets: h.secrets}
}

func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, p := range sensitiveKeyParts {
		if strings.Contains(lk, p) {
			return true
		}
	}
	return false
}

func (h *RedactingHandler) attr(a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		g := v.Group()
		out := make([]any, len(g))
		for i, ga := range g {
			out[i] = h.attr(ga)
		}
		return slog.Group(a.Key, out...)
	case slog.KindString:
		return slog.String(a.Key, h.scrub(v.String()))
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, h.scrub(err.Error()))
		}
		return slog.String(a.Key, h.scrub(v.String()))
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}

func (h *RedactingHandler) scrub(s string) string {
	h.secrets.mu.RLock()
	defer h.secrets.mu.RUnlock()
	for _, sec := range h.secrets.v {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, redacted)
		}
	}
	return s
}
