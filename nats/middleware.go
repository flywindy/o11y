package nats

import (
	"context"

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

func (c headerCarrier) Get(key string) string { return c.h.Get(key) }

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
