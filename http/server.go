package http

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const defaultServerOperation = "http.server"

// NewServerHandler wraps next with otelhttp server instrumentation.
//
// The SDK's tracer provider, meter provider, and propagator are always passed
// explicitly so otelhttp never falls back to process-wide OpenTelemetry
// globals. User options are appended last so callers can override otelhttp
// behavior such as span names, filters, and public endpoint handling.
func NewServerHandler(next http.Handler, tp trace.TracerProvider, mp metric.MeterProvider, prop propagation.TextMapPropagator, opts ...Option) http.Handler {
	base := []otelhttp.Option{
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithMeterProvider(mp),
		otelhttp.WithPropagators(prop),
		otelhttp.WithSpanNameFormatter(defaultServerSpanName),
	}
	base = append(base, opts...)
	return otelhttp.NewHandler(next, defaultServerOperation, base...)
}

func defaultServerSpanName(_ string, r *http.Request) string {
	if r.Pattern == "" {
		return r.Method
	}
	if strings.Contains(r.Pattern, " ") {
		return r.Pattern
	}
	return r.Method + " " + r.Pattern
}
