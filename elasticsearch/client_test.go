package elasticsearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	elastic "github.com/elastic/go-elasticsearch/v8"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// esStub returns an httptest server that answers like Elasticsearch: it sets
// the X-Elastic-Product header the client's product check requires and returns
// the supplied status with an empty JSON body.
func esStub(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordingProvider returns a TracerProvider backed by an in-memory recorder so
// tests can assert the spans the upstream instrumentation produced landed on
// the supplied provider rather than the OTel global.
func recordingProvider() (trace.TracerProvider, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return tp, rec
}

func attrMap(kvs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

// noopMeter is the MeterProvider for tests that only assert spans.
func noopMeter() metric.MeterProvider { return metricnoop.NewMeterProvider() }

// recordingMeter returns a MeterProvider backed by a ManualReader so tests can
// collect the db.client.operation.duration samples the facade records.
func recordingMeter(views ...sdkmetric.View) (metric.MeterProvider, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	return sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(views...)), reader
}

// collectDuration collects the reader and returns the
// db.client.operation.duration histogram recorded under this package's
// instrumentation scope, failing the test when it is absent.
func collectDuration(t *testing.T, reader *sdkmetric.ManualReader) metricdata.Histogram[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != instrumentationName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "db.client.operation.duration" {
				continue
			}
			if m.Unit != "s" {
				t.Errorf("unit = %q, want s", m.Unit)
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("data type = %T, want Histogram[float64]", m.Data)
			}
			return h
		}
	}
	t.Fatalf("db.client.operation.duration not recorded under scope %s", instrumentationName)
	return metricdata.Histogram[float64]{}
}

// singleDataPoint asserts the histogram holds exactly one data point with one
// sample and returns it.
func singleDataPoint(t *testing.T, h metricdata.Histogram[float64]) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	if len(h.DataPoints) != 1 {
		t.Fatalf("got %d data points, want 1", len(h.DataPoints))
	}
	dp := h.DataPoints[0]
	if dp.Count != 1 {
		t.Fatalf("sample count = %d, want 1", dp.Count)
	}
	return dp
}

func dpAttrs(dp metricdata.HistogramDataPoint[float64]) map[attribute.Key]attribute.Value {
	return attrMap(dp.Attributes.ToSlice())
}

func TestNewClient_NilTracerProvider(t *testing.T) {
	if _, err := NewClient(elastic.Config{}, nil, noopMeter()); err == nil ||
		!strings.Contains(err.Error(), "tracer provider must not be nil") {
		t.Fatalf("NewClient(nil tp): got %v, want tracer-provider error", err)
	}
	if _, err := NewTypedClient(elastic.Config{}, nil, noopMeter()); err == nil ||
		!strings.Contains(err.Error(), "tracer provider must not be nil") {
		t.Fatalf("NewTypedClient(nil tp): got %v, want tracer-provider error", err)
	}
}

// TestNewClient_NilMeterProvider: the metric layer is SDK-owned and must never
// fall back to the global MeterProvider (ADR 0003), so a nil mp is rejected the
// same way a nil tp is.
func TestNewClient_NilMeterProvider(t *testing.T) {
	if _, err := NewClient(elastic.Config{}, noop.NewTracerProvider(), nil); err == nil ||
		!strings.Contains(err.Error(), "meter provider must not be nil") {
		t.Fatalf("NewClient(nil mp): got %v, want meter-provider error", err)
	}
	if _, err := NewTypedClient(elastic.Config{}, noop.NewTracerProvider(), nil); err == nil ||
		!strings.Contains(err.Error(), "meter provider must not be nil") {
		t.Fatalf("NewTypedClient(nil mp): got %v, want meter-provider error", err)
	}
}

// TestSearch_EmitsClientSpan exercises the index-addressing search endpoint and
// asserts the span the pinned upstream emits, including the exact (legacy)
// attribute keys recorded in ADR 0020 §4 and docs/semconv.md.
func TestSearch_EmitsClientSpan(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	tp, rec := recordingProvider()

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res, err := client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]

	if span.SpanKind() != trace.SpanKindClient {
		t.Fatalf("span kind = %v, want client", span.SpanKind())
	}

	// ADR 0023: span name is {db.system.name}.{operation} {target}, rewritten by
	// the facade from the bare upstream endpoint id ("search").
	if got := span.Name(); got != "elasticsearch.search my-index" {
		t.Errorf("span name = %q, want %q", got, "elasticsearch.search my-index")
	}

	attrs := attrMap(span.Attributes())
	// ADR 0020 §4: the pinned elastic-transport-go/v8 v8.8.0 emits the legacy
	// core keys (db.system, not db.system.name; db.operation, not
	// db.operation.name) and the deprecated db.elasticsearch.path_parts.* key.
	if got := attrs["db.system"].AsString(); got != "elasticsearch" {
		t.Errorf("db.system = %q, want elasticsearch", got)
	}
	if got := attrs["db.operation"].AsString(); got != "search" {
		t.Errorf("db.operation = %q, want search", got)
	}
	if got := attrs["db.elasticsearch.path_parts.index"].AsString(); got != "my-index" {
		t.Errorf("db.elasticsearch.path_parts.index = %q, want my-index", got)
	}
	if got := attrs["http.request.method"].AsString(); got != http.MethodPost {
		t.Errorf("http.request.method = %q, want POST", got)
	}
	// The current-semconv spellings must be absent (drift is inherited, not
	// normalized at the boundary).
	if _, ok := attrs["db.system.name"]; ok {
		t.Error("unexpected db.system.name: upstream emits the legacy db.system")
	}
	if _, ok := attrs["db.operation.name"]; ok {
		t.Error("unexpected db.operation.name: upstream emits the legacy db.operation")
	}
}

