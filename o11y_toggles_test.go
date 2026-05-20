package o11y_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y"
	"github.com/flywindy/o11y/internal/testutil"
)

// TestToggles_DefaultAllEnabled verifies the zero-value behaviour: all three
// pillars are active when no explicit toggle option is provided.
func TestToggles_DefaultAllEnabled(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.True(t, sdk.Toggles.Trace, "trace should be enabled by default")
	assert.True(t, sdk.Toggles.Metrics, "metrics should be enabled by default")
	assert.True(t, sdk.Toggles.Log, "log should be enabled by default")
}

// TestToggles_TraceDisabled checks that a no-op TracerProvider is returned and
// no OTLP span requests are sent when trace is turned off.
func TestToggles_TraceDisabled(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)
	opts := append(commonOpts(srv.URL), o11y.WithTraceEnabled(false))
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Trace)
	assert.NotNil(t, sdk.TracerProvider(), "TracerProvider must never be nil")
	assert.NotNil(t, sdk.MeterProvider(), "MeterProvider must still be available")
	assert.NotNil(t, sdk.Logger, "Logger must still be available")
	assert.NotNil(t, sdk.Propagator, "Propagator must still be set for header forwarding")

	// A span produced by the no-op provider carries an invalid (zero) trace ID.
	_, span := sdk.Tracer("toggle-test").Start(context.Background(), "noop-span")
	span.End()
	assert.False(t, span.SpanContext().TraceID().IsValid(),
		"no-op span should have an invalid (zero) trace ID")

	testutil.MustShutdown(t.Context(), t, sdk)

	// No trace export requests should have been made.
	for _, r := range srv.Requests() {
		assert.NotEqual(t, "/v1/traces", r.Path,
			"disabled trace pillar must not export spans")
	}
}

// TestToggles_MetricsDisabled checks that a no-op MeterProvider is returned and
// no Prometheus HTTP server is started when metrics is turned off.
func TestToggles_MetricsDisabled(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	// Obtain a free ephemeral port, release it, then pass the address to
	// WithMetricsAddr. Since metrics is disabled the SDK never binds it, so
	// the subsequent probe must fail regardless of which port was chosen.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	metricsAddr := ln.Addr().String()
	require.NoError(t, ln.Close())

	opts := append(commonOpts(srv.URL),
		o11y.WithMetricsEnabled(false),
		o11y.WithMetricsAddr(metricsAddr),
	)
	sdk, initErr := o11y.Init(context.Background(), opts...)
	require.NoError(t, initErr)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Metrics)
	assert.NotNil(t, sdk.MeterProvider(), "MeterProvider must never be nil")
	assert.NotNil(t, sdk.TracerProvider(), "TracerProvider must still be available")

	// The Prometheus /metrics endpoint must not be reachable.
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, probeErr := client.Get("http://" + metricsAddr + "/metrics")
	if probeErr == nil {
		resp.Body.Close()
		t.Fatal("Prometheus server should not be listening when metrics is disabled")
	}
}

// TestToggles_LogDisabled checks that the Logger is stdout-only (no OTLP log
// requests) when log export is turned off, while the logger itself remains
// functional.
func TestToggles_LogDisabled(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)
	opts := append(commonOpts(srv.URL), o11y.WithLogEnabled(false))
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)

	assert.False(t, sdk.Toggles.Log)
	assert.NotNil(t, sdk.Logger, "Logger must never be nil even when OTLP logs are off")

	// Logger must not panic when called.
	assert.NotPanics(t, func() {
		sdk.Logger.Info("log disabled test message")
	})

	testutil.MustShutdown(t.Context(), t, sdk)

	for _, r := range srv.Requests() {
		assert.NotEqual(t, "/v1/logs", r.Path,
			"disabled log pillar must not export log records via OTLP")
	}
}

