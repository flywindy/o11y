package o11y

import "log/slog"

// DefaultMetricsAddr is the default listen address for the built-in
// Prometheus /metrics HTTP server.
const DefaultMetricsAddr = ":2112"

// DefaultMaxUniqueRoutes is the default cap for distinct http.route values
// before overflow routes are reported as "other" where exporter support
// allows rewriting before export.
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

	// Metrics
	metricsAddr         string
	metricsOTLPEndpoint string // non-empty → OTLP push instead of Prometheus pull
	runtimeMetrics      bool
	histogramBuckets    []float64
	namespace           string
	disableDefaultViews bool
	maxUniqueRoutes     int
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
// server latency histograms. Defaults to DefaultLatencyBuckets; override
// only when your service has a genuinely different latency profile.
// Changing these from the package default makes cross-service P99
// comparisons inconsistent.
func WithHistogramBuckets(buckets []float64) Option {
	return func(c *Config) {
		c.histogramBuckets = cloneFloat64s(buckets)
	}
}

// WithDisableDefaultViews disables SDK-managed HTTP metric views.
func WithDisableDefaultViews() Option {
	return func(c *Config) {
		c.disableDefaultViews = true
	}
}

// WithMaxUniqueRoutes sets the distinct http.route cap. Values <= 0 use
// DefaultMaxUniqueRoutes.
func WithMaxUniqueRoutes(n int) Option {
	return func(c *Config) {
		if n <= 0 {
			c.maxUniqueRoutes = DefaultMaxUniqueRoutes
			return
		}
		c.maxUniqueRoutes = n
	}
}

// defaultConfig returns a *Config initialized with the package's built-in defaults.
// It sets otlpEndpoint to "http://localhost:4318", logLevel to slog.LevelInfo, metricsAddr to DefaultMetricsAddr, runtimeMetrics to true, and histogramBuckets to DefaultLatencyBuckets.
func defaultConfig() *Config {
	return &Config{
		otlpEndpoint:     "http://localhost:4318",
		logLevel:         slog.LevelInfo,
		metricsAddr:      DefaultMetricsAddr,
		runtimeMetrics:   true,
		histogramBuckets: cloneFloat64s(defaultLatencyBuckets),
		maxUniqueRoutes:  DefaultMaxUniqueRoutes,
	}
}

func cloneFloat64s(in []float64) []float64 {
	if len(in) == 0 {
		return nil
	}
	out := make([]float64, len(in))
	copy(out, in)
	return out
}
