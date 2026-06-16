package elasticsearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	elastic "github.com/elastic/go-elasticsearch/v8"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
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

func TestNewClient_NilTracerProvider(t *testing.T) {
	if _, err := NewClient(elastic.Config{}, nil); err == nil ||
		!strings.Contains(err.Error(), "tracer provider must not be nil") {
		t.Fatalf("NewClient(nil tp): got %v, want tracer-provider error", err)
	}
	if _, err := NewTypedClient(elastic.Config{}, nil); err == nil ||
		!strings.Contains(err.Error(), "tracer provider must not be nil") {
		t.Fatalf("NewTypedClient(nil tp): got %v, want tracer-provider error", err)
	}
}

// TestSearch_EmitsClientSpan exercises the index-addressing search endpoint and
// asserts the span the pinned upstream emits, including the exact (legacy)
// attribute keys recorded in ADR 0020 §4 and docs/semconv.md.
func TestSearch_EmitsClientSpan(t *testing.T) {
	srv := esStub(t, http.StatusOK)
	tp, rec := recordingProvider()

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp)
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
		client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, opts...)
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

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp, WithSearchBody(true))
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

	client, err := NewTypedClient(elastic.Config{Addresses: []string{srv.URL}}, tp)
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
	}, tp)
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
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp)
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

// TestHTTPError_SetsErrorStatus asserts the facade reflects an ES HTTP error
// response (which the low-level API returns as (*Response, nil)) on the span:
// status = Error plus http.response.status_code. The bare upstream does neither
// (ADR 0020 §4).
func TestHTTPError_SetsErrorStatus(t *testing.T) {
	srv := esStub(t, http.StatusInternalServerError)
	tp, rec := recordingProvider()

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp)
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
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp)
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

	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}}, tp)
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
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp)
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
	client, err := NewClient(elastic.Config{Addresses: []string{srv.URL}, DisableRetry: true}, tp)
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
