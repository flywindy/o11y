package o11y_test

import (
	"context"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"

	"github.com/flywindy/o11y"
	"github.com/flywindy/o11y/internal/testutil"
)

var leLabel = regexp.MustCompile(`le="([^"]+)"`)

// scrapeBucketBounds waits for family's bucket lines and returns their le
// values in exposition order.
func scrapeBucketBounds(t *testing.T, addr, family string) []string {
	t.Helper()
	var out []string
	require.Eventually(t, func() bool {
		body, err := testutil.TryScrapeMetrics(t.Context(), addr)
		if err != nil {
			return false
		}
		out = out[:0]
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, family+"_bucket{") {
				continue
			}
			if m := leLabel.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		}
		return len(out) > 0
	}, 2*time.Second, 50*time.Millisecond)
	return out
}

// TestWithInstrumentHistogramBuckets checks the override reaches an
// instrument the SDK has no view for, beating the instrument's advisory
// boundaries, while the shared boundaries keep applying elsewhere.
func TestWithInstrumentHistogramBuckets(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	addr := testutil.FreeAddr(t)

	opts := append(commonOpts(srv.URL),
		o11y.WithMetricsAddr(addr),
		o11y.WithRuntimeMetrics(false),
		o11y.WithHistogramBuckets([]float64{0.1, 1, 10}),
		o11y.WithInstrumentHistogramBuckets("rpc.server.call.duration", []float64{1, 10}),
		// A later call for the same instrument replaces the earlier one.
		o11y.WithInstrumentHistogramBuckets("rpc.server.call.duration", []float64{0.5, 10, 30}),
	)
	sdk, err := o11y.Init(t.Context(), opts...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	meter := sdk.Meter("app")
	rpc, err := meter.Float64Histogram("rpc.server.call.duration",
		metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(o11y.DefaultLatencyBuckets()...))
	require.NoError(t, err)
	rpc.Record(t.Context(), 20)
	httpClient, err := meter.Float64Histogram("http.client.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	httpClient.Record(t.Context(), 0.2)

	assert.Equal(t, []string{"0.5", "10", "30", "+Inf"},
		scrapeBucketBounds(t, addr, "rpc_server_call_duration_seconds"))
	assert.Equal(t, []string{"0.1", "1", "10", "+Inf"},
		scrapeBucketBounds(t, addr, "http_client_request_duration_seconds"))
}

// TestWithInstrumentHistogramBuckets_CopiesInput pins that mutating the
// caller's slice after the option is built does not leak into the SDK.
func TestWithInstrumentHistogramBuckets_CopiesInput(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	addr := testutil.FreeAddr(t)

	buckets := []float64{0.5, 30}
	opt := o11y.WithInstrumentHistogramBuckets("rpc.client.call.duration", buckets)
	buckets[1] = 0.1 // would make the set unsorted if it were shared

	opts := append(commonOpts(srv.URL), o11y.WithMetricsAddr(addr), o11y.WithRuntimeMetrics(false), opt)
	sdk, err := o11y.Init(t.Context(), opts...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	hist, err := sdk.Meter("app").Float64Histogram("rpc.client.call.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	hist.Record(t.Context(), 1)

	assert.Equal(t, []string{"0.5", "30", "+Inf"},
		scrapeBucketBounds(t, addr, "rpc_client_call_duration_seconds"))
}

// TestInit_RejectsInvalidInstrumentHistogramBuckets locks down the validation
// of both the instrument name and its boundaries; every error names the
// offending instrument.
func TestInit_RejectsInvalidInstrumentHistogramBuckets(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	cases := []struct {
		name       string
		instrument string
		buckets    []float64
		want       string
	}{
		{"empty name", "", []float64{1}, "instrument name must not be empty"},
		{"blank name", "  ", []float64{1}, "instrument name must not be empty"},
		{"star wildcard", "rpc.*.call.duration", []float64{1}, "wildcard"},
		{"question wildcard", "rpc.server.call.duratio?", []float64{1}, "wildcard"},
		{"empty buckets", "rpc.server.call.duration", nil, "must not be empty"},
		{"unsorted", "rpc.server.call.duration", []float64{30, 1}, "strictly increasing"},
		{"non-positive", "rpc.server.call.duration", []float64{0, 1}, "must be strictly positive"},
		{"nan", "rpc.server.call.duration", []float64{math.NaN()}, "must be a finite number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append(commonOpts(srv.URL),
				o11y.WithInstrumentHistogramBuckets(tc.instrument, tc.buckets))
			_, err := o11y.Init(context.Background(), opts...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			if strings.TrimSpace(tc.instrument) != "" {
				assert.Contains(t, err.Error(), tc.instrument)
			}
		})
	}
}
