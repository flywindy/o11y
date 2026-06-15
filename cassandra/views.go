package cassandra

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// MetricViews returns the metric views required to keep Cassandra metric labels
// aligned with the SDK's semantic-convention and cardinality contract.
//
// histogramBuckets are applied to db.client.operation.duration and
// db.client.connection.create_time so Cassandra latency buckets follow the
// SDK's configured WithHistogramBuckets policy (matching the HTTP, MongoDB, and
// Redis duration histograms).
//
// The operation-duration view is scoped to this package's instrumentation so it
// never matches another integration's db.client.operation.duration instrument
// (e.g. the Redis or MongoDB wrapper's), which would otherwise produce a
// duplicate, conflicting stream when several wrappers are active in the same
// process. An allow-keys filter bounds the label set (ADR 0019 §7).
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.operation.duration",
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.DBSystemNameKey,
					semconv.DBOperationNameKey,
					semconv.DBNamespaceKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
				),
			},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.connection.create_time",
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.DBSystemNameKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
				),
			},
		),
	}
}
