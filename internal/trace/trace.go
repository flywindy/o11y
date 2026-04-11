package trace

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// InitTracer initializes the OTLP HTTP exporter and a TracerProvider.
// It does not mutate any global state; the caller is responsible for wiring
// the returned provider and propagator as needed.
func InitTracer(ctx context.Context, serviceName, serviceVersion, environment, endpoint string) (*sdktrace.TracerProvider, propagation.TextMapPropagator, error) {
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

	// 2. Resource — service identity plus process and host metadata.
	//    WithFromEnv also picks up OTEL_RESOURCE_ATTRIBUTES / OTEL_SERVICE_NAME.
	resOpts := []resource.Option{
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceNameKey.String(serviceName)),
	}
	if serviceVersion != "" {
		resOpts = append(resOpts, resource.WithAttributes(
			semconv.ServiceVersionKey.String(serviceVersion),
		))
	}
	if environment != "" {
		resOpts = append(resOpts, resource.WithAttributes(
			semconv.DeploymentEnvironmentKey.String(environment),
		))
	}

	res, err := resource.New(ctx, resOpts...)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// 3. TracerProvider with BatchSpanProcessor
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	// 4. Composite propagator (W3C TraceContext + Baggage)
	prop := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)

	initSucceeded = true
	return tp, prop, nil
}
