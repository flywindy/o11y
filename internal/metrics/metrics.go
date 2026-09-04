// Package metrics encapsulates the OTel MeterProvider used by the top-level
// o11y SDK. It supports two exporter strategies selected at init time:
//
//   - Prometheus pull (default): a private registry + HTTP server on
//     cfg.MetricsAddr exposes /metrics for Prometheus scraping.
//   - OTLP push: when cfg.MetricsOTLPEndpoint is set, metrics are exported
//     via OTLP/HTTP and no HTTP server is started. Use this for serverless
//     environments where exposing a scrape port is not possible.
//
// Neither strategy touches global OTel or Prometheus state.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/flywindy/o11y/internal/metricscap"
	"github.com/flywindy/o11y/internal/views"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	otlpmetrichttp "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// Config is the subset of SDK configuration that the metrics subsystem needs.
//
// When Resource is non-nil, InitMeter uses it directly and skips building its
// own. All service-identity attributes (service.name, service.version,
// deployment.environment.name, service.namespace) must already be present
// in the provided Resource; they are not validated separately.
//
// When MetricsOTLPEndpoint is non-empty, the Prometheus pull model is bypassed
// entirely: an OTLP/HTTP exporter is used instead and MetricsAddr is ignored.
type Config struct {
	// Resource is an optional pre-built OTel Resource shared with the
	// TracerProvider and LoggerProvider. When set, the standalone
	// ServiceName/ServiceVersion/Environment/Namespace fields are ignored for
	// Resource construction (but Namespace is still used for validation when nil).
	Resource *resource.Resource

	// MetricsOTLPEndpoint, when non-empty, switches the exporter to OTLP push.
	// No Prometheus HTTP server is started. Intended for serverless environments.
	// Example: "http://otel-collector:4318"
	MetricsOTLPEndpoint string

	// OTLPHeaders, when non-empty, are attached to every OTLP/HTTP request
	// emitted by the metrics exporter (used for authentication against
	// managed observability backends). Ignored on the Prometheus pull path.
	OTLPHeaders map[string]string

	ServiceName    string
	ServiceVersion string
	Environment    string
	// Namespace maps to service.namespace (OTel semconv). Required when
	// Resource is nil. It becomes a constant Prometheus label
	// (service_namespace="...") on every series for alert routing and
	// ownership governance.
	Namespace string

	MetricsAddr         string
	RuntimeMetrics      bool
	HistogramBuckets    []float64
	DisableDefaultViews bool
	ExtraViews          []sdkmetric.View
	MaxUniqueRoutes     int

	// MaxUniqueCollections caps distinct db.collection.name values on the
	// Cassandra and Elasticsearch client metrics at the export boundary,
	// collapsing the overflow to "other", with an independent budget per
	// integration (see collectionCapScopes). For Cassandra, whose schema is
	// DDL-fixed and small, this guards against the SDK's CQL tokenizer
	// mis-reading a statement shape; for Elasticsearch it bounds date-rolled
	// index names.
	MaxUniqueCollections int

	// ExtraHTTPServerAttrKeys augments the SDK-managed attribute allow-list
	// for the http.server.request.duration view. Promoting caller-controlled
	// keys onto the exported series (rather than letting them fall through to
	// the exemplar) keeps PromQL aggregations meaningful and avoids the
	// OpenMetrics 128-rune exemplar-label cap. Cardinality is the caller's
	// responsibility.
	ExtraHTTPServerAttrKeys []string

	// Exemplars controls whether the Prometheus pull handler enables
	// OpenMetrics content negotiation, which is the only path through which
	// otelprom emits per-bucket exemplars. Set false to suppress the
	// `le="1"` → `le="1.0"` bucket-boundary format change that OpenMetrics
	// introduces, at the cost of disabling trace-to-metric linkage.
	Exemplars bool
}

// Closer is a function that shuts down a component. For the Prometheus path it
// shuts down the HTTP server; for the OTLP path it shuts down the exporter.
// It is always safe to call even if the component was never started.
type Closer func(context.Context) error

