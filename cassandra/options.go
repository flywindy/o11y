package cassandra

import "go.opentelemetry.io/otel/attribute"

// Option configures Cassandra instrumentation behavior.
type Option func(*config)

type config struct {
	queryTextEnabled bool
	attrs            []attribute.KeyValue
}

// WithQueryText controls whether the CQL statement text is recorded as
// db.query.text on spans.
//
// The default is false (ADR 0019 §6). CQL statements are parameterized (bound
// values are sent separately and are never captured here), so the statement
// text is low-PII, but it can still be high-cardinality and reveal schema and
// table topology. Services that want the statement on spans opt in explicitly.
func WithQueryText(enabled bool) Option {
	return func(cfg *config) {
		cfg.queryTextEnabled = enabled
	}
}

// WithAttributes appends extra attributes to every emitted Cassandra span.
//
// The SDK's built-in semantic-convention attributes (db.system.name,
// db.namespace, db.operation.name, db.collection.name, server.address,
// server.port, etc.) take precedence: if a supplied attribute reuses one of
// those keys, the built-in value wins and the supplied one is dropped.
//
// These attributes are never applied to metric samples; Cassandra metric labels
// are fixed by the SDK to keep cardinality bounded.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(cfg *config) {
		cfg.attrs = append(cfg.attrs, attrs...)
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
