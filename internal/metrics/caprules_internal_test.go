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
