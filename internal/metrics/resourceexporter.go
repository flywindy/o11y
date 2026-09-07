package metrics

import (
	"context"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// guardedResourceExporter exports every batch with the Resource the SDK
// built instead of the one the MeterProvider holds.
//
// sdkmetric.WithResource merges resource.Environment() underneath whatever
// Resource it is given, so an OTEL_RESOURCE_ATTRIBUTES key that
// EnvResourceAttributes rejected — telemetry_sdk_name=evil next to the
// detected telemetry.sdk.name — is back on the provider's Resource by the
// time the reader collects. On the Prometheus pull path targetInfoCollector
// renders target_info from the guarded Resource, so the provider's copy is
// never exported there; on the OTLP push path the exporter is what ships
// the Resource, so this wrapper swaps it back before a downstream Prometheus
// translation (prometheusremotewrite in the collector) could join the alias
// into the label the guard protects. Metric data is exported unchanged.
type guardedResourceExporter struct {
	sdkmetric.Exporter
	res *resource.Resource
}

// withGuardedResource wraps exporter so it exports res as the Resource.
func withGuardedResource(exporter sdkmetric.Exporter, res *resource.Resource) sdkmetric.Exporter {
	return guardedResourceExporter{Exporter: exporter, res: res}
}

// Export implements sdkmetric.Exporter.
func (e guardedResourceExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	rm.Resource = e.res
	return e.Exporter.Export(ctx, rm)
}
