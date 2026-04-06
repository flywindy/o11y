package log

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// OtelSlogHandler is a custom slog.Handler that wraps another handler and
// injects trace_id and span_id into log records when a valid trace is present in the context.
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
			slog.String("trace_id", span.SpanContext().TraceID().String()),
			slog.String("span_id", span.SpanContext().SpanID().String()),
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
