package http_test

import (
	"bufio"
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

	o11yhttp "github.com/flywindy/o11y/http"
	"github.com/flywindy/o11y/internal/metrics"
	"github.com/flywindy/o11y/internal/testutil"
)

// TestMiddleware_Records verifies the histogram is emitted with expected labels.
func TestMiddleware_Records(t *testing.T) {
	addr := testutil.FreeAddr(t)

	provider, closer, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:      "test-svc",
		Namespace:        "platform",
		MetricsAddr:      addr,
		RuntimeMetrics:   false,
		HistogramBuckets: []float64{0.001, 0.01, 0.1, 1},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = provider.Shutdown(ctx)
	})

	mw := o11yhttp.New(context.Background(), provider.Meter("httpmw_test"))

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

	body := testutil.ScrapeMetrics(t, addr)
	// One histogram line per status code we exercised.
	assert.Contains(t, body, `http_response_status_code="200"`)
	assert.Contains(t, body, `http_response_status_code="404"`)
	assert.Contains(t, body, `http_response_status_code="500"`)
	assert.Contains(t, body, `http_request_method="GET"`)
	assert.Contains(t, body, `http_route="/ok"`)
	assert.Contains(t, body, `http_route="/implicit"`)
	assert.Contains(t, body, `service_namespace="platform"`, "service.namespace constant label must propagate to every series")
	// _count is our traffic counter surrogate.
	assert.Contains(t, body, "http_server_request_duration_seconds_count")
}

// TestMiddleware_CardinalityCap verifies that paths beyond maxUniquePaths
// collapse to the literal "other" label.
func TestMiddleware_CardinalityCap(t *testing.T) {
	addr := testutil.FreeAddr(t)

	provider, closer, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:      "test-svc",
		Namespace:        "platform",
		MetricsAddr:      addr,
		RuntimeMetrics:   false,
		HistogramBuckets: []float64{1},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = provider.Shutdown(ctx)
	})

	mw := o11yhttp.New(
		context.Background(),
		provider.Meter("cardinality_test"),
		o11yhttp.WithMaxUniquePaths(3),
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

	body := testutil.ScrapeMetrics(t, addr)
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
	addr := testutil.FreeAddr(t)

	provider, closer, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:      "test-svc",
		Namespace:        "platform",
		MetricsAddr:      addr,
		RuntimeMetrics:   false,
		HistogramBuckets: []float64{1},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = provider.Shutdown(ctx)
	})

	mw := o11yhttp.New(
		context.Background(),
		provider.Meter("normalizer_test"),
		o11yhttp.WithPathNormalizer(func(r *http.Request) string {
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

	body := testutil.ScrapeMetrics(t, addr)
	assert.Contains(t, body, `http_route="/users/:id"`)
	assert.NotContains(t, body, `http_route="/users/1"`)
}

// responseWriterWithDeadline is a mock ResponseWriter that also implements
// SetWriteDeadline, allowing us to test ResponseController.Unwrap compatibility.
type responseWriterWithDeadline struct {
	http.ResponseWriter
	deadlineSet bool
}

func (w *responseWriterWithDeadline) SetWriteDeadline(time.Time) error {
	w.deadlineSet = true
	return nil
}

// quickMeter spins up an in-memory MeterProvider on a kernel-chosen scrape
// port. It's a focused helper for tests that don't need to inspect metric
// values, only behaviour around the wrapped ResponseWriter.
func quickMeter(t *testing.T) (mw func(http.Handler) http.Handler, shutdown func()) {
	t.Helper()
	provider, closer, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName: "test", Namespace: "test", MetricsAddr: "127.0.0.1:0",
	})
	require.NoError(t, err)
	mw = o11yhttp.New(context.Background(), provider.Meter("test"))
	shutdown = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = provider.Shutdown(ctx)
	}
	return mw, shutdown
}

func TestMiddleware_ResponseControllerUnwrap(t *testing.T) {
	mw, shutdown := quickMeter(t)
	defer shutdown()

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		err := rc.SetWriteDeadline(time.Now().Add(time.Second))
		assert.NoError(t, err, "ResponseController.SetWriteDeadline should succeed via Unwrap")
	}))

	rec := httptest.NewRecorder()
	mock := &responseWriterWithDeadline{ResponseWriter: rec}
	req := httptest.NewRequest("GET", "/", nil)

	handler.ServeHTTP(mock, req)
	assert.True(t, mock.deadlineSet, "The underlying writer's SetWriteDeadline should have been called")
}

// fullCapableWriter implements every optional ResponseWriter interface we
// care about (Flusher, Hijacker, ReaderFrom). Used to verify that the
// middleware faithfully forwards each capability.
type fullCapableWriter struct {
	http.ResponseWriter
	flushed     bool
	hijacked    bool
	readFromSrc string
}

func (f *fullCapableWriter) Flush() { f.flushed = true }

func (f *fullCapableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijacked = true
	return nil, nil, nil
}