// The SDK cardinality limit is an in-process memory guard, not the exported
// route presentation cap. Derive it from the bounded HTTP keyspace so
// WithMaxUniqueRoutes(n) can preserve route detail across normal method/status
// combinations before the SDK overflow guard intentionally drops labels.
const (
	sdkCardinalityMethodBudget = 16
	sdkCardinalityStatusBudget = 64

	// sdkCardinalityCollectionBudget is the per-collection envelope for the
	// Cassandra and Elasticsearch query metrics: db.operation.name ×
	// server.address/port × error.type (× db.response.status_code for ES, which
	// shares error.type's values), the other bounded dimensions a
	// db.collection.name series is multiplied by. Elasticsearch has more endpoint
	// ids than Cassandra has statement verbs, but a service exercises a handful
	// per index; the route budget dominates at the shipped defaults anyway.
	sdkCardinalityCollectionBudget = 128
)

// capInstrument pairs an instrument name with the Prometheus family name it is
// exposed as, so the OTLP and Prometheus cap rules key on the same instrument.
type capInstrument struct {
	instrument string
	family     string
}

// collectionCapScope is one SDK integration whose client metrics carry
// db.collection.name, with the instrumentation scope it records under and the
// budget its instruments share.
type collectionCapScope struct {
	scope       string
	budget      string
	instruments []capInstrument
}

// cassandraScope is the instrumentation scope the cassandra package records
// under. The collection cap is restricted to it because instrument names are not
// unique across scopes: db.client.operation.duration is the standard semconv name
// and is also emitted by the Redis and MongoDB integrations and by any
// caller-defined database instrumentation. A name-only rule would rewrite those
// streams too and make unrelated collections share the Cassandra budget, even in
// processes that never use Cassandra (the cap is installed by default).
const cassandraScope = "github.com/flywindy/o11y/cassandra"

// cassandraCollectionBudget makes the two Cassandra instruments share one
// distinct-value budget. With separate budgets they can disagree about which
// tables overflowed — a batch records a duration sample but no attempts sample,
// so the buckets fill differently — and the same table could export as its own
// name on one instrument and as "other" on the other, which breaks any query
// that joins them per table.
const cassandraCollectionBudget = "cassandra/db.collection.name"

// cassandraCollectionInstruments are the Cassandra client metrics that carry
// db.collection.name, paired with the Prometheus family name each is exposed as.
var cassandraCollectionInstruments = []capInstrument{
	{instrument: "db.client.operation.duration", family: "db_client_operation_duration_seconds"},
	{instrument: "cassandra.query.attempts", family: "cassandra_query_attempts_total"},
}

// elasticsearchScope is the instrumentation scope the elasticsearch package
// records its SDK-owned metric under (ADR 0027 §5). It gets its own scoped rule
// and its own budget: an Elasticsearch index name and a Cassandra table are
// unrelated dimensions, so one integration's overflow must not evict the other's
// values from the exported label set.
const elasticsearchScope = views.ElasticsearchScope

// elasticsearchCollectionBudget is the distinct-value budget for the
// Elasticsearch index label. Only one instrument carries it today; the budget
// key keeps a future ES instrument (e.g. an attempts counter) consistent with the
// duration histogram about which indices overflowed.
const elasticsearchCollectionBudget = "elasticsearch/db.collection.name"

// elasticsearchCollectionInstruments are the Elasticsearch client metrics that
// carry db.collection.name.
var elasticsearchCollectionInstruments = []capInstrument{
	{instrument: "db.client.operation.duration", family: "db_client_operation_duration_seconds"},
}

// collectionCapScopes lists every integration the db.collection.name cap
// applies to. Each entry is scoped and budgeted independently.
var collectionCapScopes = []collectionCapScope{
	{scope: cassandraScope, budget: cassandraCollectionBudget, instruments: cassandraCollectionInstruments},
	{scope: elasticsearchScope, budget: elasticsearchCollectionBudget, instruments: elasticsearchCollectionInstruments},
}

