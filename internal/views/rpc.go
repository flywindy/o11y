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
// does, under its own scope, so the views match the name in every scope. They
// set the boundaries alone. RPC label sets belong to whoever records them, and
// an allow-list here would silently strip dimensions such as a caller's site
// or rpc.method. Without these views a histogram created with no advisory
// boundaries falls back to OTel's millisecond-scale defaults ([0 … 10000]),
// which put every sub-second call in the first bucket.
func RPC(histogramBuckets []float64) []sdkmetric.View {
	views := make([]sdkmetric.View, 0, 2)
	for _, name := range []string{"rpc.server.call.duration", "rpc.client.call.duration"} {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{Name: name, Kind: sdkmetric.InstrumentKindHistogram},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
			},
		))
	}
	return views
}
