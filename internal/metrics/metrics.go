// Package metrics encapsulates the OTel MeterProvider used by the top-level
// o11y SDK. It supports two exporter strategies selected at init time:
//
//   - Prometheus pull (default): a private registry + HTTP server on
//     cfg.MetricsAddr exposes /metrics for Prometheus scraping.
//   - OTLP push: when cfg.MetricsOTLPEndpoint is set, metrics are exported
//     via OTLP/HTTP and no HTTP server is started. Use this for serverless
//     environments where exposing a scrape port is not possible.
//
// Neither strategy touches global OTel or Prometheus state.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/flywindy/o11y/internal/metricscap"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	otlpmetrichttp "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// Config is the subset of SDK configuration that the metrics subsystem needs.
//
// When Resource is non-nil, InitMeter uses it directly and skips building its
// own. All service-identity attributes (service.name, service.version,
// deployment.environment.name, service.namespace) must already be present
// in the provided Resource; they are not validated separately.
//
// When MetricsOTLPEndpoint is non-empty, the Prometheus pull model is bypassed
// entirely: an OTLP/HTTP exporter is used instead and MetricsAddr is ignored.
type Config struct {
	// Resource is an optional pre-built OTel Resource shared with the
	// TracerProvider and LoggerProvider. When set, the standalone
	// ServiceName/ServiceVersion/Environment/Namespace fields are ignored for
	// Resource construction (but Namespace is still used for validation when nil).
	Resource *resource.Resource

	// MetricsOTLPEndpoint, when non-empty, switches the exporter to OTLP push.
	// No Prometheus HTTP server is started. Intended for serverless environments.
	// Example: "http://otel-collector:4318"
	MetricsOTLPEndpoint string

	// OTLPHeaders, when non-empty, are attached to every OTLP/HTTP request
	// emitted by the metrics exporter (used for authentication against
	// managed observability backends). Ignored on the Prometheus pull path.
	OTLPHeaders map[string]string

	ServiceName    string
	ServiceVersion string
	Environment    string
	// Namespace maps to service.namespace (OTel semconv). Required when
	// Resource is nil. It becomes a constant Prometheus label
	// (service_namespace="...") on every series for alert routing and
	// ownership governance.
	Namespace string

	MetricsAddr         string
	RuntimeMetrics      bool
	HistogramBuckets    []float64
	DisableDefaultViews bool
	MaxUniqueRoutes     int

	// ExtraHTTPServerAttrKeys augments the SDK-managed attribute allow-list
	// for the http.server.request.duration view. Promoting caller-controlled
	// keys onto the exported series (rather than letting them fall through to
	// the exemplar) keeps PromQL aggregations meaningful and avoids the
	// OpenMetrics 128-rune exemplar-label cap. Cardinality is the caller's
	// responsibility.
	ExtraHTTPServerAttrKeys []string
}

// Closer is a function that shuts down a component. For the Prometheus path it
// shuts down the HTTP server; for the OTLP path it shuts down the exporter.
// It is always safe to call even if the component was never started.
type Closer func(context.Context) error

// The SDK cardinality limit is an in-process memory guard, not the exported
// route presentation cap. Derive it from the bounded HTTP keyspace so
// WithMaxUniqueRoutes(n) can preserve route detail across normal method/status
// combinations before the SDK overflow guard intentionally drops labels.
const (
	sdkCardinalityMethodBudget = 16
	sdkCardinalityStatusBudget = 64
)