// otlpCapRules builds the export-boundary attribute caps for the OTLP push path.
func otlpCapRules(cfg Config) []metricscap.Rule {
	var rules []metricscap.Rule
	if cfg.MaxUniqueRoutes > 0 {
		rules = append(rules,
			metricscap.Rule{
				InstrumentName: "http.server.request.duration",
				Key:            semconv.HTTPRouteKey,
				Max:            cfg.MaxUniqueRoutes,
			},
			metricscap.Rule{
				InstrumentName: "http.client.request.duration",
				Key:            semconv.HTTPRouteKey,
				Max:            cfg.MaxUniqueRoutes,
			},
		)
	}
	if cfg.MaxUniqueCollections > 0 {
		for _, sc := range collectionCapScopes {
			for _, inst := range sc.instruments {
				rules = append(rules, metricscap.Rule{
					InstrumentName: inst.instrument,
					ScopeName:      sc.scope,
					Key:            semconv.DBCollectionNameKey,
					Max:            cfg.MaxUniqueCollections,
					BudgetKey:      sc.budget,
				})
			}
		}
	}
	return rules
}

// prometheusCapRules mirrors otlpCapRules for the Prometheus pull path, which
// caps by rendered metric-family and label name rather than instrument name and
// attribute key.
func prometheusCapRules(cfg Config) []metricscap.PrometheusRule {
	var rules []metricscap.PrometheusRule
	if cfg.MaxUniqueRoutes > 0 {
		rules = append(rules,
			metricscap.PrometheusRule{
				MetricName: "http_server_request_duration_seconds",
				LabelName:  "http_route",
				Max:        cfg.MaxUniqueRoutes,
			},
			metricscap.PrometheusRule{
				MetricName: "http_client_request_duration_seconds",
				LabelName:  "http_route",
				Max:        cfg.MaxUniqueRoutes,
			},
		)
	}
	if cfg.MaxUniqueCollections > 0 {
		for _, sc := range collectionCapScopes {
			for _, inst := range sc.instruments {
				rules = append(rules, metricscap.PrometheusRule{
					MetricName: inst.family,
					ScopeName:  sc.scope,
					LabelName:  "db_collection_name",
					Max:        cfg.MaxUniqueCollections,
					BudgetKey:  sc.budget,
				})
			}
		}
	}
	return rules
}

// InitMeter initializes an OTel MeterProvider and returns it together with a
// Closer that must be called during SDK shutdown.
//
// Exporter strategy:
//   - cfg.MetricsOTLPEndpoint == "" → Prometheus pull: private registry +
//     HTTP server on cfg.MetricsAddr; Closer shuts down the HTTP server.
//   - cfg.MetricsOTLPEndpoint != "" → OTLP push: otlpmetrichttp exporter;
//     Closer shuts down the exporter. MetricsAddr is ignored.
//
// Bind errors (Prometheus path) are surfaced synchronously.
func InitMeter(ctx context.Context, cfg Config) (*sdkmetric.MeterProvider, Closer, error) {
	res, err := resolveResource(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	views := defaultViews(cfg)
	views = append(views, cfg.ExtraViews...)

	if cfg.MetricsOTLPEndpoint != "" {
		return initOTLP(ctx, cfg, res, views)
	}
	return initPrometheus(ctx, cfg, res, views)
}

func defaultViews(cfg Config) []sdkmetric.View {
	if cfg.DisableDefaultViews {
		return nil
	}
	histogram := sdkmetric.AggregationExplicitBucketHistogram{
		Boundaries: cfg.HistogramBuckets,
	}
	serverKeys := make([]attribute.Key, 0, 3+len(cfg.ExtraHTTPServerAttrKeys))
	serverKeys = append(serverKeys,
		semconv.HTTPRequestMethodKey,
		semconv.HTTPRouteKey,
		semconv.HTTPResponseStatusCodeKey,
	)
	for _, k := range cfg.ExtraHTTPServerAttrKeys {
		serverKeys = append(serverKeys, attribute.Key(k))
	}
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.duration"},
			sdkmetric.Stream{
				Aggregation:                       histogram,
				AttributeFilter:                   attribute.NewAllowKeysFilter(serverKeys...),
				ExemplarReservoirProviderSelector: dropFilteredAttrsExemplarSelector,
			},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.client.request.duration"},
			sdkmetric.Stream{
				Aggregation: histogram,
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.HTTPRequestMethodKey,
					semconv.HTTPRouteKey,
					semconv.HTTPResponseStatusCodeKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
				),
				ExemplarReservoirProviderSelector: dropFilteredAttrsExemplarSelector,
			},
		),
	}
}

