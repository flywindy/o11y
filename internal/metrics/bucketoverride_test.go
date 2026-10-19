package metrics_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"

	"github.com/flywindy/o11y/internal/metrics"
	"github.com/flywindy/o11y/internal/testutil"
	"github.com/flywindy/o11y/internal/views"
)

// startMeter runs InitMeter on the Prometheus path and registers its teardown.
func startMeter(t *testing.T, cfg metrics.Config) *sdkmetric.MeterProvider {
	t.Helper()
	mp, closer, err := metrics.InitMeter(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	})
	return mp
}

var leLabel = regexp.MustCompile(`le="([^"]+)"`)

// bucketBounds returns the le values of family's bucket lines in exposition
// order. The tests record one attribute set per family, so a second stream for
// the same instrument shows up as repeated boundaries.
func bucketBounds(body, family string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, family+"_bucket{") {
			continue
		}
		if m := leLabel.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// TestInitMeter_BucketOverrideAppliesToUnviewedInstrument covers an instrument
// the SDK installs no view for: the override must beat the instrument's own
// advisory boundaries, which are otherwise the only ones that apply.
func TestInitMeter_BucketOverrideAppliesToUnviewedInstrument(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.RuntimeMetrics = false
	cfg.HistogramBucketOverrides = map[string][]float64{
		"rpc.server.call.duration": {0.5, 10, 30},
	}
	mp := startMeter(t, cfg)

	hist, err := mp.Meter("app").Float64Histogram("rpc.server.call.duration",
		metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(0.005, 1, 10))
	require.NoError(t, err)
	hist.Record(context.Background(), 20)

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Equal(t, []string{"0.5", "10", "30", "+Inf"},
		bucketBounds(body, "rpc_server_call_duration_seconds"), body)
}

// TestInitMeter_BucketOverrideReplacesSDKViewBoundaries covers an instrument
// the SDK already has a view for: the override replaces that view's boundaries
// in place, so the view's attribute filter keeps applying and no second stream
// is created.
func TestInitMeter_BucketOverrideReplacesSDKViewBoundaries(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.RuntimeMetrics = false
	cfg.HistogramBucketOverrides = map[string][]float64{
		"http.server.request.duration": {0.5, 30},
	}
	mp := startMeter(t, cfg)

	meter := mp.Meter("app")
	server, err := meter.Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	server.Record(context.Background(), 0.01, metric.WithAttributes(
		semconv.HTTPRequestMethodKey.String("GET"),
		semconv.HTTPRoute("/orders/{id}"),
		semconv.HTTPResponseStatusCode(http.StatusOK),
		semconv.URLFull("https://example.test/orders/123"),
	))
	client, err := meter.Float64Histogram("http.client.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	client.Record(context.Background(), 0.01)

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Equal(t, []string{"0.5", "30", "+Inf"},
		bucketBounds(body, "http_server_request_duration_seconds"), body)
	assert.NotContains(t, body, "url_full", "the SDK view's attribute filter must still apply")
	assert.Equal(t, []string{"0.1", "1", "10", "+Inf"},
		bucketBounds(body, "http_client_request_duration_seconds"),
		"an instrument without an override keeps the shared boundaries")
}

// TestInitMeter_BucketOverrideReplacesScopedExtraViewBoundaries covers a
// scoped integration view passed as an extra view, and an identically named
// instrument from another scope that no SDK view claims: both take the
// override, the integration's attribute filter included.
func TestInitMeter_BucketOverrideReplacesScopedExtraViewBoundaries(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.RuntimeMetrics = false
	cfg.ExtraViews = views.Redis(cfg.HistogramBuckets)
	cfg.HistogramBucketOverrides = map[string][]float64{
		"db.client.operation.duration": {0.25, 30},
	}
	mp := startMeter(t, cfg)

	for _, scope := range []string{views.RedisScope, "app"} {
		hist, err := mp.Meter(scope).Float64Histogram("db.client.operation.duration", metric.WithUnit("s"))
		require.NoError(t, err)
		hist.Record(context.Background(), 0.01, metric.WithAttributes(
			semconv.DBSystemNameKey.String("redis"),
			semconv.DBQueryText("GET secret"),
		))
	}

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Equal(t, []string{"0.25", "30", "+Inf", "0.25", "30", "+Inf"},
		bucketBounds(body, "db_client_operation_duration_seconds"),
		"one stream per scope, both on the override: %s", body)
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `otel_scope_name="`+views.RedisScope+`"`) {
			assert.NotContains(t, line, "db_query_text", "the redis view's attribute filter must still apply")
		}
	}
}

