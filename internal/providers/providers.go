// Package providers holds the one rule the integration facades share for a nil
// tracer provider, meter provider or propagator.
//
// The rule: a constructor that can return an error rejects nil; one that
// cannot substitutes an implementation that does nothing. In neither case is
// the OpenTelemetry global consulted (ADR 0003 keeps SDK code off the
// globals). This package is the second half. It
// exists because the upstream instrumentation libraries the facades wrap read
// a nil argument as "not set" and fall back to otel.GetTracerProvider,
// otel.GetMeterProvider and otel.GetTextMapPropagator, so passing a nil
// through is what selects the globals.
package providers

import (
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// OrNoop returns tp, mp and prop with each nil one replaced by an
// implementation that does nothing: a no-op tracer provider, a no-op meter
// provider, and an empty composite propagator, which neither injects nor
// extracts.
//
// It is for constructors that have no error to return. The values are never
// nil afterwards, so handing them to an upstream library cannot make it reach
// for a process-wide global.
//
// A nil propagator becomes an empty one rather than W3C Trace Context: the
// caller did not ask for any header to be written, and choosing one for them
// would send traceparent to a server they may not have meant to trace, while
// dropping the baggage the SDK's own propagator carries.
func OrNoop(
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	prop propagation.TextMapPropagator,
) (trace.TracerProvider, metric.MeterProvider, propagation.TextMapPropagator) {
	if tp == nil {
		tp = tracenoop.NewTracerProvider()
	}
	if mp == nil {
		mp = metricnoop.NewMeterProvider()
	}
	if prop == nil {
		prop = propagation.NewCompositeTextMapPropagator()
	}
	return tp, mp, prop
}
