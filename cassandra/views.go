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
//
// The query-path views allow db.collection.name; it reaches the series only when
// the observer emits it (on by default, see cassandra.WithCollectionMetricLabel)
// and a single table was resolved. Its distinct-value count is capped separately
// at the export boundary by o11y.WithMaxUniqueCollections — an allow-keys filter
// bounds which keys appear, not how many values a key takes.
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
					semconv.DBCollectionNameKey,
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
					semconv.DBClientConnectionPoolNameKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
				),
			},
		),
		// The two SDK-owned attempt counters carry the same bounded label set by
		// construction (metricAttrs / ObserveConnect), but they get the same
		// allow-keys backstop view as the histograms so a future stray attribute
		// cannot leak into them either. No bucket/aggregation override: counters
		// keep their default sum aggregation.
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "cassandra.query.attempts",
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.DBSystemNameKey,
					semconv.DBOperationNameKey,
					semconv.DBNamespaceKey,
					semconv.DBCollectionNameKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
				),
			},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "cassandra.connection.attempts",
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.DBSystemNameKey,
					semconv.DBClientConnectionPoolNameKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
				),
			},
		),
	}
}
