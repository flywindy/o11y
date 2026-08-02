package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// The cap-rule builders are the single place both export paths agree on which
// attributes are bounded. The Prometheus path is covered end-to-end by
// TestInitMeter_MaxUniqueCollections*; these tests pin the OTLP path, whose
// rules are keyed on instrument name and attribute key rather than on the
// rendered family and label names.

func TestOTLPCapRulesIncludeCassandraCollectionCap(t *testing.T) {
	rules := otlpCapRules(Config{MaxUniqueRoutes: 1000, MaxUniqueCollections: 200})

	byInstrument := map[string]int{}
	for _, r := range rules {
		if r.Key == semconv.DBCollectionNameKey {
			byInstrument[r.InstrumentName] = r.Max
		}
	}
	assert.Equal(t, map[string]int{
		"db.client.operation.duration": 200,
		"cassandra.query.attempts":     200,
	}, byInstrument)
}

// Every collection rule must name the Cassandra scope and share one budget.
// db.client.operation.duration is the standard semconv instrument name, also
// emitted by Redis/MongoDB and by caller-defined instrumentation, so an
// unscoped rule would rewrite foreign streams; independent budgets would let the
// two instruments disagree about which tables overflowed.
func TestCassandraCapRulesAreScopedAndShareABudget(t *testing.T) {
	cfg := Config{MaxUniqueRoutes: 1000, MaxUniqueCollections: 200}

	for _, r := range otlpCapRules(cfg) {
		if r.Key != semconv.DBCollectionNameKey {
			continue
		}
		assert.Equal(t, cassandraScope, r.ScopeName, "OTLP rule for %s must be scoped", r.InstrumentName)
		assert.Equal(t, cassandraCollectionBudget, r.BudgetKey, "OTLP rule for %s must share the budget", r.InstrumentName)
	}
	for _, r := range prometheusCapRules(cfg) {
		if r.LabelName != "db_collection_name" {
			continue
		}
		assert.Equal(t, cassandraScope, r.ScopeName, "Prometheus rule for %s must be scoped", r.MetricName)
		assert.Equal(t, cassandraCollectionBudget, r.BudgetKey, "Prometheus rule for %s must share the budget", r.MetricName)
	}

	// The route caps stay unscoped: http.server/client.request.duration are
	// emitted by several instrumentations the SDK legitimately caps together.
	for _, r := range otlpCapRules(cfg) {
		if r.Key == semconv.HTTPRouteKey {
			assert.Empty(t, r.ScopeName, "route cap must keep matching every scope")
		}
	}
}

// The SDK cardinality limit is one global per-stream guard, so it must cover
// every capped dimension. Deriving it from routes alone lets a low
// WithMaxUniqueRoutes collapse Cassandra table series into otel.metric.overflow
// while they are still inside MaxUniqueCollections.
func TestCardinalityLimitBudgetCoversBothDimensions(t *testing.T) {
	routeOnly := cardinalityLimitBudget(1000, 0)
	collectionOnly := cardinalityLimitBudget(0, 200)
	assert.Positive(t, routeOnly)
	assert.Positive(t, collectionOnly)

	// A tiny route cap must not shrink the budget below what the collection cap
	// needs; the limit is the larger of the two.
	assert.Equal(t, collectionOnly, cardinalityLimitBudget(1, 200),
		"a low route cap must not starve the collection dimension")
	// At the shipped defaults the route budget dominates, so this is a no-op.
	assert.Equal(t, routeOnly, cardinalityLimitBudget(1000, 200))
	// Both disabled means no limit is installed.
	assert.Zero(t, cardinalityLimitBudget(0, 0))
	// Saturates instead of overflowing.
	assert.Positive(t, cardinalityLimitBudget(int(^uint(0)>>1), 0))
}

// Each cap is independent: configuring one must not install or suppress the
// other, so a caller can bound tables without bounding routes and vice versa.
func TestCapRulesAreIndependent(t *testing.T) {
	routesOnly := otlpCapRules(Config{MaxUniqueRoutes: 10})
	assert.Len(t, routesOnly, 2)
	for _, r := range routesOnly {
		assert.Equal(t, semconv.HTTPRouteKey, r.Key)
	}

	collectionsOnly := otlpCapRules(Config{MaxUniqueCollections: 10})
	assert.Len(t, collectionsOnly, 2)
	for _, r := range collectionsOnly {
		assert.Equal(t, semconv.DBCollectionNameKey, r.Key)
	}

	assert.Empty(t, otlpCapRules(Config{}))
	assert.Empty(t, prometheusCapRules(Config{}))
}

// The two export paths must cap the same set of instruments — a rule present on
// one path only would mean the label is bounded for Prometheus users but not for
// OTLP users (or the reverse).
func TestCapRulePathsCoverTheSameInstruments(t *testing.T) {
	cfg := Config{MaxUniqueRoutes: 10, MaxUniqueCollections: 10}
	assert.Len(t, prometheusCapRules(cfg), len(otlpCapRules(cfg)))

	families := map[string]bool{}
	for _, r := range prometheusCapRules(cfg) {
		families[r.MetricName] = true
	}
	for _, inst := range cassandraCollectionInstruments {
		assert.True(t, families[inst.family],
			"Prometheus path is missing a cap rule for %s", inst.instrument)
	}
}
