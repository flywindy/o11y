package o11y_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y"
	"github.com/flywindy/o11y/internal/testutil"
)

// TestWithLogLevel checks the level reaches the SDK's Logger: records below
// it are disabled, records at or above it are enabled, and the default is
// INFO when the option is not given.
func TestWithLogLevel(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		opts    []o11y.Option
		enabled []slog.Level
		muted   []slog.Level
	}{
		{
			name:    "default is info",
			enabled: []slog.Level{slog.LevelInfo, slog.LevelWarn, slog.LevelError},
			muted:   []slog.Level{slog.LevelDebug},
		},
		{
			name:    "warn",
			opts:    []o11y.Option{o11y.WithLogLevel(slog.LevelWarn)},
			enabled: []slog.Level{slog.LevelWarn, slog.LevelError},
			muted:   []slog.Level{slog.LevelDebug, slog.LevelInfo},
		},
		{
			name:    "debug",
			opts:    []o11y.Option{o11y.WithLogLevel(slog.LevelDebug)},
			enabled: []slog.Level{slog.LevelDebug, slog.LevelInfo},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sdk, err := o11y.Init(ctx, append(commonOpts(srv.URL), tc.opts...)...)
			require.NoError(t, err)
			defer testutil.MustShutdown(ctx, t, sdk)

			for _, level := range tc.enabled {
				assert.True(t, sdk.Logger.Enabled(ctx, level), "%s should be enabled", level)
			}
			for _, level := range tc.muted {
				assert.False(t, sdk.Logger.Enabled(ctx, level), "%s should be disabled", level)
			}
		})
	}
}

// TestWithRuntimeMetrics scrapes /metrics with runtime metrics on (the
// default) and off: the Go runtime instruments must appear only when enabled.
func TestWithRuntimeMetrics(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	for _, tc := range []struct {
		name string
		opts []o11y.Option
		want bool
	}{
		{name: "default on", want: true},
		{name: "explicitly on", opts: []o11y.Option{o11y.WithRuntimeMetrics(true)}, want: true},
		{name: "off", opts: []o11y.Option{o11y.WithRuntimeMetrics(false)}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := testutil.FreeAddr(t)
			opts := append(commonOpts(srv.URL), o11y.WithMetricsAddr(addr))
			sdk, err := o11y.Init(t.Context(), append(opts, tc.opts...)...)
			require.NoError(t, err)
			defer testutil.MustShutdown(t.Context(), t, sdk)

			// target_info is always served, so its presence says the scrape
			// endpoint is up and the runtime check below is meaningful.
			var body string
			require.Eventually(t, func() bool {
				b, err := testutil.TryScrapeMetrics(t.Context(), addr)
				body = b
				return err == nil && strings.Contains(b, "target_info{")
			}, 2*time.Second, 50*time.Millisecond)

			assert.Equal(t, tc.want, strings.Contains(body, "go_goroutine_count"),
				"go_goroutine_count present = %t, want %t", !tc.want, tc.want)
		})
	}
}

// TestWithMetricsOTLPEndpoint checks the push path: metrics are exported to
// the given endpoint's /v1/metrics, and the Prometheus scrape server is not
// started.
func TestWithMetricsOTLPEndpoint(t *testing.T) {
	traces := testutil.FakeOTLPServer(t)
	metricsSrv := testutil.NewCapturingOTLPServer(t)
	addr := testutil.FreeAddr(t)

	opts := append(commonOpts(traces.URL),
		o11y.WithMetricsAddr(addr),
		o11y.WithMetricsOTLPEndpoint(metricsSrv.URL),
	)
	sdk, err := o11y.Init(t.Context(), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sdk.Shutdown(context.Background()) }) // no-op after the explicit Shutdown below

	_, err = testutil.TryScrapeMetrics(t.Context(), addr)
	assert.Error(t, err, "the Prometheus scrape server must not start on the push path")

	counter, err := sdk.Meter("options-test").Int64Counter("options.test.pushed")
	require.NoError(t, err)
	counter.Add(t.Context(), 1)

	// Shutdown flushes the periodic reader, so the push happens here rather
	// than on the reader's interval.
	require.NoError(t, sdk.Shutdown(t.Context()))

	var paths []string
	for _, r := range metricsSrv.Requests() {
		paths = append(paths, r.Path)
	}
	assert.Contains(t, paths, "/v1/metrics", "metrics must be pushed to the configured endpoint")
}

// TestDefaultLatencyBuckets pins the documented boundaries and that each call
// returns an independent copy: mutating one result must not change the next.
func TestDefaultLatencyBuckets(t *testing.T) {
	want := []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

	first := o11y.DefaultLatencyBuckets()
	assert.Equal(t, want, first)

	first[0] = 999
	grown := append(first, 42)
	grown[1] = 999

	assert.Equal(t, want, o11y.DefaultLatencyBuckets(), "a caller's writes, in place or through append, must not reach the SDK's defaults")
}

// TestWithProfilingAuthHeaders_ReachesInit passes a header name that is not a
// valid HTTP token through the option. Init must reject it by option name,
// which shows the option reaches the SDK's configuration and its validation;
// the check runs before the profiler would start, so nothing is uploaded. The
// hand-off from there to the profiler is not exercised: the profiler is
// process-global and uploads on a 15s schedule, which a unit test cannot wait
// on or safely share.
func TestWithProfilingAuthHeaders_ReachesInit(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	opts := append(commonOpts(srv.URL),
		o11y.WithProfilingEnabled(true),
		o11y.WithProfilingEndpoint("http://127.0.0.1:1"),
		o11y.WithProfilingAuthHeaders(map[string]string{"x api key": "k"}),
	)
	sdk, err := o11y.Init(t.Context(), opts...)
	if err == nil {
		// A regression that let Init succeed would have started the
		// process-global profiler; stop it so later tests are unaffected.
		_ = sdk.Shutdown(context.Background())
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WithProfilingAuthHeaders is not a valid HTTP header name")
}
