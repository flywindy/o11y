package log

import (
	"context"
	"errors"
	"log/slog"
)

// MultiHandler fans out each slog record to multiple slog.Handler implementations.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler returns a slog.Handler that forwards records to all provided
// handlers. Enabled returns true if at least one handler is enabled for the
// given level. Handle delivers records only to handlers that report themselves
// as enabled, collecting and joining any errors.
func NewMultiHandler(handlers ...slog.Handler) slog.Handler {
	h := make([]slog.Handler, len(handlers))
	copy(h, handlers)
	return &MultiHandler{handlers: h}
}

// Enabled reports whether at least one of the underlying handlers is enabled
// for the given level.
func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle forwards r to every handler that is Enabled for r's level. The record
// is cloned before each delivery so handlers cannot interfere with each other.
// All errors are collected and returned as a joined error.
func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// WithAttrs returns a new MultiHandler whose underlying handlers each have the
// given attributes pre-populated.
func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: handlers}
}

// WithGroup returns a new MultiHandler where each underlying handler has the
// given group name applied.
func (m *MultiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: handlers}
}
