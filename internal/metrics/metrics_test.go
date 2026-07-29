package metrics_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

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
		Exemplars:        true,
	}
}

func TestInitMeter_DefaultHTTPViewsFilterServerAttributes(t *testing.T) {
	addr := testutil.FreeAddr(t)
	mp, closer, err := metrics.InitMeter(context.Background(), baseConfig(addr))
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	hist, err := mp.Meter("test").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	hist.Record(context.Background(), 0.01, metric.WithAttributes(
		semconv.HTTPRequestMethodKey.String("GET"),
		semconv.HTTPRoute("/orders/{id}"),
		semconv.HTTPResponseStatusCode(http.StatusOK),
		semconv.URLFull("https://example.test/orders/123?token=secret"),
		semconv.ClientAddress("203.0.113.10"),
	))

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "http_server_request_duration_seconds_count{") {
			continue
		}
		assert.Contains(t, line, `http_request_method="GET"`)
		assert.Contains(t, line, `http_route="/orders/{id}"`)
		assert.Contains(t, line, `http_response_status_code="200"`)
		assert.NotContains(t, line, "url_full")
		assert.NotContains(t, line, "client_address")
		return
	}
	t.Fatal("http_server_request_duration_seconds_count not found")
}

// TestInitMeter_ExtraHTTPServerAttributeKeysAppearOnSeries verifies that
// keys promoted via ExtraHTTPServerAttrKeys survive the default view filter
// and appear as Prometheus labels on http_server_request_duration_seconds_*.
func TestInitMeter_ExtraHTTPServerAttributeKeysAppearOnSeries(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.ExtraHTTPServerAttrKeys = []string{"app_name", "bot_name"}

	mp, closer, err := metrics.InitMeter(context.Background(), cfg)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	hist, err := mp.Meter("test").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	hist.Record(context.Background(), 0.01, metric.WithAttributes(
		semconv.HTTPRequestMethodKey.String("POST"),
		semconv.HTTPRoute("/v1/chat"),
		semconv.HTTPResponseStatusCode(http.StatusOK),
		attribute.String("app_name", "line-bot"),
		attribute.String("bot_name", "customer-service"),
		attribute.String("server.address", "should-still-be-filtered"),
	))

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "http_server_request_duration_seconds_count{") {
			continue
		}
		assert.Contains(t, line, `app_name="line-bot"`)
		assert.Contains(t, line, `bot_name="customer-service"`)
		assert.Contains(t, line, `http_route="/v1/chat"`)
		assert.NotContains(t, line, "server_address", "non-allow-listed keys must still be filtered")
		return
	}
	t.Fatal("http_server_request_duration_seconds_count not found")
}

// TestInitMeter_ExemplarsStayUnderRuneCap reproduces the failure mode
// callers see in production after attaching extra attributes via
// o11ygin.WithMetricAttributesFn (or any otelgin/otelhttp default attribute
// set): a record carrying several attributes that the SDK view filter
// drops. The OTel SDK routes dropped attributes into the exemplar's
// FilteredAttributes; otelprom then asks client_golang to build an
// exemplar from them; client_golang validates combined runes against
// ExemplarMaxRunes (128), fails, and the otelprom exporter reports the
// failure via otel.Handle — which is exactly the noisy
// "exemplar labels have N runes, exceeding the limit of 128" log that
// callers report. With dropFilteredAttrsExemplarSelector in place, the
// reservoir hands an empty FilteredAttributes set to otelprom, the rune
// check passes, and otel.Handle is never invoked.
func TestInitMeter_ExemplarsStayUnderRuneCap(t *testing.T) {
	captured := newCapturingErrorHandler()
	prevHandler := otel.GetErrorHandler()
	otel.SetErrorHandler(captured)
	t.Cleanup(func() { otel.SetErrorHandler(prevHandler) })

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

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	hist, err := mp.Meter("test").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	hist.Record(ctx, 0.05, metric.WithAttributes(
		semconv.HTTPRequestMethodKey.String("POST"),
		semconv.HTTPRoute("/api/v1/messages"),
		semconv.HTTPResponseStatusCode(http.StatusOK),
		attribute.String("server.address", "my-bot-gateway.platform.svc.cluster.local"),
		attribute.Int("server.port", 8080),
		attribute.String("url.scheme", "https"),
		attribute.String("network.protocol.name", "http"),
		attribute.String("network.protocol.version", "1.1"),
		attribute.String("user_agent.original", "Mozilla/5.0 (compatible; LineBot/2.0)"),
	))

	// Force the otelprom exporter Collect path to run. /metrics scrape is the
	// production trigger.
	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	require.Contains(t, body, "http_server_request_duration_seconds_bucket")

	for _, e := range captured.errors() {
		assert.NotContainsf(t, e, "exemplar labels",
			"otel error handler must not receive the 128-rune exemplar overflow; "+
				"observed error indicates dropFilteredAttrsExemplarSelector is not "+
				"wired in for http.server.request.duration: %s", e)
	}
}

