package elasticsearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/elastic/elastic-transport-go/v8/elastictransport"
	elastic "github.com/elastic/go-elasticsearch/v8"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

// Option configures Elasticsearch instrumentation behavior.
type Option func(*config)

type config struct {
	captureSearchBody bool
}

// WithSearchBody controls whether search query bodies are captured on spans
// (as the upstream db.statement attribute) for the search-family endpoints the
// first-party instrumentation supports. Default: false.
//
// Search bodies can be large and may carry user-supplied terms (PII), so they
// are captured only on explicit opt-in — consistent with
// redis.WithCommandTextEnabled and the ADR 0019 cassandra.WithQueryText
// posture (ADR 0020 §5).
//
// This governs only the request body. The span's url.full attribute (always
// emitted by the upstream) includes the URL query string, so a query-string
// search (client.Search.WithQuery("...")) records its terms regardless of this
// option; use the body DSL for sensitive search terms.
func WithSearchBody(enabled bool) Option {
	return func(cfg *config) {
		cfg.captureSearchBody = enabled
	}
}

func newConfig(opts []Option) config {
	cfg := config{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// NewClient builds an instrumented low-level Elasticsearch client.
//
// cfg is the caller's elasticsearch.Config (addresses, TLS, auth, retry); the
// returned value is the standard *elasticsearch.Client, so existing call sites
// and the low-level API are unchanged. The first-party OpenTelemetry
// instrumentation is wired into the transport before the client is built, so
// every request — including the first — is traced.
//
// tp is required and rejected when nil (ADR 0003 — the facade never falls back
// to the global TracerProvider). This signature deliberately diverges from the
// SDK's usual (ctx, …, tp, mp, prop) shape: the upstream instrumentation
// accepts only a TracerProvider, the integration is trace-only (ADR 0020 §6),
// and it does not propagate trace context toward Elasticsearch (§3).
//
// Any cfg.Instrumentation set by the caller is replaced by the o11y-wired
// instrumentation. cfg is taken by value, so the caller's own Config struct is
// not mutated.
func NewClient(cfg elastic.Config, tp trace.TracerProvider, opts ...Option) (*elastic.Client, error) {
	if err := instrument(&cfg, tp, opts...); err != nil {
		return nil, fmt.Errorf("elasticsearch new client: %w", err)
	}
	client, err := elastic.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch new client: %w", err)
	}
	return client, nil
}

// NewTypedClient builds an instrumented typed Elasticsearch client.
//
// go-elasticsearch v8 keeps the typed client behind a separate constructor that
// is not reachable from *elasticsearch.Client, so this parallel constructor
// lets typed-API call sites stay on the typed API while sharing the same
// instrumentation wiring as NewClient. tp is required and rejected when nil.
//
// Call typed requests with their .Do(ctx) terminator (e.g.
// client.Search().Index("idx").Do(ctx)) to get a fully populated span. The
// lower-level .Perform(ctx) escape hatch is NOT fully instrumented: in
// go-elasticsearch v8.19.3 typed Perform starts the span on a shadowed local
// context and then builds the request with the original context, so path
// parts, request attributes, and error status are not recorded on the span.
// This is an upstream quirk a T2 facade cannot patch; use .Do(ctx).
func NewTypedClient(cfg elastic.Config, tp trace.TracerProvider, opts ...Option) (*elastic.TypedClient, error) {
	if err := instrument(&cfg, tp, opts...); err != nil {
		return nil, fmt.Errorf("elasticsearch new typed client: %w", err)
	}
	client, err := elastic.NewTypedClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch new typed client: %w", err)
	}
	return client, nil
}

// instrument validates tp and installs the client's first-party OTel
// instrumentation on cfg with the SDK TracerProvider wired in.
//
// elastic.NewOpenTelemetryInstrumentation falls back to the OTel global
// TracerProvider only when its provider argument is nil (verified against
// elastic-transport-go/v8 v8.8.0 elastictransport/instrumentation.go:
// NewOtelInstrumentation). Because tp is rejected when nil, the fallback never
// fires and no otel.SetX call is on this path (ADR 0003).
func instrument(cfg *elastic.Config, tp trace.TracerProvider, opts ...Option) error {
	if tp == nil {
		return errors.New("tracer provider must not be nil")
	}
	c := newConfig(opts)
	cfg.Instrumentation = instrumentation{
		Instrumentation: elastic.NewOpenTelemetryInstrumentation(tp, c.captureSearchBody),
	}
	return nil
}

// instrumentation wraps the client's first-party OTel instrumentation with two
// thin, SDK-owned behaviors that the bare upstream lacks (ADR 0020 §4):
//
//   - RecordRequestBody is made a no-op on a nil body. With WithSearchBody(true)
//     the pinned elastic-transport-go/v8 v8.8.0 calls bytes.Buffer.ReadFrom(query)
//     unconditionally for search-family endpoints, and the generated API passes
//     a nil body for bodyless searches (e.g. a query-string-only search), so
//     ReadFrom on a nil reader would panic.
//   - AfterResponse records http.response.status_code and sets the span status
//     to Error for HTTP error responses. The bare upstream returns (*Response,
//     nil) for 4xx/5xx and only RecordError-s transport failures, so without
//     this an ES-rejected request (a bad query, a 429, a 500) would leave the
//     span status UNSET and carry no status code.
type instrumentation struct {
	elastictransport.Instrumentation
}

func (g instrumentation) RecordRequestBody(ctx context.Context, endpoint string, query io.Reader) io.ReadCloser {
	if query == nil {
		return nil
	}
	return g.Instrumentation.RecordRequestBody(ctx, endpoint, query)
}

// AfterResponse runs after every HTTP attempt (the transport calls it once per
// try, including retries). It surfaces the response status code and reflects
// HTTP error responses in the span status.
//
// Retry handling: only retryable error statuses (>= 400) are retried, so the
// only path that sets Ok is the terminal successful attempt, which overrides an
// Error left by an earlier retried attempt (the OTel SDK permits Error -> Ok but
// not the reverse). A request that exhausts retries on errors ends Error;
// http.response.status_code reflects the final attempt.
func (g instrumentation) AfterResponse(ctx context.Context, res *http.Response) {
	g.Instrumentation.AfterResponse(ctx, res)
	if res == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(semconv.HTTPResponseStatusCode(res.StatusCode))
	if res.StatusCode >= 400 {
		span.SetStatus(codes.Error, http.StatusText(res.StatusCode))
	} else {
		span.SetStatus(codes.Ok, "")
	}
}
