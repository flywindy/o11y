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
	views := []sdkmetric.View{
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

	poolAttrs := attribute.NewAllowKeysFilter(
		semconv.DBSystemNameKey,
		semconv.DBClientConnectionPoolNameKey,
		semconv.ServerAddressKey,
		semconv.ServerPortKey,
	)
	views = append(views,
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
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.connection.create_time",
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
				AttributeFilter: poolAttrs,
			},
		),
	)

	for _, name := range []string{
		"db.client.connection.idle.min",
		"db.client.connection.max",
		"db.client.connection.pending_requests",
		"db.client.connection.timeouts",
	} {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  name,
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{AttributeFilter: poolAttrs},
		))
	}

	for _, name := range []string{
		"db.client.connection.idle.max",
		"db.client.connection.use_time",
		"db.client.connection.wait_time",
	} {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  name,
				Scope: instrumentation.Scope{Name: instrumentationName},
			},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
		))
	}

	return views
}
