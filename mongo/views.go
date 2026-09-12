package mongo

import (
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/flywindy/o11y/internal/views"
)

// MetricViews returns the metric views required to keep MongoDB metric labels
// aligned with the SDK's semantic-convention and cardinality contract: the SDK
// histogram buckets for otelmongo's db.client.operation.duration and this
// package's db.client.connection.create_time, allow-keys filters bounding the
// pool label sets, and drop views for the pool instruments the SDK does not
// publish. Each view is scoped to the instrumentation that emits the
// instrument, so it never matches another integration's identically named one.
//
// o11y.Init registers these views automatically. Services that build their own
// MeterProvider must register them via sdkmetric.WithView(MetricViews(...)...)
// on that provider; a view applies only to the MeterProvider it is registered
// with.
//
// The definitions live in the driver-free internal/views package so the root
// o11y package can register them without importing the mongo driver
// (ADR 0026 Option A); this function is the public re-export.
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	return views.Mongo(histogramBuckets)
}
