package o11y_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y"
	"github.com/flywindy/o11y/internal/testutil"
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
			testutil.MustShutdown(t, sdk)
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

	testutil.MustShutdown(t, sdk)
}

func TestInit_HandlesNilOption(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	opts := append([]o11y.Option{nil}, commonOpts(srv.URL)...)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	require.NotNil(t, sdk)
	testutil.MustShutdown(t, sdk)
}

func TestSDK_TracerIsNamed(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	defer testutil.MustShutdown(t, sdk)

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

// TestInit_AcceptsExtremeValidBuckets ensures very small and very large
// finite values pass validation; only NaN/Inf/non-positive should be rejected.
func TestInit_AcceptsExtremeValidBuckets(t *testing.T) {
	srv := testutil.FakeOTLPServer(t)

	opts := append(commonOpts(srv.URL),
		o11y.WithHistogramBuckets([]float64{1e-9, 1, 1e9}),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	testutil.MustShutdown(t, sdk)
}

// TestInit_OTLPHeadersForwarded confirms WithOTLPHeaders reaches the
// outbound HTTP request. We use a capturing fake server in place of a real
// OTel Collector and trigger a forced flush so the trace exporter sends at
// least one request before shutdown.
func TestInit_OTLPHeadersForwarded(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)

	opts := append(commonOpts(srv.URL),
		o11y.WithOTLPHeaders(map[string]string{
			"Authorization":   "Bearer secret-token",
			"X-Scope-OrgID":   "tenant-42",
			"X-Honeycomb-Team": "abc123",
		}),
	)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)

	// Emit at least one span so the exporter has something to send, then
	// force a flush before shutdown to make the assertion deterministic.
	_, span := sdk.Tracer("hdr-test").Start(context.Background(), "probe")
	span.End()
	require.NoError(t, sdk.TracerProvider().ForceFlush(context.Background()))
	testutil.MustShutdown(t, sdk)

	requests := srv.Requests()
	require.NotEmpty(t, requests, "exporter should have made at least one OTLP request")

	// Find a trace request — log requests may also flow through the same
	// server but we only need a single hit to confirm header forwarding.
	var sawAuth, sawScope, sawTeam bool
	for _, r := range requests {
		if r.Header.Get("Authorization") == "Bearer secret-token" {
			sawAuth = true
		}
		if r.Header.Get("X-Scope-Orgid") == "tenant-42" || r.Header.Get("X-Scope-OrgID") == "tenant-42" {
			sawScope = true
		}
		if r.Header.Get("X-Honeycomb-Team") == "abc123" {
			sawTeam = true
		}
	}
	assert.True(t, sawAuth, "Authorization header must be forwarded on OTLP requests")
	assert.True(t, sawScope, "X-Scope-OrgID header must be forwarded on OTLP requests")
	assert.True(t, sawTeam, "custom auth header must be forwarded on OTLP requests")
}
