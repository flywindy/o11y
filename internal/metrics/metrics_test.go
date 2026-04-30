package metrics_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y/internal/metrics"
	"github.com/flywindy/o11y/internal/testutil"
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

// TestInitMeter_HappyPath verifies that InitMeter stands up a working
// /metrics endpoint whose scrape output includes runtime metrics and the
// team resource attribute as a constant label.
func TestInitMeter_HappyPath(t *testing.T) {
	addr := testutil.FreeAddr(t)

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

	// runtime.Start registers async instruments lazily; the Prometheus
	// exporter pulls them on each scrape. Poll instead of fixed-sleep so
	// the test isn't fragile under load. TryScrapeMetrics returns an error
	// instead of calling t.FailNow so a transient scrape failure inside
	// the polling window drives a retry rather than aborting the test.
	assert.Eventually(t, func() bool {
		body, err := testutil.TryScrapeMetrics(t.Context(), addr)
		if err != nil {
			return false
		}
		return strings.Contains(body, "go_goroutine") ||
			strings.Contains(body, "process_runtime_go_goroutines")
	}, 2*time.Second, 50*time.Millisecond,
		"runtime metrics should appear within timeout")

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Contains(t, body, `service_namespace="platform"`, "service.namespace resource attribute must appear as a constant label")
	assert.Contains(t, body, `service_name="test-svc"`, "service_name must appear as a constant label")
	assert.Contains(t, body, `service_version="0.0.1"`, "service_version must appear as a constant label")
	assert.Contains(t, body, `deployment_environment_name="test"`, "deployment_environment_name must appear as a constant label")
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
	addr := testutil.FreeAddr(t)

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

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.NotContains(t, body, "process_runtime_go_goroutines")
}

// TestInitMeter_OTLPHeadersAttached confirms that the OTLP push path forwards
// configured headers on every outbound request.
func TestInitMeter_OTLPHeadersAttached(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)

	mp, closer, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:         "test-svc",
		Namespace:           "platform",
		MetricsOTLPEndpoint: srv.URL,
		OTLPHeaders:         map[string]string{"Authorization": "Bearer xyz"},
		RuntimeMetrics:      false,
		HistogramBuckets:    []float64{1},
	})
	require.NoError(t, err)

	// Force a metrics export via Shutdown's drain so the capturing server
	// observes at least one request.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mp.ForceFlush(ctx))
	_ = closer(ctx)
	_ = mp.Shutdown(ctx)

	requests := srv.Requests()
	require.NotEmpty(t, requests, "OTLP metrics exporter should produce at least one request")
	saw := false
	for _, r := range requests {
		if r.Header.Get("Authorization") == "Bearer xyz" {
			saw = true
			break
		}
	}
	assert.True(t, saw, "Authorization header must propagate to OTLP metrics requests")
}
