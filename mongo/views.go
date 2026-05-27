package mongo

import (
	otelmongo "go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
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
//
// The view is scoped to the otelmongo instrumentation that emits the metric so
// it never matches another integration's db.client.operation.duration
// instrument (e.g. the Redis wrapper's), which would otherwise produce a
// duplicate, conflicting stream when both wrappers are active in the same
// process.
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.operation.duration",
				Scope: instrumentation.Scope{Name: otelmongo.ScopeName},
			},
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
