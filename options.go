package o11y

import "log/slog"

// Config defines the configuration for the o11y SDK.
type Config struct {
	serviceName  string
	environment  string
	otlpEndpoint string
	logLevel     slog.Level
}

// Option is a functional option for configuring the o11y SDK.
type Option func(*Config)

// WithServiceName sets the service name for trace resource attributes.
func WithServiceName(name string) Option {
	return func(c *Config) {
		c.serviceName = name
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

func defaultConfig() *Config {
	return &Config{
		otlpEndpoint: "http://localhost:4318",
		logLevel:     slog.LevelInfo,
	}
}
