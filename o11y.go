package o11y

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/metrics"
	"github.com/flywindy/o11y/internal/trace"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// SDK holds the initialized observability providers.
// It does not mutate any global state; callers wire it however they like,
// e.g. slog.SetDefault(obs.Logger) or otel.SetTracerProvider(obs.TracerProvider()).
type SDK struct {
	// Logger is a structured slog.Logger pre-populated with service.name
	// (and environment when set) that automatically injects trace_id and
	// span_id into every log record when a span is active.
	Logger *slog.Logger

	// Propagator is the W3C TraceContext + Baggage composite propagator.
	// Pass it to nats.Inject / nats.Extract for distributed tracing over NATS.
	Propagator propagation.TextMapPropagator

	provider      *sdktrace.TracerProvider
	meterProvider *sdkmetric.MeterProvider
	shutdowns     []func(context.Context) error
}

// TracerProvider returns the underlying sdktrace.TracerProvider.
// Use this to wire the SDK's provider as the global OTel tracer provider
// if needed, e.g. otel.SetTracerProvider(sdk.TracerProvider()).
func (s *SDK) TracerProvider() *sdktrace.TracerProvider {
	return s.provider
}

// Tracer returns a named tracer from the SDK's TracerProvider.
func (s *SDK) Tracer(name string) oteltrace.Tracer {
	return s.provider.Tracer(name)
}

// MeterProvider returns the underlying sdkmetric.MeterProvider. Use this
// when wiring SDK-produced metrics into instrumentation libraries that
// accept an OTel MeterProvider directly.
func (s *SDK) MeterProvider() *sdkmetric.MeterProvider {
	return s.meterProvider
}

// Meter returns a named meter from the SDK's MeterProvider, mirroring the
// shape of Tracer. Pass the returned meter to httpmw.New or to your own
// instrumentation code.
func (s *SDK) Meter(name string) metric.Meter {
	return s.meterProvider.Meter(name)
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
// instance ready for use. WithServiceName and WithTeam are required.
func Init(ctx context.Context, opts ...Option) (*SDK, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}

	if cfg.serviceName == "" {
		return nil, errors.New("service name is required (use WithServiceName)")
	}
	if cfg.team == "" {
		return nil, errors.New("team is required (use WithTeam)")
	}

	// 1. Initialize TracerProvider and propagator (no global state set here)
	tp, prop, err := trace.InitTracer(ctx, cfg.serviceName, cfg.serviceVersion, cfg.environment, cfg.otlpEndpoint)
	if err != nil {
		return nil, err
	}

	// 2. Initialize MeterProvider + Prometheus scrape endpoint. On failure,
	//    shut down the already-initialized tracer so we do not leak its
	//    background batch processor.
	mp, metricsServer, err := metrics.InitMeter(ctx, metrics.Config{
		ServiceName:      cfg.serviceName,
		ServiceVersion:   cfg.serviceVersion,
		Environment:      cfg.environment,
		Team:             cfg.team,
		MetricsAddr:      cfg.metricsAddr,
		RuntimeMetrics:   cfg.runtimeMetrics,
		HistogramBuckets: cfg.histogramBuckets,
	})
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, err
	}

	// 3. Build a structured logger with OTel trace correlation.
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

	// Shutdowns run in registration order. Drain scrape traffic first
	// (metricsServer), then flush the meter provider, then flush traces.
	// tp already has its own batch flush, so putting it last is fine.
	return &SDK{
		Logger:        logger,
		Propagator:    prop,
		provider:      tp,
		meterProvider: mp,
		shutdowns: []func(context.Context) error{
			metricsServer.Shutdown,
			mp.Shutdown,
			tp.Shutdown,
		},
	}, nil
}
