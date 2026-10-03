package log

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
)

// ErrorStringHandler renders error-valued attributes as their Error() string
// before handing records to the wrapped handler.
//
// The otelslog bridge from v0.20 moves an error-valued attribute out of the
// attributes and into the log record's error slot, which the log SDK exports
// as exception.message / exception.type: slog.Any("error", err) would reach
// the OTLP pipeline without its "error" key, and of several error attributes
// only the last would survive. Earlier bridges exported the error as a string
// attribute under its own key; this handler keeps that shape. It converts an
// error value at any depth, top level and inside groups alike; inside keyed
// groups the bridge would export the same string itself, but converting it
// here means an Error method that panics (a typed nil pointer, say) is
// recovered as slog's own handlers recover it, instead of panicking inside
// the bridge. A LogValuer is not resolved here, so it is not resolved twice.
type ErrorStringHandler struct {
	slog.Handler
}

// NewErrorStringHandler wraps h. A nil h returns nil.
func NewErrorStringHandler(h slog.Handler) slog.Handler {
	if h == nil {
		return nil
	}
	return &ErrorStringHandler{Handler: h}
}

// Handle passes r on with every error value replaced by its Error() string.
// A record without one is passed on as it is; the check allocates nothing.
func (h *ErrorStringHandler) Handle(ctx context.Context, r slog.Record) error {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		found = hasError(a)
		return !found
	})
	if !found {
		return h.Handler.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		converted, _ := stringifyErrors(a)
		out.AddAttrs(converted)
		return true
	})
	return h.Handler.Handle(ctx, out)
}

// WithAttrs converts attrs the same way before passing them on.
func (h *ErrorStringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	converted := make([]slog.Attr, len(attrs))
	changed := false
	for i, a := range attrs {
		var ok bool
		converted[i], ok = stringifyErrors(a)
		changed = changed || ok
	}
	if !changed {
		converted = attrs
	}
	return &ErrorStringHandler{Handler: h.Handler.WithAttrs(converted)}
}

// WithGroup returns a handler that opens the group on the wrapped handler.
func (h *ErrorStringHandler) WithGroup(name string) slog.Handler {
	return &ErrorStringHandler{Handler: h.Handler.WithGroup(name)}
}

// hasError reports whether a holds an error value at any depth of group
// values.
func hasError(a slog.Attr) bool {
	switch a.Value.Kind() {
	case slog.KindAny:
		_, ok := a.Value.Any().(error)
		return ok
	case slog.KindGroup:
		for _, g := range a.Value.Group() {
			if hasError(g) {
				return true
			}
		}
	}
	return false
}

// stringifyErrors returns a with each error value, at any depth of group
// values, replaced by its Error() string, and whether anything was replaced.
func stringifyErrors(a slog.Attr) (slog.Attr, bool) {
	switch a.Value.Kind() {
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, errorString(err)), true
		}
	case slog.KindGroup:
		group := a.Value.Group()
		var converted []any
		for i, g := range group {
			c, ok := stringifyErrors(g)
			if ok && converted == nil {
				converted = make([]any, len(group))
				for j := range i {
					converted[j] = group[j]
				}
			}
			if converted != nil {
				converted[i] = c
			}
		}
		if converted != nil {
			return slog.Group(a.Key, converted...), true
		}
	}
	return a, false
}

// errorString returns err.Error(). When Error panics it returns "<nil>" for
// a nil pointer receiver and a panic description otherwise, as slog's own
// handlers do, so a bad error value cannot take the logging call down.
func errorString(err error) (s string) {
	defer func() {
		if r := recover(); r != nil {
			if v := reflect.ValueOf(err); v.Kind() == reflect.Pointer && v.IsNil() {
				s = "<nil>"
				return
			}
			s = fmt.Sprintf("!PANIC: %v", r)
		}
	}()
	return err.Error()
}
