package elasticsearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
