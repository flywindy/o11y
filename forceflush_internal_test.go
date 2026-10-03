package o11y

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestForceFlush_SignalsTogetherThenMetrics pins the two stages: the trace
// and log flushers run concurrently, the metric flusher only after both
// returned, and a signal flusher that hangs until its share runs out leaves
// the metric flusher a live context.
func TestForceFlush_SignalsTogetherThenMetrics(t *testing.T) {
	var running, maxRunning atomic.Int32
	enter := func() {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				return
			}
		}
	}
	var metricsSawLive, metricsAfterSignals bool
	sdk := &SDK{
		flushSignals: []func(context.Context) error{
			func(ctx context.Context) error { enter(); defer running.Add(-1); <-ctx.Done(); return ctx.Err() },
			func(context.Context) error {
				enter()
				defer running.Add(-1)
				time.Sleep(50 * time.Millisecond)
				return nil
			},
		},
		flushMetrics: func(ctx context.Context) error {
			metricsAfterSignals = running.Load() == 0
			metricsSawLive = ctx.Err() == nil
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := sdk.ForceFlush(ctx)

	assert.ErrorIs(t, err, context.DeadlineExceeded, "the slow flusher's own timeout is reported")
	assert.Equal(t, int32(2), maxRunning.Load(), "traces and logs flush concurrently")
	assert.True(t, metricsAfterSignals, "metrics flush after both signal flushers returned")
	assert.True(t, metricsSawLive, "metrics still get a live context after a slow signal flush")
	assert.NoError(t, ctx.Err(), "the caller's deadline itself was not exhausted")
}

// TestForceFlush_SignalsGetTheWholeBudgetWithoutMetrics pins that, with no
// metric flusher (the Prometheus pull path), the signal stage is not given
// half the deadline for a stage that does not exist.
func TestForceFlush_SignalsGetTheWholeBudgetWithoutMetrics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	want, _ := ctx.Deadline()
	var got time.Time
	sdk := &SDK{flushSignals: []func(context.Context) error{
		func(ctx context.Context) error { got, _ = ctx.Deadline(); return nil },
	}}

	require.NoError(t, sdk.ForceFlush(ctx))
	assert.Equal(t, want, got)
}

// TestForceFlush_RedactsAndJoinsErrors pins that every flusher runs even
// after one fails, and that the joined error carries no endpoint credential
// or header value while still matching the exporter's own error.
func TestForceFlush_RedactsAndJoinsErrors(t *testing.T) {
	exportErr := fmt.Errorf(`Post "http://alice:***@collector:4318/v1/traces": dial tcp: i/o timeout`) //nolint:err113
	metricErr := errors.New("metric exporter: header glc_token rejected")
	var ran atomic.Int32
	sdk := &SDK{
		diagnosticEndpoints: []string{"http://alice:s3cretpw@collector:4318"},
		diagnosticSecrets:   []string{"glc_token"},
		flushSignals: []func(context.Context) error{
			func(context.Context) error { ran.Add(1); return exportErr },
			func(context.Context) error { ran.Add(1); return nil },
		},
		flushMetrics: func(context.Context) error { ran.Add(1); return metricErr },
	}

	err := sdk.ForceFlush(context.Background())

	require.Error(t, err)
	assert.Equal(t, int32(3), ran.Load(), "a failing flusher does not stop the rest")
	assert.NotContains(t, err.Error(), "alice")
	assert.NotContains(t, err.Error(), "glc_token")
	assert.Contains(t, err.Error(), "collector:4318")
	assert.ErrorIs(t, err, exportErr)
	assert.ErrorIs(t, err, metricErr)
}

// TestForceFlush_NoOpAfterShutdown pins that ForceFlush does nothing once
// Shutdown has started.
func TestForceFlush_NoOpAfterShutdown(t *testing.T) {
	flushed := false
	sdk := &SDK{flushSignals: []func(context.Context) error{
		func(context.Context) error { flushed = true; return errors.New("flushed after shutdown") },
	}}
	require.NoError(t, sdk.Shutdown(context.Background()))

	assert.NoError(t, sdk.ForceFlush(context.Background()))
	assert.False(t, flushed)
}

