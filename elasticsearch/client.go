package elasticsearch

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/elastic/elastic-transport-go/v8/elastictransport"
	elastic "github.com/elastic/go-elasticsearch/v8"
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
	cfg.Instrumentation = nilBodyGuard{
		Instrumentation: elastic.NewOpenTelemetryInstrumentation(tp, c.captureSearchBody),
	}
	return nil
}

// nilBodyGuard wraps the client's first-party instrumentation to make
// RecordRequestBody a no-op when the request carries no body.
//
// With WithSearchBody(true), the pinned elastic-transport-go/v8 v8.8.0 calls
// bytes.Buffer.ReadFrom(query) unconditionally for search-family endpoints
// (instrumentation.go RecordRequestBody). The generated API passes a nil body
// for bodyless searches (e.g. client.Search() with only query-string params),
// and ReadFrom on a nil reader panics. Guarding here keeps WithSearchBody(true)
// safe for every search call; it changes no attribute the upstream emits.
type nilBodyGuard struct {
	elastictransport.Instrumentation
}

func (g nilBodyGuard) RecordRequestBody(ctx context.Context, endpoint string, query io.Reader) io.ReadCloser {
	if query == nil {
		return nil
	}
	return g.Instrumentation.RecordRequestBody(ctx, endpoint, query)
}
