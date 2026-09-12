package cassandra

import (
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/flywindy/o11y/internal/views"
)

// MetricViews returns the metric views required to keep Cassandra metric labels
// aligned with the SDK's semantic-convention and cardinality contract: the SDK
// histogram buckets for db.client.operation.duration and
// db.client.connection.create_time, and allow-keys filters bounding those and
// the two SDK-owned attempt counters (ADR 0019 §7). Each view is scoped to this
// package's instrumentation scope so it never matches another integration's
// identically named instrument.
//
// o11y.Init registers these views automatically. Services that build their own
// MeterProvider must register them via sdkmetric.WithView(MetricViews(...)...)
// on that provider; a view applies only to the MeterProvider it is registered
// with.
//
// The definitions live in the driver-free internal/views package so the root
// o11y package can register them without importing gocql (ADR 0026 Option A);
// this function is the public re-export.
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	return views.Cassandra(histogramBuckets)
}
