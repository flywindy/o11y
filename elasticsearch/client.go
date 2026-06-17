package elasticsearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/elastic/elastic-transport-go/v8/elastictransport"
	elastic "github.com/elastic/go-elasticsearch/v8"
	estypes "github.com/elastic/go-elasticsearch/v8/typedapi/types"
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
// every request issued through the client's API methods — client.Search(...),
// client.Index(...), the typed client's .Do(ctx), and so on — is traced,
// including the first.
//
// One upstream caveat: the generated API methods copy the instrumentation onto
// the request, but the bare low-level request structs do not. Constructing a
// request struct directly and calling it — esapi.SearchRequest{...}.Do(ctx,
// client) — leaves its Instrument field nil, so that path emits no span. Use the
// client helper methods (which is the idiomatic low-level API) to get tracing.
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

// instrumentation wraps the client's first-party OTel instrumentation with
// thin, SDK-owned behaviors that the bare upstream lacks (ADR 0020 §4):
//
//   - RecordRequestBody is made a no-op on a nil body. With WithSearchBody(true)
//     the pinned elastic-transport-go/v8 v8.8.0 calls bytes.Buffer.ReadFrom(query)
//     unconditionally for search-family endpoints, and the generated API passes
//     a nil body for bodyless searches (e.g. a query-string-only search), so
//     ReadFrom on a nil reader would panic.
//   - Close records http.response.status_code and reflects an ES HTTP error
//     response in the span status. The bare upstream returns (*Response, nil)
//     for 4xx/5xx and only RecordError-s transport failures, so without this an
//     ES-rejected request (a bad query, a 429, a 500) would leave the span
//     status UNSET and carry no status code.
//   - Start and RecordPathPart rewrite the span name from the bare endpoint id
//     ("search") to the cross-package {system.name}.{operation} {target} form
//     ("elasticsearch.search my-index"), per ADR 0023.
type instrumentation struct {
	elastictransport.Instrumentation
}

// responseStateKey carries a per-request *responseState through the request
// context from Start to AfterResponse and Close.
type responseStateKey struct{}

// esSystem is the db.system.name value for Elasticsearch; it prefixes the span
// name per the cross-package convention {system.name}.{operation} {target}
// (ADR 0023).
const esSystem = "elasticsearch"

// responseState carries per-request data between the instrumentation callbacks
// (all of which receive the request context). operation is the endpoint id from
// Start, used by RecordPathPart to build the span name. statusCode is the last
// code seen by AfterResponse, and errored records whether RecordError fired for
// a terminal failure that did not return a usable ES response.
//
// The HTTP status (attribute + Error decision) is settled in Close, not per
// attempt: AfterResponse runs once per attempt and cannot see the terminal
// outcome, and a per-attempt SetStatus would be final under the OTel SDK and
// could mask a transport/product-check error that RecordError reports only after
// the response is in hand. When RecordError fired for a terminal failure (a
// transport failure, a context cancellation, or a product-check failure),
// statusCode may be a stale code from an earlier retried attempt rather than the
// caller's outcome; Close then defers entirely to RecordError's Error status and
// emits no (possibly stale) status code. Typed API response errors are different:
// they run RecordError after decoding the ES error body, but still have a final
// HTTP response, so Close keeps the status-code attribute for dashboards.
type responseState struct {
	operation  string
	statusCode int
	errored    bool
}

// Start prefixes the upstream span name (the bare endpoint id, e.g. "search")
// with the system name so it reads "elasticsearch.search", per ADR 0023. The
// target (index) is appended later by RecordPathPart, once it is known.
func (g instrumentation) Start(ctx context.Context, name string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = g.Instrumentation.Start(ctx, esSystem+"."+name)
	return context.WithValue(ctx, responseStateKey{}, &responseState{operation: name})
}

// RecordPathPart appends the index to the span name as the {target} component
// (e.g. "elasticsearch.search my-index"), matching the cross-package convention
// (ADR 0023). Endpoints without an index path part keep the bare
// "elasticsearch.{operation}" name (target omitted), like mongodb.ping.
func (g instrumentation) RecordPathPart(ctx context.Context, pathPart, value string) {
	g.Instrumentation.RecordPathPart(ctx, pathPart, value)
	if pathPart != "index" || value == "" {
		return
	}
	st, ok := ctx.Value(responseStateKey{}).(*responseState)
	if !ok || st.operation == "" {
		return
	}
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetName(esSystem + "." + st.operation + " " + value)
	}
}

func (g instrumentation) RecordRequestBody(ctx context.Context, endpoint string, query io.Reader) io.ReadCloser {
	if query == nil {
		return nil
	}
	return g.Instrumentation.RecordRequestBody(ctx, endpoint, query)
}

// AfterResponse runs after every HTTP attempt (once per try, including retries).
// It only stashes the status code; the status attribute and Error decision are
// made in Close so they reflect the terminal outcome (see responseState).
func (g instrumentation) AfterResponse(ctx context.Context, res *http.Response) {
	g.Instrumentation.AfterResponse(ctx, res)
	if res == nil {
		return
	}
	if st, ok := ctx.Value(responseStateKey{}).(*responseState); ok {
		st.statusCode = res.StatusCode
	}
}

// RecordError fires when the request ends on an error (transport failure,
// context cancellation, or product-check failure). The upstream sets the span
// status to Error; we note it so Close does not also emit a possibly-stale
// status code from an earlier retried attempt.
func (g instrumentation) RecordError(ctx context.Context, err error) {
	g.Instrumentation.RecordError(ctx, err)
	if isTypedResponseError(err) {
		return
	}
	if st, ok := ctx.Value(responseStateKey{}).(*responseState); ok {
		st.errored = true
	}
}

func isTypedResponseError(err error) bool {
	var esErr *estypes.ElasticsearchError
	return errors.As(err, &esErr)
}

// Close runs once per request (deferred by the generated API, after any
// RecordError) and ends the span. Before that it reflects an ES HTTP error
// response on the span — these are returned as (*Response, nil) by the low-level
// API and the bare upstream leaves them UNSET.
//
// If RecordError already fired, the request ended on an error that owns the
// status; the stashed code may be from an earlier retried attempt, so we touch
// neither status nor attribute. Otherwise the request returned a response, so we
// record http.response.status_code and set status = Error when it is > 299 —
// mirroring the client's own esapi.Response.IsError, so redirects/proxy errors
// (3xx) and 4xx/5xx are flagged while a retried 5xx→2xx stays successful (UNSET).
func (g instrumentation) Close(ctx context.Context) {
	if st, ok := ctx.Value(responseStateKey{}).(*responseState); ok && !st.errored && st.statusCode != 0 {
		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.SetAttributes(semconv.HTTPResponseStatusCode(st.statusCode))
			if st.statusCode > 299 {
				span.SetStatus(codes.Error, http.StatusText(st.statusCode))
			}
		}
	}
	g.Instrumentation.Close(ctx)
}