// TestSearchBody_OptIn asserts db.statement is absent by default and present
// only under WithSearchBody(true) (ADR 0020 §5).
func TestSearchBody_OptIn(t *testing.T) {
	const body = `{"query":{"match_all":{}}}`

	search := func(opts ...Option) sdktrace.ReadOnlySpan {
		t.Helper()
		srv := esStub(t, http.StatusOK)
		tp, rec := recordingProvider()
		client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, noopMeter(), opts...)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		res, err := client.Search(
			client.Search.WithContext(context.Background()),
			client.Search.WithIndex("my-index"),
			client.Search.WithBody(strings.NewReader(body)),
		)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		_ = res.Body.Close()
		spans := rec.Ended()
		if len(spans) != 1 {
			t.Fatalf("got %d spans, want 1", len(spans))
		}
		return spans[0]
	}

	if _, ok := attrMap(search().Attributes())["db.statement"]; ok {
		t.Error("db.statement present by default; want absent")
	}

	got := attrMap(search(WithSearchBody(true)).Attributes())["db.statement"].AsString()
	if got != body {
		t.Errorf("db.statement = %q, want %q", got, body)
	}
}

// TestSearchBody_NilBodyNoPanic guards the nilBodyGuard wrapper: a search-family
// request with no body under WithSearchBody(true) must not panic. The pinned
// upstream calls bytes.Buffer.ReadFrom(query) unconditionally for search
// endpoints, which panics on the nil body the generated API passes for a
// bodyless search.
func TestSearchBody_NilBodyNoPanic(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	tp, rec := recordingProvider()

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, noopMeter(), WithSearchBody(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// No WithBody — r.Body is nil; without the guard this panics upstream.
	res, err := client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if _, ok := attrMap(spans[0].Attributes())["db.statement"]; ok {
		t.Error("db.statement present for a bodyless search; want absent")
	}
}

// TestTypedClient_Do_EmitsSpan asserts the typed client's idiomatic .Do(ctx)
// path is fully instrumented (the documented, supported typed path — unlike the
// raw .Perform escape hatch, see NewTypedClient). It backs the claim that
// NewTypedClient shares the same instrumentation wiring as NewClient.
func TestTypedClient_Do_EmitsSpan(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	tp, rec := recordingProvider()

	client, err := NewTypedClient(elastic.Config{Addresses: []string{srv.URL}}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewTypedClient: %v", err)
	}

	if _, err := client.Search().Index("my-index").Do(context.Background()); err != nil {
		t.Fatalf("typed Search.Do: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.SpanKind() != trace.SpanKindClient {
		t.Fatalf("span kind = %v, want client", span.SpanKind())
	}
	attrs := attrMap(span.Attributes())
	if got := attrs["db.operation"].AsString(); got != "search" {
		t.Errorf("db.operation = %q, want search", got)
	}
	if got := attrs["db.elasticsearch.path_parts.index"].AsString(); got != "my-index" {
		t.Errorf("db.elasticsearch.path_parts.index = %q, want my-index", got)
	}
}

// TestTypedClient_Do_HTTPErrorKeepsStatusCode asserts that typed API response
// errors keep the final http.response.status_code attribute. The typed Do path
// calls RecordError after decoding the ES error response body; the facade must
// not treat that as a terminal transport/product-check error that suppresses the
// status code.
func TestTypedClient_Do_HTTPErrorKeepsStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"illegal_argument_exception","reason":"bad request"},"status":400}`))
	}))
	t.Cleanup(srv.Close)

	tp, rec := recordingProvider()
	client, err := NewTypedClient(elastic.Config{Addresses: []string{srv.URL}}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewTypedClient: %v", err)
	}

	_, err = client.Search().Index("my-index").Do(context.Background())
	if err == nil {
		t.Fatal("typed Search.Do: want ES response error, got nil")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error", span.Status().Code)
	}
	if got := attrMap(span.Attributes())["http.response.status_code"].AsInt64(); got != http.StatusBadRequest {
		t.Errorf("http.response.status_code = %d, want 400", got)
	}
}

// TestFailedRequest_StatusErrorNoErrorType asserts a failed request sets span
// status = Error and records the exception, with no error.type attribute,
// matching elastic-transport-go/v8 v8.8.0's RecordError (ADR 0020 §4 †).
func TestFailedRequest_StatusErrorNoErrorType(t *testing.T) {
	// No stub: the address is unroutable so the transport errors before any
	// response, driving the instrumentation's RecordError path.
	tp, rec := recordingProvider()
	client, err := NewClient(elastic.Config{
		Addresses:    []string{"http://127.0.0.1:1"},
		MaxRetries:   0,
		DisableRetry: true,
	}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res, err := client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err == nil {
		_ = res.Body.Close()
		t.Fatal("Search against unroutable address: got nil error, want failure")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]

	if span.Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error", span.Status().Code)
	}
	if _, ok := attrMap(span.Attributes())["error.type"]; ok {
		t.Error("unexpected error.type: pinned upstream sets none (ADR 0020 §4)")
	}
	if len(span.Events()) == 0 {
		t.Error("expected a recorded exception event from RecordError")
	}
}

// TestProviderWiring asserts spans land on the supplied provider, not the
// global one (ADR 0003).
func TestProviderWiring(t *testing.T) {
	srv := esStub(t, http.StatusOK)

	globalTP, globalRec := recordingProvider()
	originalGlobal := otel.GetTracerProvider()
	otel.SetTracerProvider(globalTP)
	t.Cleanup(func() { otel.SetTracerProvider(originalGlobal) })

	tp, rec := recordingProvider()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := client.Search(client.Search.WithContext(context.Background()))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	if got := len(rec.Ended()); got != 1 {
		t.Errorf("supplied provider recorded %d spans, want 1", got)
	}
	if got := len(globalRec.Ended()); got != 0 {
		t.Errorf("global provider recorded %d spans, want 0 (no global fallback)", got)
	}
}

// TestSearch_NoContextNoopProviderNoPanic asserts the facade tolerates the
// generated low-level helper's default nil context. Without normalization in
// Start, the no-op tracer path panics when it calls ContextWithSpan(nil, ...).
func TestSearch_NoContextNoopProviderNoPanic(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, noop.NewTracerProvider(), noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res, err := client.Search(client.Search.WithIndex("my-index"))
	if err != nil {
		t.Fatalf("Search without WithContext: %v", err)
	}
	_ = res.Body.Close()
}

// TestHTTPError_SetsErrorStatus asserts the facade reflects an ES HTTP error
// response (which the low-level API returns as (*Response, nil)) on the span:
// status = Error plus http.response.status_code. The bare upstream does neither
// (ADR 0020 §4).
func TestHTTPError_SetsErrorStatus(t *testing.T) {
	srv := esStub(t, http.StatusInternalServerError)
	tp, rec := recordingProvider()

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// A 500 is not a transport error: Search returns a response with nil err.
	res, err := client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error", span.Status().Code)
	}
	if got := attrMap(span.Attributes())["http.response.status_code"].AsInt64(); got != http.StatusInternalServerError {
		t.Errorf("http.response.status_code = %d, want 500", got)
	}
}

// TestHTTPError_RetryThenSuccess asserts the retry handling: an attempt that
// fails with a retryable status (503) and then succeeds (200) ends with a
// single span that is not marked Error (status UNSET) because the facade
// reflects only the final attempt, and whose http.response.status_code reflects
// that final attempt.
func TestHTTPError_RetryThenSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // retried by default
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	tp, rec := recordingProvider()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res, err := client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server calls = %d, want 2 (one retry)", got)
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1 (one span across retries)", len(spans))
	}
	span := spans[0]
	if span.Status().Code != codes.Unset {
		t.Errorf("status code = %v, want Unset (a retried 503->200 must not be marked Error)", span.Status().Code)
	}
	if got := attrMap(span.Attributes())["http.response.status_code"].AsInt64(); got != http.StatusOK {
		t.Errorf("http.response.status_code = %d, want 200", got)
	}
}

// TestSpanName_NoIndex_OmitsTarget asserts a search without an index keeps the
// bare "elasticsearch.search" name (target omitted), per ADR 0023.
func TestSpanName_NoIndex_OmitsTarget(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	tp, rec := recordingProvider()

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res, err := client.Search(client.Search.WithContext(context.Background()))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if got := spans[0].Name(); got != "elasticsearch.search" {
		t.Errorf("span name = %q, want %q", got, "elasticsearch.search")
	}
}

// TestHTTP3xx_MarksError asserts a redirect/proxy 3xx (which the client's own
// esapi.Response.IsError treats as a failure, status > 299) is marked Error on
// the span — not left successful. Regression test for PR #57 review.
func TestHTTP3xx_MarksError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		// No Location header, so net/http does not follow the redirect.
		w.WriteHeader(http.StatusFound) // 302
	}))
	t.Cleanup(srv.Close)

	tp, rec := recordingProvider()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res, err := client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error (3xx is > 299)", span.Status().Code)
	}
	if got := attrMap(span.Attributes())["http.response.status_code"].AsInt64(); got != http.StatusFound {
		t.Errorf("http.response.status_code = %d, want 302", got)
	}
}

// TestProductCheckFailure_StaysError asserts that a 200 response that then fails
// the client's product check (no X-Elastic-Product header — e.g. a proxy or a
// non-ES service) keeps the span status = Error reported by the upstream
// RecordError. The facade must not have overwritten it with Ok. Regression test
// for PR #57 review.
func TestProductCheckFailure_StaysError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately omit X-Elastic-Product: the product check fails.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	tp, rec := recordingProvider()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// The product check turns this 200 into a client-level error.
	_, err = client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err == nil {
		t.Fatal("Search: want product-check error, got nil")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if got := spans[0].Status().Code; got != codes.Error {
		t.Errorf("status code = %v, want Error (product-check failure must not be reported Ok)", got)
	}
}

// TestRetryThenTransportError_NoStaleStatusCode asserts that when a retryable
// 503 is followed by a terminal transport error, the span is Error (from the
// upstream RecordError) and does NOT carry the stale 503 status code from the
// earlier attempt — the status code reflects the caller's terminal outcome, not
// an intermediate retry. Regression test for PR #57 review.
func TestRetryThenTransportError_NoStaleStatusCode(t *testing.T) {
	var calls int32
	handlerErr := make(chan error, 1)
	recordHandlerErr := func(err error) {
		select {
		case handlerErr <- err:
		default:
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusServiceUnavailable) // 503, retried by default
			return
		}
		// Subsequent attempts: sever the connection to force a transport error.
		hj, ok := w.(http.Hijacker)
		if !ok {
			recordHandlerErr(errors.New("ResponseWriter is not a Hijacker"))
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			recordHandlerErr(err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	tp, rec := recordingProvider()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, noopMeter())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err == nil {
		t.Fatal("Search: want transport error after retries, got nil")
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("server calls = %d, want >= 2 (a retry after the 503)", got)
	}
	select {
	case err := <-handlerErr:
		t.Fatalf("server handler: %v", err)
	default:
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error", span.Status().Code)
	}
	if _, present := attrMap(span.Attributes())["http.response.status_code"]; present {
		t.Error("http.response.status_code present, want absent (503 was an earlier retried attempt, not the terminal transport error)")
	}
}

// TestSearch_RecordsOperationDuration is the core ADR 0027 contract: one
// db.client.operation.duration sample per request, recorded under this
// package's own instrumentation scope with the current semconv v1.39.0 keys
// (not the legacy spellings the upstream span carries), and correlated to the
// ES CLIENT span via an exemplar.
func TestSearch_RecordsOperationDuration(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	tp, rec := recordingProvider()
	mp, reader := recordingMeter()

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	dp := singleDataPoint(t, collectDuration(t, reader))
	if dp.Sum <= 0 {
		t.Errorf("duration sum = %v, want > 0", dp.Sum)
	}
	attrs := dpAttrs(dp)
	if got := attrs[semconv.DBSystemNameKey].AsString(); got != "elasticsearch" {
		t.Errorf("db.system.name = %q, want elasticsearch", got)
	}
	if got := attrs[semconv.DBOperationNameKey].AsString(); got != "search" {
		t.Errorf("db.operation.name = %q, want search", got)
	}
	if got := attrs[semconv.DBCollectionNameKey].AsString(); got != "my-index" {
		t.Errorf("db.collection.name = %q, want my-index", got)
	}
	srvURL, _ := url.Parse(srv.URL)
	if got := attrs[semconv.ServerAddressKey].AsString(); got != srvURL.Hostname() {
		t.Errorf("server.address = %q, want %q", got, srvURL.Hostname())
	}
	wantPort, _ := strconv.Atoi(srvURL.Port())
	if got := attrs[semconv.ServerPortKey].AsInt64(); got != int64(wantPort) {
		t.Errorf("server.port = %d, want %d", got, wantPort)
	}
	if _, ok := attrs[semconv.ErrorTypeKey]; ok {
		t.Error("error.type present on a successful request; want absent")
	}
	if _, ok := attrs[semconv.DBResponseStatusCodeKey]; ok {
		t.Error("db.response.status_code present on a successful request; want absent (failures only)")
	}
	// The metric is SDK-owned, so the legacy upstream keys must not leak in.
	for _, legacy := range []attribute.Key{"db.system", "db.operation", "db.elasticsearch.path_parts.index", "url.full"} {
		if _, ok := attrs[legacy]; ok {
			t.Errorf("legacy/span-only key %s present on the metric", legacy)
		}
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if len(dp.Exemplars) != 1 {
		t.Fatalf("got %d exemplars, want 1 (recorded against the ES span context)", len(dp.Exemplars))
	}
	if got, want := dp.Exemplars[0].SpanID, spans[0].SpanContext().SpanID(); string(got) != string(want[:]) {
		t.Errorf("exemplar span id = %x, want the ES client span %x", got, want)
	}
}

// TestMetric_HTTPErrorClassifiedByStatus asserts an ES HTTP error response is
// classified on the metric by its status code (error.type = "500"), the same
// > 299 boundary the span status uses.
func TestMetric_HTTPErrorClassifiedByStatus(t *testing.T) {
	srv := esStub(t, http.StatusInternalServerError)
	tp, _ := recordingProvider()
	mp, reader := recordingMeter()

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	if got := attrs[semconv.ErrorTypeKey].AsString(); got != "500" {
		t.Errorf("error.type = %q, want 500", got)
	}
	// The domain-specific status attribute accompanies error.type (semconv
	// error.type guidance: record the domain attribute and error.type both).
	if got := attrs[semconv.DBResponseStatusCodeKey].AsString(); got != "500" {
		t.Errorf("db.response.status_code = %q, want 500", got)
	}
	if got := attrs[semconv.DBOperationNameKey].AsString(); got != "search" {
		t.Errorf("db.operation.name = %q, want search (kept on failures)", got)
	}
}

// TestMetric_TypedResponseErrorClassifiedByStatus asserts the typed API's
// ElasticsearchError (RecordError after decoding the error body, but with a
// final response in hand) is classified by status code, not by Go type.
func TestMetric_TypedResponseErrorClassifiedByStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"illegal_argument_exception","reason":"bad request"},"status":400}`))
	}))
	t.Cleanup(srv.Close)

	tp, _ := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewTypedClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
	if err != nil {
		t.Fatalf("NewTypedClient: %v", err)
	}
	if _, err := client.Search().Index("my-index").Do(context.Background()); err == nil {
		t.Fatal("typed Search.Do: want ES response error, got nil")
	}

	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	if got := attrs[semconv.ErrorTypeKey].AsString(); got != "400" {
		t.Errorf("error.type = %q, want 400", got)
	}
	if got := attrs[semconv.DBResponseStatusCodeKey].AsString(); got != "400" {
		t.Errorf("db.response.status_code = %q, want 400", got)
	}
	if got := attrs[semconv.DBCollectionNameKey].AsString(); got != "my-index" {
		t.Errorf("db.collection.name = %q, want my-index (typed .Do path records the index)", got)
	}
}

