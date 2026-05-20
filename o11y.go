// Package o11y is the top-level entry point for the SDK. It exposes Init for
// constructing a configured *SDK that bundles trace, metric, log, and optional
// profiling providers together with the W3C TraceContext+Baggage propagator
// and a dual-output slog logger.
//
// The SDK never mutates global OpenTelemetry state; callers wire the returned
// providers into their application explicitly (e.g. otel.SetTracerProvider).
//
// See ADR 0001 (log format strategy), ADR 0002 (metrics strategy), and
// ADR 0007 (OTLP authentication) for the design rationale.
package o11y

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"

	o11ylog "github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/metrics"
	"github.com/flywindy/o11y/internal/profiling"
	"github.com/flywindy/o11y/internal/trace"
	otelpyroscope "github.com/grafana/otel-profiling-go"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// FeatureToggles reports which observability pillars were enabled at Init time.
// Use sdk.Toggles to inspect active state at runtime, e.g. for health-check
// endpoints or to conditionally log a startup warning when a pillar is off.
type FeatureToggles struct {
	Trace   bool
	Metrics bool
	Log     bool
}

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
	// When WithLogEnabled(false) is set, only the stdout destination is active.
	Logger *slog.Logger

	// Propagator is the W3C TraceContext + Baggage composite propagator.
	// Pass it to nats.Inject / nats.Extract for distributed tracing over NATS.
	// The propagator is always set (even when trace is disabled) so that
	// incoming trace headers are still parsed and forwarded to downstream services.
	Propagator propagation.TextMapPropagator

	// Toggles reports which observability pillars were enabled at Init time.
	Toggles FeatureToggles

	// Internal providers are the concrete SDK-owned providers we shut down;
	// they are nil when the corresponding pillar is disabled.
	// Public providers are always non-nil (noop when disabled), so callers
	// never need to nil-check TracerProvider() or MeterProvider(). They may
	// also be wrapped by integrations such as otel-profiling-go. See ADR 0012 §2.
	tracerProviderInternal *sdktrace.TracerProvider
	tracerProviderPublic   oteltrace.TracerProvider
	meterProviderInternal  *sdkmetric.MeterProvider
	meterProviderPublic    metric.MeterProvider
	shutdowns              []func(context.Context) error

	shutdownOnce sync.Once
	shutdownErr  error
}

// TracerProvider returns the SDK's tracer provider interface.
// Use this to wire the SDK's provider as the global OTel tracer provider
// if needed, e.g. otel.SetTracerProvider(sdk.TracerProvider()).
func (s *SDK) TracerProvider() oteltrace.TracerProvider {
	return s.tracerProviderPublic
}

// Tracer returns a named tracer from the SDK's TracerProvider.
func (s *SDK) Tracer(name string) oteltrace.Tracer {
	return s.tracerProviderPublic.Tracer(name)
}

// MeterProvider returns the SDK's meter provider interface. Use this
// when wiring SDK-produced metrics into instrumentation libraries that
// accept an OTel MeterProvider directly.
func (s *SDK) MeterProvider() metric.MeterProvider {
	return s.meterProviderPublic
}

// Meter returns a named meter from the SDK's MeterProvider for custom
// instrumentation. HTTP server and client instrumentation are provided by the
// http.NewServerHandler and http.NewTransport facades, which accept the SDK's
// MeterProvider directly.
func (s *SDK) Meter(name string) metric.Meter {
	return s.meterProviderPublic.Meter(name)
}

