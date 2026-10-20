package views

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestRPC pins which instruments the RPC views claim: seconds-unit histograms
// of the two semconv names in any scope, and nothing recorded in another unit
// or under another name, which keep their own boundaries.
func TestRPC(t *testing.T) {
	advisory := metric.WithExplicitBucketBoundaries(0.5, 5)
	cases := []struct {
		name  string
		scope string
		inst  string
		unit  string
		want  []float64
	}{
		{"server in seconds", "app", "rpc.server.call.duration", "s", []float64{1, 30}},
		{"client in seconds, other scope", "lib", "rpc.client.call.duration", "s", []float64{1, 30}},
		{"milliseconds keep their advisory", "app", "rpc.server.call.duration", "ms", []float64{0.5, 5}},
		{"no unit keeps its advisory", "app", "rpc.server.call.duration", "", []float64{0.5, 5}},
		{"legacy name keeps its advisory", "app", "rpc.server.duration", "s", []float64{0.5, 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(RPC([]float64{1, 30})...))
			t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

			h, err := mp.Meter(tc.scope).Float64Histogram(tc.inst, metric.WithUnit(tc.unit), advisory)
			require.NoError(t, err)
			h.Record(context.Background(), 2)

			var rm metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(context.Background(), &rm))
			require.Len(t, rm.ScopeMetrics, 1)
			require.Len(t, rm.ScopeMetrics[0].Metrics, 1)
			data, ok := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			assert.Equal(t, tc.want, data.DataPoints[0].Bounds)
		})
	}
}
