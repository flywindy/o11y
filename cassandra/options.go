package cassandra

import "go.opentelemetry.io/otel/attribute"

// Option configures Cassandra instrumentation behavior.
type Option func(*config)

type config struct {
	queryTextEnabled      bool
	hostAttributesEnabled bool
	attrs                 []attribute.KeyValue
	poolName              string
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

// WithHostAttributes controls whether the contacted-coordinator host topology
// is recorded on query spans: network.peer.address / network.peer.port and
// cassandra.coordinator.id / cassandra.coordinator.dc.
//
// The default is false. These attributes are Opt-In (ADR 0019 §5): the package
// leads with the conformant server.* contact-point keys, and the per-node
// address/UUID are kept off by default since some deployments treat internal
// host topology as sensitive. Enable this to make token-aware routing and the
// coordinating node visible per span. They are never emitted as metric labels.
func WithHostAttributes(enabled bool) Option {
	return func(cfg *config) {
		cfg.hostAttributesEnabled = enabled
	}
}

// WithPoolName sets the application-defined connection-pool name reported as
// db.client.connection.pool.name on Cassandra connection metrics
// (db.client.connection.create_time and cassandra.connection.attempts).
//
// gocql exposes no pool identifier, and semconv treats the pool name as a
// required label on connection metrics, so when omitted the SDK synthesizes a
// stable name from the configured contact point (e.g. "cassandra/10.0.0.1:9042").
// Set this explicitly to disambiguate multiple sessions that target the same
// contact point, which would otherwise share the synthesized name.
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