// TestMetric_TransportErrorClassifiedByType asserts a terminal transport
// failure is classified by the Go error type (the classification the SDK's
// other integrations use), never by a stale status code, and carries no
// server.port for an unreachable node only when the transport never selected
// one.
func TestMetric_TransportErrorClassifiedByType(t *testing.T) {
	tp, _ := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewClient(elastic.Config{
		Addresses:    []string{"http://127.0.0.1:1"},
		MaxRetries:   0,
		DisableRetry: true,
	}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, searchErr := client.Search(
		client.Search.WithContext(context.Background()),
		client.Search.WithIndex("my-index"),
	)
	if searchErr == nil {
		t.Fatal("Search against unroutable address: got nil error, want failure")
	}

	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	// The low-level API returns the transport error unwrapped, so the label is
	// exactly what the shared classifier yields for it.
	if got, want := attrs[semconv.ErrorTypeKey].AsString(), errorType(searchErr); got != want {
		t.Errorf("error.type = %q, want %q", got, want)
	}
	if _, err := strconv.Atoi(attrs[semconv.ErrorTypeKey].AsString()); err == nil {
		t.Error("error.type is a status code on a transport failure; want the error type")
	}
	// A refused connection surfaces from net.Dialer as *net.OpError on every
	// platform; pin it so the classifier's output is a concrete, known bucket.
	if got := attrs[semconv.ErrorTypeKey].AsString(); got != "*net.OpError" {
		t.Errorf("error.type = %q, want *net.OpError", got)
	}
	if _, ok := attrs[semconv.DBResponseStatusCodeKey]; ok {
		t.Error("db.response.status_code present on a transport failure; want absent (no response)")
	}
}

// TestMetric_ContextCanceledClassifiedBySentinel asserts a caller cancellation
// maps to the stable "context.Canceled" label rather than a transport wrapper
// type, so dashboards can separate caller cancellations from node failures.
func TestMetric_ContextCanceledClassifiedBySentinel(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	tp, _ := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Search(client.Search.WithContext(ctx), client.Search.WithIndex("my-index")); err == nil {
		t.Fatal("Search with canceled context: got nil error, want failure")
	}

	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	if got := attrs[semconv.ErrorTypeKey].AsString(); got != "context.Canceled" {
		t.Errorf("error.type = %q, want context.Canceled", got)
	}
}

// TestMetric_RetryThenSuccessIsOneSuccessfulSample asserts a request retried
// from a 503 to a 200 records a single sample (one per request, not per
// attempt) with no error.type — the terminal outcome, as with the span.
func TestMetric_RetryThenSuccessIsOneSuccessfulSample(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	tp, _ := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := client.Search(client.Search.WithContext(context.Background()), client.Search.WithIndex("my-index"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server calls = %d, want 2 (one retry)", got)
	}

	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	if _, ok := attrs[semconv.ErrorTypeKey]; ok {
		t.Errorf("error.type = %q on a retried 503->200; want absent", attrs[semconv.ErrorTypeKey].AsString())
	}
}

// TestMetric_ProductCheckFailureIsAnError asserts a 200 that fails the client's
// product check is a failure on the metric too (classified by error type), not
// a success with a 200 stashed from the response.
func TestMetric_ProductCheckFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json") // no X-Elastic-Product
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	tp, _ := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Search(client.Search.WithContext(context.Background()), client.Search.WithIndex("my-index")); err == nil {
		t.Fatal("Search: want product-check error, got nil")
	}

	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	got, ok := attrs[semconv.ErrorTypeKey]
	if !ok {
		t.Fatal("error.type absent on a product-check failure; want present")
	}
	if got.AsString() == "200" {
		t.Error("error.type = 200: the stale response code must not classify a product-check failure")
	}
}

// TestMetric_CollectionLabelPolicy pins the db.collection.name rules (ADR 0027
// §4): present for a single index (wildcards included), absent for a
// comma-separated multi-index request, absent when no index is addressed, and
// absent under WithCollectionMetricLabel(false) while the span keeps its index
// path part.
func TestMetric_CollectionLabelPolicy(t *testing.T) {
	search := func(t *testing.T, indices []string, opts ...Option) (map[attribute.Key]attribute.Value, sdktrace.ReadOnlySpan) {
		t.Helper()
		srv := esStub(t, http.StatusOK)
		tp, rec := recordingProvider()
		mp, reader := recordingMeter()
		client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp, opts...)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		res, err := client.Search(client.Search.WithContext(context.Background()), client.Search.WithIndex(indices...))
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		_ = res.Body.Close()
		spans := rec.Ended()
		if len(spans) != 1 {
			t.Fatalf("got %d spans, want 1", len(spans))
		}
		return dpAttrs(singleDataPoint(t, collectDuration(t, reader))), spans[0]
	}

	t.Run("single index", func(t *testing.T) {
		attrs, _ := search(t, []string{"orders"})
		if got := attrs[semconv.DBCollectionNameKey].AsString(); got != "orders" {
			t.Errorf("db.collection.name = %q, want orders", got)
		}
	})
	t.Run("wildcard is one addressed name", func(t *testing.T) {
		attrs, _ := search(t, []string{"logs-*"})
		if got := attrs[semconv.DBCollectionNameKey].AsString(); got != "logs-*" {
			t.Errorf("db.collection.name = %q, want logs-*", got)
		}
	})
	t.Run("multi-index omitted", func(t *testing.T) {
		attrs, span := search(t, []string{"a", "b"})
		if v, ok := attrs[semconv.DBCollectionNameKey]; ok {
			t.Errorf("db.collection.name = %q for a multi-index search; want absent", v.AsString())
		}
		if got := attrMap(span.Attributes())["db.elasticsearch.path_parts.index"].AsString(); got != "a,b" {
			t.Errorf("span path part = %q, want a,b (span keeps the full list)", got)
		}
	})
	t.Run("no index omitted", func(t *testing.T) {
		attrs, _ := search(t, nil)
		if v, ok := attrs[semconv.DBCollectionNameKey]; ok {
			t.Errorf("db.collection.name = %q for an index-less search; want absent", v.AsString())
		}
		if got := attrs[semconv.DBOperationNameKey].AsString(); got != "search" {
			t.Errorf("db.operation.name = %q, want search", got)
		}
	})
	t.Run("opt-out keeps span attribute", func(t *testing.T) {
		attrs, span := search(t, []string{"orders"}, WithCollectionMetricLabel(false))
		if v, ok := attrs[semconv.DBCollectionNameKey]; ok {
			t.Errorf("db.collection.name = %q under WithCollectionMetricLabel(false); want absent", v.AsString())
		}
		if got := attrMap(span.Attributes())["db.elasticsearch.path_parts.index"].AsString(); got != "orders" {
			t.Errorf("span path part = %q, want orders (opt-out is metric-only)", got)
		}
	})
}

