package elasticsearch

import (
	"go.opentelemetry.io/otel/attribute"
	sdkinstrumentation "go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// MetricViews returns the metric views required to keep Elasticsearch metric
// labels aligned with the SDK's semantic-convention and cardinality contract.
//
// histogramBuckets are applied to db.client.operation.duration so Elasticsearch
// latency buckets follow the SDK's configured WithHistogramBuckets policy
// (matching the HTTP, MongoDB, Redis, and Cassandra duration histograms).
//
// The view is scoped to this package's instrumentation so it never matches
// another integration's db.client.operation.duration instrument (e.g. the Redis,
// MongoDB, or Cassandra wrapper's), which would otherwise produce a duplicate,
// conflicting stream when several wrappers are active in the same process. An
// allow-keys filter bounds the label set (ADR 0027 §3).
//
// The view allows db.collection.name; it reaches the series only when the
// facade emits it (on by default, see WithCollectionMetricLabel) and the request
// addressed a single index. Its distinct-value count is capped separately at
// the export boundary by o11y.WithMaxUniqueCollections — an allow-keys filter
// bounds which keys appear, not how many values a key takes.
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.operation.duration",
				Scope: sdkinstrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.DBSystemNameKey,
					semconv.DBOperationNameKey,
					semconv.DBCollectionNameKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
					semconv.DBResponseStatusCodeKey,
				),
			},
		),
	}
}
