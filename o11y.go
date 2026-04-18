package o11y

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	o11ylog "github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/metrics"
	"github.com/flywindy/o11y/internal/trace"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// SDK holds the initialized observability providers.
// It does not mutate any global state; callers wire it however they like,
// e.g. slog.SetDefault(obs.Logger) or otel.SetTracerProvider(obs.TracerProvider()).
type SDK struct {
	// Logger writes structured log records to two destinations:
	//   • stdout       – JSON with service.name, traceId, and spanId fields
	//                    (for local development and container log collection via Fluentd)
	//   • OTel Collector – OTLP/HTTP → Loki (full OTel Log Data Model; service
	//                    identity comes from the shared Resource, not per-record attrs)
	// When a span is active in the context, traceId and spanId are included
	// automatically in both destinations.
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
// shape of Tracer. Pass the returned meter to http.New or to your own
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

// Init initializes and returns a configured *SDK for the calling service.
//
// The following options are required; Init returns an error if any are missing
// or invalid:
//   - WithServiceName    — identifies the service
//   - WithServiceVersion — used for canary / rollback tracking
//   - WithEnvironment    — must be one of: production, staging, development, testing
//     (common aliases such as "prod" and "stg" are normalized automatically)
//   - WithServiceNamespace — identifies the owning team / k8s namespace
//
// On success the SDK contains a tracer provider, meter provider (Prometheus
// scrape or OTLP push), logger provider (stdout JSON + OTLP/HTTP → Loki), and
// an ordered shutdown list. Init does not set global OpenTelemetry state.
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
	if cfg.serviceVersion == "" {
		return nil, errors.New("service version is required (use WithServiceVersion)")
	}
	if cfg.namespace == "" {
		return nil, errors.New("service namespace is required (use WithServiceNamespace)")
	}
	normalized, err := normalizeEnvironment(cfg.environment)
	if err != nil {
		return nil, err
	}
	cfg.environment = normalized

	// 1. Build a shared Resource so TracerProvider, MeterProvider, and
	//    LoggerProvider all carry identical service-identity attributes.
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// 2. Initialize TracerProvider (no global state).
	tp, prop, err := trace.InitTracer(ctx, cfg.otlpEndpoint, res)
	if err != nil {
		return nil, err
	}

	// 3. Initialize MeterProvider + Prometheus scrape endpoint. On failure,
	//    shut down the already-initialized tracer to avoid leaking its
	//    background batch processor. The shared Resource is passed so that
	//    service identity attributes are identical across all three providers.
	mp, metricsCloser, err := metrics.InitMeter(ctx, metrics.Config{
		Resource:            res,
		MetricsOTLPEndpoint: cfg.metricsOTLPEndpoint,
		MetricsAddr:         cfg.metricsAddr,
		RuntimeMetrics:      cfg.runtimeMetrics,
		HistogramBuckets:    cfg.histogramBuckets,
	})
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, err
	}

	// 4. Initialize LoggerProvider (no global state). On failure, shut down
	//    previously initialized providers in reverse order.
	lp, err := o11ylog.InitLogger(ctx, cfg.otlpEndpoint, res)
	if err != nil {
		_ = metricsCloser(ctx)
		_ = mp.Shutdown(ctx)
		_ = tp.Shutdown(ctx)
		return nil, err
	}

	// 5. Build a dual-output logger:
	//
	//    a) OTLP handler (otelslog bridge):
	//       Converts slog records to OTel Log Data Model records and exports them
	//       via OTLP/HTTP to the OTel Collector → Loki. service.name and
	//       deployment.environment come from the shared Resource, not as
	//       per-record attributes. traceId and spanId are extracted from the
	//       context automatically by the bridge.
	//
	//    b) Stdout handler:
	//       Writes JSON to stdout for local development and container log scraping.
	//       service.name and environment are added as JSON fields here.
	//       OtelSlogHandler wraps this to inject traceId and spanId.
	otelOpts := []otelslog.Option{
		otelslog.WithLoggerProvider(lp),
		otelslog.WithSchemaURL(semconv.SchemaURL),
	}
	if cfg.serviceVersion != "" {
		otelOpts = append(otelOpts, otelslog.WithVersion(cfg.serviceVersion))
	}
	// Wrap the OTLP handler with a minimum-level gate so that both outputs
	// honour the same logLevel. Without this, the otelslog bridge would emit
	// records at all levels regardless of the configured threshold.
	otelHandler := &leveledHandler{
		Handler: otelslog.NewHandler("github.com/flywindy/o11y", otelOpts...),
		min:     cfg.logLevel,
	}

	stdoutAttrs := []slog.Attr{slog.String("service.name", cfg.serviceName)}
	if cfg.environment != "" {
		stdoutAttrs = append(stdoutAttrs, slog.String("environment", cfg.environment))
	}
	stdoutBase := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.logLevel,
	}).WithAttrs(stdoutAttrs)
	stdoutHandler := o11ylog.NewOTelHandler(stdoutBase)

	logger := slog.New(o11ylog.NewMultiHandler(otelHandler, stdoutHandler))

	// Shutdowns run in registration order: drain scrape traffic first
	// (metricsServer), then flush the meter provider, then logs, then traces.
	return &SDK{
		Logger:        logger,
		Propagator:    prop,
		provider:      tp,
		meterProvider: mp,
		shutdowns: []func(context.Context) error{
			metricsCloser,
			mp.Shutdown,
			lp.Shutdown,
			tp.Shutdown,
		},
	}, nil
}

