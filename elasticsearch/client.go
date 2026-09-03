package elasticsearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/elastic/elastic-transport-go/v8/elastictransport"
	elastic "github.com/elastic/go-elasticsearch/v8"
	estypes "github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName is the instrumentation scope the SDK-owned Elasticsearch
// metrics are recorded under (ADR 0027 §5). Spans keep the upstream tracer
// scope ("elasticsearch-api"): the first-party instrumentation owns them and the
// facade only wraps its callbacks.
const instrumentationName = "github.com/flywindy/o11y/elasticsearch"

// Option configures Elasticsearch instrumentation behavior.
type Option func(*config)

type config struct {
	captureSearchBody bool
	// collectionMetricLabelDisabled is stored negated because the label is on by
	// default: the zero-value config then means "shipped defaults", so a config
	// literal built in a test cannot silently disagree with what NewClient does.
	collectionMetricLabelDisabled bool
}

// collectionMetricLabel reports whether db.collection.name is recorded as a
// metric label. See WithCollectionMetricLabel.
func (c config) collectionMetricLabel() bool { return !c.collectionMetricLabelDisabled }

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

// WithCollectionMetricLabel controls whether db.collection.name (the addressed
// index) is recorded as a label on db.client.operation.duration.
//
// The default is true (ADR 0027 §4). semconv v1.39.0 marks db.collection.name
// Conditionally Required on db.client.operation.duration "if readily available
// and if a database call is performed on a single collection". The index is
// already captured for the span (as the upstream path-part attribute), and the
// label is emitted only when the request addresses exactly one index: it is
// omitted for requests with no index path part (a cross-index _search,
// cluster.health, a bulk whose index is set per action line) and for a
// comma-separated multi-index list, which is the semconv condition failing
// rather than a cardinality guard. A wildcard or alias (logs-*) is one addressed
// name and is kept as-is.
//
// Pass false to keep the index off the metric entirely, e.g. for services whose
// index names roll by date and would otherwise mint a series per day.
//
// Bounding: MetricViews' allow-keys filter governs which keys reach the series,
// and o11y.WithMaxUniqueCollections collapses index values beyond its cap to
// "other". The cap lives in the SDK's export pipeline, so it applies only to a
// MeterProvider built by o11y.Init. A caller passing their own MeterProvider to
// NewClient gets the view (it travels with MetricViews) but not the cap, and
// should register an equivalent cap or a cardinality limit on that provider.
func WithCollectionMetricLabel(enabled bool) Option {
	return func(cfg *config) {
		cfg.collectionMetricLabelDisabled = !enabled
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
// including the first, and records one db.client.operation.duration sample
// (ADR 0027).
//
// One upstream caveat: the generated API methods copy the instrumentation onto
// the request, but the bare low-level request structs do not. Constructing a
// request struct directly and calling it — esapi.SearchRequest{...}.Do(ctx,
// client) — leaves its Instrument field nil, so that path emits no span and no
// metric. Use the client helper methods (which is the idiomatic low-level API)
// to get telemetry.
//
// tp and mp are required and rejected when nil (ADR 0003 — the facade never
// falls back to the global providers). Spans come from the upstream
// instrumentation, which accepts only a TracerProvider; the operation-duration
// metric is SDK-owned and recorded on mp. There is no propagator parameter: the
// upstream does not propagate trace context toward Elasticsearch (ADR 0020 §3).
//
// Any cfg.Instrumentation set by the caller is replaced by the o11y-wired
// instrumentation. cfg is taken by value, so the caller's own Config struct is
// not mutated.
func NewClient(
	cfg elastic.Config,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	opts ...Option,
) (*elastic.Client, error) {
	if err := instrument(&cfg, tp, mp, opts...); err != nil {
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
// instrumentation wiring as NewClient. tp and mp are required and rejected when
// nil.
//
// Call typed requests with their .Do(ctx) terminator (e.g.
// client.Search().Index("idx").Do(ctx)) to get a fully populated span and
// metric sample. The lower-level .Perform(ctx) escape hatch is NOT fully
// instrumented: in go-elasticsearch v8.19.3 typed Perform starts the span on a
// shadowed local context and then builds the request with the original
// context, so path parts, request attributes, error status, and the metric's
// index/server/error labels are not recorded. This is an upstream quirk a T2
// facade cannot patch; use .Do(ctx).
func NewTypedClient(
	cfg elastic.Config,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	opts ...Option,
) (*elastic.TypedClient, error) {
	if err := instrument(&cfg, tp, mp, opts...); err != nil {
		return nil, fmt.Errorf("elasticsearch new typed client: %w", err)
	}
	client, err := elastic.NewTypedClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch new typed client: %w", err)
	}
	return client, nil
}

// instrument validates the providers and installs the client's first-party OTel
// instrumentation on cfg with the SDK TracerProvider wired in, wrapped with the
// SDK-owned metric recorder built on the SDK MeterProvider.
//
// elastic.NewOpenTelemetryInstrumentation falls back to the OTel global
// TracerProvider only when its provider argument is nil (verified against
// elastic-transport-go/v8 v8.8.0 elastictransport/instrumentation.go:
// NewOtelInstrumentation). Because tp is rejected when nil, the fallback never
// fires and no otel.SetX call is on this path (ADR 0003). The meter is created
// from mp directly; no global MeterProvider is consulted on any path.
func instrument(cfg *elastic.Config, tp trace.TracerProvider, mp metric.MeterProvider, opts ...Option) error {
	if tp == nil {
		return errors.New("tracer provider must not be nil")
	}
	if mp == nil {
		return errors.New("meter provider must not be nil")
	}
	c := newConfig(opts)
	inst, err := newInstruments(mp.Meter(instrumentationName, metric.WithSchemaURL(semconv.SchemaURL)))
	if err != nil {
		return err
	}
	cfg.Instrumentation = instrumentation{
		Instrumentation: elastic.NewOpenTelemetryInstrumentation(tp, c.captureSearchBody),
		inst:            inst,
		cfg:             c,
	}
	return nil
}

// instrumentation wraps the client's first-party OTel instrumentation with
// thin, SDK-owned behaviors that the bare upstream lacks (ADR 0020 §4,
// ADR 0027):
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
//   - Close records one db.client.operation.duration sample per request
//     (Start → Close, so it spans retries and the product check like the span
//     does) with the bounded label set built from what the callbacks observed
//     (ADR 0027 §3). The metric is recorded whether or not the span is sampled.
type instrumentation struct {
	elastictransport.Instrumentation
	inst *instruments
	cfg  config
}

// responseStateKey carries a per-request *responseState through the request
// context from Start to AfterResponse and Close.
type responseStateKey struct{}

// esSystem is the db.system.name value for Elasticsearch; it prefixes the span
// name per the cross-package convention {system.name}.{operation} {target}
// (ADR 0023).
const esSystem = "elasticsearch"

// serverAddr is the server.address / server.port pair observed on a request.
type serverAddr struct {
	host string
	port int
}

// responseState carries per-request data between the instrumentation callbacks
// (all of which receive the request context). start is the Start timestamp the
// duration metric is measured from. operation is the endpoint id from Start,
// used by RecordPathPart to build the span name and as db.operation.name.
// index is the index path part (db.collection.name when it names a single
// index). server is the host the terminal attempt was routed to, read from the
// request URL in AfterRequest. statusCode is the last code seen by
// AfterResponse, and errored records whether RecordError fired for a terminal
// failure that did not return a usable ES response, with err the error it
// reported (classified into error.type on the metric).
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
	start      time.Time
	operation  string
	index      string
	server     serverAddr
	statusCode int
	errored    bool
	err        error
}

func stateFromContext(ctx context.Context) (*responseState, bool) {
	if ctx == nil {
		return nil, false
	}
	st, ok := ctx.Value(responseStateKey{}).(*responseState)
	return st, ok
}

// Start prefixes the upstream span name (the bare endpoint id, e.g. "search")
// with the system name so it reads "elasticsearch.search", per ADR 0023. The
// target (index) is appended later by RecordPathPart, once it is known. It also
// stamps the start time the duration metric is measured from.
func (g instrumentation) Start(ctx context.Context, name string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = g.Instrumentation.Start(ctx, esSystem+"."+name)
	return context.WithValue(ctx, responseStateKey{}, &responseState{
		start:     time.Now(),
		operation: name,
	})
}

// RecordPathPart appends the index to the span name as the {target} component
// (e.g. "elasticsearch.search my-index"), matching the cross-package convention
// (ADR 0023). Endpoints without an index path part keep the bare
// "elasticsearch.{operation}" name (target omitted), like mongodb.ping. The
// index is also kept for the metric's db.collection.name label.
func (g instrumentation) RecordPathPart(ctx context.Context, pathPart, value string) {
	g.Instrumentation.RecordPathPart(ctx, pathPart, value)
	if pathPart != "index" || value == "" {
		return
	}
	st, ok := stateFromContext(ctx)
	if !ok || st.operation == "" {
		return
	}
	st.index = value
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

// AfterRequest runs once per request after the transport returns. The
// transport rewrites req.URL's scheme and host in place on every attempt with
// the connection it selected, so at this point the URL names the node the
// terminal attempt was routed to; the upstream reads server.address from it for
// the span and the facade keeps it for the metric. A request that never reached
// the transport's connection selection (no live node) leaves the host empty
// and the metric carries no server.* labels.
func (g instrumentation) AfterRequest(req *http.Request, system, endpoint string) {
	g.Instrumentation.AfterRequest(req, system, endpoint)
	if req == nil || req.URL == nil {
		return
	}
	st, ok := stateFromContext(req.Context())
	if !ok {
		return
	}
	st.server.host = req.URL.Hostname()
	if port, err := strconv.Atoi(req.URL.Port()); err == nil && port > 0 {
		st.server.port = port
	}
}

// AfterResponse runs after every HTTP attempt (once per try, including retries).
// It only stashes the status code; the status attribute and Error decision are
// made in Close so they reflect the terminal outcome (see responseState).
func (g instrumentation) AfterResponse(ctx context.Context, res *http.Response) {
	g.Instrumentation.AfterResponse(ctx, res)
	if res == nil {
		return
	}
	if st, ok := stateFromContext(ctx); ok {
		st.statusCode = res.StatusCode
	}
}

// RecordError fires when the request ends on an error (transport failure,
// context cancellation, or product-check failure). The upstream sets the span
// status to Error; we note it so Close does not also emit a possibly-stale
// status code from an earlier retried attempt, and keep the error so the metric
// can classify it into error.type.
func (g instrumentation) RecordError(ctx context.Context, err error) {
	g.Instrumentation.RecordError(ctx, err)
	if isTypedResponseError(err) {
		return
	}
	if st, ok := stateFromContext(ctx); ok {
		st.errored = true
		st.err = err
	}
}

func isTypedResponseError(err error) bool {
	var esErr *estypes.ElasticsearchError
	return errors.As(err, &esErr)
}

// Close runs once per request (deferred by the generated API, after any
// RecordError) and ends the span. Before that it records the operation-duration
// sample and reflects an ES HTTP error response on the span — these are
// returned as (*Response, nil) by the low-level API and the bare upstream
// leaves them UNSET.
//
// If RecordError already fired, the request ended on an error that owns the
// status; the stashed code may be from an earlier retried attempt, so we touch
// neither status nor attribute. Otherwise the request returned a response, so we
// record http.response.status_code and set status = Error when it is > 299 —
// mirroring the client's own esapi.Response.IsError, so redirects/proxy errors
// (3xx) and 4xx/5xx are flagged while a retried 5xx→2xx stays successful (UNSET).
// The metric's error.type follows the same decision (see metricErrorType).
func (g instrumentation) Close(ctx context.Context) {
	if st, ok := stateFromContext(ctx); ok {
		g.recordDuration(ctx, st)
		if !st.errored && st.statusCode != 0 {
			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.SetAttributes(semconv.HTTPResponseStatusCode(st.statusCode))
				if st.statusCode > 299 {
					span.SetStatus(codes.Error, http.StatusText(st.statusCode))
				}
			}
		}
	}
	g.Instrumentation.Close(ctx)
}

// recordDuration records the db.client.operation.duration sample for a request.
// It records against the request context — which carries this operation's
// CLIENT span — so the sample's exemplar references the ES span rather than the
// caller's parent span. The metric does not depend on the span being sampled:
// the state travels in the context regardless of the span's recording state.
func (g instrumentation) recordDuration(ctx context.Context, st *responseState) {
	if g.inst == nil || st.start.IsZero() {
		return
	}
	g.inst.operationDuration.Record(ctx, time.Since(st.start).Seconds(),
		metric.WithAttributes(g.metricAttrs(st)...))
}

// metricAttrs builds the bounded label set for db.client.operation.duration
// (ADR 0027 §3). Every key is the current semconv v1.39.0 spelling — the metric
// is SDK-owned, so it does not inherit the legacy keys the upstream span emits.
// The MetricViews allow-keys filter is the backstop.
func (g instrumentation) metricAttrs(st *responseState) []attribute.KeyValue {
	attrs := []attribute.KeyValue{semconv.DBSystemNameElasticsearch}
	if st.operation != "" {
		attrs = append(attrs, semconv.DBOperationName(st.operation))
	}
	if index := singleIndex(st.index); index != "" && g.cfg.collectionMetricLabel() {
		attrs = append(attrs, semconv.DBCollectionName(index))
	}
	if st.server.host != "" {
		attrs = append(attrs, semconv.ServerAddress(st.server.host))
		if st.server.port > 0 {
			attrs = append(attrs, semconv.ServerPort(st.server.port))
		}
	}
	if errType := metricErrorType(st); errType != "" {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errType))
	}
	return attrs
}