// initPrometheus sets up the Prometheus pull path.
// runtimeStart is a seam over runtime.Start so tests can exercise the
// init-failure branches, which otherwise only trigger on instrument-creation
// errors. It mirrors internal/profiling's pyroscopeStart.
var runtimeStart = runtime.Start

func initPrometheus(ctx context.Context, cfg Config, res *resource.Resource, views []sdkmetric.View) (*sdkmetric.MeterProvider, Closer, error) {
	reg := prometheus.NewRegistry()

	// Resource attributes in the allow filter become constant labels on every
	// series, so service_namespace="..." is guaranteed on every instrument including runtime.
	// The key "deployment.environment.name" matches the pinned semconv version.
	exporter, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithResourceAsConstantLabels(attribute.NewAllowKeysFilter(
			"service.namespace",
			"service.name",
			"service.version",
			"deployment.environment.name",
		)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics: create prometheus exporter: %w", err)
	}

	var (
		initSucceeded bool
		listener      net.Listener
		provider      *sdkmetric.MeterProvider
	)
	defer func() {
		if initSucceeded {
			return
		}
		if provider != nil {
			_ = provider.Shutdown(ctx)
		}
		if listener != nil {
			_ = listener.Close()
		}
	}()

	provider = sdkmetric.NewMeterProvider(
		meterProviderOptions(exporter, res, views, cfg.MaxUniqueRoutes, cfg.MaxUniqueCollections)...,
	)

	if cfg.RuntimeMetrics {
		if err := runtimeStart(runtime.WithMeterProvider(provider)); err != nil {
			return nil, nil, fmt.Errorf("metrics: start runtime metrics: %w", err)
		}
	}

	listener, err = net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics: listen on %s: %w", cfg.MetricsAddr, err)
	}

	gatherer := prometheus.Gatherer(reg)
	if rules := prometheusCapRules(cfg); len(rules) > 0 {
		gatherer = metricscap.NewGatherer(reg, rules...)
	}

	mux := http.NewServeMux()
	// EnableOpenMetrics lets the handler content-negotiate to OpenMetrics
	// format (Accept: application/openmetrics-text). That is the only
	// exposition format otelprom renders per-bucket exemplars in, so without
	// it the trace-to-metric linkage in Grafana / Tempo stays empty for both
	// SDK-managed and caller-defined histograms. OpenMetrics also normalises
	// integer histogram bucket boundaries to a `.0` suffix on the rendered
	// `le` label (e.g. `le="1"` → `le="1.0"`); callers whose dashboards or
	// recording rules cannot tolerate that one-time series-identity change
	// can suppress the renegotiation via WithExemplars(false).
	mux.Handle("/metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		EnableOpenMetrics: cfg.Exemplars,
	}))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()

	initSucceeded = true
	return provider, server.Shutdown, nil
}

