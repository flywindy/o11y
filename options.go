package o11y

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// DefaultMetricsAddr is the default listen address for the built-in
// Prometheus /metrics HTTP server.
const DefaultMetricsAddr = ":2112"

// DefaultMaxUniqueRoutes is the default export-boundary cap for distinct
// http.route values. Additional SDK cardinality limits protect in-process
// aggregation memory before export.
const DefaultMaxUniqueRoutes = 1000

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

	// Tracing
	sampler          sdktrace.Sampler
	samplingRatio    float64
	samplingRatioSet bool

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
// Passing nil leaves sampling unset, preserving the OTel environment/default
// sampler path. A non-nil sampler overrides OTEL_TRACES_SAMPLER /
// OTEL_TRACES_SAMPLER_ARG for this SDK instance.
func WithTraceSampler(sampler sdktrace.Sampler) Option {
	return func(c *Config) {
		c.sampler = sampler
		c.samplingRatioSet = false
		c.samplingRatio = 0
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
//     HTTP semconv keys, the four resource constants, and the three
//     otelprom scope labels otel_scope_name / _version / _schema_url), or
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

// reservedHTTPServerPromLabels lists the Prometheus label names the SDK
// already exports on http_server_request_duration_* series:
//
//   - three from the view allow-list: http_request_method, http_route,
//     http_response_status_code;
//   - four resource attributes promoted to constant labels by
//     internal/metrics.initPrometheus (service_*, deployment_*);
//   - three scope labels otelprom adds to every series unless WithoutScopeInfo
//     is set: otel_scope_name, otel_scope_version, otel_scope_schema_url.
//
// They are stored in their post-normalization form so the equality check is
// just a map lookup.
var reservedHTTPServerPromLabels = []string{
	"http_request_method",
	"http_route",
	"http_response_status_code",
	"service_name",
	"service_namespace",
	"service_version",
	"deployment_environment_name",
	"otel_scope_name",
	"otel_scope_version",
	"otel_scope_schema_url",
}

// normalizePrometheusLabelName mirrors the classic non-UTF8 Prometheus label
// normalization that otelprom (via github.com/prometheus/otlptranslator)
// applies on export: any rune outside [a-zA-Z0-9] becomes '_', adjacent
// underscores collapse to a single one, and a leading digit is prefixed
// with "key_" so the result is a syntactically valid Prometheus label.
//
// Reproducing the same rules at option-time is what lets us detect
// collisions before they reach the exporter, where two distinct attribute
// values would otherwise be silently merged into one label value.
func normalizePrometheusLabelName(key string) string {
	if key == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(key))
	prevUnderscore := false
	for _, r := range key {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteRune('_')
			prevUnderscore = true
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	if first := out[0]; first >= '0' && first <= '9' {
		out = "key_" + out
	}
	return out
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

// defaultConfig returns a *Config initialized with the package's built-in
// defaults. Feature toggles default to true but respect the O11Y_*_ENABLED
// environment variables so operators can disable pillars without code changes.
// Explicit options (WithTraceEnabled etc.) always win over env vars.
// Invalid env var values are collected in initWarnings and emitted after Init
// builds its logger.
func defaultConfig() *Config {
	cfg := &Config{
		otlpEndpoint:     "http://localhost:4318",
		logLevel:         slog.LevelInfo,
		metricsAddr:      DefaultMetricsAddr,
		runtimeMetrics:   true,
		histogramBuckets: cloneFloat64s(defaultLatencyBuckets),
		maxUniqueRoutes:  DefaultMaxUniqueRoutes,
		exemplars:        true,
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