// InitMeter initializes an OTel MeterProvider and returns it together with a
// Closer that must be called during SDK shutdown.
//
// Exporter strategy:
//   - cfg.MetricsOTLPEndpoint == "" → Prometheus pull: private registry +
//     HTTP server on cfg.MetricsAddr; Closer shuts down the HTTP server.
//   - cfg.MetricsOTLPEndpoint != "" → OTLP push: otlpmetrichttp exporter;
//     Closer shuts down the exporter. MetricsAddr is ignored.
//
// Bind errors (Prometheus path) are surfaced synchronously.
func InitMeter(ctx context.Context, cfg Config) (*sdkmetric.MeterProvider, Closer, error) {
	res, err := resolveResource(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	views := defaultViews(cfg)

	if cfg.MetricsOTLPEndpoint != "" {
		return initOTLP(ctx, cfg, res, views)
	}
	return initPrometheus(ctx, cfg, res, views)
}

func defaultViews(cfg Config) []sdkmetric.View {
	if cfg.DisableDefaultViews {
		return nil
	}
	histogram := sdkmetric.AggregationExplicitBucketHistogram{
		Boundaries: cfg.HistogramBuckets,
	}
	serverKeys := make([]attribute.Key, 0, 3+len(cfg.ExtraHTTPServerAttrKeys))
	serverKeys = append(serverKeys,
		semconv.HTTPRequestMethodKey,
		semconv.HTTPRouteKey,
		semconv.HTTPResponseStatusCodeKey,
	)
	for _, k := range cfg.ExtraHTTPServerAttrKeys {
		serverKeys = append(serverKeys, attribute.Key(k))
	}
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.duration"},
			sdkmetric.Stream{
				Aggregation:                       histogram,
				AttributeFilter:                   attribute.NewAllowKeysFilter(serverKeys...),
				ExemplarReservoirProviderSelector: dropFilteredAttrsExemplarSelector,
			},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.client.request.duration"},
			sdkmetric.Stream{
				Aggregation: histogram,
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.HTTPRequestMethodKey,
					semconv.HTTPResponseStatusCodeKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
				),
				ExemplarReservoirProviderSelector: dropFilteredAttrsExemplarSelector,
			},
		),
	}
}

// initPrometheus sets up the Prometheus pull path.
func initPrometheus(ctx context.Context, cfg Config, res *resource.Resource, views []sdkmetric.View) (*sdkmetric.MeterProvider, Closer, error) {
	reg := prometheus.NewRegistry()

	// Resource attributes in the allow filter become constant labels on every
	// series, so service_namespace="..." is guaranteed on every instrument including runtime.
	// The key "deployment.environment.name" matches the pinned semconv version.
	exporter, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithResourceAsConstantLabels(attribute.NewAllowKeysFilter(
			"service.namespace",
			"service.name",
			"service.version",
			"deployment.environment.name",
		)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics: create prometheus exporter: %w", err)
	}

	var (
		initSucceeded bool
		listener      net.Listener
		provider      *sdkmetric.MeterProvider
	)
	defer func() {
		if initSucceeded {
			return
		}
		if provider != nil {
			_ = provider.Shutdown(ctx)
		}
		if listener != nil {
			_ = listener.Close()
		}
	}()

	provider = sdkmetric.NewMeterProvider(
		meterProviderOptions(exporter, res, views, cfg.MaxUniqueRoutes)...,
	)

	if cfg.RuntimeMetrics {
		if err := runtime.Start(runtime.WithMeterProvider(provider)); err != nil {
			return nil, nil, fmt.Errorf("metrics: start runtime metrics: %w", err)
		}
	}

	listener, err = net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics: listen on %s: %w", cfg.MetricsAddr, err)
	}

	gatherer := prometheus.Gatherer(reg)
	if cfg.MaxUniqueRoutes > 0 {
		gatherer = metricscap.NewGatherer(reg, metricscap.PrometheusRule{
			MetricName: "http_server_request_duration_seconds",
			LabelName:  "http_route",
			Max:        cfg.MaxUniqueRoutes,
		})
	}

	mux := http.NewServeMux()
	// EnableOpenMetrics lets the handler content-negotiate to OpenMetrics
	// format (Accept: application/openmetrics-text). That is the only
	// exposition format that carries per-bucket exemplars, so without this
	// flag Prometheus would scrape the plain text format and the
	// trace-to-metric linkage in Grafana would silently be empty. Both
	// the SDK-managed HTTP histograms and any caller-recorded histograms
	// inherit exemplar export from a single negotiation.
	// EnableOpenMetrics lets the handler content-negotiate to OpenMetrics
	// format (Accept: application/openmetrics-text). That is the only
	// exposition format that carries per-bucket exemplars, so without this
	// flag Prometheus would scrape the plain text format and the
	// trace-to-metric linkage in Grafana would silently be empty. Both
	// the SDK-managed HTTP histograms and any caller-recorded histograms
	// inherit exemplar export from a single negotiation.
	mux.Handle("/metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()

	initSucceeded = true
	return provider, server.Shutdown, nil
}

