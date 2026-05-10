package metricscap_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/flywindy/o11y/internal/metricscap"
)

const durationName = "http.server.request.duration"

func histogram(routes ...string) metricdata.ResourceMetrics {
	points := make([]metricdata.HistogramDataPoint[float64], 0, len(routes))
	for _, route := range routes {
		points = append(points, metricdata.HistogramDataPoint[float64]{
			Attributes: attribute.NewSet(
				attribute.String("http.request.method", "GET"),
				attribute.String("http.route", route),
				attribute.Int("http.response.status_code", 200),
			),
			Count:        1,
			Sum:          0.01,
			Bounds:       []float64{0.1, 1},
			BucketCounts: []uint64{1, 0, 0},
			Min:          metricdata.NewExtrema(0.01),
			Max:          metricdata.NewExtrema(0.01),
		})
	}
	return metricdata.ResourceMetrics{
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Metrics: []metricdata.Metrics{{
				Name: durationName,
				Data: metricdata.Histogram[float64]{DataPoints: points},
			}},
		}},
	}
}

func routesFrom(t *testing.T, rm metricdata.ResourceMetrics) []string {
	t.Helper()
	data, ok := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	routes := make([]string, 0, len(data.DataPoints))
	for _, dp := range data.DataPoints {
		value, ok := dp.Attributes.Value("http.route")
		require.True(t, ok)
		routes = append(routes, value.AsString())
	}
	return routes
}

func TestLimiter_UnderCapPassesThrough(t *testing.T) {
	limiter := metricscap.NewLimiter(metricscap.Rule{
		InstrumentName: durationName,
		Key:            "http.route",
		Max:            3,
	})
	rm := histogram("/a", "/b", "/c")

	limiter.Rewrite(&rm)

	assert.ElementsMatch(t, []string{"/a", "/b", "/c"}, routesFrom(t, rm))
}

func TestLimiter_OverCapCollapsesToOther(t *testing.T) {
	limiter := metricscap.NewLimiter(metricscap.Rule{
		InstrumentName: durationName,
		Key:            "http.route",
		Max:            2,
	})
	rm := histogram("/a", "/b", "/c", "/d")

	limiter.Rewrite(&rm)

	assert.ElementsMatch(t, []string{"/a", "/b", metricscap.OverflowValue}, routesFrom(t, rm))
}

func TestLimiter_RehashesAndMergesCollapsedDatapoints(t *testing.T) {
	limiter := metricscap.NewLimiter(metricscap.Rule{
		InstrumentName: durationName,
		Key:            "http.route",
		Max:            1,
	})
	rm := histogram("/keep", "/overflow-a", "/overflow-b")

	limiter.Rewrite(&rm)

	data := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64])
	require.Len(t, data.DataPoints, 2)

	var overflow metricdata.HistogramDataPoint[float64]
	for _, dp := range data.DataPoints {
		value, _ := dp.Attributes.Value("http.route")
		if value.AsString() == metricscap.OverflowValue {
			overflow = dp
		}
	}
	assert.Equal(t, uint64(2), overflow.Count)
	assert.Equal(t, []uint64{2, 0, 0}, overflow.BucketCounts)
	assert.InDelta(t, 0.02, overflow.Sum, 0.000001)
}

func TestLimiter_DoesNotMutateInputDatapointSlices(t *testing.T) {
	limiter := metricscap.NewLimiter(metricscap.Rule{
		InstrumentName: durationName,
		Key:            "http.route",
		Max:            1,
	})
	rm := histogram("/keep", "/overflow-a", "/overflow-b")
	data := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64])
	originalPoints := data.DataPoints

	limiter.Rewrite(&rm)

	route, ok := originalPoints[1].Attributes.Value("http.route")
	require.True(t, ok)
	assert.Equal(t, "/overflow-a", route.AsString())
	assert.Equal(t, []uint64{1, 0, 0}, originalPoints[1].BucketCounts)
}

func TestLimiter_MergesCollapsedGaugeDatapoints(t *testing.T) {
	limiter := metricscap.NewLimiter(metricscap.Rule{
		InstrumentName: durationName,
		Key:            "http.route",
		Max:            1,
	})
	rm := metricdata.ResourceMetrics{
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Metrics: []metricdata.Metrics{{
				Name: durationName,
				Data: metricdata.Gauge[int64]{DataPoints: []metricdata.DataPoint[int64]{
					{Attributes: routeSet("/keep"), Value: 1},
					{Attributes: routeSet("/overflow-a"), Value: 2},
					{Attributes: routeSet("/overflow-b"), Value: 3},
				}},
			}},
		}},
	}

	limiter.Rewrite(&rm)

	data := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Gauge[int64])
	require.Len(t, data.DataPoints, 2)
	for _, dp := range data.DataPoints {
		value, _ := dp.Attributes.Value("http.route")
		if value.AsString() == metricscap.OverflowValue {
			assert.Equal(t, int64(3), dp.Value)
		}
	}
}

func TestLimiter_IsConcurrentSafe(_ *testing.T) {
	limiter := metricscap.NewLimiter(metricscap.Rule{
		InstrumentName: durationName,
		Key:            attribute.Key("http.route"),
		Max:            10,
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rm := histogram(fmt.Sprintf("/path-%d", i))
			limiter.Rewrite(&rm)
		}(i)
	}
	wg.Wait()
}

func routeSet(route string) attribute.Set {
	return attribute.NewSet(attribute.String("http.route", route))
}
