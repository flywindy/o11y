// Package metrics encapsulates the Prometheus-exporting OTel MeterProvider
// used by the top-level o11y SDK. It is intentionally isolated from the
// trace and log subsystems: InitMeter owns its own Prometheus registry,
// HTTP server, and meter provider, and returns every shutdown knob the
// caller needs to tear them down in order.
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
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config is the subset of SDK configuration that the metrics subsystem needs.
// It is deliberately a struct rather than a list of parameters so adding a
// new option does not churn every caller.
type Config struct {
	ServiceName      string
	ServiceVersion   string
	Environment      string
	Team             string
	MetricsAddr      string
	RuntimeMetrics   bool
	HistogramBuckets []float64
}

// InitMeter builds a private Prometheus registry, an OTel MeterProvider
// backed by a Prometheus exporter, optionally starts Go runtime metrics,
// and boots an HTTP server that exposes GET /metrics on cfg.MetricsAddr.
//
// It does not mutate any global state. The returned *http.Server and
// *sdkmetric.MeterProvider must both be shut down by the caller, in that
// order, to drain in-flight scrapes before flushing the meter.
//
// Bind errors are surfaced synchronously: if cfg.MetricsAddr is already in
// use, InitMeter returns the error instead of letting a background goroutine
// InitMeter creates an isolated Prometheus-backed OpenTelemetry MeterProvider and an HTTP server
// that exposes a single /metrics endpoint.
//
// It returns the created *sdkmetric.MeterProvider and *http.Server bound to cfg.MetricsAddr, or
// an error if initialization fails (for example, when cfg.Team is empty, exporter/resource/view
// creation fails, runtime metrics cannot be started, or the metrics listener cannot be bound).
// On success the caller is responsible for shutting down the HTTP server first, then shutting
// down the returned MeterProvider.
func InitMeter(ctx context.Context, cfg Config) (*sdkmetric.MeterProvider, *http.Server, error) {
	if cfg.Team == "" {
		return nil, nil, errors.New("metrics: Team is required")
	}

	// 1. Private registry — no global Prometheus state is touched.
	reg := prometheus.NewRegistry()

	// 2. OTel Prometheus exporter. Resource attributes in the allow filter
	//    become constant labels on every series (including runtime metrics),
	//    so team="..." cannot be accidentally omitted by any instrument.
	exporter, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithResourceAsConstantLabels(attribute.NewAllowKeysFilter(
			"team",
			"service.name",
			"service.version",
			"deployment.environment",
		)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics: create prometheus exporter: %w", err)
	}

	// Guard: on any subsequent failure, release everything we have opened
	// so we do not leak goroutines, listeners, or HTTP clients.
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

	// 3. Resource — mirrors internal/trace/trace.go plus the team attribute.
	resOpts := []resource.Option{
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			attribute.String("team", cfg.Team),
		),
	}
	if cfg.ServiceVersion != "" {
		resOpts = append(resOpts, resource.WithAttributes(
			semconv.ServiceVersionKey.String(cfg.ServiceVersion),
		))
	}
	if cfg.Environment != "" {
		resOpts = append(resOpts, resource.WithAttributes(
			semconv.DeploymentEnvironmentKey.String(cfg.Environment),
		))
	}
	res, err := resource.New(ctx, resOpts...)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, nil, fmt.Errorf("metrics: create resource: %w", err)
	}

	// 4. View pinning the standard latency buckets on HTTP server histograms
	//    only — user-authored histograms via sdk.Meter(...) keep their own
	//    boundaries so app-specific instruments are not silently overridden.
	buckets := cfg.HistogramBuckets
	httpView := sdkmetric.NewView(
		sdkmetric.Instrument{Name: "http.server.*"},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: buckets,
			},
		},
	)

	// 5. MeterProvider.
	provider = sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(httpView),
	)

	// 6. Runtime metrics (Saturation golden signal).
	if cfg.RuntimeMetrics {
		if err := runtime.Start(runtime.WithMeterProvider(provider)); err != nil {
			return nil, nil, fmt.Errorf("metrics: start runtime metrics: %w", err)
		}
	}

	// 7. Bind the scrape listener synchronously so bind errors surface here.
	listener, err = net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics: listen on %s: %w", cfg.MetricsAddr, err)
	}

	// 8. Mux exposes only /metrics — nothing else should live on this port.
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 9. Serve in the background. http.ErrServerClosed is the expected
	//    outcome of Shutdown and is not worth logging.
	go func() {
		_ = server.Serve(listener)
	}()

	initSucceeded = true
	return provider, server, nil
}
