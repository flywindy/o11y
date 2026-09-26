package gin

import (
	ginframework "github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Middleware returns the canonical gin middleware chain for o11y tracing:
// otelgin, which opens the server span, followed by a handler that records
// each gin.Context.Errors entry as one exception event carrying
// gin.error.type. otelgin would record each entry again on its own; the chain
// keeps it from doing so, and every middleware outside the chain still sees
// c.Errors as the handlers left them. A request excluded by WithFilter or
// WithSkipPaths is not instrumented by the chain at all. Do not add
// ErrorRecorder after it.
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
	options := applyOptions(opts)
	base = append(base, options.otel...)
	var tracedKey *chainKey
	if filters := options.filters; len(filters) > 0 {
		tracedKey = &chainKey{name: "o11y.gin.traced"}
		base = append(base, otelgin.WithGinFilter(func(c *ginframework.Context) bool {
			for _, filter := range filters {
				if !filter(c.Request) {
					return false
				}
			}
			c.Set(tracedKey, true)
			return true
		}))
	}
	hiddenKey := &chainKey{name: "o11y.gin.hidden_errors"}
	instrument := otelgin.Middleware(service, base...)
	return []ginframework.HandlerFunc{
		func(c *ginframework.Context) {
			defer restoreErrors(c, hiddenKey)
			instrument(c)
		},
		chainErrors(tracedKey, hiddenKey),
	}
}