// TestShutdown_WaitsForARunningForceFlush pins that Shutdown does not close
// components under a ForceFlush that is already running: its closers start
// only after the flush returned.
func TestShutdown_WaitsForARunningForceFlush(t *testing.T) {
	inFlush := make(chan struct{})
	release := make(chan struct{})
	var flushDone atomic.Bool
	var closedDuringFlush bool
	sdk := &SDK{
		flushSignals: []func(context.Context) error{
			func(context.Context) error {
				close(inFlush)
				<-release
				flushDone.Store(true)
				return nil
			},
		},
		shutdowns: []func(context.Context) error{
			func(context.Context) error { closedDuringFlush = !flushDone.Load(); return nil },
		},
	}

	var wg sync.WaitGroup
	wg.Go(func() { assert.NoError(t, sdk.ForceFlush(context.Background())) })
	<-inFlush
	shutdownDone := make(chan error, 1)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	go func() { shutdownDone <- sdk.Shutdown(shutdownCtx) }()

	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while a ForceFlush was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-shutdownDone)
	wg.Wait()
	assert.False(t, closedDuringFlush, "closers run only after the flush finished")
}

// TestShutdown_BoundsTheWaitForAForceFlush pins that a hung ForceFlush holds
// Shutdown for at most one share of its deadline: the closers after the wait
// still run with a live context.
func TestShutdown_BoundsTheWaitForAForceFlush(t *testing.T) {
	inFlush := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var closerSawLive bool
	sdk := &SDK{
		flushSignals: []func(context.Context) error{
			func(context.Context) error { close(inFlush); <-release; return nil },
		},
		shutdowns: []func(context.Context) error{
			func(ctx context.Context) error { closerSawLive = ctx.Err() == nil; return nil },
		},
	}
	go func() { _ = sdk.ForceFlush(context.Background()) }()
	<-inFlush

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	require.NoError(t, sdk.Shutdown(ctx))
	assert.True(t, closerSawLive, "the wait took one share, not the whole deadline")
}

// TestShutdown_DoesNotWaitWithoutADeadline pins that Shutdown with no
// deadline closes the components at once instead of waiting on a flush that
// may itself be waiting for them to close.
func TestShutdown_DoesNotWaitWithoutADeadline(t *testing.T) {
	inFlush := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan struct{})
	sdk := &SDK{
		flushSignals: []func(context.Context) error{
			func(context.Context) error { close(inFlush); <-release; return nil },
		},
		shutdowns: []func(context.Context) error{
			func(context.Context) error { close(closed); close(release); return nil },
		},
	}
	go func() { _ = sdk.ForceFlush(context.Background()) }()
	<-inFlush

	done := make(chan error, 1)
	go func() { done <- sdk.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown without a deadline waited on the running flush")
	}
	<-closed
}

// TestInit_WiresFlushers pins the flush list Init builds: traces and logs
// whenever their pillar is on, metrics only on the OTLP push path, nothing for
// a pillar that is off.
func TestInit_WiresFlushers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	base := []Option{
		WithServiceName("svc"), WithServiceVersion("1.0.0"),
		WithServiceNamespace("platform"), WithEnvironment("testing"),
		WithOTLPEndpoint(srv.URL), WithMetricsAddr("127.0.0.1:0"),
	}
	for _, tc := range []struct {
		name        string
		opts        []Option
		signals     int
		wantMetrics bool
	}{
		{name: "pull path", signals: 2},
		{name: "push path", opts: []Option{WithMetricsOTLPEndpoint(srv.URL)}, signals: 2, wantMetrics: true},
		{name: "traces off", opts: []Option{WithTraceEnabled(false)}, signals: 1},
		{
			name:    "everything off",
			opts:    []Option{WithTraceEnabled(false), WithLogEnabled(false), WithMetricsEnabled(false), WithMetricsOTLPEndpoint(srv.URL)},
			signals: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sdk, err := Init(context.Background(), append(append([]Option{}, base...), tc.opts...)...)
			require.NoError(t, err)
			t.Cleanup(func() { _ = sdk.Shutdown(context.Background()) })
			assert.Len(t, sdk.flushSignals, tc.signals)
			assert.Equal(t, tc.wantMetrics, sdk.flushMetrics != nil)
			assert.NoError(t, sdk.ForceFlush(context.Background()))
		})
	}
}
