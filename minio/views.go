package minio

import (
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/flywindy/o11y/internal/views"
)

// MetricViews returns the metric views that bound the cardinality of
// minio.client.operation.duration and pin its histogram buckets to the SDK's
// WithHistogramBuckets policy. The view is scoped to this package's
// instrumentation scope so it never matches another integration's
// operation-duration instrument.
//
// o11y.Init registers these views automatically. Services that build their own
// MeterProvider must register them via sdkmetric.WithView(MetricViews(...)...)
// on that provider; a view applies only to the MeterProvider it is registered
// with, and without it the allowlist is not in effect.
//
// The definitions live in the driver-free internal/views package so the root
// o11y package can register them without importing the minio-go client
// (ADR 0026 Option A); this function is the public re-export.
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	return views.Minio(histogramBuckets)
}
