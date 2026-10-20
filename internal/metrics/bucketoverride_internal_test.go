package metrics

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectBounds records one value on each instrument record creates through a
// provider holding views, collects in memory, and returns the exported data of
// the metric named name. No exporter or network is involved, so this pins the
// views themselves — what the OTLP path exports, before the Prometheus path's
// reserved-key guard wraps them.
func collectBounds(t *testing.T, views []sdkmetric.View, name string, record func(metric.Meter)) metricdata.Aggregation {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(views...))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	record(mp.Meter("app"))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data
			}
		}
	}
	t.Fatalf("metric %q not collected", name)
	return nil
}

func float64Bounds(t *testing.T, data metricdata.Aggregation) []float64 {
	t.Helper()
	h, ok := data.(metricdata.Histogram[float64])
	require.True(t, ok, "want an explicit float64 histogram, got %T", data)
	require.Len(t, h.DataPoints, 1)
	return h.DataPoints[0].Bounds
}

func TestApplyHistogramBucketOverrides(t *testing.T) {
	exponential := sdkmetric.NewView(
		sdkmetric.Instrument{Name: "app.exp.duration"},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{MaxSize: 160, MaxScale: 20}},
	)
	overrides := map[string][]float64{
		"app.float.duration": {1, 30},
		"app.int.size":       {10, 100},
		"app.exp.duration":   {1, 30},
		"app.case.duration":  {1, 30},
	}

	cases := []struct {
		name   string
		metric string
		record func(metric.Meter)
		check  func(*testing.T, metricdata.Aggregation)
	}{
		{
			name:   "float64 histogram with advisory takes the override",
			metric: "app.float.duration",
			record: func(m metric.Meter) {
				h, _ := m.Float64Histogram("app.float.duration", metric.WithExplicitBucketBoundaries(0.5, 5))
				h.Record(context.Background(), 2)
			},
			check: func(t *testing.T, d metricdata.Aggregation) {
				assert.Equal(t, []float64{1, 30}, float64Bounds(t, d))
			},
		},
		{
			name:   "int64 histogram takes the override",
			metric: "app.int.size",
			record: func(m metric.Meter) {
				h, _ := m.Int64Histogram("app.int.size")
				h.Record(context.Background(), 20)
			},
			check: func(t *testing.T, d metricdata.Aggregation) {
				h, ok := d.(metricdata.Histogram[int64])
				require.True(t, ok, "got %T", d)
				require.Len(t, h.DataPoints, 1)
				assert.Equal(t, []float64{10, 100}, h.DataPoints[0].Bounds)
			},
		},
		{
			name:   "view choosing an exponential histogram is left alone",
			metric: "app.exp.duration",
			record: func(m metric.Meter) {
				h, _ := m.Float64Histogram("app.exp.duration")
				h.Record(context.Background(), 2)
			},
			check: func(t *testing.T, d metricdata.Aggregation) {
				_, ok := d.(metricdata.ExponentialHistogram[float64])
				assert.True(t, ok, "want an exponential histogram, got %T", d)
			},
		},
		{
			name:   "name differing only in case is not matched",
			metric: "App.Case.Duration",
			record: func(m metric.Meter) {
				h, _ := m.Float64Histogram("App.Case.Duration", metric.WithExplicitBucketBoundaries(0.5, 5))
				h.Record(context.Background(), 2)
			},
			check: func(t *testing.T, d metricdata.Aggregation) {
				assert.Equal(t, []float64{0.5, 5}, float64Bounds(t, d), "the advisory stays")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			views := applyHistogramBucketOverrides([]sdkmetric.View{exponential}, overrides)
			tc.check(t, collectBounds(t, views, tc.metric, tc.record))
		})
	}
}

func TestApplyHistogramBucketOverrides_NoOverridesReturnsInput(t *testing.T) {
	in := []sdkmetric.View{sdkmetric.NewView(sdkmetric.Instrument{Name: "x"}, sdkmetric.Stream{})}
	for _, overrides := range []map[string][]float64{nil, {}} {
		out := applyHistogramBucketOverrides(in, overrides)
		require.Len(t, out, 1)
		assert.Same(t, &in[0], &out[0], "no overrides must not rebuild the view list")
	}
}
