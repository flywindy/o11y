package o11y

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/flywindy/o11y/internal/baggageattrs"
	"github.com/flywindy/o11y/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// DefaultMetricsAddr is the default listen address for the built-in
// Prometheus /metrics HTTP server.
const DefaultMetricsAddr = ":2112"

// DefaultMaxUniqueRoutes is the default export-boundary cap for distinct
// http.route values. Additional SDK cardinality limits protect in-process
// aggregation memory before export.
const DefaultMaxUniqueRoutes = 1000

// MaxBaggageAttributeKeys bounds the application-defined baggage keys one SDK
// instance materializes. user.name has a separate opt-in and does not count.
const MaxBaggageAttributeKeys = 8

// DefaultMaxUniqueCollections is the default export-boundary cap for distinct
// db.collection.name values on the Cassandra client metrics.
//
// A Cassandra schema's table count is fixed by DDL and is normally in the tens,
// so this is not a budget the label is expected to approach — it is a backstop
// against a statement shape the SDK's CQL tokenizer mis-reads, which would
// otherwise turn a bounded label into an unbounded one. It is set well below
// DefaultMaxUniqueRoutes because a schema is a much smaller keyspace than a
// service's URL space.
const DefaultMaxUniqueCollections = 200

// defaultLatencyBuckets is the SLO-friendly histogram boundary set applied
// to all http.server.* histograms when the caller does not override it.
// Standardizing these boundaries across the company keeps P99 calculations
// directly comparable between services. Exposed via DefaultLatencyBuckets()
// so the slice cannot be mutated by callers.
var defaultLatencyBuckets = []float64{
	.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10,
}

// DefaultLatencyBuckets returns a fresh copy of the SDK's default histogram
// boundaries. It returns a copy so that callers who keep a reference cannot
// accidentally mutate the package-level defaults.
func DefaultLatencyBuckets() []float64 {
	return cloneFloat64s(defaultLatencyBuckets)
}

// Config defines the configuration for the o11y SDK.
type Config struct {
	serviceName    string
	serviceVersion string
	environment    string
	otlpEndpoint   string
	otlpHeaders    map[string]string
	logLevel       slog.Level

	// Resource attributes added by WithResourceAttributes, on top of the
	// service identity above and the detected host / process / SDK set.
	resourceAttrs []attribute.KeyValue

	// Tracing
	sampler          sdktrace.Sampler
	samplingRatio    float64
	samplingRatioSet bool
	userBaggage      bool
	baggageKeys      []string

	// Profiling
	profilingEndpoint    string
	profilingAuthHeaders map[string]string

	// Metrics
	metricsAddr             string
	metricsOTLPEndpoint     string // non-empty → OTLP push instead of Prometheus pull
	runtimeMetrics          bool
	histogramBuckets        []float64
	namespace               string
	disableDefaultViews     bool
	maxUniqueRoutes         int
	maxUniqueCollections    int
	extraHTTPServerAttrKeys []string
	exemplars               bool

	// Feature toggles. Trace, metrics, and log default to true; profiling
	// defaults to false because it is an opt-in fourth signal. All four can
	// be overridden via WithTraceEnabled / WithMetricsEnabled /
	// WithLogEnabled / WithProfilingEnabled or the corresponding
	// O11Y_*_ENABLED environment variables. Profiling requires both the
	// toggle to be on AND a non-empty WithProfilingEndpoint before the SDK
	// will start the Pyroscope profiler.
	traceEnabled     bool
	metricsEnabled   bool
	logEnabled       bool
	profilingEnabled bool

	// initWarnings holds non-fatal diagnostics collected before the SDK logger
	// exists (e.g. unparseable O11Y_*_ENABLED env var values). They are emitted
	// via WarnContext after Init builds its logger.
	initWarnings []string
}

// Option is a functional option for configuring the o11y SDK.
type Option func(*Config)

// WithServiceName sets the service name for trace resource attributes.
func WithServiceName(name string) Option {
	return func(c *Config) {
		c.serviceName = name
	}
}

