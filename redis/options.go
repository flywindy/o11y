package redis

import "go.opentelemetry.io/otel/attribute"

// Option configures Redis instrumentation behavior.
type Option func(*config)

type config struct {
	commandTextEnabled bool
	attrs              []attribute.KeyValue
	poolName           string
}

// WithCommandTextEnabled controls whether the full Redis command text is
// recorded as db.query.text on spans.
//
// The default is false because Redis commands commonly contain sensitive key
// names or values. When enabled, command text is truncated to 1 KiB.
func WithCommandTextEnabled(enabled bool) Option {
	return func(cfg *config) {
		cfg.commandTextEnabled = enabled
	}
}

// WithAttributes appends extra attributes to every emitted Redis span.
//
// These attributes are never applied to metric samples; Redis metric labels are
// fixed by the SDK to keep cardinality bounded.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(cfg *config) {
		cfg.attrs = append(cfg.attrs, attrs...)
	}
}

// WithPoolName sets the application-defined connection-pool name used on Redis
// pool metric samples.
//
// When omitted, Wrap synthesizes a process-unique name from the wrapped client
// pointer. Cluster and Ring clients append each shard address to the base name.
func WithPoolName(name string) Option {
	return func(cfg *config) {
		cfg.poolName = name
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
