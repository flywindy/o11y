package trace

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// InitTracer initializes the OTLP HTTP exporter and a TracerProvider using the
// provided Resource. Accepting a pre-built Resource allows the TracerProvider
// and LoggerProvider to share identical service-identity attributes.
// It does not mutate any global state; the caller is responsible for wiring
// the returned provider and propagator as needed.
func InitTracer(ctx context.Context, endpoint string, res *resource.Resource) (*sdktrace.TracerProvider, propagation.TextMapPropagator, error) {
	// 1. OTLP HTTP trace exporter
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// Guard: shut down the exporter if any subsequent step fails so we do not
	// leak the underlying HTTP client or background flush goroutine.
	var initSucceeded bool
	defer func() {
		if !initSucceeded {
			_ = exporter.Shutdown(ctx)
		}
	}()

	// 2. TracerProvider with BatchSpanProcessor
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	// 3. Composite propagator (W3C TraceContext + Baggage)
	prop := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)

	initSucceeded = true
	return tp, prop, nil
}
