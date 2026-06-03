package o11y_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y"
	"github.com/flywindy/o11y/internal/testutil"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// commonOpts returns the full set of required options for Init to succeed in
// tests. It uses a randomly chosen metrics port so concurrent tests never
// fight over :2112.
func commonOpts(srvURL string) []o11y.Option {
	return []o11y.Option{
		o11y.WithServiceName("test-svc"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithMetricsAddr("127.0.0.1:0"),
		o11y.WithOTLPEndpoint(srvURL),
	}
}

func TestInit_MissingServiceName(t *testing.T) {
	_, err := o11y.Init(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service name is required")
}

func TestInit_MissingServiceVersion(t *testing.T) {
	_, err := o11y.Init(context.Background(),
		o11y.WithServiceName("test-svc"),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service version is required")
}

func TestInit_MissingNamespace(t *testing.T) {
	_, err := o11y.Init(context.Background(),
		o11y.WithServiceName("test-svc"),
		o11y.WithServiceVersion("0.1.0"),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service namespace is required")
}

func TestInit_MissingEnvironment(t *testing.T) {
	_, err := o11y.Init(context.Background(),
		o11y.WithServiceName("test-svc"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithServiceNamespace("platform"),
		// WithEnvironment omitted — defaultConfig has no default env
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deployment environment is required")
}

func TestInit_UnknownEnvironment(t *testing.T) {
	_, err := o11y.Init(context.Background(),
		o11y.WithServiceName("test-svc"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithEnvironment("uat"), // not in the allowed set
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown deployment environment")
}

// TestInit_EnvironmentAliases verifies that common shorthand values are
// normalized to canonical names without error.
func TestInit_EnvironmentAliases(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	aliases := []string{"prod", "stg", "stage", "dev", "test"}
	for _, alias := range aliases {
		t.Run(alias, func(t *testing.T) {
			opts := append(commonOpts(srv.URL), o11y.WithEnvironment(alias))
			sdk, err := o11y.Init(context.Background(), opts...)
			require.NoError(t, err, "alias %q should be accepted", alias)
			testutil.MustShutdown(t.Context(), t, sdk)
		})
	}
}

func TestInit_Success(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	require.NotNil(t, sdk)
	assert.NotNil(t, sdk.Logger, "Logger must be set")
	assert.NotNil(t, sdk.Propagator, "Propagator must be set")
	assert.NotNil(t, sdk.Tracer("test"), "Tracer must be obtainable")
	assert.NotNil(t, sdk.TracerProvider(), "TracerProvider must be obtainable")
	assert.NotNil(t, sdk.Meter("test"), "Meter must be obtainable")
	assert.NotNil(t, sdk.MeterProvider(), "MeterProvider must be obtainable")

	testutil.MustShutdown(t.Context(), t, sdk)
}

func TestInit_HandlesNilOption(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	opts := append([]o11y.Option{nil}, commonOpts(srv.URL)...)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	require.NotNil(t, sdk)
	testutil.MustShutdown(t.Context(), t, sdk)
}

func TestSDK_TracerIsNamed(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.NotNil(t, sdk.Tracer("a"))
	assert.NotNil(t, sdk.Tracer("b"))
}

// TestInit_RejectsInvalidHistogramBuckets locks down the validation paths so
// future refactors cannot quietly disable them.
func TestInit_RejectsInvalidHistogramBuckets(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	cases := []struct {
		name    string
		buckets []float64
		want    string
	}{
		{"empty", []float64{}, "must not be empty"},
		{"non-positive", []float64{0, 1}, "must be strictly positive"},
		{"negative", []float64{-1, 1}, "must be strictly positive"},
		{"unsorted", []float64{0.5, 0.1}, "strictly increasing"},
		{"duplicate", []float64{0.5, 0.5}, "strictly increasing"},
		{"nan", []float64{math.NaN(), 1}, "must be a finite number"},
		{"posinf", []float64{math.Inf(1), 1}, "must be a finite number"},
		{"neginf", []float64{math.Inf(-1), 1}, "must be a finite number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append(commonOpts(srv.URL), o11y.WithHistogramBuckets(tc.buckets))
			_, err := o11y.Init(context.Background(), opts...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestInit_RejectsInvalidSamplingRatio verifies the SDK rejects typed sampling
// ratios that would otherwise be silently clamped by the OTel SDK.
func TestInit_RejectsInvalidSamplingRatio(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	cases := []struct {
		name  string
		ratio float64
	}{
		{"negative", -0.1},
		{"above_one", 1.1},
		{"nan", math.NaN()},
		{"posinf", math.Inf(1)},
		{"neginf", math.Inf(-1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append(commonOpts(srv.URL), o11y.WithSamplingRatio(tc.ratio))
			_, err := o11y.Init(context.Background(), opts...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "sampling ratio must be between 0.0 and 1.0 inclusive")
		})
	}
}

// TestInit_SkipsSamplingRatioValidationWhenTraceDisabled verifies sampling
// configuration does not block startup when the trace pillar is disabled.
func TestInit_SkipsSamplingRatioValidationWhenTraceDisabled(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	opts := append(commonOpts(srv.URL),
		o11y.WithTraceEnabled(false),
		o11y.WithSamplingRatio(math.NaN()),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Trace)
	assert.False(t, startRootSpanSampled(sdk),
		"trace-disabled SDK should expose a no-op tracer provider")
}

// TestInit_AcceptsSamplingRatioBounds verifies valid boundary and hot-path
// sampling ratios can initialize successfully.
func TestInit_AcceptsSamplingRatioBounds(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	for _, ratio := range []float64{0, 0.001, 1} {
		t.Run(fmt.Sprintf("%g", ratio), func(t *testing.T) {
			opts := append(commonOpts(srv.URL), o11y.WithSamplingRatio(ratio))
			sdk, err := o11y.Init(context.Background(), opts...)
			require.NoError(t, err)
			testutil.MustShutdown(t.Context(), t, sdk)
		})
	}
}

// TestInit_SamplingPrecedence locks down ADR 0015 precedence: typed samplers
// override OTel env samplers, while an unset typed option preserves env/defaults.
func TestInit_SamplingPrecedence(t *testing.T) {
	t.Run("env sampler applies when typed option unset", func(t *testing.T) {
		srv := testutil.FakeOTLPServer(t)
		t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
		unsetEnvForTest(t, "OTEL_TRACES_SAMPLER_ARG")

		sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
		require.NoError(t, err)
		defer testutil.MustShutdown(t.Context(), t, sdk)

		assert.False(t, startRootSpanSampled(sdk),
			"OTEL_TRACES_SAMPLER=always_off must be honored when no typed sampler is set")
	})

	t.Run("typed sampling ratio overrides env sampler", func(t *testing.T) {
		srv := testutil.FakeOTLPServer(t)
		t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
		unsetEnvForTest(t, "OTEL_TRACES_SAMPLER_ARG")

		opts := append(commonOpts(srv.URL), o11y.WithSamplingRatio(1.0))
		sdk, err := o11y.Init(context.Background(), opts...)
		require.NoError(t, err)
		defer testutil.MustShutdown(t.Context(), t, sdk)

		assert.True(t, startRootSpanSampled(sdk),
			"WithSamplingRatio must override OTEL_TRACES_SAMPLER")
	})

	t.Run("default sampler samples when env and typed option unset", func(t *testing.T) {
		srv := testutil.FakeOTLPServer(t)
		unsetEnvForTest(t, "OTEL_TRACES_SAMPLER")
		unsetEnvForTest(t, "OTEL_TRACES_SAMPLER_ARG")

		sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
		require.NoError(t, err)
		defer testutil.MustShutdown(t.Context(), t, sdk)

		assert.True(t, startRootSpanSampled(sdk),
			"OTel default ParentBased(AlwaysSample) should sample root spans")
	})
}

// TestInit_WithTraceSampler verifies custom sampler wiring and the nil sampler
// behavior that intentionally leaves OTel env/default sampling active.
func TestInit_WithTraceSampler(t *testing.T) {
	t.Run("custom sampler", func(t *testing.T) {
		srv := testutil.FakeOTLPServer(t)
		opts := append(commonOpts(srv.URL), o11y.WithTraceSampler(sdktrace.NeverSample()))
		sdk, err := o11y.Init(context.Background(), opts...)
		require.NoError(t, err)
		defer testutil.MustShutdown(t.Context(), t, sdk)

		assert.False(t, startRootSpanSampled(sdk),
			"custom NeverSample sampler should drop root spans")
	})

	t.Run("nil sampler preserves env", func(t *testing.T) {
		srv := testutil.FakeOTLPServer(t)
		t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
		unsetEnvForTest(t, "OTEL_TRACES_SAMPLER_ARG")

		opts := append(commonOpts(srv.URL), o11y.WithTraceSampler(nil))
		sdk, err := o11y.Init(context.Background(), opts...)
		require.NoError(t, err)
		defer testutil.MustShutdown(t.Context(), t, sdk)

		assert.False(t, startRootSpanSampled(sdk),
			"WithTraceSampler(nil) should leave OTEL_TRACES_SAMPLER active")
	})
}

// startRootSpanSampled starts a local root span and reports whether the active
// sampler marked it as sampled.
func startRootSpanSampled(sdk *o11y.SDK) bool {
	_, span := sdk.Tracer("sampling-test").Start(context.Background(), "root")
	defer span.End()
	return span.SpanContext().IsSampled()
}

// unsetEnvForTest removes an environment variable for the current test and
// restores its previous state during cleanup.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	oldValue, hadOldValue := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() {
		if hadOldValue {
			_ = os.Setenv(key, oldValue)
			return
		}
		_ = os.Unsetenv(key)
	})
}

// TestInit_AcceptsExtremeValidBuckets ensures very small and very large
// finite values pass validation; only NaN/Inf/non-positive should be rejected.
func TestInit_AcceptsExtremeValidBuckets(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	opts := append(commonOpts(srv.URL),
		o11y.WithHistogramBuckets([]float64{1e-9, 1, 1e9}),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	testutil.MustShutdown(t.Context(), t, sdk)
}

func TestInit_AcceptsHTTPMetricGovernanceOptions(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	opts := append(commonOpts(srv.URL),
		o11y.WithDisableDefaultViews(),
		o11y.WithMaxUniqueRoutes(7),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	testutil.MustShutdown(t.Context(), t, sdk)
}

// TestInit_OTLPHeadersForwarded confirms WithOTLPHeaders reaches the
// outbound HTTP request. We use a capturing fake server in place of a real
// OTel Collector and trigger a forced flush so the trace exporter sends at
// least one request before shutdown.
func TestInit_OTLPHeadersForwarded(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)

	opts := append(commonOpts(srv.URL),
		o11y.WithOTLPHeaders(map[string]string{
			"Authorization":    "Bearer secret-token",
			"X-Scope-OrgID":    "tenant-42",
			"X-Honeycomb-Team": "abc123",
		}),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)

	// Emit at least one span so the exporter has something to send; Shutdown
	// drains the batch processor and makes the assertion deterministic.
	_, span := sdk.Tracer("hdr-test").Start(context.Background(), "probe")
	span.End()
	testutil.MustShutdown(t.Context(), t, sdk)

	requests := srv.Requests()
	require.NotEmpty(t, requests, "exporter should have made at least one OTLP request")

	// Verify every captured request carries every configured header — not
	// just at least one. An existential check would silently miss a
	// regression that drops the header on retries or later batches.
	// http.Header.Get applies textproto.CanonicalMIMEHeaderKey, so any
	// casing of the input maps to the same canonical key.
	for i, r := range requests {
		assert.Equal(t, "Bearer secret-token", r.Header.Get("Authorization"),
			"Authorization header must propagate on every OTLP request (request[%d] %s %s)",
			i, r.Method, r.Path)
		assert.Equal(t, "tenant-42", r.Header.Get("X-Scope-OrgID"),
			"X-Scope-OrgID header must propagate on every OTLP request (request[%d])", i)
		assert.Equal(t, "abc123", r.Header.Get("X-Honeycomb-Team"),
			"custom auth header must propagate on every OTLP request (request[%d])", i)
	}
}
