package o11y

import "log/slog"

// DefaultMetricsAddr is the default listen address for the built-in
// Prometheus /metrics HTTP server.
var DefaultMetricsAddr = ":2112"

// DefaultLatencyBuckets is the SLO-friendly histogram boundary set applied
// to all http.server.* histograms when the caller does not override it.
// Standardizing these boundaries across the company keeps P99 calculations
// directly comparable between services.
var DefaultLatencyBuckets = []float64{
	.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10,
}

// Config defines the configuration for the o11y SDK.
type Config struct {
	serviceName    string
	serviceVersion string
	environment    string
	otlpEndpoint   string
	logLevel       slog.Level

	// Metrics
	metricsAddr      string
	runtimeMetrics   bool
	histogramBuckets []float64
	team             string
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

// WithOTLPEndpoint sets the OTLP/HTTP collector endpoint.
func WithOTLPEndpoint(endpoint string) Option {
	return func(c *Config) {
		c.otlpEndpoint = endpoint
	}
}

// WithLogLevel returns an Option that sets the minimum logging level for the SDK.
func WithLogLevel(level slog.Level) Option {
	return func(c *Config) {
		c.logLevel = level
	}
}

// WithTeam sets the owning team label applied to every metric emitted by
// the SDK. It is required: Init returns an error when it is empty. The
// value becomes a constant Prometheus label (team="...") on every series,
// WithTeam returns an Option that sets the team field on a Config.
// The team value is used by SRE for alert routing and governance.
func WithTeam(team string) Option {
	return func(c *Config) {
		c.team = team
	}
}

// WithMetricsAddr overrides the listen address of the built-in Prometheus
// WithMetricsAddr returns an Option that sets the metrics HTTP server listen address to the provided addr.
// If not set, the metrics server defaults to DefaultMetricsAddr (":2112").
func WithMetricsAddr(addr string) Option {
	return func(c *Config) {
		c.metricsAddr = addr
	}
}

// WithRuntimeMetrics toggles collection of Go runtime metrics (goroutines,
// GC, memory, etc.) via the OTel runtime instrumentation. Defaults to true,
// WithRuntimeMetrics sets whether collection of runtime-derived metrics is enabled.
// When enabled, runtime metrics (e.g., goroutines, memory, GC) are collected and exposed to support saturation monitoring as expected by SRE.
func WithRuntimeMetrics(enabled bool) Option {
	return func(c *Config) {
		c.runtimeMetrics = enabled
	}
}

// WithHistogramBuckets overrides the histogram boundaries applied to HTTP
// server latency histograms. Defaults to DefaultLatencyBuckets; override
// only when your service has a genuinely different latency profile, since
// WithHistogramBuckets returns an Option that sets the histogram bucket boundaries used for latency histograms.
// Changing these from the package default will make cross-service P99 comparisons inconsistent.
func WithHistogramBuckets(buckets []float64) Option {
	return func(c *Config) {
		c.histogramBuckets = buckets
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
		histogramBuckets: DefaultLatencyBuckets,
	}
}
