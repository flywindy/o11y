//go:build integration

package cassandra

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// TestIntegrationHealthyPath exercises NewSession against a real Cassandra. It
// is build-tagged out of the default `go test ./...` run and requires a running
// node addressed by CASSANDRA_ADDR (e.g. 127.0.0.1:9042), matching the
// consumer's own integration-test posture (ADR 0019 Testing). Run with:
//
//	CASSANDRA_ADDR=127.0.0.1:9042 go test -tags=integration ./cassandra/
func TestIntegrationHealthyPath(t *testing.T) {
	addr := os.Getenv("CASSANDRA_ADDR")
	if addr == "" {
		t.Skip("set CASSANDRA_ADDR to run the Cassandra integration test")
	}

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testBuckets)...),
	)

	cluster := gocql.NewCluster(addr)
	cluster.ConnectTimeout = 10 * time.Second
	cluster.Timeout = 10 * time.Second

	session, err := NewSession(cluster, tp, mp)
	require.NoError(t, err)
	defer session.Close()

	const ks = "o11y_cassandra_it"
	require.NoError(t, session.Query(
		`CREATE KEYSPACE IF NOT EXISTS `+ks+
			` WITH replication = {'class':'SimpleStrategy','replication_factor':1}`,
	).Exec())
	require.NoError(t, session.Query(
		`CREATE TABLE IF NOT EXISTS `+ks+`.rooms (id text PRIMARY KEY, name text)`,
	).Exec())

	require.NoError(t, session.Query(
		`INSERT INTO `+ks+`.rooms (id, name) VALUES (?, ?)`, "r1", "general",
	).Exec())

	var name string
	require.NoError(t, session.Query(
		`SELECT name FROM `+ks+`.rooms WHERE id = ?`, "r1",
	).Scan(&name))
	assert.Equal(t, "general", name)

	batch := session.NewBatch(gocql.LoggedBatch)
	batch.Query(`INSERT INTO `+ks+`.rooms (id, name) VALUES (?, ?)`, "r2", "random")
	batch.Query(`INSERT INTO `+ks+`.rooms (id, name) VALUES (?, ?)`, "r3", "ops")
	batchCtx, cancelBatch := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelBatch()
	require.NoError(t, ExecuteBatch(batchCtx, session, batch))

	// Conditional (LWT) batch through the CAS seam: must execute, apply, and be
	// instrumented like the plain batch.
	casBatch := session.NewBatch(gocql.LoggedBatch)
	casBatch.Query(`INSERT INTO `+ks+`.rooms (id, name) VALUES (?, ?) IF NOT EXISTS`, "r4", "cas")
	applied, _, err := ExecuteBatchCAS(batchCtx, session, casBatch)
	require.NoError(t, err)
	assert.True(t, applied, "first conditional insert should apply")

	// Map-destination CAS seam: same instrumentation. r4 already exists, so this
	// must report not-applied while still emitting a batch span.
	mapBatch := session.NewBatch(gocql.LoggedBatch)
	mapBatch.Query(`INSERT INTO `+ks+`.rooms (id, name) VALUES (?, ?) IF NOT EXISTS`, "r4", "cas-2")
	mapApplied, _, err := MapExecuteBatchCAS(batchCtx, session, mapBatch, map[string]interface{}{})
	require.NoError(t, err)
	assert.False(t, mapApplied, "conditional insert on an existing row must not apply")

	// Force span export.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFlush()
	require.NoError(t, tp.ForceFlush(flushCtx))
	spans := sr.Ended()
	require.NotEmpty(t, spans)

	var sawCassandra, batchSpans int
	for _, span := range spans {
		for _, a := range span.Attributes() {
			if a.Key == semconv.DBSystemNameKey && a.Value.AsString() == "cassandra" {
				sawCassandra++
			}
			if a.Key == semconv.DBOperationNameKey && a.Value.AsString() == "BATCH" {
				batchSpans++
			}
		}
	}
	assert.Positive(t, sawCassandra, "expected at least one cassandra span")
	// One per seam: ExecuteBatch + ExecuteBatchCAS + MapExecuteBatchCAS. Counting
	// (not a bool) ensures a single broken path cannot pass on another's span.
	assert.GreaterOrEqual(t, batchSpans, 3, "expected a batch span from each of the three batch seams")
}
