package o11y

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/trace"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// SDK holds the initialized observability providers.
// It does not mutate any global state; callers wire it however they like,
// e.g. slog.SetDefault(sdk.Logger) or otel.SetTracerProvider(sdk.provider).
type SDK struct {
	// Logger is a structured slog.Logger that automatically injects
	// trace_id and span_id into every log record when a span is active.
	Logger *slog.Logger

	// Propagator is the W3C TraceContext + Baggage composite propagator.
	// Pass it to nats.Inject / nats.Extract for distributed tracing over NATS.
	Propagator propagation.TextMapPropagator

	provider *sdktrace.TracerProvider
}

// Tracer returns a named tracer from the SDK's TracerProvider.
func (s *SDK) Tracer(name string) oteltrace.Tracer {
	return s.provider.Tracer(name)
}

// Shutdown gracefully flushes in-flight spans and shuts down all providers.
// Always call this before process exit; use a context with a timeout to cap the wait.
func (s *SDK) Shutdown(ctx context.Context) error {
	if err := s.provider.Shutdown(ctx); err != nil {
		s.Logger.ErrorContext(ctx, "failed to shut down tracer provider", slog.Any("error", err))
		return err
	}
	return nil
}

// Init initializes the o11y SDK with the provided options and returns an SDK
// instance ready for use. WithServiceName is required.
func Init(ctx context.Context, opts ...Option) (*SDK, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.serviceName == "" {
		return nil, errors.New("service name is required (use WithServiceName)")
	}

	// 1. Initialize TracerProvider and propagator (no global state set here)
	tp, prop, err := trace.InitTracer(ctx, cfg.serviceName, cfg.environment, cfg.otlpEndpoint)
	if err != nil {
		return nil, err
	}

	// 2. Build a structured logger with OTel trace correlation
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.logLevel,
	})
	logger := slog.New(log.NewOTelHandler(jsonHandler))

	return &SDK{
		Logger:     logger,
		Propagator: prop,
		provider:   tp,
	}, nil
}
