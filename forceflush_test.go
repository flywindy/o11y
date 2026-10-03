package o11y_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y"
	"github.com/flywindy/o11y/internal/testutil"
)

// TestForceFlush_ExportsWithoutShutdown checks ForceFlush pushes the span, the
// log record and, on the OTLP metrics path, the metric that are buffered,
// before any Shutdown, and that the SDK keeps working afterwards.
func TestForceFlush_ExportsWithoutShutdown(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)
	opts := append(commonOpts(srv.URL), o11y.WithMetricsOTLPEndpoint(srv.URL))
	sdk, err := o11y.Init(t.Context(), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sdk.Shutdown(context.Background()) })

	_, span := sdk.Tracer("flush-test").Start(t.Context(), "op")
	span.End()
	sdk.Logger.InfoContext(t.Context(), "flush me")
	counter, err := sdk.Meter("flush-test").Int64Counter("flush.test.count")
	require.NoError(t, err)
	counter.Add(t.Context(), 1)

	require.NoError(t, sdk.ForceFlush(t.Context()))

	paths := map[string]bool{}
	for _, r := range srv.Requests() {
		paths[r.Path] = true
	}
	for _, want := range []string{"/v1/traces", "/v1/logs", "/v1/metrics"} {
		assert.True(t, paths[want], "ForceFlush must export %s before Shutdown", want)
	}

	// The SDK is still live: a second unit of work flushes too.
	before := len(srv.Requests())
	_, span = sdk.Tracer("flush-test").Start(t.Context(), "op2")
	span.End()
	require.NoError(t, sdk.ForceFlush(t.Context()))
	assert.Greater(t, len(srv.Requests()), before, "a later ForceFlush exports the next batch")
}
