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

func TestOTLPCapRulesIncludeCollectionCaps(t *testing.T) {
	rules := otlpCapRules(Config{MaxUniqueRoutes: 1000, MaxUniqueCollections: 200})

	byScopedInstrument := map[string]int{}
	for _, r := range rules {
		if r.Key == semconv.DBCollectionNameKey {
			byScopedInstrument[r.ScopeName+"|"+r.InstrumentName] = r.Max
		}
	}
	assert.Equal(t, map[string]int{
		cassandraScope + "|db.client.operation.duration":     200,
		cassandraScope + "|cassandra.query.attempts":         200,
		elasticsearchScope + "|db.client.operation.duration": 200,
	}, byScopedInstrument)
}

// Every collection rule must name its integration's scope and share that
// integration's budget. db.client.operation.duration is the standard semconv
// instrument name, also emitted by Redis/MongoDB and by caller-defined
// instrumentation, so an unscoped rule would rewrite foreign streams;
// independent budgets within one integration would let its instruments disagree
// about which collections overflowed, while budgets must stay separate across
// integrations so an Elasticsearch index overflow cannot evict Cassandra tables.
func TestCollectionCapRulesAreScopedAndBudgetedPerIntegration(t *testing.T) {
	cfg := Config{MaxUniqueRoutes: 1000, MaxUniqueCollections: 200}
	budgets := map[string]string{
		cassandraScope:     cassandraCollectionBudget,
		elasticsearchScope: elasticsearchCollectionBudget,
	}
	assert.NotEqual(t, cassandraCollectionBudget, elasticsearchCollectionBudget,
		"integrations must not share one collection budget")

	for _, r := range otlpCapRules(cfg) {
		if r.Key != semconv.DBCollectionNameKey {
			continue
		}
		want, ok := budgets[r.ScopeName]
		assert.True(t, ok, "OTLP rule for %s must be scoped to a known integration, got %q", r.InstrumentName, r.ScopeName)
		assert.Equal(t, want, r.BudgetKey, "OTLP rule for %s/%s must share its integration's budget", r.ScopeName, r.InstrumentName)
	}
	for _, r := range prometheusCapRules(cfg) {
		if r.LabelName != "db_collection_name" {
			continue
		}
		want, ok := budgets[r.ScopeName]
		assert.True(t, ok, "Prometheus rule for %s must be scoped to a known integration, got %q", r.MetricName, r.ScopeName)
		assert.Equal(t, want, r.BudgetKey, "Prometheus rule for %s/%s must share its integration's budget", r.ScopeName, r.MetricName)
	}

	// The route caps stay unscoped: http.server/client.request.duration are
	// emitted by several instrumentations the SDK legitimately caps together.
	for _, r := range otlpCapRules(cfg) {
		if r.Key == semconv.HTTPRouteKey {
			assert.Empty(t, r.ScopeName, "route cap must keep matching every scope")
		}
	}
}

// The SDK cardinality limit is one global per-stream guard that also bounds
// application instruments, so it must be a real ceiling: a floor of the OTel
// default, a small multiple of each export cap on top, never the theoretical
// method × status envelope that made it 1,024,000 before.
func TestCardinalityLimitBudget(t *testing.T) {
	// Shipped defaults: 4 × 1000 routes.
	assert.Equal(t, 4000, cardinalityLimitBudget(1000, 200, 0))
	// The floor holds when the caps are lowered or disabled, so a low route
	// cap neither starves the collection dimension nor drops below OTel's own
	// default for application instruments.
	assert.Equal(t, DefaultCardinalityLimit, cardinalityLimitBudget(1, 200, 0))
	assert.Equal(t, DefaultCardinalityLimit, cardinalityLimitBudget(0, 0, 0))
	// The larger cap wins.
	assert.Equal(t, 8000, cardinalityLimitBudget(100, 2000, 0))
	// An explicit override replaces the derivation entirely.
	assert.Equal(t, 100, cardinalityLimitBudget(1000, 200, 100))
	assert.Equal(t, 50000, cardinalityLimitBudget(1000, 200, 50000))
	// Saturates instead of overflowing.
	assert.Positive(t, cardinalityLimitBudget(int(^uint(0)>>1), 0, 0))
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
	assert.Len(t, collectionsOnly, 3) // two Cassandra instruments + one Elasticsearch
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
	for _, sc := range collectionCapScopes {
		for _, inst := range sc.instruments {
			assert.True(t, families[inst.family],
				"Prometheus path is missing a cap rule for %s/%s", sc.scope, inst.instrument)
		}
	}
}