// buildResource creates an OTel Resource with service identity and host/process
// metadata shared by all three providers (trace, metrics, logs).
// ErrPartialResource is treated as non-fatal: some detectors (e.g. process info
// on restricted hosts) may fail, but the remaining attributes are still useful.
func buildResource(ctx context.Context, cfg *Config) (*resource.Resource, error) {
	opts := []resource.Option{
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceNameKey.String(cfg.serviceName)),
	}
	opts = append(opts,
		resource.WithAttributes(semconv.ServiceVersionKey.String(cfg.serviceVersion)),
		resource.WithAttributes(semconv.DeploymentEnvironmentNameKey.String(cfg.environment)),
	)
	opts = append(opts, resource.WithAttributes(
		semconv.ServiceNamespaceKey.String(cfg.namespace),
	))
	res, err := resource.New(ctx, opts...)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}
	return res, nil
}

// leveledHandler wraps a slog.Handler and gates Enabled on a minimum level.
// This ensures the OTLP bridge honours the same log level configured for stdout,
// since the otelslog bridge does not apply level filtering by default.
type leveledHandler struct {
	slog.Handler
	min slog.Level
}

func (h *leveledHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.min
}

func (h *leveledHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &leveledHandler{Handler: h.Handler.WithAttrs(attrs), min: h.min}
}

func (h *leveledHandler) WithGroup(name string) slog.Handler {
	return &leveledHandler{Handler: h.Handler.WithGroup(name), min: h.min}
}

// envAliases maps common shorthands to their canonical deployment.environment.name
// values. The canonical set is: production, staging, development, testing.
var envAliases = map[string]string{
	"production":  "production",
	"prod":        "production",
	"staging":     "staging",
	"stage":       "staging",
	"stg":         "staging",
	"development": "development",
	"develop":     "development",
	"dev":         "development",
	"testing":     "testing",
	"test":        "testing",
}

// normalizeEnvironment returns the canonical deployment environment name for
// the given input, or an error if the value is not recognized. An empty input
// is rejected so that unset environments cannot silently propagate to telemetry.
func normalizeEnvironment(env string) (string, error) {
	if env == "" {
		return "", errors.New("deployment environment is required (use WithEnvironment); " +
			"accepted values: production, staging, development, testing")
	}
	if canonical, ok := envAliases[env]; ok {
		return canonical, nil
	}
	return "", fmt.Errorf(
		"unknown deployment environment %q; accepted values: production, staging, development, testing",
		env,
	)
}
