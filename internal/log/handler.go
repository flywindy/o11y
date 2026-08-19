// Package log encapsulates the OTel LoggerProvider and the slog handler
// chain (multi-handler, OTel-aware trace context injector) used by the
// top-level o11y SDK. Nothing in this package touches global slog or OTel
// state — the SDK wires returned types into the application.
package log

import (
	"context"
	"log/slog"
	"slices"

	"github.com/flywindy/o11y/internal/baggageattrs"
	"go.opentelemetry.io/otel/trace"
)

// Trace correlation field names. Log pipelines (Loki, Fluentd) index these as
// top-level fields, so they are part of the SDK's external contract.
const (
	traceIDKey = "traceId"
	spanIDKey  = "spanId"
)

// OtelSlogHandler is a custom slog.Handler that wraps another handler and
// injects traceId and spanId into log records when a valid trace is present in
// the context.
//
// The identifiers are always written at the record's top level, even when the
// logger has opened groups via slog.Logger.WithGroup. That guarantee is why
// this handler does not forward WithGroup to the handler it wraps: attributes a
// handler adds to a record are qualified into whatever groups are open beneath
// it, so delegating the group would bury traceId inside it
// ({"req":{"traceId":…}}) and silently break every log-to-trace query keyed on
// the top-level field. Instead the open groups are recorded here and re-applied
// to the record's own attributes at Handle time, leaving the top level free.
//
// A group opened on the base handler before it was passed to NewOTelHandler is
// outside this handler's control and still nests the identifiers.
type OtelSlogHandler struct {
	base slog.Handler
	// groups holds the currently open groups, outermost first. It is nil for
	// the common ungrouped logger, which keeps Handle on its original path.
	groups []groupFrame
}

// groupFrame is one open group: its name, plus the attributes WithAttrs
// installed inside it after it was opened. Those attributes are held here
// rather than preformatted by the base handler so they nest with the record's
// own attributes instead of being written above the group.
type groupFrame struct {
	name  string
	attrs []slog.Attr
}

// NewOTelHandler returns a new OtelSlogHandler wrapping the provided handler.
func NewOTelHandler(base slog.Handler) slog.Handler {
	return &OtelSlogHandler{base: base}
}

// Handle implements slog.Handler.Handle and adds trace/span IDs to the record.
func (h *OtelSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if len(h.groups) > 0 {
		return h.base.Handle(ctx, h.regroup(r, sc))
	}
	if !sc.IsValid() {
		return h.base.Handle(ctx, r)
	}
	// Clone before mutating: the record belongs to the caller and its attribute
	// array may be shared with a sibling handler, which the slog handler
	// contract forbids writing through.
	r = r.Clone()
	r.AddAttrs(
		slog.String(traceIDKey, sc.TraceID().String()),
		slog.String(spanIDKey, sc.SpanID().String()),
	)
	return h.base.Handle(ctx, r)
}

// regroup rebuilds r so the trace identifiers sit at the top level and the
// record's own attributes are nested inside the groups this handler holds
// open. A group that ends up with no attributes collapses entirely, matching
// how slog elides an empty group.
func (h *OtelSlogHandler) regroup(r slog.Record, sc trace.SpanContext) slog.Record {
	nested := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(attr slog.Attr) bool {
		nested = append(nested, attr)
		return true
	})

	for i := len(h.groups) - 1; i >= 0; i-- {
		frame := h.groups[i]
		inner := nested
		if len(frame.attrs) > 0 {
			inner = make([]slog.Attr, 0, len(frame.attrs)+len(nested))
			inner = append(inner, frame.attrs...)
			inner = append(inner, nested...)
		}
		if len(inner) == 0 {
			nested = nil
			continue
		}
		nested = []slog.Attr{{Key: frame.name, Value: slog.GroupValue(inner...)}}
	}

	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	if sc.IsValid() {
		out.AddAttrs(
			slog.String(traceIDKey, sc.TraceID().String()),
			slog.String(spanIDKey, sc.SpanID().String()),
		)
	}
	out.AddAttrs(nested...)
	return out
}

// Enabled implements slog.Handler.Enabled by delegating to the wrapped handler.
func (h *OtelSlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

// WithAttrs implements slog.Handler.WithAttrs. With no group open the attrs go
// straight to the base handler so it can preformat them; inside a group they
// are held on the innermost frame so they nest with the record's attributes.
func (h *OtelSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	if len(h.groups) == 0 {
		return &OtelSlogHandler{base: h.base.WithAttrs(attrs)}
	}
	// Resolve now, once, as the base handler would when it preformats attrs at
	// derivation time. Held attrs are re-attached to every record, so an
	// unresolved LogValuer would otherwise be resolved once per log call —
	// making a stateful or expensive value behave differently purely because a
	// group was open.
	resolved := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		resolved[i], _ = resolveAttr(attr)
	}
	groups := slices.Clone(h.groups)
	last := len(groups) - 1
	// Clip so appending cannot write into an array a sibling handler derived
	// from the same parent is also holding.
	groups[last].attrs = append(slices.Clip(groups[last].attrs), resolved...)
	return &OtelSlogHandler{base: h.base, groups: groups}
}

// WithGroup implements slog.Handler.WithGroup by recording the group here
// rather than opening it on the base handler, so that Handle can keep the
// trace identifiers above it. An empty name is a no-op, per the slog contract.
func (h *OtelSlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]groupFrame, len(h.groups), len(h.groups)+1)
	copy(groups, h.groups)
	return &OtelSlogHandler{base: h.base, groups: append(groups, groupFrame{name: name})}
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
	//
	// owned tracks whether r is a record this call may write to. The record
	// handed in belongs to the caller and may share its attribute array with a
	// sibling handler, so it is cloned before the first AddAttrs; a resolved
	// record is freshly built here and is already ours.
	owned := false
	if recordHasLogValuer(r) {
		r = resolveRecord(r)
		owned = true
	}
	r.Attrs(func(attr slog.Attr) bool {
		suppressShadowed(attr, baggageAttrs)
		return true
	})
	for _, attr := range baggageAttrs {
		if attr.Equal(slog.Attr{}) {
			continue
		}
		if !owned {
			r = r.Clone()
			owned = true
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

// WithGroup implements slog.Handler.WithGroup by delegating to the wrapped
// handler. An empty name is a no-op, per the slog contract; forwarding it would
// also discard presetKeys for a group that was never opened.
func (h *BaggageHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
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
