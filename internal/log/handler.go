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
	whitelist  baggageattrs.Whitelist
	presetKeys map[string]struct{}
}

// NewBaggageHandler returns a slog handler that copies whitelisted baggage
// members onto every log record before delegating to base.
func NewBaggageHandler(base slog.Handler, whitelist baggageattrs.Whitelist) slog.Handler {
	return &BaggageHandler{Handler: base, whitelist: whitelist}
}

// Handle implements slog.Handler.Handle and adds whitelisted baggage attrs.
func (h *BaggageHandler) Handle(ctx context.Context, r slog.Record) error {
	baggageAttrs := h.whitelist.LogAttrsFromContext(ctx)
	if len(baggageAttrs) == 0 {
		return h.Handler.Handle(ctx, r)
	}

	resolved := make([]slog.Attr, 0, r.NumAttrs())
	present := cloneKeySet(h.presetKeys)
	changed := false
	r.Attrs(func(attr slog.Attr) bool {
		resolvedAttr, attrChanged := resolveAttr(attr)
		changed = changed || attrChanged
		resolved = append(resolved, resolvedAttr)
		collectSameLevelKeys(resolvedAttr, present)
		return true
	})
	if changed {
		r = slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
		r.AddAttrs(resolved...)
	}
	for _, attr := range baggageAttrs {
		if _, exists := present[attr.Key]; exists {
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
	resolved := make([]slog.Attr, len(attrs))
	presetKeys := cloneKeySet(h.presetKeys)
	for i, attr := range attrs {
		resolved[i], _ = resolveAttr(attr)
		collectSameLevelKeys(resolved[i], presetKeys)
	}
	return &BaggageHandler{
		Handler:    h.Handler.WithAttrs(resolved),
		whitelist:  h.whitelist,
		presetKeys: presetKeys,
	}
}

// WithGroup implements slog.Handler.WithGroup by delegating to the wrapped handler.
func (h *BaggageHandler) WithGroup(name string) slog.Handler {
	return &BaggageHandler{
		Handler:   h.Handler.WithGroup(name),
		whitelist: h.whitelist,
	}
}

func resolveAttr(attr slog.Attr) (slog.Attr, bool) {
	changed := false
	if attr.Value.Kind() == slog.KindLogValuer {
		attr.Value = attr.Value.Resolve()
		changed = true
	}
	if attr.Value.Kind() != slog.KindGroup {
		return attr, changed
	}
	children := attr.Value.Group()
	resolved := make([]slog.Attr, len(children))
	for i, child := range children {
		var childChanged bool
		resolved[i], childChanged = resolveAttr(child)
		changed = changed || childChanged
	}
	if changed {
		attr.Value = slog.GroupValue(resolved...)
	}
	return attr, changed
}

func collectSameLevelKeys(attr slog.Attr, keys map[string]struct{}) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	if attr.Key != "" {
		keys[attr.Key] = struct{}{}
		return
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, child := range attr.Value.Group() {
			collectSameLevelKeys(child, keys)
		}
	}
}

func cloneKeySet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for key := range in {
		out[key] = struct{}{}
	}
	return out
}
