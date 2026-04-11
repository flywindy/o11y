package o11y

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSDK_ShutdownCombinesErrors verifies that Shutdown iterates all registered
// component shutdown functions and returns a joined error when one or more fail.
func TestSDK_ShutdownCombinesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sdk, err := Init(context.Background(),
		WithServiceName("test-svc"),
		WithOTLPEndpoint(srv.URL),
	)
	require.NoError(t, err)

	// Inject a second failing shutdown to simulate a multi-component SDK.
	injectedErr := errors.New("injected shutdown failure")
	sdk.shutdowns = append(sdk.shutdowns, func(context.Context) error {
		return injectedErr
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = sdk.Shutdown(ctx)
	require.Error(t, err, "Shutdown must return a non-nil error when a component fails")
	assert.ErrorIs(t, err, injectedErr, "joined error must contain the injected shutdown error")
}
