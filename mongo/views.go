package mongo

import (
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// MetricViews returns the metric views required to keep MongoDB metric labels
// aligned with the SDK's semantic-convention and cardinality contract.
//
// histogramBuckets are applied to db.client.operation.duration so MongoDB
// latency buckets follow the SDK's configured WithHistogramBuckets policy
// (matching the HTTP and Redis duration histograms) instead of the contrib
// instrument's baked-in boundaries.
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "db.client.operation.duration"},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.DBSystemNameKey,
					semconv.DBOperationNameKey,
					semconv.NetworkPeerAddressKey,
					semconv.NetworkPeerPortKey,
					semconv.ErrorTypeKey,
				),
			},
		),
	}
}