// WithServiceVersion sets the service version (e.g. "1.4.2") for trace
// resource attributes. Used in OTel as service.version and is especially
// useful for canary deployments and version-based trace filtering.
func WithServiceVersion(version string) Option {
	return func(c *Config) {
		c.serviceVersion = version
	}
}

// WithEnvironment sets the deployment environment (e.g., "production", "staging").
func WithEnvironment(env string) Option {
	return func(c *Config) {
		c.environment = env
	}
}

// WithOTLPEndpoint sets the OTLP/HTTP collector endpoint used for traces and
// logs. The endpoint must be reachable by the SDK's process (no proxy is
// configured by this package).
//
// Production note: prefer https:// in production deployments. The default
// http://localhost:4318 is intended for local development against an OTel
// Collector running on the same host. When sending telemetry across a
// network boundary, use TLS — observability traffic carries trace IDs,
// hostnames, error messages and stack traces that should not be exposed in
// plaintext.
//
// If the endpoint requires authentication (Grafana Cloud, Honeycomb, NewRelic,
// Datadog, ...), pair this option with WithOTLPHeaders to attach the API
// token / Bearer header to every OTLP request.
func WithOTLPEndpoint(endpoint string) Option {
	return func(c *Config) {
		c.otlpEndpoint = endpoint
	}
}

// WithSamplingRatio configures head sampling for root traces using
// ParentBased(TraceIDRatioBased(ratio)). Use this on high-throughput producer
// services to reduce in-process span allocation, batch-processor pressure, and
// downstream trace volume while preserving whole-trace consistency through
// context propagation.
//
// The ratio must be between 0.0 and 1.0 inclusive; Init returns an error for
// out-of-range, NaN, or infinite values instead of relying on OTel's native
// clamping. When this option is set, it overrides OTEL_TRACES_SAMPLER /
// OTEL_TRACES_SAMPLER_ARG for this SDK instance. When unset, those OTel env
// vars remain active.
func WithSamplingRatio(ratio float64) Option {
	return func(c *Config) {
		c.samplingRatio = ratio
		c.samplingRatioSet = true
		c.sampler = nil
	}
}

// WithTraceSampler configures an explicit OpenTelemetry sampler for the SDK's
// TracerProvider. It is an escape hatch for custom samplers not expressible via
// WithSamplingRatio or the OTEL_TRACES_SAMPLER environment variable.
//
// Passing nil is a no-op: it leaves whatever sampling configuration is already
// in place, whether that is an earlier WithSamplingRatio or the OTel
// environment/default sampler path. A non-nil sampler overrides
// OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG and any earlier
// WithSamplingRatio for this SDK instance.
func WithTraceSampler(sampler sdktrace.Sampler) Option {
	return func(c *Config) {
		if sampler == nil {
			// A wrapper that maps configuration to options often ends up
			// passing nil for "no custom sampler". That must not erase a
			// ratio the caller set beside it, or the service silently runs
			// at 100 % head sampling.
			return
		}
		c.sampler = sampler
		c.samplingRatioSet = false
		c.samplingRatio = 0
	}
}

// WithUserBaggage enables ADR 0016 opt-in materialization of whitelisted user
// identity baggage onto this service's spans and log records.
//
// This option does not create baggage by itself. Use ContextWithUser after
// authentication to put user.name into the context; WithUserBaggage then copies
// that whitelisted value onto spans at start time and onto slog records emitted
// by this SDK's logger. The feature is off by default because user.name is PII
// and propagated baggage can cross service boundaries.
// user.name is tracked separately from application keys and does not consume
// MaxBaggageAttributeKeys.
func WithUserBaggage() Option {
	return func(c *Config) {
		c.userBaggage = true
	}
}

