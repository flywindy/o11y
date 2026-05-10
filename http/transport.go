package http

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// NewTransport wraps base with otelhttp client instrumentation.
//
// If base is nil, http.DefaultTransport is used. The SDK providers and
// propagator are wired explicitly to avoid reading OpenTelemetry globals.
func NewTransport(base http.RoundTripper, tp trace.TracerProvider, mp metric.MeterProvider, prop propagation.TextMapPropagator, opts ...Option) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	all := []otelhttp.Option{otelhttp.WithTracerProvider(tp), otelhttp.WithMeterProvider(mp), otelhttp.WithPropagators(prop)}
	all = append(all, opts...)
	return otelhttp.NewTransport(base, all...)
}
