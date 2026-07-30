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

// The cap contract is "at most Max distinct real values, plus one shared
// overflow bucket". A real label value equal to the sentinel must consume a
// budget slot like any other; it previously short-circuited, so with Max=1 a
// real "other" and "/rooms" were *both* admitted — two real values exported
// while the budget was never exhausted. Reported against the Cassandra
// collection cap, where a table may legitimately be named "other", but the same
// held for http.route.
func TestLimiter_OverflowValueAsRealLabelStillConsumesBudget(t *testing.T) {
	limiter := metricscap.NewLimiter(metricscap.Rule{
		InstrumentName: durationName,
		Key:            "http.route",
		Max:            1,
	})
	rm := histogram(metricscap.OverflowValue, "/rooms")

	limiter.Rewrite(&rm)

	routes := routesFrom(t, rm)
	assert.Equal(t, []string{metricscap.OverflowValue}, routes,
		"the real \"other\" takes the only slot and /rooms collapses into it, got %v", routes)
}

// The sentinel arriving mid-stream behaves the same: it takes a slot, and later
// values collapse.
func TestLimiter_OverflowValueAdmittedMidStreamStillCapsOthers(t *testing.T) {
	limiter := metricscap.NewLimiter(metricscap.Rule{
		InstrumentName: durationName,
		Key:            "http.route",
		Max:            2,
	})
	rm := histogram("/a", metricscap.OverflowValue, "/b", "/c")

	limiter.Rewrite(&rm)

	routes := routesFrom(t, rm)
	assert.ElementsMatch(t, []string{"/a", metricscap.OverflowValue}, routes)
}

// Reverse order: the budget is filled by ordinary values before a real "other"
// arrives, so it collapses into the overflow bucket. The result is identical to
// ordinary overflow with no real "other" involved — Max real values plus the
// shared bucket — which is the contract, not a leak. Pinned because review read
// the Max+1 series count here as a cap violation.
func TestLimiter_OverflowValueArrivingAfterBudgetIsFullMatchesOrdinaryOverflow(t *testing.T) {
	newLimiter := func() *metricscap.Limiter {
		return metricscap.NewLimiter(metricscap.Rule{
			InstrumentName: durationName,
			Key:            "http.route",
			Max:            2,
		})
	}

	withRealOther := histogram("/a", "/b", metricscap.OverflowValue)
	newLimiter().Rewrite(&withRealOther)

	withoutRealOther := histogram("/a", "/b", "/c")
	newLimiter().Rewrite(&withoutRealOther)

	assert.ElementsMatch(t, routesFrom(t, withoutRealOther), routesFrom(t, withRealOther),
		"a real \"other\" arriving after the budget is full must behave exactly like any other overflowing value")
	assert.ElementsMatch(t, []string{"/a", "/b", metricscap.OverflowValue}, routesFrom(t, withRealOther),
		"Max real values plus the shared overflow bucket is the contract")
}