// WithBaggageAttributes enables materialization of application-defined W3C
// baggage members onto spans and SDK log records. This option does not create
// baggage; use ContextWithBaggageValue after validating the source value.
// Calls accumulate and de-duplicate, and the first MaxBaggageAttributeKeys
// valid application keys are used.
//
// Empty, non-token, overlong, and reserved keys are dropped with a startup
// warning. Reservations include SDK and slog fields, the complete pinned
// semantic-convention catalog and parameterized namespaces, and user.name,
// whose PII contract requires WithUserBaggage. Init fails if an otherwise valid
// key collides with this process's effective Resource attributes, including
// OTEL_RESOURCE_ATTRIBUTES. Never use a materialized key as a metric label.
func WithBaggageAttributes(keys ...string) Option {
	return func(c *Config) {
		seen := make(map[string]struct{}, len(c.baggageKeys)+len(keys))
		for _, key := range c.baggageKeys {
			seen[key] = struct{}{}
		}
		for _, key := range keys {
			if _, ok := seen[key]; ok {
				continue
			}
			// Recorded before validation so a repeated invalid key is reported
			// once rather than once per occurrence.
			seen[key] = struct{}{}
			if err := baggageattrs.ValidBaggageKey(key); err != nil {
				c.initWarnings = append(c.initWarnings, fmt.Sprintf(
					"WithBaggageAttributes: dropping key %q: %v", key, err,
				))
				continue
			}
			c.baggageKeys = append(c.baggageKeys, key)
		}
	}
}

// WithOTLPHeaders attaches custom HTTP headers to every OTLP/HTTP request
// emitted by the SDK (traces, logs, and OTLP metrics push). Typical use
// cases:
//
//   - Authentication against managed observability backends, e.g.
//     {"Authorization": "Bearer <token>"} or
//     {"X-Honeycomb-Team": "<api-key>"}.
//   - Multi-tenant routing on a shared Collector, e.g.
//     {"X-Scope-OrgID": "<tenant>"}.
//
// Calling WithOTLPHeaders multiple times merges into the same map; later
// calls overwrite earlier values for the same header key. Header values are
// not logged.
func WithOTLPHeaders(headers map[string]string) Option {
	return func(c *Config) {
		if len(headers) == 0 {
			return
		}
		if c.otlpHeaders == nil {
			c.otlpHeaders = make(map[string]string, len(headers))
		}
		for k, v := range headers {
			c.otlpHeaders[k] = v
		}
	}
}

// WithProfilingEndpoint sets the Pyroscope-compatible ingest endpoint.
// Examples:
//   - "http://alloy.infra.svc.cluster.local:4040" for Grafana Alloy
//   - "http://pyroscope.infra.svc.cluster.local:4040" for direct Pyroscope ingest
//
// When empty, profiling is fully disabled: no profiler goroutines are started,
// no pprof globals are touched, and trace spans are not annotated with
// pyroscope.profile.id. Default: empty.
func WithProfilingEndpoint(endpoint string) Option {
	return func(c *Config) {
		c.profilingEndpoint = endpoint
	}
}

// WithProfilingAuthHeaders attaches custom HTTP headers to every Pyroscope
// profile push. Use this for Grafana Cloud Profiles, Basic auth via an
// Authorization header, or multi-tenant routing such as X-Scope-OrgID.
//
// Calling WithProfilingAuthHeaders multiple times merges into the same map;
// later calls overwrite earlier values for the same header key. Header values
// are not logged.
func WithProfilingAuthHeaders(headers map[string]string) Option {
	return func(c *Config) {
		if len(headers) == 0 {
			return
		}
		if c.profilingAuthHeaders == nil {
			c.profilingAuthHeaders = make(map[string]string, len(headers))
		}
		for k, v := range headers {
			c.profilingAuthHeaders[k] = v
		}
	}
}

// WithLogLevel returns an Option that sets the minimum logging level for the SDK.
func WithLogLevel(level slog.Level) Option {
	return func(c *Config) {
		c.logLevel = level
	}
}

// WithServiceNamespace sets the service.namespace resource attribute (OTel
// semconv). It is required: Init returns an error when empty. The value
// identifies the owning team or product unit and maps naturally to the
// Kubernetes namespace when services are namespaced by product. It becomes a
// constant Prometheus label (service_namespace="...") on every series and
// appears on all three observability signals (traces, logs, metrics).
func WithServiceNamespace(namespace string) Option {
	return func(c *Config) {
		c.namespace = namespace
	}
}

