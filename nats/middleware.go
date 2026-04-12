package nats

import (
	"context"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/propagation"
)

// Inject injects the tracing context from ctx into the NATS message headers
// using the provided propagator. Pass sdk.Propagator obtained from o11y.Init.
func Inject(ctx context.Context, prop propagation.TextMapPropagator, msg *nats.Msg) {
	if msg.Header == nil {
		msg.Header = make(nats.Header)
	}
	prop.Inject(ctx, propagation.HeaderCarrier(msg.Header))
}

// Extract extracts the tracing context from the NATS message headers
// using the provided propagator and returns an enriched context.
// If the message has no headers the original ctx is returned unchanged.
func Extract(ctx context.Context, prop propagation.TextMapPropagator, msg *nats.Msg) context.Context {
	if msg.Header == nil {
		return ctx
	}
	return prop.Extract(ctx, propagation.HeaderCarrier(msg.Header))
}