type capturingErrorHandler struct {
	mu   sync.Mutex
	errs []string
}

func newCapturingErrorHandler() *capturingErrorHandler {
	return &capturingErrorHandler{}
}

func (h *capturingErrorHandler) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs = append(h.errs, err.Error())
}

func (h *capturingErrorHandler) errors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.errs))
	copy(out, h.errs)
	return out
}

// TestInitMeter_OpenMetricsScrapeIncludesExemplars verifies that the
// Prometheus pull handler content-negotiates to OpenMetrics format when the
// scraper advertises support, and that exemplars recorded inside a sampled
// trace context show up in the response body. This is the end-to-end signal
// that Grafana's exemplar UI / Tempo trace-to-metric linkage will work — the
// plain Prometheus exposition format the handler used to default to does not
// carry exemplars at all, so scrapers got an empty exemplar store regardless
// of `--enable-feature=exemplar-storage`.
func TestInitMeter_OpenMetricsScrapeIncludesExemplars(t *testing.T) {
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

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	hist, err := mp.Meter("test").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	hist.Record(ctx, 0.05, metric.WithAttributes(
		semconv.HTTPRequestMethodKey.String("POST"),
		semconv.HTTPRoute("/api/v1/messages"),
		semconv.HTTPResponseStatusCode(http.StatusOK),
	))

	body := scrapeOpenMetrics(t.Context(), t, addr)
	require.Contains(t, body, "# TYPE http_server_request_duration_seconds histogram",
		"openmetrics scrape must include the SDK histogram")

	// Exemplar line shape: ..._bucket{...} <count> # {trace_id="...",span_id="..."} <value> <timestamp>
	var exemplarLine string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "# {") && strings.Contains(line, "trace_id=") {
			exemplarLine = line
			break
		}
	}
	require.NotEmptyf(t, exemplarLine,
		"openmetrics scrape must contain at least one trace-linked exemplar; "+
			"if missing, EnableOpenMetrics on promhttp.HandlerOpts is likely off")
	assert.Contains(t, exemplarLine, traceID.String(), "exemplar must carry trace_id")
	assert.Contains(t, exemplarLine, spanID.String(), "exemplar must carry span_id")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(body), "# EOF"),
		"openmetrics format must terminate with the EOF marker; absence indicates fallback to plain Prometheus exposition format")
}