// WithResourceAttributes adds attributes to the OTel Resource shared by the
// trace, metrics and log providers, next to the service identity and the
// detected host, process and SDK attributes. On the Prometheus path they
// appear on `target_info`; on OTLP they travel with every span, metric data
// point and log record, so keep them to values that are true for the whole
// process lifetime and are safe to store in every backend.
//
// Keys the SDK owns — service.name, service.version, service.namespace and
// deployment.environment.name — are not accepted here: they are dropped with
// a startup warning so the identity options stay the single source of truth.
// An attribute with an empty key is dropped the same way. Values given here
// override the same key from OTEL_RESOURCE_ATTRIBUTES; the last value wins
// when a key is given twice.
func WithResourceAttributes(attrs ...attribute.KeyValue) Option {
	return func(c *Config) {
		for _, kv := range attrs {
			switch {
			case kv.Key == "":
				c.initWarnings = append(c.initWarnings,
					"WithResourceAttributes: ignoring an attribute with an empty key")
			case isSDKResourceIdentityKey(kv.Key):
				c.initWarnings = append(c.initWarnings, fmt.Sprintf(
					"WithResourceAttributes: ignoring %q; it is set by the SDK's identity options (WithServiceName / WithServiceVersion / WithServiceNamespace / WithEnvironment)",
					string(kv.Key)))
			default:
				c.resourceAttrs = append(c.resourceAttrs, kv)
			}
		}
	}
}

// isSDKResourceIdentityKey reports whether key is one of the four resource
// attributes the identity options own.
func isSDKResourceIdentityKey(key attribute.Key) bool {
	switch key {
	case semconv.ServiceNameKey, semconv.ServiceVersionKey,
		semconv.ServiceNamespaceKey, semconv.DeploymentEnvironmentNameKey:
		return true
	}
	return false
}

// WithMetricsOTLPEndpoint switches the metrics exporter from Prometheus pull
// to OTLP push. When set, the /metrics HTTP server is not started and metrics
// are exported via OTLP/HTTP to the given endpoint. Use this for serverless
// environments (Lambda, Cloud Run) where exposing a scrape port is not
// possible. When unset, the default Prometheus pull model is used.
//
// Example: o11y.WithMetricsOTLPEndpoint("http://collector:4318")
func WithMetricsOTLPEndpoint(endpoint string) Option {
	return func(c *Config) {
		c.metricsOTLPEndpoint = endpoint
	}
}

// WithMetricsAddr returns an Option that sets the metrics HTTP server listen address to the provided addr.
// If not set, the metrics server defaults to DefaultMetricsAddr (":2112").
func WithMetricsAddr(addr string) Option {
	return func(c *Config) {
		c.metricsAddr = addr
	}
}

// WithRuntimeMetrics toggles collection of Go runtime metrics (goroutines,
// GC, memory, etc.) via OTel runtime instrumentation. Defaults to true.
func WithRuntimeMetrics(enabled bool) Option {
	return func(c *Config) {
		c.runtimeMetrics = enabled
	}
}

// WithHistogramBuckets overrides the histogram boundaries applied to HTTP
// server and client latency histograms. Defaults to DefaultLatencyBuckets;
// override only when your service has a genuinely different latency profile.
// Changing these from the package default makes cross-service P99
// comparisons inconsistent.
func WithHistogramBuckets(buckets []float64) Option {
	return func(c *Config) {
		c.histogramBuckets = cloneFloat64s(buckets)
	}
}

// WithTraceEnabled controls whether the SDK initialises a real TracerProvider
// and exports spans via OTLP. When false, Init returns a no-op TracerProvider;
// the W3C TraceContext propagator still parses and forwards trace headers so
// downstream services are unaffected.
// Default: true (env var O11Y_TRACE_ENABLED overrides the built-in default).
func WithTraceEnabled(enabled bool) Option {
	return func(c *Config) { c.traceEnabled = enabled }
}

