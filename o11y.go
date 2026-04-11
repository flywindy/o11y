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
	// Logger is a structured slog.Logger pre-populated with service.name
	// (and environment when set) that automatically injects trace_id and
	// span_id into every log record when a span is active.
	Logger *slog.Logger

	// Propagator is the W3C TraceContext + Baggage composite propagator.
	// Pass it to nats.Inject / nats.Extract for distributed tracing over NATS.
	Propagator propagation.TextMapPropagator

	provider  *sdktrace.TracerProvider
	shutdowns []func(context.Context) error
}

// Tracer returns a named tracer from the SDK's TracerProvider.
func (s *SDK) Tracer(name string) oteltrace.Tracer {
	return s.provider.Tracer(name)
}

// Shutdown gracefully flushes and shuts down all registered SDK components.
// Each component is attempted even if a previous one fails; all errors are
// logged and returned joined. Always call with a context that has a timeout
// to cap the flush wait.
func (s *SDK) Shutdown(ctx context.Context) error {
	var errs []error
	for _, fn := range s.shutdowns {
		if err := fn(ctx); err != nil {
			s.Logger.ErrorContext(ctx, "SDK component shutdown failed", slog.Any("error", err))
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
	tp, prop, err := trace.InitTracer(ctx, cfg.serviceName, cfg.serviceVersion, cfg.environment, cfg.otlpEndpoint)
	if err != nil {
		return nil, err
	}

	// 2. Build a structured logger with OTel trace correlation.
	//    Pre-populate service.name and (if set) environment so that every log
	//    record carries these fields without the caller having to add them manually.
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.logLevel,
	})
	logFields := []any{slog.String("service.name", cfg.serviceName)}
	if cfg.environment != "" {
		logFields = append(logFields, slog.String("environment", cfg.environment))
	}
	logger := slog.New(log.NewOTelHandler(jsonHandler)).With(logFields...)

	return &SDK{
		Logger:     logger,
		Propagator: prop,
		provider:   tp,
		shutdowns:  []func(context.Context) error{tp.Shutdown},
	}, nil
}