// TestInitMeter_ExemplarsDisabledSuppressesOpenMetrics verifies that
// Exemplars=false keeps the handler on the plain Prometheus exposition
// format (no exemplar lines, no "# EOF" sentinel, integer `le` boundaries
// stay integer). This is the opt-out users need when migrating dashboards
// that hardcode integer histogram bucket labels.
func TestInitMeter_ExemplarsDisabledSuppressesOpenMetrics(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.RuntimeMetrics = false
	cfg.Exemplars = false

	mp, closer, err := metrics.InitMeter(context.Background(), cfg)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		SpanID:     trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	hist, err := mp.Meter("test").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	hist.Record(ctx, 0.05, metric.WithAttributes(
		semconv.HTTPRequestMethodKey.String("GET"),
		semconv.HTTPRoute("/x"),
		semconv.HTTPResponseStatusCode(http.StatusOK),
	))

	// Even though the scraper advertises OpenMetrics, the handler must
	// fall back to text/plain because EnableOpenMetrics is off.
	reqCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/metrics", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Containsf(t, resp.Header.Get("Content-Type"), "text/plain",
		"WithExemplars(false) must keep the handler on plain Prometheus format (got %q)",
		resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "# {trace_id=",
		"no exemplar line may be rendered when Exemplars=false")
	assert.NotContains(t, string(body), "# EOF",
		"plain Prometheus format has no openmetrics EOF sentinel")
	// Integer bucket boundary keeps its integer rendering — no `.0` suffix.
	assert.Contains(t, string(body), `le="1"`,
		"plain Prometheus format keeps integer le boundaries; this is the compatibility path WithExemplars(false) preserves")
}

func scrapeOpenMetrics(ctx context.Context, t *testing.T, addr string) string {
	t.Helper()
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/metrics", nil)
	require.NoError(t, err)
	// Matches the Accept header real Prometheus scrapers send when OpenMetrics
	// is enabled in their scrape_config.
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"openmetrics scrape failed; body=%s", string(body))
	assert.Containsf(t, resp.Header.Get("Content-Type"), "openmetrics-text",
		"handler must content-negotiate to OpenMetrics (got Content-Type %q)",
		resp.Header.Get("Content-Type"))
	return string(body)
}

func TestInitMeter_DisableDefaultViewsKeepsHTTPAttributes(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.DisableDefaultViews = true

	mp, closer, err := metrics.InitMeter(context.Background(), cfg)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	hist, err := mp.Meter("test").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	hist.Record(context.Background(), 0.01, metric.WithAttributes(
		semconv.HTTPRequestMethodKey.String("GET"),
		semconv.HTTPRoute("/orders/{id}"),
		semconv.HTTPResponseStatusCode(http.StatusOK),
		semconv.URLFull("https://example.test/orders/123"),
	))

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Contains(t, body, "url_full")
}

func TestInitMeter_MaxUniqueRoutesCapsPrometheusRoutes(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.MaxUniqueRoutes = 1
	cfg.RuntimeMetrics = false

	mp, closer, err := metrics.InitMeter(context.Background(), cfg)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	hist, err := mp.Meter("test").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	for _, route := range []string{"/keep", "/overflow-a", "/overflow-b"} {
		hist.Record(context.Background(), 0.01, metric.WithAttributes(
			semconv.HTTPRequestMethodKey.String("GET"),
			semconv.HTTPRoute(route),
			semconv.HTTPResponseStatusCode(http.StatusOK),
		))
	}

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Contains(t, body, `http_route="other"`)
	assert.Contains(t, body, `http_route="/keep"`)
	assert.NotContains(t, body, `http_route="/overflow-a"`)
	assert.NotContains(t, body, `http_route="/overflow-b"`)
	var foundOtherCount bool
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "http_server_request_duration_seconds_count{") &&
			strings.Contains(line, `http_route="other"`) &&
			strings.HasSuffix(line, "} 2") {
			foundOtherCount = true
			break
		}
	}
	assert.True(t, foundOtherCount, "overflow route count should merge to 2")
}

func TestInitMeter_MaxUniqueRoutesAppliesSDKOverflow(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.MaxUniqueRoutes = 1
	cfg.RuntimeMetrics = false

	mp, closer, err := metrics.InitMeter(context.Background(), cfg)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	hist, err := mp.Meter("test").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	for i := 0; i < 1100; i++ {
		hist.Record(context.Background(), 0.01, metric.WithAttributes(
			semconv.HTTPRequestMethodKey.String("GET"),
			semconv.HTTPRoute("/route-"+strconv.Itoa(i)),
			semconv.HTTPResponseStatusCode(http.StatusOK),
		))
	}

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Contains(t, body, `otel_metric_overflow="true"`)
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
	// Asserting every captured request — not just one — catches a regression
	// where the header is dropped on retries or later periodic exports.
	for i, r := range requests {
		assert.Equal(t, "Bearer xyz", r.Header.Get("Authorization"),
			"Authorization header must propagate to every OTLP metrics request (request[%d] %s %s)",
			i, r.Method, r.Path)
	}
}

