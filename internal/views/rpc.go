package views

import (
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// RPC returns the views that put the semconv RPC call-duration histograms,
// rpc.server.call.duration and rpc.client.call.duration, on the SDK's
// WithHistogramBuckets policy, so a service's request/reply latency shares its
// boundaries with http.server.request.duration and the two stay comparable.
//
// No SDK integration records these instruments; an application or library
// does, under its own scope, so the views match the name in every scope. Like
// any view that sets an aggregation, they replace whatever boundaries the
// instrument advised at creation; without them a histogram that advised none
// would take OTel's millisecond-scale defaults ([0, 5, 10 … 10000]), which put
// every call under 5s in one bucket.
//
// They set the boundaries alone. RPC label sets belong to whoever records
// them, and an allow-list here would silently strip dimensions such as a
// caller's site or rpc.method.
//
// The views also require the semconv unit, "s": a recorder that keeps the
// pre-v1.39 millisecond unit under these names, or sets no unit, keeps its own
// boundaries rather than taking a seconds-scale set that would put every call
// in +Inf. The names are the semconv v1.39.0 ones (Release Candidate); TestRPC
// pins them, so a semconv upgrade that renames them has to revisit this view.
//
// No other view may claim these names without a scope: the SDK keeps the
// first matching view's stream, so a later allow-list view would be dropped
// silently.
func RPC(histogramBuckets []float64) []sdkmetric.View {
	views := make([]sdkmetric.View, 0, 2)
	for _, name := range []string{"rpc.server.call.duration", "rpc.client.call.duration"} {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{Name: name, Kind: sdkmetric.InstrumentKindHistogram, Unit: "s"},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
			},
		))
	}
	return views
}