// TestToggles_AllDisabled verifies that Init succeeds and Shutdown is clean
// when all three pillars are disabled simultaneously.
func TestToggles_AllDisabled(t *testing.T) {
	// No fake OTLP server needed — nothing should connect.
	opts := []o11y.Option{
		o11y.WithServiceName("all-disabled-svc"),
		o11y.WithServiceVersion("0.0.1"),
		o11y.WithEnvironment("testing"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithTraceEnabled(false),
		o11y.WithMetricsEnabled(false),
		o11y.WithLogEnabled(false),
	}
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	require.NotNil(t, sdk)

	assert.False(t, sdk.Toggles.Trace)
	assert.False(t, sdk.Toggles.Metrics)
	assert.False(t, sdk.Toggles.Log)

	// All public accessors must return non-nil so callers never need to nil-check.
	assert.NotNil(t, sdk.TracerProvider())
	assert.NotNil(t, sdk.MeterProvider())
	assert.NotNil(t, sdk.Logger)
	assert.NotNil(t, sdk.Propagator)

	// Shutdown must be clean even when nothing was initialized.
	err = sdk.Shutdown(context.Background())
	assert.NoError(t, err, "Shutdown of all-disabled SDK should be a no-op")
}

// TestToggles_WithEnvVar_TraceDisabled verifies that the O11Y_TRACE_ENABLED
// environment variable is respected when no code-level option overrides it.
func TestToggles_WithEnvVar_TraceDisabled(t *testing.T) {
	t.Setenv("O11Y_TRACE_ENABLED", "false")

	srv := testutil.FakeOTLPServer(t)
	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Trace, "O11Y_TRACE_ENABLED=false must disable trace")
}

// TestToggles_CodeOptionOverridesEnvVar checks that an explicit
// WithTraceEnabled(true) takes precedence over O11Y_TRACE_ENABLED=false.
func TestToggles_CodeOptionOverridesEnvVar(t *testing.T) {
	t.Setenv("O11Y_TRACE_ENABLED", "false")

	srv := testutil.FakeOTLPServer(t)
	opts := append(commonOpts(srv.URL), o11y.WithTraceEnabled(true))
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.True(t, sdk.Toggles.Trace,
		"explicit WithTraceEnabled(true) must win over O11Y_TRACE_ENABLED=false")
}

// TestToggles_WithEnvVar_MetricsDisabled confirms the env-var path for metrics.
func TestToggles_WithEnvVar_MetricsDisabled(t *testing.T) {
	t.Setenv("O11Y_METRICS_ENABLED", "0")

	srv := testutil.FakeOTLPServer(t)
	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Metrics, "O11Y_METRICS_ENABLED=0 must disable metrics")
}

// TestToggles_WithEnvVar_LogDisabled confirms the env-var path for logs.
func TestToggles_WithEnvVar_LogDisabled(t *testing.T) {
	t.Setenv("O11Y_LOG_ENABLED", "false")

	srv := testutil.FakeOTLPServer(t)
	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Log, "O11Y_LOG_ENABLED=false must disable log OTLP export")
	assert.NotNil(t, sdk.Logger, "Logger must still be available (stdout-only mode)")
}

// TestToggles_InvalidEnvVar_FallsBackToDefault verifies that an unrecognised
// env var value (e.g. "yes" instead of "true") is silently ignored and the
// pillar defaults to enabled — Init must not fail.
func TestToggles_InvalidEnvVar_FallsBackToDefault(t *testing.T) {
	t.Setenv("O11Y_TRACE_ENABLED", "yes")   // not a valid bool
	t.Setenv("O11Y_METRICS_ENABLED", "on")  // not a valid bool
	t.Setenv("O11Y_LOG_ENABLED", "enabled") // not a valid bool

	srv := testutil.FakeOTLPServer(t)
	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err, "invalid env var values must not cause Init to fail")
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.True(t, sdk.Toggles.Trace, "invalid O11Y_TRACE_ENABLED should fall back to default true")
	assert.True(t, sdk.Toggles.Metrics, "invalid O11Y_METRICS_ENABLED should fall back to default true")
	assert.True(t, sdk.Toggles.Log, "invalid O11Y_LOG_ENABLED should fall back to default true")
}

