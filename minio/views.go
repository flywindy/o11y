package minio

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// MetricViews returns the metric.Views that bound the cardinality of
// minio.client.operation.duration and pin its histogram buckets to the
// SDK's WithHistogramBuckets policy.
//
// OTel Go Views are fixed at MeterProvider construction and cannot be
// retro-fitted at instrument creation, so this slice is composed by
// o11y.Init via the ExtraViews seam (matching the redis/mongo pattern).
// Services that build their own MeterProvider must register the same
// views via sdkmetric.WithView(...) at construction; otherwise the
// allowlist is not in effect.
//
// The view is scoped to this package's instrumentation scope so it never
// matches another integration's operation-duration instrument.
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "minio.client.operation.duration",
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
				AttributeFilter: attribute.NewAllowKeysFilter(
					objectStoreOperationNameKey,
					objectStoreBucketNameKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
				),
			},
		),
	}
}