// Shutdown gracefully flushes and shuts down all registered SDK components.
// Each component is attempted even if a previous one fails; all errors are
// logged and returned joined. Always call with a context that has a timeout
// to cap the flush wait.
//
// Shutdown is idempotent: subsequent calls return the same joined error
// without rerunning any closer. Callers may safely register Shutdown in
// multiple defer chains (for example, both in main and in a signal handler)
// without risking double-shutdown of underlying exporters.
func (s *SDK) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		var errs []error
		for _, fn := range s.shutdowns {
			if err := fn(ctx); err != nil {
				s.Logger.ErrorContext(ctx, "SDK component shutdown failed", slog.Any("error", err))
				errs = append(errs, err)
			}
		}
		s.shutdownErr = errors.Join(errs...)
	})
	return s.shutdownErr
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

	if err := validateHistogramBuckets(cfg.histogramBuckets); err != nil {
		return nil, err
	}

	// 1. Build a shared Resource so TracerProvider, MeterProvider, and
	//    LoggerProvider all carry identical service-identity attributes.
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// 2. Initialize TracerProvider (no global state).
	//    When trace is disabled, a no-op provider is used and the W3C propagator
	//    is constructed directly so that downstream trace headers are still
	//    forwarded correctly even without local span creation.
	var tpInternal *sdktrace.TracerProvider
	tracerProviderPublic := oteltrace.TracerProvider(tracenoop.NewTracerProvider())
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	tpShutdown := func(_ context.Context) error { return nil }

	if cfg.traceEnabled {
		tp, p, initErr := trace.InitTracer(ctx, cfg.otlpEndpoint, cfg.otlpHeaders, res)
		if initErr != nil {
			return nil, initErr
		}
		tpInternal, prop = tp, p
		tracerProviderPublic = tp
		if cfg.profilingEndpoint != "" {
			tracerProviderPublic = otelpyroscope.NewTracerProvider(tp)
		}
		tpShutdown = tp.Shutdown
	}

	// 3. Initialize MeterProvider + Prometheus scrape endpoint. On failure,
	//    shut down the already-initialized tracer to avoid leaking its
	//    background batch processor. The shared Resource is passed so that
	//    service identity attributes are identical across all three providers.
	//    When metrics is disabled, a no-op provider is used and no HTTP server
	//    is started; existing Grafana dashboards are unaffected.
	var mpInternal *sdkmetric.MeterProvider
	meterProviderPublic := metric.MeterProvider(metricnoop.NewMeterProvider())
	metricsCloser := metrics.Closer(func(_ context.Context) error { return nil })
	mpShutdown := func(_ context.Context) error { return nil }

	if cfg.metricsEnabled {
		mp, closer, initErr := metrics.InitMeter(ctx, metrics.Config{
			Resource:            res,
			MetricsOTLPEndpoint: cfg.metricsOTLPEndpoint,
			OTLPHeaders:         cfg.otlpHeaders,
			MetricsAddr:         cfg.metricsAddr,
			RuntimeMetrics:      cfg.runtimeMetrics,
			HistogramBuckets:    cfg.histogramBuckets,
			DisableDefaultViews: cfg.disableDefaultViews,
			MaxUniqueRoutes:     cfg.maxUniqueRoutes,
		})
		if initErr != nil {
			_ = tpShutdown(ctx)
			return nil, initErr
		}
		mpInternal, meterProviderPublic = mp, mp
		metricsCloser, mpShutdown = closer, mp.Shutdown
	}

	// 4. Build the stdout JSON handler, shared by both the dual-output (log
	//    enabled) and stdout-only (log disabled) paths below.
	stdoutAttrs := []slog.Attr{slog.String("service.name", cfg.serviceName)}
	if cfg.environment != "" {
		stdoutAttrs = append(stdoutAttrs, slog.String("environment", cfg.environment))
	}
	stdoutBase := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.logLevel,
	}).WithAttrs(stdoutAttrs)
	stdoutHandler := o11ylog.NewOTelHandler(stdoutBase)

	// 5. Initialize LoggerProvider and build the dual-output logger.
	//    When log is disabled, only the stdout handler is active; no OTLP
	//    connection is attempted and no LoggerProvider is started.
	lpShutdown := func(_ context.Context) error { return nil }
	var logger *slog.Logger

	if cfg.logEnabled {
		lp, initErr := o11ylog.InitLogger(ctx, cfg.otlpEndpoint, cfg.otlpHeaders, res)
		if initErr != nil {
			_ = metricsCloser(ctx)
			_ = mpShutdown(ctx)
			_ = tpShutdown(ctx)
			return nil, initErr
		}
		lpShutdown = lp.Shutdown

		// Dual-output logger:
		//   a) OTLP handler (otelslog bridge) → OTel Collector → Loki
		//   b) Stdout handler → JSON for local dev / container log scraping
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
		logger = slog.New(o11ylog.NewMultiHandler(otelHandler, stdoutHandler))
	} else {
		logger = slog.New(stdoutHandler)
	}

	// Emit diagnostics that could not be logged before the logger existed.
	// initWarnings holds invalid O11Y_*_ENABLED values collected at config time.
	for _, w := range cfg.initWarnings {
		logger.WarnContext(ctx, w)
	}
	// Warn about disabled pillars so operators can confirm intent at startup.
	if !cfg.traceEnabled {
		logger.WarnContext(ctx, "trace pillar disabled; using no-op TracerProvider",
			slog.String("toggle", "O11Y_TRACE_ENABLED"),
		)
	}
	if !cfg.metricsEnabled {
		logger.WarnContext(ctx, "metrics pillar disabled; Prometheus server not started",
			slog.String("toggle", "O11Y_METRICS_ENABLED"),
		)
	}
	if !cfg.logEnabled {
		logger.WarnContext(ctx, "log pillar disabled; OTLP log export skipped (stdout only)",
			slog.String("toggle", "O11Y_LOG_ENABLED"),
		)
	}

	var profilerCloser func(context.Context) error
	if cfg.profilingEndpoint != "" {
		closer, startErr := profiling.Start(ctx, profiling.Config{
			ServiceName: cfg.serviceName,
			Endpoint:    cfg.profilingEndpoint,
			AuthHeaders: cfg.profilingAuthHeaders,
			Resource:    res,
			Logger:      logger,
		})
		if startErr != nil {
			if errors.Is(startErr, profiling.ErrAlreadyStarted) {
				_ = metricsCloser(ctx)
				_ = mpShutdown(ctx)
				_ = lpShutdown(ctx)
				_ = tpShutdown(ctx)
				return nil, startErr
			}
			logger.WarnContext(ctx, "profiling disabled after Pyroscope start failure",
				slog.String("endpoint", cfg.profilingEndpoint),
				slog.Any("error", startErr),
			)
		} else {
			profilerCloser = closer
		}
	}

	// Shutdowns run in registration order: drain scrape traffic first
	// (metricsServer), then flush the meter provider, logs, optional profiling,
	// then traces. Disabled pillars contribute a no-op that returns nil.
	shutdowns := []func(context.Context) error{
		metricsCloser,
		mpShutdown,
		lpShutdown,
	}
	if profilerCloser != nil {
		shutdowns = append(shutdowns, profilerCloser)
	}
	shutdowns = append(shutdowns, tpShutdown)

	return &SDK{
		Logger:     logger,
		Propagator: prop,
		Toggles: FeatureToggles{
			Trace:   cfg.traceEnabled,
			Metrics: cfg.metricsEnabled,
			Log:     cfg.logEnabled,
		},
		tracerProviderInternal: tpInternal,
		tracerProviderPublic:   tracerProviderPublic,
		meterProviderInternal:  mpInternal,
		meterProviderPublic:    meterProviderPublic,
		shutdowns:              shutdowns,
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

// validateHistogramBuckets ensures the histogram boundaries are in a state
// that the OTel SDK can consume safely. It rejects empty slices, NaN, ±Inf,
// non-positive values, and unsorted sequences. The OTel spec leaves behavior
// undefined for invalid inputs, so we fail fast at Init time instead of
// emitting silently broken histograms in production.
func validateHistogramBuckets(buckets []float64) error {
	if len(buckets) == 0 {
		return errors.New("histogram buckets must not be empty (use WithHistogramBuckets " +
			"to override, or accept the default)")
	}
	for i, b := range buckets {
		if math.IsNaN(b) || math.IsInf(b, 0) {
			return fmt.Errorf("histogram bucket[%d] = %v must be a finite number", i, b)
		}
		if b <= 0 {
			return fmt.Errorf("histogram bucket[%d] = %v must be strictly positive (seconds)", i, b)
		}
		if i > 0 && b <= buckets[i-1] {
			return fmt.Errorf(
				"histogram buckets must be strictly increasing: bucket[%d]=%v is not greater than bucket[%d]=%v",
				i, b, i-1, buckets[i-1],
			)
		}
	}
	return nil
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
