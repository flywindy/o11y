// Package log encapsulates the OTel LoggerProvider and the slog handler
// chain (multi-handler, OTel-aware trace context injector) used by the
// top-level o11y SDK. Nothing in this package touches global slog or OTel
// state — the SDK wires returned types into the application.
package log

import (
	"context"
	"log/slog"

	"github.com/flywindy/o11y/internal/baggageattrs"
	"go.opentelemetry.io/otel/trace"
)

// OtelSlogHandler is a custom slog.Handler that wraps another handler and
// injects traceId and spanId into log records when a valid trace is present in the context.
type OtelSlogHandler struct {
	slog.Handler
}

// NewOTelHandler returns a new OtelSlogHandler wrapping the provided handler.
func NewOTelHandler(base slog.Handler) slog.Handler {
	return &OtelSlogHandler{Handler: base}
}

// Handle implements slog.Handler.Handle and adds trace/span IDs to the record.
func (h *OtelSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().IsValid() {
		r.AddAttrs(
			slog.String("traceId", span.SpanContext().TraceID().String()),
			slog.String("spanId", span.SpanContext().SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// Enabled implements slog.Handler.Enabled by delegating to the wrapped handler.
func (h *OtelSlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(ctx, level)
}

// WithAttrs implements slog.Handler.WithAttrs by delegating to the wrapped handler.
func (h *OtelSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &OtelSlogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.WithGroup by delegating to the wrapped handler.
func (h *OtelSlogHandler) WithGroup(name string) slog.Handler {
	return &OtelSlogHandler{Handler: h.Handler.WithGroup(name)}
}

// BaggageHandler wraps another handler and materializes whitelisted baggage
// members as log record attributes.
type BaggageHandler struct {
	slog.Handler
	hasUserNameAttr bool
}

// NewBaggageHandler returns a slog handler that copies whitelisted baggage
// members onto every log record before delegating to base.
func NewBaggageHandler(base slog.Handler) slog.Handler {
	return &BaggageHandler{Handler: base}
}

// Handle implements slog.Handler.Handle and adds whitelisted baggage attrs.
func (h *BaggageHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, attr := range baggageattrs.LogAttrsFromContext(ctx) {
		if h.hasUserNameAttr && attr.Key == baggageattrs.UserNameKey {
			continue
		}
		if recordHasAttr(r, attr.Key) {
			continue
		}
		r.AddAttrs(attr)
	}
	return h.Handler.Handle(ctx, r)
}

// Enabled implements slog.Handler.Enabled by delegating to the wrapped handler.
func (h *BaggageHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(ctx, level)
}

// WithAttrs implements slog.Handler.WithAttrs by delegating to the wrapped handler.
func (h *BaggageHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hasUserNameAttr := h.hasUserNameAttr
	for _, attr := range attrs {
		if attr.Key == baggageattrs.UserNameKey {
			hasUserNameAttr = true
			break
		}
	}
	return &BaggageHandler{
		Handler:         h.Handler.WithAttrs(attrs),
		hasUserNameAttr: hasUserNameAttr,
	}
}

// WithGroup implements slog.Handler.WithGroup by delegating to the wrapped handler.
func (h *BaggageHandler) WithGroup(name string) slog.Handler {
	return &BaggageHandler{
		Handler:         h.Handler.WithGroup(name),
		hasUserNameAttr: h.hasUserNameAttr,
	}
}

func recordHasAttr(r slog.Record, key string) bool {
	var ok bool
	r.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			ok = true
			return false
		}
		return true
	})
	return ok
}