// WithMetricsEnabled controls whether the SDK initialises a real MeterProvider.
// When false, no Prometheus HTTP server is started and no OTLP metrics are
// exported. All instrumentation that accepts a MeterProvider receives a no-op
// provider, preserving compile-time compatibility with zero runtime cost.
// Default: true (env var O11Y_METRICS_ENABLED overrides the built-in default).
func WithMetricsEnabled(enabled bool) Option {
	return func(c *Config) { c.metricsEnabled = enabled }
}

// WithLogEnabled controls whether the SDK exports logs via OTLP to the
// OTel Collector. When false, slog records are written to stdout only; no
// OTLP log provider is started and no collector connection is attempted.
// Default: true (env var O11Y_LOG_ENABLED overrides the built-in default).
func WithLogEnabled(enabled bool) Option {
	return func(c *Config) { c.logEnabled = enabled }
}

// WithProfilingEnabled controls whether the SDK starts the Pyroscope profiler
// and wraps the TracerProvider with the trace-to-profile bridge. Profiling is
// opt-in: it requires both this toggle to be on AND a non-empty endpoint set
// via WithProfilingEndpoint. Either condition alone is insufficient. This
// lets operators stage a profiling rollout (or roll it back) without removing
// the endpoint from deployment manifests.
//
// Default: false (env var O11Y_PROFILING_ENABLED overrides the built-in
// default). Unlike trace/metrics/log, profiling defaults off because it is
// an additional fourth signal that must be explicitly enabled.
func WithProfilingEnabled(enabled bool) Option {
	return func(c *Config) { c.profilingEnabled = enabled }
}

// WithExemplars controls whether the Prometheus pull `/metrics` handler
// content-negotiates the OpenMetrics exposition format. OpenMetrics is the
// only format the Prometheus exporter renders per-bucket exemplars in, so
// this option also governs whether trace-to-metric linkage in Grafana / Tempo
// works for SDK-managed and caller-defined histograms. Default: true.
//
// Set to false only as a temporary mitigation when migrating a service whose
// existing PromQL queries, recording rules, or alert rules hardcode integer
// histogram bucket boundaries (e.g. `le="1"`, `le="5"`, `le="10"`). The
// OpenMetrics format emits those as `le="1.0"`, `le="5.0"`, `le="10.0"` —
// the underlying bucket boundary is identical (`float64`) but the rendered
// label string differs, so queries matching the integer form silently stop
// matching after rollout. Aggregate queries such as `histogram_quantile(...)`
// are unaffected. Update the dependent queries, then remove this option.
//
// Has no effect on the OTLP push path (WithMetricsOTLPEndpoint): exemplars
// travel in the OTLP proto and are not impacted by Prometheus text-format
// negotiation.
func WithExemplars(enabled bool) Option {
	return func(c *Config) {
		c.exemplars = enabled
	}
}

// WithDisableDefaultViews disables SDK-managed HTTP metric views.
func WithDisableDefaultViews() Option {
	return func(c *Config) {
		c.disableDefaultViews = true
	}
}