// singleIndex returns the index when the request addresses exactly one, and ""
// otherwise. The generated API joins a multi-index request with commas, which is
// the semconv "single collection" condition failing; a wildcard or alias is
// still one addressed name.
func singleIndex(index string) string {
	if index == "" || strings.Contains(index, ",") {
		return ""
	}
	return index
}

// metricErrorType classifies a request's terminal outcome for error.type, or
// returns "" for success (semconv: error.type is set only on failure).
//
//   - A terminal error reported by RecordError (transport failure, context
//     cancellation, product-check failure) is classified like the SDK's other
//     integrations: the stable context sentinels, else the Go error type.
//   - An ES HTTP error response — including a typed-API ElasticsearchError,
//     which still has a final response — is classified by its status code as a
//     string ("429", "500"), the HTTP client semconv's error.type convention,
//     with the same > 299 boundary the span status uses.
func metricErrorType(st *responseState) string {
	switch {
	case st.errored:
		return errorType(st.err)
	case st.statusCode > 299:
		return strconv.Itoa(st.statusCode)
	}
	return ""
}

// errorType classifies a terminal error for the error.type attribute,
// preferring the stable context sentinels over the concrete Go type name
// (same rule as the cassandra and redis packages).
func errorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	default:
		if typ := reflect.TypeOf(err); typ != nil {
			return typ.String()
		}
		return "_OTHER"
	}
}
