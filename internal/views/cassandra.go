package views

import (
	"go.opentelemetry.io/otel/attribute"
	sdkinstrumentation "go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// CassandraScope is the instrumentation scope the cassandra package records its
// metrics under. It is defined here, not in the cassandra package, so the root
// package can name the scope without importing gocql (ADR 0026 Option A); the
// cassandra package's instrumentationName is an alias of this constant, so the
// two cannot drift.
const CassandraScope = "github.com/flywindy/o11y/cassandra"

// Cassandra returns the metric views required to keep Cassandra metric labels
// aligned with the SDK's semantic-convention and cardinality contract.
//
// histogramBuckets are applied to db.client.operation.duration and
// db.client.connection.create_time so Cassandra latency buckets follow the
// SDK's configured WithHistogramBuckets policy (matching the HTTP, MongoDB, and
// Redis duration histograms).
//
// The operation-duration view is scoped to the cassandra instrumentation so it
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
func Cassandra(histogramBuckets []float64) []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.operation.duration",
				Scope: sdkinstrumentation.Scope{Name: CassandraScope},
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
				Scope: sdkinstrumentation.Scope{Name: CassandraScope},
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
				Scope: sdkinstrumentation.Scope{Name: CassandraScope},
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
				Scope: sdkinstrumentation.Scope{Name: CassandraScope},
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
