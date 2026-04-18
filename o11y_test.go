package o11y_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y"
)

// fakeOTLPServer returns an httptest.Server that accepts any OTLP/HTTP request.
func fakeOTLPServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func doShutdown(t *testing.T, sdk *o11y.SDK) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, sdk.Shutdown(ctx))
}

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
	srv := fakeOTLPServer(t)
	defer srv.Close()

	aliases := []string{"prod", "stg", "stage", "dev", "test"}
	for _, alias := range aliases {
		t.Run(alias, func(t *testing.T) {
			opts := append(commonOpts(srv.URL), o11y.WithEnvironment(alias))
			sdk, err := o11y.Init(context.Background(), opts...)
			require.NoError(t, err, "alias %q should be accepted", alias)
			doShutdown(t, sdk)
		})
	}
}

func TestInit_Success(t *testing.T) {
	srv := fakeOTLPServer(t)
	defer srv.Close()

	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	require.NotNil(t, sdk)
	assert.NotNil(t, sdk.Logger, "Logger must be set")
	assert.NotNil(t, sdk.Propagator, "Propagator must be set")
	assert.NotNil(t, sdk.Tracer("test"), "Tracer must be obtainable")
	assert.NotNil(t, sdk.TracerProvider(), "TracerProvider must be obtainable")
	assert.NotNil(t, sdk.Meter("test"), "Meter must be obtainable")
	assert.NotNil(t, sdk.MeterProvider(), "MeterProvider must be obtainable")

	doShutdown(t, sdk)
}

func TestInit_HandlesNilOption(t *testing.T) {
	srv := fakeOTLPServer(t)
	defer srv.Close()

	opts := append([]o11y.Option{nil}, commonOpts(srv.URL)...)
	sdk, err := o11y.Init(context.Background(), opts...)
	require.NoError(t, err)
	require.NotNil(t, sdk)
	doShutdown(t, sdk)
}

func TestSDK_TracerIsNamed(t *testing.T) {
	srv := fakeOTLPServer(t)
	defer srv.Close()

	sdk, err := o11y.Init(context.Background(), commonOpts(srv.URL)...)
	require.NoError(t, err)
	defer doShutdown(t, sdk)

	assert.NotNil(t, sdk.Tracer("a"))
	assert.NotNil(t, sdk.Tracer("b"))
}
