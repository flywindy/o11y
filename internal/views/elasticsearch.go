// Package views holds metric view definitions for the SDK's integrations in a
// driver-free leaf package: it imports only OpenTelemetry, never a database or
// messaging client.
//
// The root o11y package composes every integration's views into o11y.Init so a
// service gets correct histogram buckets and bounded labels with zero
// configuration. Importing an integration package for that alone would make
// its driver a compile-time dependency of every consumer of the root package
// (ADR 0026 Option A). Each integration re-exports its function here as its
// public MetricViews, so the documented self-built-MeterProvider path is
// unchanged.
package views

import (
	"go.opentelemetry.io/otel/attribute"
	sdkinstrumentation "go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// ElasticsearchScope is the instrumentation scope the elasticsearch package
// records its SDK-owned metrics under (ADR 0027 §5). It is defined here, not in
// the elasticsearch package, so the root package and internal/metrics can name
// the scope without importing the go-elasticsearch client.
const ElasticsearchScope = "github.com/flywindy/o11y/elasticsearch"

// Elasticsearch returns the metric views that keep the elasticsearch package's
// labels aligned with the SDK's semantic-convention and cardinality contract.
//
// histogramBuckets are applied to db.client.operation.duration so Elasticsearch
// latency buckets follow the SDK's configured WithHistogramBuckets policy
// (matching the HTTP, MongoDB, Redis, and Cassandra duration histograms).
//
// The view is scoped to the elasticsearch instrumentation scope so it never
// matches another integration's db.client.operation.duration instrument (e.g.
// the Redis, MongoDB, or Cassandra wrapper's), which would otherwise produce a
// duplicate, conflicting stream when several wrappers are active in the same
// process. An allow-keys filter bounds the label set (ADR 0027 §3).
//
// The view allows db.collection.name; it reaches the series only when the
// facade emits it (on by default, see elasticsearch.WithCollectionMetricLabel)
// and the request addressed a single index. Its distinct-value count is capped
// separately at the export boundary by o11y.WithMaxUniqueCollections — an
// allow-keys filter bounds which keys appear, not how many values a key takes.
func Elasticsearch(histogramBuckets []float64) []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.operation.duration",
				Scope: sdkinstrumentation.Scope{Name: ElasticsearchScope},
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