// initOTLP sets up the OTLP push path.
func initOTLP(ctx context.Context, cfg Config, res *resource.Resource, views []sdkmetric.View) (*sdkmetric.MeterProvider, Closer, error) {
	expOpts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(cfg.MetricsOTLPEndpoint),
	}
	if len(cfg.OTLPHeaders) > 0 {
		expOpts = append(expOpts, otlpmetrichttp.WithHeaders(cfg.OTLPHeaders))
	}
	exporter, err := otlpmetrichttp.New(ctx, expOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("metrics: create OTLP exporter: %w", err)
	}
	cappedExporter := sdkmetric.Exporter(exporter)
	if rules := otlpCapRules(cfg); len(rules) > 0 {
		cappedExporter = metricscap.NewExporter(exporter, rules...)
	}

	var initSucceeded bool
	var provider *sdkmetric.MeterProvider
	defer func() {
		if initSucceeded {
			return
		}
		// Shut the provider down, not just the exporter: NewPeriodicReader
		// starts its export goroutine at construction, and only the provider's
		// Shutdown drains it (it in turn shuts the exporter down). Releasing
		// the exporter alone would leave that goroutine exporting against a
		// closed exporter once per interval for the life of the process.
		if provider != nil {
			_ = provider.Shutdown(ctx)
			return
		}
		_ = exporter.Shutdown(ctx)
	}()

	provider = sdkmetric.NewMeterProvider(
		meterProviderOptions(sdkmetric.NewPeriodicReader(cappedExporter), res, views, cfg.MaxUniqueRoutes, cfg.MaxUniqueCollections)...,
	)

	if cfg.RuntimeMetrics {
		if err := runtimeStart(runtime.WithMeterProvider(provider)); err != nil {
			return nil, nil, fmt.Errorf("metrics: start runtime metrics: %w", err)
		}
	}

	initSucceeded = true
	// provider.Shutdown drains the PeriodicReader which in turn calls
	// exporter.Shutdown. Returning exporter.Shutdown here would cause a
	// second shutdown when o11y.go also calls mp.Shutdown, so we return a
	// no-op: the MeterProvider shutdown path handles everything.
	return provider, func(_ context.Context) error { return nil }, nil
}

func meterProviderOptions(reader sdkmetric.Reader, res *resource.Resource, views []sdkmetric.View, maxUniqueRoutes, maxUniqueCollections int) []sdkmetric.Option {
	opts := []sdkmetric.Option{
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(views...),
	}
	if limit := cardinalityLimitBudget(maxUniqueRoutes, maxUniqueCollections); limit > 0 {
		opts = append(opts, sdkmetric.WithCardinalityLimit(limit))
	}
	return opts
}

// cardinalityLimitBudget derives the in-process SDK cardinality limit, which is
// a single global per-stream guard rather than a per-instrument one. It must
// therefore accommodate every capped dimension the SDK exports, not just routes:
// deriving it from MaxUniqueRoutes alone means a caller who lowers that option
// (say to 1, giving 1024) can push the Cassandra streams over the limit, and the
// OTel SDK then collapses the excess into otel.metric.overflow — dropping
// db.collection.name entirely for data that was well inside MaxUniqueCollections.
// The limit is the larger of the two budgets so neither dimension can starve the
// other; at the default settings the route budget dominates and this is a no-op.
func cardinalityLimitBudget(maxUniqueRoutes, maxUniqueCollections int) int {
	routes := scaleBudget(maxUniqueRoutes, sdkCardinalityMethodBudget*sdkCardinalityStatusBudget)
	collections := scaleBudget(maxUniqueCollections, sdkCardinalityCollectionBudget)
	return max(routes, collections)
}

// scaleBudget returns n*per, saturating rather than overflowing.
func scaleBudget(n, per int) int {
	const maxInt = int(^uint(0) >> 1)
	if n <= 0 {
		return 0
	}
	if n > maxInt/per {
		return maxInt
	}
	return n * per
}

// resolveResource returns the Resource to attach to the MeterProvider.
// When cfg.Resource is set it is returned directly (shared with the trace and
// log providers). Otherwise a standalone Resource is built from the Config
// fields, which requires Namespace to be non-empty.
func resolveResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	if cfg.Resource != nil {
		return cfg.Resource, nil
	}

	if cfg.Namespace == "" {
		return nil, errors.New("metrics: Namespace is required")
	}

	resOpts := []resource.Option{
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.ServiceNamespaceKey.String(cfg.Namespace),
		),
	}
	if cfg.ServiceVersion != "" {
		resOpts = append(resOpts, resource.WithAttributes(
			semconv.ServiceVersionKey.String(cfg.ServiceVersion),
		))
	}
	if cfg.Environment != "" {
		resOpts = append(resOpts, resource.WithAttributes(
			semconv.DeploymentEnvironmentNameKey.String(cfg.Environment),
		))
	}
	res, err := resource.New(ctx, resOpts...)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("metrics: create resource: %w", err)
	}
	return res, nil
}
