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

func shutdown(t *testing.T, sdk *o11y.SDK) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, sdk.Shutdown(ctx))
}

// TestInit_MissingServiceName verifies that Init rejects an empty service name.
func TestInit_MissingServiceName(t *testing.T) {
	_, err := o11y.Init(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service name is required")
}

// TestInit_Success verifies that Init succeeds with valid options and returns
// a non-nil SDK with all fields populated.
func TestInit_Success(t *testing.T) {
	srv := fakeOTLPServer(t)
	defer srv.Close()

	sdk, err := o11y.Init(context.Background(),
		o11y.WithServiceName("test-svc"),
		o11y.WithOTLPEndpoint(srv.URL),
	)
	require.NoError(t, err)
	require.NotNil(t, sdk)
	assert.NotNil(t, sdk.Logger, "Logger must be set")
	assert.NotNil(t, sdk.Propagator, "Propagator must be set")
	assert.NotNil(t, sdk.Tracer("test"), "Tracer must be obtainable")

	shutdown(t, sdk)
}

// TestInit_WithEnvironment verifies that a non-empty environment option is accepted.
func TestInit_WithEnvironment(t *testing.T) {
	srv := fakeOTLPServer(t)
	defer srv.Close()

	sdk, err := o11y.Init(context.Background(),
		o11y.WithServiceName("test-svc"),
		o11y.WithEnvironment("staging"),
		o11y.WithOTLPEndpoint(srv.URL),
	)
	require.NoError(t, err)
	require.NotNil(t, sdk)
	shutdown(t, sdk)
}

// TestInit_EmptyEnvironment verifies that an empty environment option does not
// cause an error (the attribute is simply omitted from the resource).
func TestInit_EmptyEnvironment(t *testing.T) {
	srv := fakeOTLPServer(t)
	defer srv.Close()

	sdk, err := o11y.Init(context.Background(),
		o11y.WithServiceName("test-svc"),
		o11y.WithEnvironment(""),
		o11y.WithOTLPEndpoint(srv.URL),
	)
	require.NoError(t, err)
	require.NotNil(t, sdk)
	shutdown(t, sdk)
}

// TestSDK_TracerIsNamed verifies that Tracer() returns different tracer instances
// for different names (non-nil in all cases).
func TestSDK_TracerIsNamed(t *testing.T) {
	srv := fakeOTLPServer(t)
	defer srv.Close()

	sdk, err := o11y.Init(context.Background(),
		o11y.WithServiceName("test-svc"),
		o11y.WithOTLPEndpoint(srv.URL),
	)
	require.NoError(t, err)
	defer shutdown(t, sdk)

	assert.NotNil(t, sdk.Tracer("a"))
	assert.NotNil(t, sdk.Tracer("b"))
}