// WithExtraHTTPServerAttributeKeys extends the SDK-managed allow-list on the
// http.server.request.duration metric view. By default that view keeps only
// http.request.method, http.route, and http.response.status_code to bound
// cardinality; any other attributes attached to the record (for example via
// o11ygin.WithMetricAttributesFn or otelhttp's WithMetricAttributesFn) are
// dropped from the exported series and end up as exemplar labels, where the
// OpenMetrics 128-rune cap quickly trips. Use this option to promote a small
// set of caller-controlled keys (e.g. "app_name", "bot_name") onto the
// series itself so they participate in PromQL aggregations.
//
// Cardinality is the caller's responsibility: every distinct value combination
// multiplies the existing route×method×status series count. Prefer keys whose
// value space is enumerable and small (tens, not thousands).
//
// Keys are checked against the otelprom Prometheus label-name normalization
// (non-alphanumeric → '_', runs collapsed, leading digits prefixed with
// "key_"). The SDK drops — with a startup WARN log — any key that, after
// normalization:
//
//   - matches a built-in label the SDK already exports (the view-allowed
//     HTTP semconv keys, the four resource constants, every otelprom
//     otel_scope_* label, the exposition-format labels le / quantile, and
//     the "__x__" reserved-name shape — the same reserved set the metric
//     views drop at record time on the Prometheus path), or
//   - matches another caller-supplied key from this or a prior
//     WithExtraHTTPServerAttributeKeys call (e.g. "app.name" and "app_name"
//     both normalize to "app_name"), or
//   - normalizes to the empty string.
//
// Accepting either form of collision would silently merge two attribute
// values into a single Prometheus label, corrupting PromQL grouping for
// that dimension.
//
// Calls accumulate. Has no effect when WithDisableDefaultViews is set,
// because no SDK-managed view exists to extend.
func WithExtraHTTPServerAttributeKeys(keys ...string) Option {
	return func(c *Config) {
		seen := make(map[string]string, len(reservedHTTPServerPromLabels)+len(c.extraHTTPServerAttrKeys)+len(keys))
		for _, builtin := range reservedHTTPServerPromLabels {
			seen[builtin] = "<built-in SDK label>"
		}
		for _, prev := range c.extraHTTPServerAttrKeys {
			seen[normalizePrometheusLabelName(prev)] = prev
		}
		for _, k := range keys {
			if k == "" {
				continue
			}
			norm := normalizePrometheusLabelName(k)
			if norm == "" || norm == "_" {
				c.initWarnings = append(c.initWarnings, fmt.Sprintf(
					"WithExtraHTTPServerAttributeKeys: key %q normalizes to an invalid Prometheus label name; dropping",
					k,
				))
				continue
			}
			// The same predicate the metric views apply at record time, so a
			// key the view would drop anyway is rejected here with a warning
			// instead of silently vanishing from the series.
			if metrics.IsReservedAttributeKey(attribute.Key(k)) {
				seen[norm] = "<built-in SDK label>"
			}
			if existing, ok := seen[norm]; ok {
				c.initWarnings = append(c.initWarnings, fmt.Sprintf(
					"WithExtraHTTPServerAttributeKeys: key %q collides with %s (both normalize to Prometheus label %q); dropping to prevent silent label-value merging",
					k, existing, norm,
				))
				continue
			}
			seen[norm] = fmt.Sprintf("%q", k)
			c.extraHTTPServerAttrKeys = append(c.extraHTTPServerAttrKeys, k)
		}
	}
}

// reservedHTTPServerPromLabels lists the Prometheus label names the SDK's
// own http.server.request.duration view already exports, in their
// post-normalization form: the three view-allowed HTTP semconv keys. The
// labels the exporter owns on every series (resource constants, otel_scope_*,
// le / quantile) are covered by metrics.IsReservedAttributeKey instead, so
// this option and the record-time views agree on one reserved set.
var reservedHTTPServerPromLabels = []string{
	"http_request_method",
	"http_route",
	"http_response_status_code",
}

// normalizePrometheusLabelName mirrors the Prometheus label normalization
// otelprom applies on export; see metrics.NormalizePrometheusLabelName.
// Reproducing the same rules at option-time is what lets us detect collisions
// before they reach the exporter.
func normalizePrometheusLabelName(key string) string {
	return metrics.NormalizePrometheusLabelName(key)
}

// WithMaxUniqueRoutes sets the distinct http.route export cap. Values <= 0
// use DefaultMaxUniqueRoutes. The SDK also derives an in-process aggregation
// cardinality budget from this value to guard against unbounded attribute sets.
func WithMaxUniqueRoutes(n int) Option {
	return func(c *Config) {
		if n <= 0 {
			c.maxUniqueRoutes = DefaultMaxUniqueRoutes
			return
		}
		c.maxUniqueRoutes = n
	}
}