// TestMetric_RecordedWhenSpanNotSampled asserts the metric does not depend on
// the span being sampled: metrics are not sampled, and the per-request state
// travels in the context regardless of the span's recording state.
func TestMetric_RecordedWhenSpanNotSampled(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()), sdktrace.WithSpanProcessor(rec))
	mp, reader := recordingMeter()

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := client.Search(client.Search.WithContext(context.Background()), client.Search.WithIndex("my-index"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	if got := len(rec.Ended()); got != 0 {
		t.Fatalf("got %d recorded spans under NeverSample, want 0", got)
	}
	dp := singleDataPoint(t, collectDuration(t, reader))
	attrs := dpAttrs(dp)
	if got := attrs[semconv.DBCollectionNameKey].AsString(); got != "my-index" {
		t.Errorf("db.collection.name = %q, want my-index (labels do not depend on span recording)", got)
	}
	if len(dp.Exemplars) != 0 {
		t.Errorf("got %d exemplars for an unsampled span, want 0", len(dp.Exemplars))
	}
}

// TestMetric_NoopTracerStillRecords asserts the metric is independent of the
// tracing side entirely: a no-op TracerProvider (spans discarded) still yields
// the duration sample.
func TestMetric_NoopTracerStillRecords(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	mp, reader := recordingMeter()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, noop.NewTracerProvider(), mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := client.Search(client.Search.WithIndex("my-index"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()
	singleDataPoint(t, collectDuration(t, reader))
}

// TestMeterProviderWiring asserts the metric lands on the supplied
// MeterProvider, not the global one (ADR 0003).
func TestMeterProviderWiring(t *testing.T) {
	srv := esStub(t, http.StatusOK)

	globalMP, globalReader := recordingMeter()
	originalGlobal := otel.GetMeterProvider()
	otel.SetMeterProvider(globalMP)
	t.Cleanup(func() { otel.SetMeterProvider(originalGlobal) })

	tp, _ := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := client.Search(client.Search.WithContext(context.Background()))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	singleDataPoint(t, collectDuration(t, reader))
	var rm metricdata.ResourceMetrics
	if err := globalReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect global: %v", err)
	}
	if got := len(rm.ScopeMetrics); got != 0 {
		t.Errorf("global provider recorded %d scopes, want 0 (no global fallback)", got)
	}
}

// TestMetricViews_BoundLabelsAndBuckets pins the MetricViews contract: the
// SDK's bucket boundaries replace OTel's millisecond-scale defaults, and the
// allow-keys filter is the backstop that drops any key outside the documented
// label set.
func TestMetricViews_BoundLabelsAndBuckets(t *testing.T) {
	buckets := []float64{0.005, 0.05, 0.5, 5}
	srv := esStub(t, http.StatusOK)
	tp, _ := recordingProvider()
	mp, reader := recordingMeter(MetricViews(buckets)...)

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := client.Search(client.Search.WithContext(context.Background()), client.Search.WithIndex("my-index"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	_ = res.Body.Close()

	h := collectDuration(t, reader)
	dp := singleDataPoint(t, h)
	if got := dp.Bounds; len(got) != len(buckets) {
		t.Fatalf("bucket bounds = %v, want %v", got, buckets)
	}
	for i := range buckets {
		if dp.Bounds[i] != buckets[i] {
			t.Fatalf("bucket bounds = %v, want %v", dp.Bounds, buckets)
		}
	}
	allowed := map[attribute.Key]bool{
		semconv.DBSystemNameKey: true, semconv.DBOperationNameKey: true, semconv.DBCollectionNameKey: true,
		semconv.ServerAddressKey: true, semconv.ServerPortKey: true, semconv.ErrorTypeKey: true,
		semconv.DBResponseStatusCodeKey: true,
	}
	for _, kv := range dp.Attributes.ToSlice() {
		if !allowed[kv.Key] {
			t.Errorf("label %s escaped the allow-keys view", kv.Key)
		}
	}

	// A foreign scope's identically named instrument must not match the view.
	views := MetricViews(buckets)
	if _, matched := views[0](sdkmetric.Instrument{Name: "db.client.operation.duration"}); matched {
		t.Error("view matched an unscoped db.client.operation.duration; it must be scoped to this package")
	}
}

// TestMetric_TypedTransportErrorNotFmtWrapper: the typed client's Perform
// wraps a transport failure with fmt.Errorf("...: %w", err) before calling
// RecordError, so a naive type-name classifier would label every typed-client
// transport failure "*fmt.wrapError" — one useless bucket instead of the
// underlying failure class. The label must be the wrapped error's type.
func TestMetric_TypedTransportErrorNotFmtWrapper(t *testing.T) {
	tp, _ := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewTypedClient(elastic.Config{
		Addresses:    []string{"http://127.0.0.1:1"},
		MaxRetries:   0,
		DisableRetry: true,
	}, tp, mp)
	if err != nil {
		t.Fatalf("NewTypedClient: %v", err)
	}
	if _, err := client.Search().Index("my-index").Do(context.Background()); err == nil {
		t.Fatal("typed Search.Do against unroutable address: got nil error, want failure")
	}

	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	if got := attrs[semconv.ErrorTypeKey].AsString(); got != "*net.OpError" {
		t.Errorf("error.type = %q, want *net.OpError (the wrapped transport error), not a fmt wrapper", got)
	}
}

// TestMetric_RetryThenTransportErrorNoStaleStatus asserts that a 503 retried
// into a terminal transport error records the transport error class and no
// db.response.status_code — the stale 503 from the earlier attempt must not
// classify the caller's outcome (mirrors the span rule in ADR 0020 §4).
func TestMetric_RetryThenTransportErrorNoStaleStatus(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}))
	t.Cleanup(srv.Close)

	tp, _ := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Search(client.Search.WithContext(context.Background()), client.Search.WithIndex("my-index")); err == nil {
		t.Fatal("Search: want transport error after retries, got nil")
	}

	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	if got := attrs[semconv.ErrorTypeKey].AsString(); got == "" || got == "503" {
		t.Errorf("error.type = %q, want a transport error class, not the stale 503", got)
	}
	if v, ok := attrs[semconv.DBResponseStatusCodeKey]; ok {
		t.Errorf("db.response.status_code = %q, want absent (terminal outcome had no response)", v.AsString())
	}
}

// esStubWithBody is esStub with a caller-supplied JSON body, for typed-API
// endpoints that decode the response even on an accepted non-2xx status.
func esStubWithBody(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTyped_AcceptedStatusIsNotAFailure pins the typed-client failure contract
// (ADR 0027 §3, Codex review on PR #90): a status the generated endpoint accepts
// as a normal result — Get returns a 404 with a nil error and Found=false — is a
// success on both the span (status UNSET, http.response.status_code kept) and
// the metric (no error.type, no db.response.status_code). Only a response the
// endpoint surfaces as an ElasticsearchError counts as an HTTP failure.
func TestTyped_AcceptedStatusIsNotAFailure(t *testing.T) {
	srv := esStubWithBody(t, http.StatusNotFound, `{"_index":"my-index","_id":"1","found":false}`)
	tp, rec := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewTypedClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
	if err != nil {
		t.Fatalf("NewTypedClient: %v", err)
	}

	res, err := client.Get("my-index", "1").Do(context.Background())
	if err != nil {
		t.Fatalf("typed Get.Do on 404: got error %v, want nil (404 is an accepted status for Get)", err)
	}
	if res.Found {
		t.Fatal("typed Get.Do on 404: Found = true, want false")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if got := spans[0].Status().Code; got != codes.Unset {
		t.Errorf("span status = %v, want Unset (accepted 404 is not a failure)", got)
	}
	if got := attrMap(spans[0].Attributes())["http.response.status_code"].AsInt64(); got != http.StatusNotFound {
		t.Errorf("http.response.status_code = %d, want 404 (still recorded for context)", got)
	}

	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	if v, ok := attrs[semconv.ErrorTypeKey]; ok {
		t.Errorf("error.type = %q on an accepted 404; want absent", v.AsString())
	}
	if v, ok := attrs[semconv.DBResponseStatusCodeKey]; ok {
		t.Errorf("db.response.status_code = %q on an accepted 404; want absent", v.AsString())
	}
	if got := attrs[semconv.DBOperationNameKey].AsString(); got != "get" {
		t.Errorf("db.operation.name = %q, want get", got)
	}
}

// TestTyped_IsSuccessPathFollowsEndpointContract covers the typed API's
// IsSuccess terminator: Exists returns (false, nil) on 404 — a success — but
// reports any other non-2xx as an error through RecordError, which the metric
// classifies as a failure.
func TestTyped_IsSuccessPathFollowsEndpointContract(t *testing.T) {
	t.Run("404 accepted", func(t *testing.T) {
		srv := esStub(t, http.StatusNotFound)
		tp, rec := recordingProvider()
		mp, reader := recordingMeter()
		client, err := NewTypedClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
		if err != nil {
			t.Fatalf("NewTypedClient: %v", err)
		}
		exists, err := client.Exists("my-index", "1").IsSuccess(context.Background())
		if err != nil || exists {
			t.Fatalf("Exists.IsSuccess on 404 = (%v, %v), want (false, nil)", exists, err)
		}
		if got := rec.Ended()[0].Status().Code; got != codes.Unset {
			t.Errorf("span status = %v, want Unset", got)
		}
		attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
		if v, ok := attrs[semconv.ErrorTypeKey]; ok {
			t.Errorf("error.type = %q on an accepted 404; want absent", v.AsString())
		}
	})
	t.Run("500 rejected", func(t *testing.T) {
		srv := esStub(t, http.StatusInternalServerError)
		tp, rec := recordingProvider()
		mp, reader := recordingMeter()
		client, err := NewTypedClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp, mp)
		if err != nil {
			t.Fatalf("NewTypedClient: %v", err)
		}
		if _, err := client.Exists("my-index", "1").IsSuccess(context.Background()); err == nil {
			t.Fatal("Exists.IsSuccess on 500: want error, got nil")
		}
		// IsSuccess reports a rejected status through a plain generated error,
		// not an ElasticsearchError; the facade must still classify it by HTTP
		// status, exactly like a typed Do failure (Codex review on PR #90).
		attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
		if got := attrs[semconv.ErrorTypeKey].AsString(); got != "500" {
			t.Errorf("error.type = %q, want 500 (typed IsSuccess rejected status)", got)
		}
		if got := attrs[semconv.DBResponseStatusCodeKey].AsString(); got != "500" {
			t.Errorf("db.response.status_code = %q, want 500", got)
		}
		spans := rec.Ended()
		if len(spans) != 1 {
			t.Fatalf("got %d spans, want 1", len(spans))
		}
		if got := spans[0].Status().Code; got != codes.Error {
			t.Errorf("span status = %v, want Error", got)
		}
		if got := attrMap(spans[0].Attributes())["http.response.status_code"].AsInt64(); got != http.StatusInternalServerError {
			t.Errorf("http.response.status_code = %d, want 500", got)
		}
	})
	t.Run("retried 503 then transport error is not a status failure", func(t *testing.T) {
		// The IsSuccess message match must be paired with the terminal status:
		// a transport error after a retried 503 carries no such message, so
		// the stale 503 must not leak into error.type.
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if atomic.AddInt32(&calls, 1) == 1 {
				w.Header().Set("X-Elastic-Product", "Elasticsearch")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		}))
		t.Cleanup(srv.Close)
		tp, _ := recordingProvider()
		mp, reader := recordingMeter()
		client, err := NewTypedClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
		if err != nil {
			t.Fatalf("NewTypedClient: %v", err)
		}
		if _, err := client.Exists("my-index", "1").IsSuccess(context.Background()); err == nil {
			t.Fatal("Exists.IsSuccess: want transport error, got nil")
		}
		attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
		if got := attrs[semconv.ErrorTypeKey].AsString(); got == "" || got == "503" {
			t.Errorf("error.type = %q, want a transport error class, not the stale 503", got)
		}
		if v, ok := attrs[semconv.DBResponseStatusCodeKey]; ok {
			t.Errorf("db.response.status_code = %q, want absent", v.AsString())
		}
	})
}