// TestInitMeter_MaxUniqueCollectionsCapsCassandraTables pins the export-boundary
// cap for db.collection.name on the Cassandra client metrics. The label is on by
// default (ADR 0019 §7, 2026-07-29 amendment) and a Cassandra schema is
// DDL-fixed, so this cap exists for the case where the SDK's CQL tokenizer
// mis-reads a statement shape and starts minting table values.
//
// It also pins the rendered Prometheus family names the cap rules are keyed on:
// those names are derived by otelprom from the instrument name and unit, so a
// naming change upstream would silently disable the cap rather than fail loudly.
func TestInitMeter_MaxUniqueCollectionsCapsCassandraTables(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.MaxUniqueCollections = 1
	cfg.RuntimeMetrics = false

	mp, closer, err := metrics.InitMeter(context.Background(), cfg)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	meter := mp.Meter("github.com/flywindy/o11y/cassandra")
	hist, err := meter.Float64Histogram("db.client.operation.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	counter, err := meter.Int64Counter("cassandra.query.attempts", metric.WithUnit("{attempt}"))
	require.NoError(t, err)
	for _, table := range []string{"keep_table", "overflow_a", "overflow_b"} {
		attrs := metric.WithAttributes(
			semconv.DBSystemNameCassandra,
			semconv.DBOperationName("SELECT"),
			semconv.DBNamespace("chat"),
			semconv.DBCollectionName(table),
		)
		hist.Record(context.Background(), 0.01, attrs)
		counter.Add(context.Background(), 1, attrs)
	}

	body := testutil.ScrapeMetrics(t.Context(), t, addr)

	// The family names the cap rules key on must be the ones actually exposed.
	require.Contains(t, body, "db_client_operation_duration_seconds")
	require.Contains(t, body, "cassandra_query_attempts_total")

	assert.Contains(t, body, `db_collection_name="keep_table"`)
	assert.Contains(t, body, `db_collection_name="other"`)
	assert.NotContains(t, body, `db_collection_name="overflow_a"`)
	assert.NotContains(t, body, `db_collection_name="overflow_b"`)

	// Both overflow tables merge into a single "other" series per instrument.
	var histOther, counterOther bool
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, `db_collection_name="other"`) {
			continue
		}
		if strings.HasPrefix(line, "db_client_operation_duration_seconds_count{") && strings.HasSuffix(line, "} 2") {
			histOther = true
		}
		if strings.HasPrefix(line, "cassandra_query_attempts_total{") && strings.HasSuffix(line, "} 2") {
			counterOther = true
		}
	}
	assert.True(t, histOther, "overflow tables should merge to one duration series with count 2")
	assert.True(t, counterOther, "overflow tables should merge to one attempts series with value 2")
}

// A zero MaxUniqueCollections installs no rule, leaving table values untouched.
func TestInitMeter_MaxUniqueCollectionsDisabledKeepsAllTables(t *testing.T) {
	addr := testutil.FreeAddr(t)
	cfg := baseConfig(addr)
	cfg.MaxUniqueCollections = 0
	cfg.RuntimeMetrics = false

	mp, closer, err := metrics.InitMeter(context.Background(), cfg)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = closer(ctx)
		_ = mp.Shutdown(ctx)
	}()

	hist, err := mp.Meter("github.com/flywindy/o11y/cassandra").
		Float64Histogram("db.client.operation.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	for _, table := range []string{"rooms", "messages_by_room"} {
		hist.Record(context.Background(), 0.01, metric.WithAttributes(
			semconv.DBSystemNameCassandra,
			semconv.DBCollectionName(table),
		))
	}

	body := testutil.ScrapeMetrics(t.Context(), t, addr)
	assert.Contains(t, body, `db_collection_name="rooms"`)
	assert.Contains(t, body, `db_collection_name="messages_by_room"`)
	assert.NotContains(t, body, `db_collection_name="other"`)
}