// WithMaxUniqueCollections sets the distinct db.collection.name export cap for
// the Cassandra client metrics (db.client.operation.duration and
// cassandra.query.attempts) and the Elasticsearch client metric
// (db.client.operation.duration, where the collection is the index). Values
// <= 0 use DefaultMaxUniqueCollections. Each integration has its own budget of
// n values, so an Elasticsearch overflow never evicts Cassandra tables or vice
// versa.
//
// Values beyond the cap are collapsed to the literal label "other" at the
// export boundary, the same mechanism WithMaxUniqueRoutes applies to http.route.
// Because a Cassandra schema's tables are DDL-fixed, reaching the cap there
// normally means the SDK's CQL tokenizer mis-read a statement shape rather than
// that the schema genuinely grew; an "other" bucket on the Cassandra metrics is
// worth investigating rather than raising the cap. Elasticsearch index names
// commonly roll by date, so an "other" bucket there is the expected signal to
// opt the label out (below) rather than a defect.
//
// Callers who would rather not carry the label at all should pass
// cassandra.WithCollectionMetricLabel(false) to NewSession or
// elasticsearch.WithCollectionMetricLabel(false) to NewClient instead — this cap
// bounds the label, it does not remove it.
func WithMaxUniqueCollections(n int) Option {
	return func(c *Config) {
		if n <= 0 {
			c.maxUniqueCollections = DefaultMaxUniqueCollections
			return
		}
		c.maxUniqueCollections = n
	}
}

// defaultConfig returns a *Config initialized with the package's built-in
// defaults. Feature toggles default to true but respect the O11Y_*_ENABLED
// environment variables so operators can disable pillars without code changes.
// Explicit options (WithTraceEnabled etc.) always win over env vars.
// Invalid env var values are collected in initWarnings and emitted after Init
// builds its logger.
func defaultConfig() *Config {
	cfg := &Config{
		otlpEndpoint:         "http://localhost:4318",
		logLevel:             slog.LevelInfo,
		metricsAddr:          DefaultMetricsAddr,
		runtimeMetrics:       true,
		histogramBuckets:     cloneFloat64s(defaultLatencyBuckets),
		maxUniqueRoutes:      DefaultMaxUniqueRoutes,
		maxUniqueCollections: DefaultMaxUniqueCollections,
		exemplars:            true,
	}
	var warn string
	cfg.traceEnabled, warn = parseBoolEnv("O11Y_TRACE_ENABLED", true)
	if warn != "" {
		cfg.initWarnings = append(cfg.initWarnings, warn)
	}
	cfg.metricsEnabled, warn = parseBoolEnv("O11Y_METRICS_ENABLED", true)
	if warn != "" {
		cfg.initWarnings = append(cfg.initWarnings, warn)
	}
	cfg.logEnabled, warn = parseBoolEnv("O11Y_LOG_ENABLED", true)
	if warn != "" {
		cfg.initWarnings = append(cfg.initWarnings, warn)
	}
	cfg.profilingEnabled, warn = parseBoolEnv("O11Y_PROFILING_ENABLED", false)
	if warn != "" {
		cfg.initWarnings = append(cfg.initWarnings, warn)
	}
	return cfg
}

// parseBoolEnv reads key from the environment and parses it as a bool.
// Returns (def, "") when the variable is absent.
// Returns (def, warning) when the value is present but not a recognised bool.
// Accepted truthy values: "1", "t", "T", "TRUE", "true", "True".
// Accepted falsy values:  "0", "f", "F", "FALSE", "false", "False".
func parseBoolEnv(key string, def bool) (bool, string) {
	v := os.Getenv(key)
	if v == "" {
		return def, ""
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Sprintf(
			"env var %s=%q is not a recognised bool (accepted: 1/t/true/0/f/false); falling back to default (%v)",
			key, v, def,
		)
	}
	return b, ""
}

func cloneFloat64s(in []float64) []float64 {
	if len(in) == 0 {
		return nil
	}
	out := make([]float64, len(in))
	copy(out, in)
	return out
}
