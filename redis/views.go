package redis

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// MetricViews returns the metric views required to keep Redis metric labels
// aligned with the SDK's semantic-convention and cardinality contract.
//
// histogramBuckets are applied to both histograms this package emits —
// db.client.operation.duration and db.client.connection.create_time — so Redis
// latency buckets follow the SDK's configured WithHistogramBuckets policy
// (matching the HTTP, MongoDB, and Cassandra duration histograms).
//
// Every view is scoped to this package's instrumentation so it never matches
// another integration's identically named instrument (e.g. the MongoDB
// wrapper's db.client.operation.duration), which would otherwise produce a
// duplicate, conflicting stream when both wrappers are active in the same
// process.
func MetricViews(histogramBuckets []float64) []sdkmetric.View {
	views := []sdkmetric.View{
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
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
				),
			},
		),
	}

	// The pool instruments this package emits itself. Their label sets are
	// already bounded by construction (poolAttrs / poolMetricAttrs in hook.go),
	// but each view carries an allow-keys backstop so a future stray attribute
	// cannot widen them — the same treatment the mongo and cassandra wrappers
	// give their pool instruments.
	poolAttrKeys := attribute.NewAllowKeysFilter(
		semconv.DBSystemNameKey,
		semconv.DBClientConnectionPoolNameKey,
		semconv.ServerAddressKey,
		semconv.ServerPortKey,
	)
	views = append(views,
		// create_time records seconds. Without this view it keeps OTel's
		// default boundaries, which are millisecond-scale ([0, 5, 10, … 10000]),
		// so every Redis dial falls into the first bucket and the histogram is
		// useless for the pool sizing it exists to support.
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.connection.create_time",
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
				AttributeFilter: poolAttrKeys,
			},
		),
		// count is the one pool instrument that also carries the used/idle
		// state dimension.
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.connection.count",
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.DBSystemNameKey,
					semconv.DBClientConnectionPoolNameKey,
					semconv.DBClientConnectionStateKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
				),
			},
		),
	)

	for _, name := range []string{
		"db.client.connection.idle.max",
		"db.client.connection.idle.min",
		"db.client.connection.max",
		"db.client.connection.timeouts",
	} {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  name,
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{AttributeFilter: poolAttrKeys},
		))
	}

	// Legacy redisotel instruments (plural "connections"). Dropped without a
	// scope filter so they are suppressed whichever library emitted them.
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
