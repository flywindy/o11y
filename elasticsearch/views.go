package elasticsearch

import (
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/flywindy/o11y/internal/views"
)

// MetricViews returns the metric views required to keep Elasticsearch metric
// labels aligned with the SDK's semantic-convention and cardinality contract:
// the SDK histogram buckets for db.client.operation.duration and an allow-keys
// filter for its label set, scoped to this package's instrumentation scope so
// the view never matches another integration's identically named instrument.
//
// o11y.Init registers these views automatically. Services that build their own
// MeterProvider must register them via sdkmetric.WithView(MetricViews(...)...)
// on that provider; a view applies only to the MeterProvider it is registered
// with.
//
// The definition lives in the driver-free internal/views package so the root
// o11y package can register it without importing the go-elasticsearch client
// (ADR 0026 Option A, applied here per ADR 0027 §5); this function is the
// public re-export.
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	return views.Elasticsearch(histogramBuckets)
}
