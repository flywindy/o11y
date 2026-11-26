// Package nats provides a T2 facade over otel-nats: a tracing-aware NATS
// connection wrapper that wires the o11y SDK's TracerProvider and Propagator
// into otelnats / oteljetstream. All NATS connections in a service should go
// through this package so that trace context propagates across publishers and
// subscribers without touching global OpenTelemetry state.
//
// Tier: T2 facade over github.com/akira-core/instrumentation-go/otel-nats
package nats
