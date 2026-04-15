package o11y

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	o11ylog "github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/trace"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// SDK holds the initialized observability providers.
// It does not mutate any global state; callers wire it however they like,
// e.g. slog.SetDefault(obs.Logger) or otel.SetTracerProvider(obs.TracerProvider()).
type SDK struct {
	// Logger writes structured log records to two destinations:
	//   • stdout       – JSON with service.name, trace_id, and span_id fields
	//                    (for local development and container log collection via Alloy)
	//   • OTel Collector – OTLP/HTTP → Loki (full OTel Log Data Model; service
	//                    identity comes from the shared Resource, not per-record attrs)
	// When a span is active in the context, trace_id and span_id are included
	// automatically in both destinations.
	Logger *slog.Logger

	// Propagator is the W3C TraceContext + Baggage composite propagator.
	// Pass it to nats.Inject / nats.Extract for distributed tracing over NATS.
	Propagator propagation.TextMapPropagator

	provider  *sdktrace.TracerProvider
	shutdowns []func(context.Context) error
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
		if opt != nil {
			opt(cfg)
		}
	}

	if cfg.serviceName == "" {
		return nil, errors.New("service name is required (use WithServiceName)")
	}

	// 1. Build a shared Resource so both TracerProvider and LoggerProvider carry
	//    identical service-identity attributes (service.name, service.version,
	//    deployment.environment, host, process). Building it once avoids drift.
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// 2. Initialize TracerProvider (no global state).
	tp, prop, err := trace.InitTracer(ctx, cfg.otlpEndpoint, res)
	if err != nil {
		return nil, err
	}

	// 3. Initialize LoggerProvider (no global state).
	//    On failure, shut down the already-created TracerProvider to avoid leaks.
	lp, err := o11ylog.InitLogger(ctx, cfg.otlpEndpoint, res)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, err
	}

	// 4. Build a dual-output logger:
	//
	//    a) OTLP handler (otelslog bridge):
	//       Converts slog records to OTel Log Data Model records and exports them
	//       via OTLP/HTTP to the OTel Collector → Loki. service.name and
	//       deployment.environment come from the shared Resource, not as
	//       per-record attributes. trace_id and span_id are extracted from the
	//       context automatically by the bridge.
	//
	//    b) Stdout handler:
	//       Writes JSON to stdout for local development and container log scraping
	//       by Alloy. service.name and environment are added as JSON fields here
	//       (they are NOT on the OTLP path to avoid duplicating Resource attrs).
	//       OtelSlogHandler wraps this to inject trace_id and span_id.
	otelOpts := []otelslog.Option{
		otelslog.WithLoggerProvider(lp),
	}
	if cfg.serviceVersion != "" {
		otelOpts = append(otelOpts, otelslog.WithVersion(cfg.serviceVersion))
	}
	otelHandler := otelslog.NewHandler("github.com/flywindy/o11y", otelOpts...)

	stdoutAttrs := []slog.Attr{slog.String("service.name", cfg.serviceName)}
	if cfg.environment != "" {
		stdoutAttrs = append(stdoutAttrs, slog.String("environment", cfg.environment))
	}
	stdoutBase := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.logLevel,
	}).WithAttrs(stdoutAttrs)
	stdoutHandler := o11ylog.NewOTelHandler(stdoutBase)

	logger := slog.New(o11ylog.NewMultiHandler(otelHandler, stdoutHandler))

	return &SDK{
		Logger:     logger,
		Propagator: prop,
		provider:   tp,
		shutdowns:  []func(context.Context) error{tp.Shutdown, lp.Shutdown},
	}, nil
}

// buildResource creates an OTel Resource with service identity and host/process
// metadata shared by both the TracerProvider and the LoggerProvider.
// ErrPartialResource is treated as non-fatal: some detectors (e.g. process info
// on restricted hosts) may fail, but the remaining attributes are still useful.
func buildResource(ctx context.Context, cfg *Config) (*resource.Resource, error) {
	opts := []resource.Option{
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceNameKey.String(cfg.serviceName)),
	}
	if cfg.serviceVersion != "" {
		opts = append(opts, resource.WithAttributes(
			semconv.ServiceVersionKey.String(cfg.serviceVersion),
		))
	}
	if cfg.environment != "" {
		opts = append(opts, resource.WithAttributes(
			semconv.DeploymentEnvironmentKey.String(cfg.environment),
		))
	}
	res, err := resource.New(ctx, opts...)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}
	return res, nil
}