// TestToggles_StartupWarning_DisabledPillar guards that Init does not panic
// or error when all three disabled-pillar warning paths are exercised together,
// and that sdk.Toggles accurately reflects the disabled state.
func TestToggles_StartupWarning_DisabledPillar(t *testing.T) {
	opts := []o11y.Option{
		o11y.WithServiceName("warn-test"),
		o11y.WithServiceVersion("0.0.1"),
		o11y.WithEnvironment("testing"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithTraceEnabled(false),
		o11y.WithMetricsEnabled(false),
		o11y.WithLogEnabled(false),
	}
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err, "Init must succeed even when all three disabled-pillar warnings fire")
	defer sdk.Shutdown(context.Background()) //nolint:errcheck

	assert.False(t, sdk.Toggles.Trace)
	assert.False(t, sdk.Toggles.Metrics)
	assert.False(t, sdk.Toggles.Log)
}

// TestToggles_Profiling_DefaultOff verifies that profiling stays inactive in
// the all-default configuration: the toggle defaults to false because
// profiling is opt-in, unlike the other three pillars.
func TestToggles_Profiling_DefaultOff(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Profiling,
		"profiling should be inactive by default (opt-in)")
}

// TestToggles_Profiling_EndpointAloneIsInsufficient verifies that setting
// WithProfilingEndpoint without also flipping the toggle does not start the
// profiler. Both conditions are required.
func TestToggles_Profiling_EndpointAloneIsInsufficient(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	opts := append(commonOpts(srv.URL),
		o11y.WithProfilingEndpoint("http://127.0.0.1:1"),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Profiling,
		"endpoint alone must not enable profiling; toggle is also required")
}

// TestToggles_ProfilingDisabled_SuppressesEndpoint verifies that
// WithProfilingEnabled(false) is honoured when an endpoint is set — the
// staged-rollback case where the deployment manifest still carries the
// endpoint but operators want to stop profiling.
func TestToggles_ProfilingDisabled_SuppressesEndpoint(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	opts := append(commonOpts(srv.URL),
		o11y.WithProfilingEndpoint("http://127.0.0.1:1"),
		o11y.WithProfilingEnabled(false),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Profiling,
		"profiling must stay inactive when explicitly disabled, even with an endpoint set")
}

// TestToggles_Profiling_ToggleAloneIsInsufficient verifies that flipping the
// toggle on (here via the env var, which also exercises that code path)
// without configuring an endpoint does not start the profiler. Both gates
// must be set.
func TestToggles_Profiling_ToggleAloneIsInsufficient(t *testing.T) {
	t.Setenv("O11Y_PROFILING_ENABLED", "true")

	srv := testutil.FakeOTLPServer(t)
	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.False(t, sdk.Toggles.Profiling,
		"toggle without endpoint must not start the profiler")
}

// TestToggles_Profiling_BothGatesEnable_StartsProfiler verifies the positive
// path: when both the toggle is on AND a Pyroscope endpoint is set, the SDK
// starts the profiler and reports Toggles.Profiling = true.
func TestToggles_Profiling_BothGatesEnable_StartsProfiler(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)
	opts := append(commonOpts(srv.URL),
		o11y.WithProfilingEndpoint("http://127.0.0.1:1"), // background upload failures are tolerated
		o11y.WithProfilingEnabled(true),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.True(t, sdk.Toggles.Profiling,
		"both gates on must start the profiler and set Toggles.Profiling")
}

// TestToggles_Profiling_CodeOptionOverridesEnvVar verifies that an explicit
// WithProfilingEnabled(true) takes precedence over O11Y_PROFILING_ENABLED=false.
// An endpoint is provided so that the assertion observes the actual on-state
// rather than always-false from the double gate.
func TestToggles_Profiling_CodeOptionOverridesEnvVar(t *testing.T) {
	t.Setenv("O11Y_PROFILING_ENABLED", "false")

	srv := testutil.FakeOTLPServer(t)
	opts := append(commonOpts(srv.URL),
		o11y.WithProfilingEndpoint("http://127.0.0.1:1"),
		o11y.WithProfilingEnabled(true),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t.Context(), t, sdk)

	assert.True(t, sdk.Toggles.Profiling,
		"explicit WithProfilingEnabled(true) must win over O11Y_PROFILING_ENABLED=false")
}