// TestInitMeter_BucketOverrideAppliesWithDefaultViewsDisabled pins that
// dropping the SDK's HTTP views does not drop an override aimed at them.
func TestInitMeter_BucketOverrideAppliesWithDefaultViewsDisabled(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.RuntimeMetrics = false
	cfg.DisableDefaultViews = true
	cfg.HistogramBucketOverrides = map[string][]float64{
		"http.server.request.duration": {0.5, 30},
	}
	mp := startMeter(t, cfg)

	hist, err := mp.Meter("app").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	hist.Record(context.Background(), 0.01, metric.WithAttributes(semconv.URLFull("https://example.test/x")))

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Equal(t, []string{"0.5", "30", "+Inf"},
		bucketBounds(body, "http_server_request_duration_seconds"), body)
	assert.Contains(t, body, "url_full", "with the default views off no attribute filter applies")
}

// TestInitMeter_BucketOverrideLeavesDroppedAndNonHistogramStreams pins that an
// override only ever reshapes a histogram: a counter sharing the name, and a
// stream an SDK view drops, stay as they were.
func TestInitMeter_BucketOverrideLeavesDroppedAndNonHistogramStreams(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.RuntimeMetrics = false
	cfg.ExtraViews = []sdkmetric.View{sdkmetric.NewView(
		sdkmetric.Instrument{Name: "app.dropped.duration"},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
	)}
	cfg.HistogramBucketOverrides = map[string][]float64{
		"app.requests":         {1, 30},
		"app.dropped.duration": {1, 30},
	}
	mp := startMeter(t, cfg)

	meter := mp.Meter("app")
	counter, err := meter.Int64Counter("app.requests")
	require.NoError(t, err)
	counter.Add(context.Background(), 1)
	dropped, err := meter.Float64Histogram("app.dropped.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	dropped.Record(context.Background(), 1)

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Contains(t, body, "# TYPE app_requests_total counter", body)
	assert.NotContains(t, body, "app_dropped_duration", "a dropped stream stays dropped")
}

// TestInitMeter_BucketOverrideFillsViewWithoutAggregation covers a view that
// claims a histogram for its attribute filter alone: the override supplies the
// aggregation and the filter keeps applying.
func TestInitMeter_BucketOverrideFillsViewWithoutAggregation(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.RuntimeMetrics = false
	cfg.ExtraViews = []sdkmetric.View{sdkmetric.NewView(
		sdkmetric.Instrument{Name: "app.filtered.duration"},
		sdkmetric.Stream{AttributeFilter: attribute.NewDenyKeysFilter("app.secret")},
	)}
	cfg.HistogramBucketOverrides = map[string][]float64{
		"app.filtered.duration": {1, 30},
	}
	mp := startMeter(t, cfg)

	hist, err := mp.Meter("app").Float64Histogram("app.filtered.duration",
		metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(0.5, 5))
	require.NoError(t, err)
	hist.Record(context.Background(), 2, metric.WithAttributes(attribute.String("app.secret", "x")))

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Equal(t, []string{"1", "30", "+Inf"},
		bucketBounds(body, "app_filtered_duration_seconds"), body)
	assert.NotContains(t, body, "app_secret", "the view's attribute filter must still apply")
}
