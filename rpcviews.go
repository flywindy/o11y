package o11y

import (
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/flywindy/o11y/internal/views"
)

// RPCMetricViews returns the views that give the semconv RPC call-duration
// histograms, rpc.server.call.duration and rpc.client.call.duration, the
// boundaries passed in, in every instrumentation scope, provided they are
// recorded in seconds ("s"). They set the boundaries only and filter no
// attributes.
//
// Init registers these views automatically with the WithHistogramBuckets set.
// Services that build their own MeterProvider must register them via
// sdkmetric.WithView(RPCMetricViews(...)...) on that provider, as they do the
// datastore packages' MetricViews; a view applies only to the MeterProvider it
// is registered with.
func RPCMetricViews(histogramBuckets []float64) []sdkmetric.View {
	return views.RPC(cloneFloat64s(histogramBuckets))
}
