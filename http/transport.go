package http

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/flywindy/o11y/internal/providers"
)

// NewTransport wraps base with otelhttp client instrumentation.
//
// If base is nil, http.DefaultTransport is used. The SDK providers and
// propagator are wired explicitly to avoid reading OpenTelemetry globals.
//
// A nil tp, mp or prop is replaced by one that does nothing: a no-op tracer
// provider (no client span), a no-op meter provider (no metrics), an empty
// propagator (no traceparent or baggage header on the outgoing request).
// NewTransport has no error to return, and otelhttp reads a nil as "not set"
// and uses the OpenTelemetry global instead, so passing one through would
// select whatever another library installed globally. Pass
// obs.TracerProvider(), obs.MeterProvider() and obs.Propagator, which are
// never nil.
func NewTransport(base http.RoundTripper, tp trace.TracerProvider, mp metric.MeterProvider, prop propagation.TextMapPropagator, opts ...Option) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	tp, mp, prop = providers.OrNoop(tp, mp, prop)
	all := []otelhttp.Option{otelhttp.WithTracerProvider(tp), otelhttp.WithMeterProvider(mp), otelhttp.WithPropagators(prop)}
	all = append(all, opts...)
	return otelhttp.NewTransport(base, all...)
}
