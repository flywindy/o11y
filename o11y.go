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
	"sort"
	"strings"
	"sync"

	o11ycassandra "github.com/flywindy/o11y/cassandra"
	"github.com/flywindy/o11y/internal/baggageattrs"
	o11ylog "github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/metrics"
	"github.com/flywindy/o11y/internal/profiling"
	"github.com/flywindy/o11y/internal/trace"
	o11yminio "github.com/flywindy/o11y/minio"
	o11ymongo "github.com/flywindy/o11y/mongo"
	o11yredis "github.com/flywindy/o11y/redis"
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
//
// Profiling reflects the combined state of WithProfilingEnabled and whether a
// non-empty Pyroscope endpoint was configured: it is true only when the SDK
// actually started a profiler.
type FeatureToggles struct {
	Trace     bool
	Metrics   bool
	Log       bool
	Profiling bool
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
	// automatically in both destinations. They stay at the record's top level
	// even on a logger derived with Logger.WithGroup, so log-to-trace queries
	// keyed on the top-level field keep matching; the group still nests that
	// logger's own attributes as usual.
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
	if err := configureSampler(cfg); err != nil {
		return nil, err
	}
	// 1. Build a shared Resource so TracerProvider, MeterProvider, and
	//    LoggerProvider all carry identical service-identity attributes.
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	whitelist, err := configureBaggageWhitelist(cfg, res)
	if err != nil {
		return nil, err
	}
	appendBaggageWarnings(cfg, whitelist)

	// 2. Initialize TracerProvider (no global state).
	//    When trace is disabled, a no-op provider is used and the W3C propagator
	//    is constructed directly so that downstream trace headers are still
	//    forwarded correctly even without local span creation.
	var tpInternal *sdktrace.TracerProvider
	tracerProviderPublic := oteltrace.TracerProvider(tracenoop.NewTracerProvider())
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	tpShutdown := func(_ context.Context) error { return nil }

	if cfg.traceEnabled {
		var spanProcessors []sdktrace.SpanProcessor
		if whitelist.Len() > 0 {
			spanProcessors = append(spanProcessors, whitelist.NewSpanProcessor())
		}
		tp, p, initErr := trace.InitTracer(ctx, cfg.otlpEndpoint, cfg.otlpHeaders, res, cfg.sampler, spanProcessors...)
		if initErr != nil {
			return nil, initErr
		}
		tpInternal, prop = tp, p
		tracerProviderPublic = tp
		// The trace-to-profile wrapper (otelpyroscope.NewTracerProvider) is
		// installed after profiling.Start succeeds (see below), not here.
		// Installing it eagerly would annotate spans with pyroscope.profile.id
		// even when profiling.Start later fails on the warn-and-continue path,
		// producing dangling identifiers that point at no real profile.
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
			ExtraViews: append(
				append(
					append(o11yredis.MetricViews(cfg.histogramBuckets), o11ymongo.MetricViews(cfg.histogramBuckets)...),
					o11yminio.MetricViews(cfg.histogramBuckets)...,
				),
				o11ycassandra.MetricViews(cfg.histogramBuckets)...,
			),
			MaxUniqueRoutes:         cfg.maxUniqueRoutes,
			MaxUniqueCollections:    cfg.maxUniqueCollections,
			ExtraHTTPServerAttrKeys: cfg.extraHTTPServerAttrKeys,
			Exemplars:               cfg.exemplars,
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
		logHandler := o11ylog.NewMultiHandler(otelHandler, stdoutHandler)
		if whitelist.Len() > 0 {
			logHandler = o11ylog.NewBaggageHandler(logHandler, whitelist)
		}
		logger = slog.New(logHandler)
	} else {
		logHandler := stdoutHandler
		if whitelist.Len() > 0 {
			logHandler = o11ylog.NewBaggageHandler(logHandler, whitelist)
		}
		logger = slog.New(logHandler)
	}

	// Emit diagnostics that could not be logged before the logger existed.
	// initWarnings holds invalid O11Y_*_ENABLED values collected at config time.
	for _, w := range cfg.initWarnings {
		logger.WarnContext(ctx, w)
	}
	if whitelist.Len() > 0 {
		logger.InfoContext(ctx, "baggage attribute materialization configured",
			slog.Any("keys", whitelist.Keys()),
		)
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
	// Warn about partial profiling configuration so operators notice misconfig
	// quickly. Profiling is opt-in and requires both the toggle and the
	// endpoint, so the all-default (toggle off, no endpoint) state is silent.
	switch {
	case !cfg.profilingEnabled && cfg.profilingEndpoint != "":
		logger.WarnContext(ctx, "profiling pillar disabled; Pyroscope endpoint ignored",
			slog.String("toggle", "O11Y_PROFILING_ENABLED"),
			slog.String("endpoint", cfg.profilingEndpoint),
		)
	case cfg.profilingEnabled && cfg.profilingEndpoint == "":
		logger.WarnContext(ctx, "profiling pillar enabled but no Pyroscope endpoint set; profiler not started",
			slog.String("toggle", "O11Y_PROFILING_ENABLED"),
			slog.String("option", "WithProfilingEndpoint"),
		)
	}

	var profilingStarted bool
	var profilerCloser func(context.Context) error
	if cfg.profilingEnabled && cfg.profilingEndpoint != "" {
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
			profilingStarted = true
			// Wrap only now that the profiler is running, so spans never
			// carry pyroscope.profile.id when no profile actually exists.
			if tpInternal != nil {
				tracerProviderPublic = otelpyroscope.NewTracerProvider(tpInternal)
			}
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
			Trace:     cfg.traceEnabled,
			Metrics:   cfg.metricsEnabled,
			Log:       cfg.logEnabled,
			Profiling: profilingStarted,
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

// configureSampler validates and materializes a typed sampling ratio only when
// tracing is enabled and the caller explicitly configured a ratio, preserving
// OTel env/default sampling when the SDK option is unset.
func configureSampler(cfg *Config) error {
	if !cfg.traceEnabled || !cfg.samplingRatioSet {
		return nil
	}
	if math.IsNaN(cfg.samplingRatio) || cfg.samplingRatio < 0 || cfg.samplingRatio > 1 {
		return fmt.Errorf("sampling ratio must be between 0.0 and 1.0 inclusive, got %v", cfg.samplingRatio)
	}
	cfg.sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.samplingRatio))
	return nil
}

func appendBaggageWarnings(cfg *Config, whitelist baggageattrs.Whitelist) {
	if whitelist.Len() > 0 && !cfg.traceEnabled {
		cfg.initWarnings = append(cfg.initWarnings,
			"baggage attribute materialization enabled while trace pillar disabled; baggage still propagates and materializes on logs, but not spans",
		)
	}
}

func configureBaggageWhitelist(cfg *Config, res *resource.Resource) (baggageattrs.Whitelist, error) {
	effective := make([]string, 0, len(cfg.baggageKeys)+1)
	if cfg.userBaggage {
		effective = append(effective, baggageattrs.UserNameKey)
	}
	effective = append(effective, cfg.baggageKeys...)

	resourceKeys := make(map[string]struct{})
	for _, attr := range res.Attributes() {
		resourceKeys[string(attr.Key)] = struct{}{}
	}
	// The collision check deliberately runs against the untruncated key list so
	// that its outcome does not depend on the order keys were registered in:
	// checking after the cap would make the same key fail or merely warn
	// depending on where it landed in the list. The cost is that a key beyond
	// MaxBaggageAttributeKeys — which would never have been materialized, and
	// so could never have shadowed the Resource attribute — still fails Init.
	// The error says so, because otherwise it points an operator at a
	// materialization path that was never live.
	collisions := make([]string, 0)
	for _, key := range effective {
		if _, ok := resourceKeys[key]; ok {
			collisions = append(collisions, key)
		}
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		msg := fmt.Sprintf(
			"baggage attribute keys collide with resource attributes: %s", strings.Join(collisions, ", "),
		)
		if overCap := collisionsBeyondCap(cfg.baggageKeys, collisions); len(overCap) > 0 {
			msg += fmt.Sprintf(
				" (%s exceed MaxBaggageAttributeKeys=%d and would not have been materialized;"+
					" the collision check runs before the cap so its result does not depend on key order)",
				strings.Join(overCap, ", "), MaxBaggageAttributeKeys,
			)
		}
		return baggageattrs.Whitelist{}, errors.New(msg)
	}

	if len(cfg.baggageKeys) > MaxBaggageAttributeKeys {
		dropped := append([]string(nil), cfg.baggageKeys[MaxBaggageAttributeKeys:]...)
		cfg.initWarnings = append(cfg.initWarnings, fmt.Sprintf(
			"WithBaggageAttributes: materializing only the first %d application keys; dropping %v",
			MaxBaggageAttributeKeys, dropped,
		))
		cfg.baggageKeys = append([]string(nil), cfg.baggageKeys[:MaxBaggageAttributeKeys]...)
	}
	effective = effective[:0]
	if cfg.userBaggage {
		effective = append(effective, baggageattrs.UserNameKey)
	}
	effective = append(effective, cfg.baggageKeys...)
	return baggageattrs.NewWhitelist(effective...), nil
}

// collisionsBeyondCap returns the colliding application keys that sit past
// MaxBaggageAttributeKeys and would have been dropped with a warning had they
// not collided. collisions must be sorted; the result preserves that order.
func collisionsBeyondCap(appKeys, collisions []string) []string {
	if len(appKeys) <= MaxBaggageAttributeKeys {
		return nil
	}
	beyond := make(map[string]struct{}, len(appKeys)-MaxBaggageAttributeKeys)
	for _, key := range appKeys[MaxBaggageAttributeKeys:] {
		beyond[key] = struct{}{}
	}
	overCap := make([]string, 0, len(collisions))
	for _, key := range collisions {
		if _, ok := beyond[key]; ok {
			overCap = append(overCap, key)
		}
	}
	return overCap
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
