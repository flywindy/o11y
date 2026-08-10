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
//
// This runs on every emitted record, so the ordinary path — a record with no
// LogValuer and no key that shadows a whitelisted one — allocates nothing
// beyond the attribute slice the whitelist already returns. Shadowed entries
// are struck out of that slice in place (it is freshly built per call and
// owned here) instead of being tracked in a per-record key set.
func (h *BaggageHandler) Handle(ctx context.Context, r slog.Record) error {
	baggageAttrs := h.whitelist.LogAttrsFromContext(ctx)
	if len(baggageAttrs) == 0 {
		return h.Handler.Handle(ctx, r)
	}

	// Attrs installed by WithAttrs at this group level win over baggage. The
	// preset set is only read here, so it is consulted in place rather than
	// copied per record.
	for i, attr := range baggageAttrs {
		if _, exists := h.presetKeys[attr.Key]; exists {
			baggageAttrs[i] = slog.Attr{}
		}
	}

	// Resolving rebuilds the record, so only records that actually carry a
	// LogValuer pay for it. Detection walks values without resolving them,
	// which keeps the resolve-exactly-once contract intact for LogValuers
	// whose value changes between calls.
	if recordHasLogValuer(r) {
		r = resolveRecord(r)
	}
	r.Attrs(func(attr slog.Attr) bool {
		suppressShadowed(attr, baggageAttrs)
		return true
	})
	for _, attr := range baggageAttrs {
		if attr.Equal(slog.Attr{}) {
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

// recordHasLogValuer reports whether any attr on r, at any depth, still needs
// resolving. It inspects Kind only and never calls Resolve.
func recordHasLogValuer(r slog.Record) bool {
	found := false
	r.Attrs(func(attr slog.Attr) bool {
		if attrHasLogValuer(attr) {
			found = true
			return false
		}
		return true
	})
	return found
}

func attrHasLogValuer(attr slog.Attr) bool {
	switch attr.Value.Kind() {
	case slog.KindLogValuer:
		return true
	case slog.KindGroup:
		for _, child := range attr.Value.Group() {
			if attrHasLogValuer(child) {
				return true
			}
		}
	}
	return false
}

// resolveRecord returns a copy of r with every LogValuer resolved exactly once.
func resolveRecord(r slog.Record) slog.Record {
	resolved := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(attr slog.Attr) bool {
		resolvedAttr, _ := resolveAttr(attr)
		resolved = append(resolved, resolvedAttr)
		return true
	})
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(resolved...)
	return out
}

// suppressShadowed zeroes every baggage attr whose key is already occupied by
// attr at the record's own group level. Only same-level keys shadow baggage,
// so it recurses into empty-key groups (which slog inlines) but not into named
// ones.
func suppressShadowed(attr slog.Attr, baggageAttrs []slog.Attr) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	if attr.Key != "" {
		for i := range baggageAttrs {
			if baggageAttrs[i].Key == attr.Key {
				baggageAttrs[i] = slog.Attr{}
			}
		}
		return
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, child := range attr.Value.Group() {
			suppressShadowed(child, baggageAttrs)
		}
	}
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
