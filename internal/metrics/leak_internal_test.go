package metrics

import (
	"context"
	"errors"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/sdk/resource"
)

// withFailingRuntimeStart makes the runtime-metrics registration fail, which is
// the one branch that returns from initOTLP / initPrometheus after the
// MeterProvider has already been constructed.
func withFailingRuntimeStart(t *testing.T, err error) {
	t.Helper()
	old := runtimeStart
	runtimeStart = func(...runtime.Option) error { return err }
	t.Cleanup(func() { runtimeStart = old })
}

// settledGoroutines returns the goroutine count once it stops moving, so a
// baseline is not skewed by goroutines an earlier test is still winding down.
func settledGoroutines() int {
	prev := -1
	for i := 0; i < 50; i++ {
		goruntime.GC()
		got := goruntime.NumGoroutine()
		if got == prev {
			return got
		}
		prev = got
		time.Sleep(10 * time.Millisecond)
	}
	return prev
}

// waitForGoroutines polls until the count drops back to at most want. A leaked
// PeriodicReader goroutine never exits, so polling is what distinguishes "not
// cleaned up yet" from "not cleaned up at all" without a fixed sleep.
func waitForGoroutines(want int) int {
	deadline := time.Now().Add(2 * time.Second)
	got := goruntime.NumGoroutine()
	for time.Now().Before(deadline) {
		goruntime.GC()
		got = goruntime.NumGoroutine()
		if got <= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got
}

// TestInitOTLP_FailedRuntimeStartLeavesNoReaderGoroutine covers the asymmetry
// between the two init paths: the OTLP one released only the exporter, but
// sdkmetric.NewPeriodicReader starts its export goroutine at construction and
// only the provider's Shutdown drains it. The goroutine outlived the failed
// Init and exported against a closed exporter once per interval for the life of
// the process.
func TestInitOTLP_FailedRuntimeStartLeavesNoReaderGoroutine(t *testing.T) {
	startErr := errors.New("runtime metrics unavailable")
	withFailingRuntimeStart(t, startErr)

	res, err := resource.New(context.Background())
	require.NoError(t, err)

	// Settle first so the baseline excludes goroutines from earlier tests.
	baseline := settledGoroutines()

	provider, closer, err := initOTLP(context.Background(), Config{
		MetricsOTLPEndpoint: "http://127.0.0.1:19999", // nothing listening; init does not dial
		RuntimeMetrics:      true,
	}, res, nil)
	require.ErrorIs(t, err, startErr)
	require.Nil(t, provider)
	require.Nil(t, closer)

	got := waitForGoroutines(baseline)
	require.LessOrEqual(t, got, baseline,
		"failed OTLP init leaked goroutines (baseline %d, now %d): the MeterProvider was not shut down", baseline, got)
}

// TestInitPrometheus_FailedRuntimeStartLeavesNoGoroutine pins the path that was
// already correct, so the symmetry cannot regress.
func TestInitPrometheus_FailedRuntimeStartLeavesNoGoroutine(t *testing.T) {
	startErr := errors.New("runtime metrics unavailable")
	withFailingRuntimeStart(t, startErr)

	res, err := resource.New(context.Background())
	require.NoError(t, err)

	baseline := settledGoroutines()

	provider, closer, err := initPrometheus(context.Background(), Config{
		MetricsAddr:    "127.0.0.1:0",
		RuntimeMetrics: true,
	}, res, nil)
	require.ErrorIs(t, err, startErr)
	require.Nil(t, provider)
	require.Nil(t, closer)

	got := waitForGoroutines(baseline)
	require.LessOrEqual(t, got, baseline,
		"failed Prometheus init leaked goroutines (baseline %d, now %d)", baseline, got)
}
