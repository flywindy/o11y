package metrics

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/protobuf/proto"
)

// partialGatherer returns one healthy family together with a gather error,
// the shape client_golang produces when one family fails validation.
type partialGatherer struct{}

func (partialGatherer) Gather() ([]*dto.MetricFamily, error) {
	fam := &dto.MetricFamily{
		Name: proto.String("healthy_total"),
		Type: dto.MetricType_COUNTER.Enum(),
		Metric: []*dto.Metric{{
			Counter: &dto.Counter{Value: proto.Float64(1)},
		}},
	}
	return []*dto.MetricFamily{fam}, errors.New(
		`collected metric "app_collision_seconds" ... duplicate label names in constant and variable labels`)
}

var _ prometheus.Gatherer = partialGatherer{}

// TestMetricsHandler_ServesHealthyFamiliesOnGatherError pins the whole point
// of ContinueOnError: a gather error costs the broken family, not the scrape.
func TestMetricsHandler_ServesHealthyFamiliesOnGatherError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	h := newMetricsHandler(partialGatherer{}, true, logger)

	scrape := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec
	}

	rec := scrape()
	assert.Equal(t, http.StatusOK, rec.Code, "a gather error must not fail the scrape")
	assert.Contains(t, rec.Body.String(), "healthy_total 1")

	assert.Equal(t, 1, strings.Count(logs.String(), "metrics scrape served with errors"))
	assert.Contains(t, logs.String(), "duplicate label names")

	// The same error on the next scrape is suppressed inside the window.
	scrape()
	assert.Equal(t, 1, strings.Count(logs.String(), "metrics scrape served with errors"),
		"an identical error within the repeat window must not be logged again")
}

func TestScrapeErrorLogger_RepeatsAfterWindow(t *testing.T) {
	var logs bytes.Buffer
	l := newScrapeErrorLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }

	l.Println("error gathering metrics:", "boom")
	l.Println("error gathering metrics:", "boom")
	require.Equal(t, 1, strings.Count(logs.String(), "boom"))

	// A different error is logged immediately.
	l.Println("error gathering metrics:", "other")
	require.Equal(t, 1, strings.Count(logs.String(), "other"))

	// The first error returns once the window has elapsed.
	now = now.Add(scrapeErrorRepeatWindow)
	l.Println("error gathering metrics:", "boom")
	assert.Equal(t, 2, strings.Count(logs.String(), "boom"))
}

// TestScrapeErrorLogger_TracksEachMessageSeparately pins that alternating
// errors — two broken families failing on successive scrapes — are each
// logged once per window rather than on every scrape.
func TestScrapeErrorLogger_TracksEachMessageSeparately(t *testing.T) {
	var logs bytes.Buffer
	l := newScrapeErrorLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		l.Println("error gathering metrics:", "family-a")
		l.Println("error gathering metrics:", "family-b")
		now = now.Add(15 * time.Second)
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "family-a"))
	assert.Equal(t, 1, strings.Count(logs.String(), "family-b"))
}

// TestScrapeErrorLogger_TableIsBounded pins the memory guard: the table never
// holds more than maxTrackedScrapeErrors entries, and the evicted (oldest)
// message is logged again when it recurs.
func TestScrapeErrorLogger_TableIsBounded(t *testing.T) {
	var logs bytes.Buffer
	l := newScrapeErrorLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < maxTrackedScrapeErrors+8; i++ {
		l.Println("error gathering metrics:", "family", i)
		now = now.Add(time.Second)
	}
	assert.LessOrEqual(t, len(l.lastSeen), maxTrackedScrapeErrors)

	l.Println("error gathering metrics:", "family", 0) // evicted, so logged again
	assert.Equal(t, 2, strings.Count(logs.String(), `metrics: family 0"`))
}

func TestScrapeErrorLogger_NilLoggerDiscards(t *testing.T) {
	l := newScrapeErrorLogger(nil)
	assert.NotPanics(t, func() { l.Println("ignored") })
}

func TestIsReservedAttributeKey(t *testing.T) {
	reserved := []string{
		"service.name", "service_name", "service-name",
		"service.namespace", "service.version", "deployment.environment.name",
		"otel.scope.name", "otel_scope_version", "otel.scope.schema_url",
		// otelprom renders every instrumentation-scope attribute as
		// otel_scope_<attr>, so the whole prefix is reserved.
		"otel.scope.foo", "otel_scope_deployment", "otel.scope.x.y",
	}
	for _, k := range reserved {
		assert.Truef(t, IsReservedAttributeKey(attribute.Key(k)), "%q should be reserved", k)
	}
	notReserved := []string{
		"http.route", "service.instance.id", "chat.room.id", "servicename", "",
		"otel.scope", "otel.scopes.name", "otel.library.name", "otelscope.name",
	}
	for _, k := range notReserved {
		assert.Falsef(t, IsReservedAttributeKey(attribute.Key(k)), "%q should not be reserved", k)
	}
}

// TestIsReservedAttributeKey_NoAllocations pins that the hot-path filter does
// not allocate, for keys that are reserved and for the far more common keys
// that are not.
func TestIsReservedAttributeKey_NoAllocations(t *testing.T) {
	keys := []attribute.Key{
		"http.route", "db.system.name", "server.address", "outcome",
		"service.name", "service-namespace", "otel.scope.name", "otel.scope.foo", "deployment.environment.name",
	}
	allocs := testing.AllocsPerRun(1000, func() {
		for _, k := range keys {
			IsReservedAttributeKey(k)
		}
	})
	assert.Zero(t, allocs, "IsReservedAttributeKey must not allocate on the recording path")
}

// TestNormalizedEqualsMatchesNormalizer pins the allocation-free comparison
// against the materializing normalizer across the key shapes it must agree on.
func TestNormalizedEqualsMatchesNormalizer(t *testing.T) {
	keys := []string{
		"service.name", "service_name", "service-name", "service..name", "service__name",
		"service.name.", ".service.name", "service.nam", "service.names", "servicename",
		"Service.Name", "otel.scope.schema_url", "otel/scope/schema/url", "deployment.environment.name",
		"1service.name", "", "s", "_",
	}
	targets := []string{"service_name", "otel_scope_schema_url", "deployment_environment_name"}
	for _, k := range keys {
		for _, want := range targets {
			assert.Equalf(t, NormalizePrometheusLabelName(k) == want, normalizedEquals(k, want),
				"normalizedEquals(%q, %q)", k, want)
		}
	}
	for _, k := range []string{"otel.scope.name", "otel.scope.foo", "otel..scope..x", "otel.scope", "otel.scopes.x", "otel_scope_", "o", ""} {
		assert.Equalf(t, strings.HasPrefix(NormalizePrometheusLabelName(k), scopeLabelPrefix),
			normalizedHasPrefix(k, scopeLabelPrefix), "normalizedHasPrefix(%q)", k)
	}
}
