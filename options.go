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

// WithLogLevel sets the minimum logging level.
func WithLogLevel(level slog.Level) Option {
	return func(c *Config) {
		c.logLevel = level
	}
}

// WithTeam sets the owning team label applied to every metric emitted by
// the SDK. It is required: Init returns an error when it is empty. The
// value becomes a constant Prometheus label (team="...") on every series,
// which SRE uses for alert routing and governance.
func WithTeam(team string) Option {
	return func(c *Config) {
		c.team = team
	}
}

// WithMetricsAddr overrides the listen address of the built-in Prometheus
// /metrics HTTP server. Defaults to DefaultMetricsAddr (":2112").
func WithMetricsAddr(addr string) Option {
	return func(c *Config) {
		c.metricsAddr = addr
	}
}

// WithRuntimeMetrics toggles collection of Go runtime metrics (goroutines,
// GC, memory, etc.) via the OTel runtime instrumentation. Defaults to true,
// which is what SRE expects for the Saturation golden signal.
func WithRuntimeMetrics(enabled bool) Option {
	return func(c *Config) {
		c.runtimeMetrics = enabled
	}
}

// WithHistogramBuckets overrides the histogram boundaries applied to HTTP
// server latency histograms. Defaults to DefaultLatencyBuckets; override
// only when your service has a genuinely different latency profile, since
// diverging from the default breaks cross-service P99 comparisons.
func WithHistogramBuckets(buckets []float64) Option {
	return func(c *Config) {
		c.histogramBuckets = buckets
	}
}

func defaultConfig() *Config {
	return &Config{
		otlpEndpoint:     "http://localhost:4318",
		logLevel:         slog.LevelInfo,
		metricsAddr:      DefaultMetricsAddr,
		runtimeMetrics:   true,
		histogramBuckets: DefaultLatencyBuckets,
	}
}
