package http

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/flywindy/o11y/internal/providers"
)

const defaultServerOperation = "http.server"

// NewServerHandler wraps next with otelhttp server instrumentation.
//
// The SDK's tracer provider, meter provider, and propagator are always passed
// explicitly so otelhttp never falls back to process-wide OpenTelemetry
// globals. User options are appended last so callers can override otelhttp
// behavior such as span names, filters, and public endpoint handling.
//
// A nil tp, mp or prop is replaced by one that does nothing: a no-op tracer
// provider (no server span), a no-op meter provider (no metrics), an empty
// propagator (the caller's trace context is not read from the request).
// NewServerHandler has no error to return, and otelhttp reads a nil as "not
// set" and uses the OpenTelemetry global instead, so passing one through would
// select whatever another library installed globally. Pass
// obs.TracerProvider(), obs.MeterProvider() and obs.Propagator, which are
// never nil.
func NewServerHandler(next http.Handler, tp trace.TracerProvider, mp metric.MeterProvider, prop propagation.TextMapPropagator, opts ...Option) http.Handler {
	tp, mp, prop = providers.OrNoop(tp, mp, prop)
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