// TestTypedStatusErrorPattern pins the generated IsSuccess error format the
// facade relies on (ADR 0006 compatibility pin) and the pairing rule.
func TestTypedStatusErrorPattern(t *testing.T) {
	gen := errors.New("an error happened during the Exists query execution, status code: 500")
	if !typedStatusError(gen, 500) {
		t.Error("generated IsSuccess error with matching terminal status must classify as a status error")
	}
	if typedStatusError(gen, 503) {
		t.Error("a status mismatch (stale earlier attempt) must not classify as a status error")
	}
	if typedStatusError(gen, 0) {
		t.Error("no terminal response must not classify as a status error")
	}
	if typedStatusError(errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), 503) {
		t.Error("a transport error must never classify as a status error")
	}
	if typedStatusError(errors.New("an error happened during the Search query execution: read: connection reset"), 500) {
		t.Error("the typed Perform transport wrapper (no status code) must not classify as a status error")
	}
}

// TestLowLevel_404FollowsIsError pins the low-level contract the typed rule is
// deliberately not applied to: the low-level API returns every response as
// (*Response, nil) and its own error test, esapi.Response.IsError, reports a
// 404 as an error, so the facade does too (ADR 0020 §4). The asymmetry with the
// typed client is documented, not accidental.
func TestLowLevel_404FollowsIsError(t *testing.T) {
	srv := esStub(t, http.StatusNotFound)
	tp, rec := recordingProvider()
	mp, reader := recordingMeter()
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, mp)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	res, err := client.Get("my-index", "1", client.Get.WithContext(context.Background()))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = res.Body.Close()
	if !res.IsError() {
		t.Fatal("esapi.Response.IsError() = false on 404; the low-level contract changed")
	}
	if got := rec.Ended()[0].Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want Error (mirrors IsError)", got)
	}
	attrs := dpAttrs(singleDataPoint(t, collectDuration(t, reader)))
	if got := attrs[semconv.ErrorTypeKey].AsString(); got != "404" {
		t.Errorf("error.type = %q, want 404", got)
	}
}
