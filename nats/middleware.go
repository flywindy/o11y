package nats

import (
	"context"
	"net/textproto"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/propagation"
)

// headerCarrier adapts nats.Header to propagation.TextMapCarrier using
// nats.Header's own Get/Set, which are case-sensitive (see nats.go's Header
// doc comment). This matters because otel-nats writes W3C propagator keys
// ("traceparent", "tracestate", "baggage") verbatim, in whatever exact case
// the propagator passes in — lowercase, per the W3C spec. The standard
// go.opentelemetry.io/otel/propagation.HeaderCarrier is backed by
// http.Header, which canonicalizes keys to MIME form ("Traceparent") on both
// Get and Set; using it here would silently fail to read back headers
// written by otel-nats's own Publish/Respond path (or by this Inject
// itself), because the canonicalized lookup key would never match the
// literal lowercase key actually stored in the map.
type headerCarrier struct {
	h nats.Header
}

// Compile-time check that headerCarrier implements propagation.ValuesGetter,
// the same way propagation.HeaderCarrier does — see Values for why this
// matters.
var _ propagation.ValuesGetter = headerCarrier{}

// Get looks up key verbatim first (the literal-case form otel-nats and this
// package's own Inject write), then falls back to the MIME-canonical form.
// The fallback exists for already-persisted JetStream messages written by a
// pre-fix SDK version, whose Inject went through propagation.HeaderCarrier
// (http.Header-backed, canonicalizing "traceparent" to "Traceparent") before
// this case-sensitivity fix landed: without it, a message sitting unconsumed
// in a durable stream across an SDK upgrade would silently lose its trace
// link the moment this Extract runs, even though the header is present under
// its canonicalized key.
func (c headerCarrier) Get(key string) string {
	if v := c.h.Get(key); v != "" {
		return v
	}
	return c.h.Get(textproto.CanonicalMIMEHeaderKey(key))
}

// Values returns every value stored under key, checked the same
// verbatim-then-canonical way as Get. propagation.Baggage.Extract type-asserts
// the carrier against propagation.ValuesGetter and, when it's implemented,
// reads every repeated header instance for "baggage" instead of only the
// first (propagation.TextMapCarrier.Get returns a single string). Without
// this method, headerCarrier would silently fall back to single-value
// extraction and drop any baggage members carried on a second or later
// "baggage" header instance — a real (if rare) capability regression from
// propagation.HeaderCarrier, which implements ValuesGetter over http.Header.
func (c headerCarrier) Values(key string) []string {
	if v := c.h.Values(key); len(v) > 0 {
		return v
	}
	return c.h.Values(textproto.CanonicalMIMEHeaderKey(key))
}

func (c headerCarrier) Set(key, value string) { c.h.Set(key, value) }

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c.h))
	for k := range c.h {
		keys = append(keys, k)
	}
	return keys
}

// Inject injects the tracing context from ctx into the NATS message headers
// using the provided propagator. Pass sdk.Propagator obtained from o11y.Init.
func Inject(ctx context.Context, prop propagation.TextMapPropagator, msg *nats.Msg) {
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	prop.Inject(ctx, headerCarrier{h: msg.Header})
}

// Extract extracts the tracing context from the NATS message headers
// using the provided propagator and returns an enriched context.
// If the message has no headers the original ctx is returned unchanged.
func Extract(ctx context.Context, prop propagation.TextMapPropagator, msg *nats.Msg) context.Context {
	if msg.Header == nil {
		return ctx
	}
	return prop.Extract(ctx, headerCarrier{h: msg.Header})
}
