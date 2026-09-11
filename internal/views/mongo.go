package views

import (
	"go.opentelemetry.io/otel/attribute"
	sdkinstrumentation "go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// MongoScope is the instrumentation scope the mongo package records its own
// pool metrics under. It is defined here, not in the mongo package, so the root
// package can name the scope without importing the mongo-driver client
// (ADR 0026 Option A); the mongo package's instrumentationName is an alias of
// this constant, so the two cannot drift.
const MongoScope = "github.com/flywindy/o11y/mongo"

// MongoContribScope is the instrumentation scope otelmongo emits
// db.client.operation.duration under. Unlike MongoScope this is not the SDK's
// to define: it is otelmongo.ScopeName, copied as a literal because importing
// otelmongo to read the constant would link the mongo driver into the root
// package — the whole cost this leaf package exists to avoid.
//
// A copy can drift from its source, so mongo.TestMongoContribScopeMatchesUpstream
// asserts the two are equal. That test lives in the mongo package, which already
// imports otelmongo, and fails if a future otelmongo release renames the scope —
// turning a silently detached view into a build failure.
const MongoContribScope = "go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo"

// Mongo returns the metric views required to keep MongoDB metric labels aligned
// with the SDK's semantic-convention and cardinality contract.
//
// histogramBuckets are applied to db.client.operation.duration so MongoDB
// latency buckets follow the SDK's configured WithHistogramBuckets policy
// (matching the HTTP and Redis duration histograms) instead of the contrib
// instrument's baked-in boundaries.
//
// That view is scoped to the otelmongo instrumentation that emits the metric so
// it never matches another integration's db.client.operation.duration
// instrument (e.g. the Redis wrapper's), which would otherwise produce a
// duplicate, conflicting stream when both wrappers are active in the same
// process. The pool views are scoped to the mongo package's own instrumentation,
// which emits them.
func Mongo(histogramBuckets []float64) []sdkmetric.View {
	views := []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "db.client.operation.duration",
				Scope: sdkinstrumentation.Scope{Name: MongoContribScope},
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
				Scope: sdkinstrumentation.Scope{Name: MongoScope},
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
				Scope: sdkinstrumentation.Scope{Name: MongoScope},
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
				Scope: sdkinstrumentation.Scope{Name: MongoScope},
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
				Scope: sdkinstrumentation.Scope{Name: MongoScope},
			},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
		))
	}

	return views
}