// initOTLP sets up the OTLP push path.
func initOTLP(ctx context.Context, cfg Config, res *resource.Resource, views []sdkmetric.View) (*sdkmetric.MeterProvider, Closer, error) {
	expOpts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(cfg.MetricsOTLPEndpoint),
	}
	if len(cfg.OTLPHeaders) > 0 {
		expOpts = append(expOpts, otlpmetrichttp.WithHeaders(cfg.OTLPHeaders))
	}
	exporter, err := otlpmetrichttp.New(ctx, expOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics: create OTLP exporter: %w", err)
	}
	cappedExporter := sdkmetric.Exporter(exporter)
	if cfg.MaxUniqueRoutes > 0 {
		cappedExporter = metricscap.NewExporter(exporter, metricscap.Rule{
			InstrumentName: "http.server.request.duration",
			Key:            semconv.HTTPRouteKey,
			Max:            cfg.MaxUniqueRoutes,
		})
	}

	var initSucceeded bool
	defer func() {
		if !initSucceeded {
			_ = exporter.Shutdown(ctx)
		}
	}()

	provider := sdkmetric.NewMeterProvider(
		meterProviderOptions(sdkmetric.NewPeriodicReader(cappedExporter), res, views, cfg.MaxUniqueRoutes)...,
	)

	if cfg.RuntimeMetrics {
		if err := runtime.Start(runtime.WithMeterProvider(provider)); err != nil {
			return nil, nil, fmt.Errorf("metrics: start runtime metrics: %w", err)
		}
	}

	initSucceeded = true
	// provider.Shutdown drains the PeriodicReader which in turn calls
	// exporter.Shutdown. Returning exporter.Shutdown here would cause a
	// second shutdown when o11y.go also calls mp.Shutdown, so we return a
	// no-op: the MeterProvider shutdown path handles everything.
	return provider, func(_ context.Context) error { return nil }, nil
}

func meterProviderOptions(reader sdkmetric.Reader, res *resource.Resource, views []sdkmetric.View, maxUniqueRoutes int) []sdkmetric.Option {
	opts := []sdkmetric.Option{
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(views...),
	}
	if maxUniqueRoutes > 0 {
		opts = append(opts, sdkmetric.WithCardinalityLimit(cardinalityLimitBudget(maxUniqueRoutes)))
	}
	return opts
}

func cardinalityLimitBudget(maxUniqueRoutes int) int {
	const maxInt = int(^uint(0) >> 1)
	perRouteBudget := sdkCardinalityMethodBudget * sdkCardinalityStatusBudget
	if maxUniqueRoutes > maxInt/perRouteBudget {
		return maxInt
	}
	return maxUniqueRoutes * perRouteBudget
}

// resolveResource returns the Resource to attach to the MeterProvider.
// When cfg.Resource is set it is returned directly (shared with the trace and
// log providers). Otherwise a standalone Resource is built from the Config
// fields, which requires Namespace to be non-empty.
func resolveResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	if cfg.Resource != nil {
		return cfg.Resource, nil
	}

	if cfg.Namespace == "" {
		return nil, errors.New("metrics: Namespace is required")
	}

	resOpts := []resource.Option{
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.ServiceNamespaceKey.String(cfg.Namespace),
		),
	}
	if cfg.ServiceVersion != "" {
		resOpts = append(resOpts, resource.WithAttributes(
			semconv.ServiceVersionKey.String(cfg.ServiceVersion),
		))
	}
	if cfg.Environment != "" {
		resOpts = append(resOpts, resource.WithAttributes(
			semconv.DeploymentEnvironmentNameKey.String(cfg.Environment),
		))
	}
	res, err := resource.New(ctx, resOpts...)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("metrics: create resource: %w", err)
	}
	return res, nil
}
