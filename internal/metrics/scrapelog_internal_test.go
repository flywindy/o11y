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

func TestScrapeErrorLogger_NilLoggerDiscards(t *testing.T) {
	l := newScrapeErrorLogger(nil)
	assert.NotPanics(t, func() { l.Println("ignored") })
}

func TestIsReservedAttributeKey(t *testing.T) {
	reserved := []string{
		"service.name", "service_name", "service-name",
		"service.namespace", "service.version", "deployment.environment.name",
		"otel.scope.name", "otel_scope_version", "otel.scope.schema_url",
	}
	for _, k := range reserved {
		assert.Truef(t, IsReservedAttributeKey(attribute.Key(k)), "%q should be reserved", k)
	}
	for _, k := range []string{"http.route", "service.instance.id", "chat.room.id", "servicename", ""} {
		assert.Falsef(t, IsReservedAttributeKey(attribute.Key(k)), "%q should not be reserved", k)
	}
}
