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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	otlpmetrichttp "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// Config is the subset of SDK configuration that the metrics subsystem needs.
//
// When Resource is non-nil, InitMeter uses it directly and skips building its
// own. Service-identity attributes (service.name, service.version,
// deployment.environment.name) and the team attribute must already be present
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

	ServiceName    string
	ServiceVersion string
	Environment    string
	// Namespace maps to service.namespace (OTel semconv). Required when
	// Resource is nil. It becomes a constant Prometheus label
	// (service_namespace="...") on every series for alert routing and
	// ownership governance.
	Namespace string

	MetricsAddr      string
	RuntimeMetrics   bool
	HistogramBuckets []float64
}

// Closer is a function that shuts down a component. For the Prometheus path it
// shuts down the HTTP server; for the OTLP path it shuts down the exporter.
// It is always safe to call even if the component was never started.
type Closer func(context.Context) error

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

	httpView := sdkmetric.NewView(
		sdkmetric.Instrument{Name: "http.server.*"},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: cfg.HistogramBuckets,
			},
		},
	)

	if cfg.MetricsOTLPEndpoint != "" {
		return initOTLP(ctx, cfg, res, httpView)
	}
	return initPrometheus(ctx, cfg, res, httpView)
}

// initPrometheus sets up the Prometheus pull path.
func initPrometheus(ctx context.Context, cfg Config, res *resource.Resource, view sdkmetric.View) (*sdkmetric.MeterProvider, Closer, error) {
	reg := prometheus.NewRegistry()

	// Resource attributes in the allow filter become constant labels on every
	// series, so service_namespace="..." is guaranteed on every instrument including runtime.
	// The key "deployment.environment.name" matches semconv v1.27.0.
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
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(view),
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

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()

	initSucceeded = true
	return provider, server.Shutdown, nil
}

// initOTLP sets up the OTLP push path.
func initOTLP(ctx context.Context, cfg Config, res *resource.Resource, view sdkmetric.View) (*sdkmetric.MeterProvider, Closer, error) {
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(cfg.MetricsOTLPEndpoint),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics: create OTLP exporter: %w", err)
	}

	var initSucceeded bool
	defer func() {
		if !initSucceeded {
			_ = exporter.Shutdown(ctx)
		}
	}()

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(view),
	)

	if cfg.RuntimeMetrics {
		if err := runtime.Start(runtime.WithMeterProvider(provider)); err != nil {
			return nil, nil, fmt.Errorf("metrics: start runtime metrics: %w", err)
		}
	}

	initSucceeded = true
	return provider, exporter.Shutdown, nil
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
