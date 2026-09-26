package gin

import (
	ginframework "github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Middleware returns the canonical gin middleware chain for o11y tracing:
// otelgin, which opens the server span and records each gin.Context.Errors
// entry as an exception event, followed by a handler that adds one "gin.error"
// event per entry carrying gin.error.type and gin.error.message, the latter
// equal to the exception.message of that entry's exception event. On a
// request otelgin filters out, the second handler records the exception events
// itself. Do not add ErrorRecorder after it.
//
// The returned slice is intended to be spread into Engine.Use:
//
//	r.Use(o11ygin.Middleware(serviceName, tp, mp, prop)...)
//
// The SDK's tracer provider, meter provider, and propagator are passed
// explicitly so otelgin never falls back to process-wide OpenTelemetry globals.
// User options are appended last for behavior customizations; provider and
// propagator overrides are intentionally not exposed by this facade.
func Middleware(service string, tp trace.TracerProvider, mp metric.MeterProvider, prop propagation.TextMapPropagator, opts ...Option) []ginframework.HandlerFunc {
	base := []otelgin.Option{
		otelgin.WithTracerProvider(tp),
		otelgin.WithMeterProvider(mp),
		otelgin.WithPropagators(prop),
	}
	base = append(base, applyOptions(opts)...)
	instrument := otelgin.Middleware(service, base...)
	return []ginframework.HandlerFunc{
		func(c *ginframework.Context) {
			c.Set(incomingSpanKey, trace.SpanContextFromContext(c.Request.Context()))
			instrument(c)
		},
		errorTypeEvents(),
	}
}
