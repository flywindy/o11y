package cassandra

import "go.opentelemetry.io/otel/attribute"

// Option configures Cassandra instrumentation behavior.
type Option func(*config)

type config struct {
	queryTextEnabled      bool
	hostAttributesEnabled bool
	attrs                 []attribute.KeyValue
	poolName              string
	// collectionMetricLabelDisabled is stored negated because the label is on by
	// default: the zero-value config then means "shipped defaults", so a config
	// literal built in a test cannot silently disagree with what NewSession does.
	collectionMetricLabelDisabled bool
}

// collectionMetricLabel reports whether db.collection.name is recorded as a
// metric label. See WithCollectionMetricLabel.
func (c config) collectionMetricLabel() bool { return !c.collectionMetricLabelDisabled }

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

// WithCollectionMetricLabel controls whether db.collection.name (the addressed
// table) is recorded as a label on db.client.operation.duration and
// cassandra.query.attempts.
//
// The default is true (ADR 0019 §7, 2026-07-29 amendment). semconv v1.39.0 marks
// db.collection.name Conditionally Required on db.client.operation.duration "if
// readily available and if a database call is performed on a single collection",
// and both conditions hold for Cassandra: the table is already parsed for the
// span, and CQL has no joins. Per-table breakdown is also the only way the
// client-side signals join to the server-side cassandra-exporter, whose
// table-level metrics carry keyspace/table labels, and the only way
// cassandra.query.attempts can answer which table is driving retries — a
// question spans cannot answer reliably, because traces are sampled and metrics
// are not.
//
// The label is omitted per-observation when no single table can be resolved (an
// unparsed statement, or a batch spanning several tables), which is the semconv
// condition failing rather than a cardinality guard.
//
// Pass false to keep the metric label set as it was before that amendment. Two
// backstops bound the label regardless: the MetricViews allow-keys filter, and
// o11y.WithMaxUniqueCollections, which collapses distinct table values beyond
// its cap to "other" at the export boundary.
func WithCollectionMetricLabel(enabled bool) Option {
	return func(cfg *config) {
		cfg.collectionMetricLabelDisabled = !enabled
	}
}

// WithAttributes appends extra attributes to every emitted Cassandra span.
//
// The SDK's built-in semantic-convention attributes (db.system.name,
// db.namespace, db.operation.name, db.collection.name, db.query.text,
// server.address, server.port, error.type, etc.) are reserved: a supplied
// attribute reusing one of those keys is always dropped, even when the SDK only
// emits that key conditionally (e.g. db.query.text is gated by WithQueryText, and
// error.type only on failures), so WithAttributes can never override the
// package's contract or smuggle in a gated key.
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
// gocql exposes no pool identifier. semconv (v1.39.0) marks the pool name
// Recommended and directs instrumentation that lacks one to synthesize a unique
// value, so when omitted the SDK derives a stable name shaped as
// "cassandra/<server.address>:<server.port>/<keyspace>" — the db.system prefix
// plus semconv's suggested "server.address:server.port/db.namespace" form (e.g.
// "cassandra/10.0.0.1:9042/chat"). Set this explicitly to disambiguate sessions
// that share both a contact point and a keyspace, which would otherwise share
// the synthesized name.
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
	// Drop any package-owned semconv keys a caller passed via WithAttributes so
	// they cannot override the built-ins or smuggle in conditionally-emitted keys.
	cfg.attrs = filterReservedAttrs(cfg.attrs)
	return cfg
}