func (f *fullCapableWriter) ReadFrom(src io.Reader) (int64, error) {
	b, err := io.ReadAll(src)
	if err != nil {
		return 0, err
	}
	f.readFromSrc = string(b)
	return int64(len(b)), nil
}

func TestMiddleware_DelegatesFlusherHijackerReaderFrom(t *testing.T) {
	mw, shutdown := quickMeter(t)
	defer shutdown()

	var (
		gotFlusher    bool
		gotHijacker   bool
		gotReaderFrom bool
	)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, gotFlusher = w.(http.Flusher)
		_, gotHijacker = w.(http.Hijacker)
		_, gotReaderFrom = w.(io.ReaderFrom)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if h, ok := w.(http.Hijacker); ok {
			_, _, _ = h.Hijack()
		}
		if rf, ok := w.(io.ReaderFrom); ok {
			_, _ = rf.ReadFrom(strings.NewReader("payload"))
		}
	}))

	mock := &fullCapableWriter{ResponseWriter: httptest.NewRecorder()}
	handler.ServeHTTP(mock, httptest.NewRequest("GET", "/", nil))

	assert.True(t, gotFlusher, "wrapper must advertise http.Flusher when underlying supports it")
	assert.True(t, gotHijacker, "wrapper must advertise http.Hijacker when underlying supports it")
	assert.True(t, gotReaderFrom, "wrapper must advertise io.ReaderFrom when underlying supports it")
	assert.True(t, mock.flushed, "Flush must reach the underlying writer")
	assert.True(t, mock.hijacked, "Hijack must reach the underlying writer")
	assert.Equal(t, "payload", mock.readFromSrc, "ReadFrom must reach the underlying writer")
}

// plainWriter implements ONLY http.ResponseWriter — none of the optional
// interfaces. The middleware wrapper must NOT silently advertise them or
// SSE / file-serve handlers will malfunction.
type plainWriter struct {
	header http.Header
	body   []byte
	code   int
}

func (p *plainWriter) Header() http.Header {
	if p.header == nil {
		p.header = make(http.Header)
	}
	return p.header
}

func (p *plainWriter) Write(b []byte) (int, error) {
	p.body = append(p.body, b...)
	return len(b), nil
}

func (p *plainWriter) WriteHeader(c int) { p.code = c }

func TestMiddleware_DoesNotAdvertiseUnsupportedOptionalInterfaces(t *testing.T) {
	mw, shutdown := quickMeter(t)
	defer shutdown()

	var (
		gotFlusher    bool
		gotHijacker   bool
		gotReaderFrom bool
	)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, gotFlusher = w.(http.Flusher)
		_, gotHijacker = w.(http.Hijacker)
		_, gotReaderFrom = w.(io.ReaderFrom)
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(&plainWriter{}, httptest.NewRequest("GET", "/", nil))

	assert.False(t, gotFlusher, "wrapper must NOT advertise Flusher when underlying doesn't support it")
	assert.False(t, gotHijacker, "wrapper must NOT advertise Hijacker when underlying doesn't support it")
	assert.False(t, gotReaderFrom, "wrapper must NOT advertise ReaderFrom when underlying doesn't support it")
}

// TestMiddleware_PanicRecordsAndRepanics verifies that a handler panic is
// converted to a status_code=500 metric sample and then re-raised so the
// http.Server's default panic handler still runs.
func TestMiddleware_PanicRecordsAndRepanics(t *testing.T) {
	addr := testutil.FreeAddr(t)

	provider, closer, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:      "test-svc",
		Namespace:        "platform",
		MetricsAddr:      addr,
		RuntimeMetrics:   false,
		HistogramBuckets: []float64{1},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = provider.Shutdown(ctx)
	})

	mw := o11yhttp.New(context.Background(), provider.Meter("panic_test"))
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("kaboom")
	}))

	require.PanicsWithValue(t, "kaboom", func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/boom", nil))
	}, "middleware must re-raise the original panic value")

	body := testutil.ScrapeMetrics(t, addr)
	assert.Contains(t, body, `http_response_status_code="500"`,
		"a handler panic before WriteHeader must be recorded as status 500")
	assert.Contains(t, body, `http_route="/boom"`)
}

// TestMiddleware_PanicAfterWriteHeaderKeepsOriginalStatus ensures the
// middleware does not retroactively change the recorded status_code if the
// handler had already committed a status before panicking.
func TestMiddleware_PanicAfterWriteHeaderKeepsOriginalStatus(t *testing.T) {
	addr := testutil.FreeAddr(t)

	provider, closer, err := metrics.InitMeter(context.Background(), metrics.Config{
		ServiceName:      "test-svc",
		Namespace:        "platform",
		MetricsAddr:      addr,
		RuntimeMetrics:   false,
		HistogramBuckets: []float64{1},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = provider.Shutdown(ctx)
	})

	mw := o11yhttp.New(context.Background(), provider.Meter("panic_after_test"))
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("late")
	}))

	require.Panics(t, func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/late", nil))
	})

	body := testutil.ScrapeMetrics(t, addr)
	assert.Contains(t, body, `http_response_status_code="202"`,
		"status committed before panic must be preserved in the metric")
}
