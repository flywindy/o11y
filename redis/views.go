package redis

import (
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// MetricViews returns the metric views required to keep Redis metric labels
// aligned with the SDK's semantic-convention and cardinality contract.
//
// histogramBuckets are applied to db.client.operation.duration so Redis latency
// buckets follow the SDK's configured WithHistogramBuckets policy (matching the
// HTTP and MongoDB duration histograms).
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	views := []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "db.client.operation.duration"},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.DBSystemNameKey,
					semconv.DBOperationNameKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
				),
			},
		),
	}

	for _, name := range []string{
		"db.client.connections.usage",
		"db.client.connections.idle.max",
		"db.client.connections.idle.min",
		"db.client.connections.max",
		"db.client.connections.pending_requests",
		"db.client.connections.timeouts",
		"db.client.connections.hits",
		"db.client.connections.misses",
		"db.client.connections.create_time",
		"db.client.connections.use_time",
		"db.client.connections.wait_time",
	} {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{Name: name},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
		))
	}

	return views
}
