package metrics_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y/internal/metrics"
)

func baseConfig(addr string) metrics.Config {
	return metrics.Config{
		ServiceName:      "test-svc",
		ServiceVersion:   "0.0.1",
		Environment:      "test",
		Namespace:        "platform",
		MetricsAddr:      addr,
		RuntimeMetrics:   true,
		HistogramBuckets: []float64{0.1, 1, 10},
	}
}

func scrape(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

// TestInitMeter_HappyPath verifies that InitMeter stands up a working
// /metrics endpoint whose scrape output includes runtime metrics and the
// team resource attribute as a constant label.
func TestInitMeter_HappyPath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	mp, closer, err := metrics.InitMeter(context.Background(), baseConfig(addr))
	require.NoError(t, err)
	require.NotNil(t, mp)
	require.NotNil(t, closer)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	// Give runtime.Start a tick to register its instruments.
	time.Sleep(100 * time.Millisecond)

	body := scrape(t, addr)
	assert.Contains(t, body, `service_namespace="platform"`, "service.namespace resource attribute must appear as a constant label")
	assert.Contains(t, body, `service_name="test-svc"`, "service_name must appear as a constant label")
	assert.Contains(t, body, `service_version="0.0.1"`, "service_version must appear as a constant label")
	assert.Contains(t, body, `deployment_environment_name="test"`, "deployment_environment_name must appear as a constant label")
	assert.True(t,
		strings.Contains(body, "go_goroutine") ||
			strings.Contains(body, "process_runtime_go_goroutines"),
		"runtime metrics should be present when RuntimeMetrics=true",
	)
}

// TestInitMeter_RequiresNamespace verifies the fail-fast guard on an empty namespace.
func TestInitMeter_RequiresNamespace(t *testing.T) {
	cfg := baseConfig("127.0.0.1:0")
	cfg.Namespace = ""
	_, _, err := metrics.InitMeter(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Namespace is required")
}

// TestInitMeter_BindFailure verifies that a port already in use surfaces
// synchronously from InitMeter instead of being swallowed by a background
// goroutine.
func TestInitMeter_BindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	cfg := baseConfig(ln.Addr().String())
	_, _, err = metrics.InitMeter(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen")
}

// TestInitMeter_OTLPPath verifies that when MetricsOTLPEndpoint is set, no
// HTTP scrape server is started and the OTLP exporter is initialized.
// We use a non-existent endpoint — the test only checks that Init succeeds
// and the returned Closer does not panic.
func TestInitMeter_OTLPPath(t *testing.T) {
	mp, closer, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:         "test-svc",
		Namespace:           "platform",
		MetricsOTLPEndpoint: "http://127.0.0.1:19999", // nothing listening — that's OK for init
		RuntimeMetrics:      false,
		HistogramBuckets:    []float64{1},
	})
	require.NoError(t, err)
	require.NotNil(t, mp)
	require.NotNil(t, closer)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = closer(ctx)
	_ = mp.Shutdown(ctx)
}

// TestInitMeter_RuntimeMetricsOff verifies that runtime metrics can be
// disabled via configuration.
func TestInitMeter_RuntimeMetricsOff(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	cfg := baseConfig(addr)
	cfg.RuntimeMetrics = false

	mp, closer, err := metrics.InitMeter(context.Background(), cfg)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	body := scrape(t, addr)
	assert.NotContains(t, body, "process_runtime_go_goroutines")
}
