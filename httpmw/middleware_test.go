package httpmw_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y/httpmw"
	"github.com/flywindy/o11y/internal/metrics"
)

func scrapeBody(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

// TestMiddleware_Records verifies the histogram is emitted with expected labels.
func TestMiddleware_Records(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	provider, srv, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:      "test-svc",
		Team:             "test-team",
		MetricsAddr:      addr,
		RuntimeMetrics:   false,
		HistogramBuckets: []float64{0.001, 0.01, 0.1, 1},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = provider.Shutdown(ctx)
	})

	mw := httpmw.New(provider.Meter("httpmw_test"))

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/notfound", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/implicit", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hi"))
	})

	ts := httptest.NewServer(mw(mux))
	defer ts.Close()

	for _, path := range []string{"/ok", "/notfound", "/boom", "/implicit"} {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	body := scrapeBody(t, addr)
	// One histogram line per status code we exercised.
	assert.Contains(t, body, `http_response_status_code="200"`)
	assert.Contains(t, body, `http_response_status_code="404"`)
	assert.Contains(t, body, `http_response_status_code="500"`)
	assert.Contains(t, body, `http_request_method="GET"`)
	assert.Contains(t, body, `http_route="/ok"`)
	assert.Contains(t, body, `http_route="/implicit"`)
	assert.Contains(t, body, `team="test-team"`, "team constant label must propagate to every series")
	// _count is our traffic counter surrogate.
	assert.Contains(t, body, "http_server_request_duration_seconds_count")
}

// TestMiddleware_CardinalityCap verifies that paths beyond maxUniquePaths
// collapse to the literal "other" label.
func TestMiddleware_CardinalityCap(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	provider, srv, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:      "test-svc",
		Team:             "test-team",
		MetricsAddr:      addr,
		RuntimeMetrics:   false,
		HistogramBuckets: []float64{1},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = provider.Shutdown(ctx)
	})

	mw := httpmw.New(
		provider.Meter("cardinality_test"),
		httpmw.WithMaxUniquePaths(3),
	)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// 5 distinct paths with a cap of 3 → 2 should collapse to "other".
	for i := 0; i < 5; i++ {
		resp, err := http.Get(fmt.Sprintf("%s/path-%d", ts.URL, i))
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	body := scrapeBody(t, addr)
	assert.Contains(t, body, `http_route="other"`, "cardinality overflow must collapse to 'other'")

	// Count distinct http_route labels in the histogram _count series. With
	// cap=3 the cap-era paths get one series each, then the rest merge into
	// 'other'. So we expect at most 4 distinct routes.
	routes := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "http_server_request_duration_seconds_count{") {
			continue
		}
		start := strings.Index(line, `http_route="`)
		if start < 0 {
			continue
		}
		start += len(`http_route="`)
		end := strings.Index(line[start:], `"`)
		if end < 0 {
			continue
		}
		routes[line[start:start+end]] = true
	}
	assert.LessOrEqual(t, len(routes), 4, "route cardinality must not exceed cap+1 (for 'other')")
}

// TestMiddleware_CustomNormalizer verifies that a caller-supplied path
// normalizer wins over the raw r.URL.Path.
func TestMiddleware_CustomNormalizer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	provider, srv, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:      "test-svc",
		Team:             "test-team",
		MetricsAddr:      addr,
		RuntimeMetrics:   false,
		HistogramBuckets: []float64{1},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = provider.Shutdown(ctx)
	})

	mw := httpmw.New(
		provider.Meter("normalizer_test"),
		httpmw.WithPathNormalizer(func(r *http.Request) string {
			if strings.HasPrefix(r.URL.Path, "/users/") {
				return "/users/:id"
			}
			return r.URL.Path
		}),
	)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ts := httptest.NewServer(handler)
	defer ts.Close()

	for _, id := range []string{"1", "2", "3"} {
		resp, err := http.Get(ts.URL + "/users/" + id)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	body := scrapeBody(t, addr)
	assert.Contains(t, body, `http_route="/users/:id"`)
	assert.NotContains(t, body, `http_route="/users/1"`)
}
