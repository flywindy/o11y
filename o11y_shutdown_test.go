package o11y

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSDK(t *testing.T) *SDK {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sdk, err := Init(context.Background(),
		WithServiceName("test-svc"),
		WithServiceVersion("0.1.0"),
		WithEnvironment("development"),
		WithServiceNamespace("platform"),
		WithMetricsAddr("127.0.0.1:0"),
		WithOTLPEndpoint(srv.URL),
	)
	require.NoError(t, err)
	return sdk
}

// TestSDK_ShutdownCombinesErrors verifies that Shutdown iterates all registered
// component shutdown functions and returns a joined error when one or more fail.
func TestSDK_ShutdownCombinesErrors(t *testing.T) {
	sdk := newTestSDK(t)

	// Inject a second failing shutdown to simulate a multi-component SDK.
	injectedErr := errors.New("injected shutdown failure")
	sdk.shutdowns = append(sdk.shutdowns, func(context.Context) error {
		return injectedErr
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := sdk.Shutdown(ctx)
	require.Error(t, err, "Shutdown must return a non-nil error when a component fails")
	assert.ErrorIs(t, err, injectedErr, "joined error must contain the injected shutdown error")
}

// TestSDK_ShutdownIsIdempotent verifies that calling Shutdown multiple times
// runs the underlying closers exactly once and that every call returns the
// same error value. This protects callers that register Shutdown both in
// main and inside a signal handler from accidentally double-flushing the
// underlying OTel exporters.
func TestSDK_ShutdownIsIdempotent(t *testing.T) {
	sdk := newTestSDK(t)

	var calls atomic.Int32
	sdk.shutdowns = append(sdk.shutdowns, func(context.Context) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first := sdk.Shutdown(ctx)
	second := sdk.Shutdown(ctx)
	third := sdk.Shutdown(ctx)

	require.NoError(t, first, "first Shutdown should succeed")
	assert.Equal(t, first, second, "subsequent Shutdown must return the same value")
	assert.Equal(t, first, third, "subsequent Shutdown must return the same value")
	assert.Equal(t, int32(1), calls.Load(),
		"underlying closers must run exactly once across repeated Shutdown calls")
}

// TestSDK_ShutdownIdempotentPreservesError ensures the cached error from the
// first call is returned by subsequent calls, even when the first call
// failed.
func TestSDK_ShutdownIdempotentPreservesError(t *testing.T) {
	sdk := newTestSDK(t)

	injected := errors.New("flush failed")
	sdk.shutdowns = append(sdk.shutdowns, func(context.Context) error { return injected })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first := sdk.Shutdown(ctx)
	second := sdk.Shutdown(ctx)
	require.Error(t, first)
	assert.ErrorIs(t, first, injected)
	assert.Equal(t, first, second, "cached error must be returned on every subsequent call")
}
