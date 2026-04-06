package o11y

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Config defines the configuration for the o11y SDK.
type Config struct {
	// ServiceName is the name of the service for trace resource attributes.
	ServiceName string
	// Environment is the deployment environment (e.g., "production", "staging").
	Environment string
	// OTLPEndpoint is the OTLP/HTTP collector endpoint (default: http://localhost:4318).
	OTLPEndpoint string
}

// Init initializes the OpenTelemetry tracer provider and sets the global slog default handler.
// It returns a shutdown function to gracefully close the provider and an error if initialization fails.
func Init(ctx context.Context, cfg Config) (func(context.Context), error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("service name is required")
	}

	if cfg.OTLPEndpoint == "" {
		cfg.OTLPEndpoint = "http://localhost:4318"
	}

	// 1. Initialize OTLP HTTP Trace Exporter
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// 2. Set up Resource (service.name, deployment.environment)
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.DeploymentEnvironmentKey.String(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// 3. Set up TracerProvider with BatchSpanProcessor
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	// 4. Set global TracerProvider and TextMapPropagator
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// 5. Set up global slog handler with OTel correlation
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(&OtelSlogHandler{Handler: jsonHandler}))

	// Return shutdown function
	shutdown := func(shutdownCtx context.Context) {
		if err := tp.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "failed to shutdown tracer provider", slog.Any("error", err))
		}
	}

	return shutdown, nil
}

// OtelSlogHandler is a custom slog.Handler that wraps another handler and
// injects trace_id and span_id into log records when a valid trace is present in the context.
type OtelSlogHandler struct {
	slog.Handler
}

// Handle implements slog.Handler.Handle and adds trace/span IDs to the record.
func (h *OtelSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().IsValid() {
		r.AddAttrs(
			slog.String("trace_id", span.SpanContext().TraceID().String()),
			slog.String("span_id", span.SpanContext().SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// Enabled implements slog.Handler.Enabled by delegating to the wrapped handler.
func (h *OtelSlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(ctx, level)
}

// WithAttrs implements slog.Handler.WithAttrs by delegating to the wrapped handler.
func (h *OtelSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &OtelSlogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.WithGroup by delegating to the wrapped handler.
func (h *OtelSlogHandler) WithGroup(name string) slog.Handler {
	return &OtelSlogHandler{Handler: h.Handler.WithGroup(name)}
}
