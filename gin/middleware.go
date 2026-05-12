package gin

import (
	ginframework "github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Middleware returns the canonical gin middleware chain for o11y tracing.
//
// The returned slice is intended to be spread into Engine.Use:
//
//	r.Use(o11ygin.Middleware(serviceName, tp, mp, prop)...)
//
// The SDK's tracer provider, meter provider, and propagator are passed
// explicitly so otelgin never falls back to process-wide OpenTelemetry globals.
// User options are appended last so callers can override otelgin behavior.
func Middleware(service string, tp trace.TracerProvider, mp metric.MeterProvider, prop propagation.TextMapPropagator, opts ...Option) []ginframework.HandlerFunc {
	base := []otelgin.Option{
		otelgin.WithTracerProvider(tp),
		otelgin.WithMeterProvider(mp),
		otelgin.WithPropagators(prop),
	}
	base = append(base, opts...)
	return []ginframework.HandlerFunc{
		otelgin.Middleware(service, base...),
		ErrorRecorder(),
	}
}
