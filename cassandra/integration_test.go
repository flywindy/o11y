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

	// Force span export.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFlush()
	require.NoError(t, tp.ForceFlush(flushCtx))
	spans := sr.Ended()
	require.NotEmpty(t, spans)

	var sawCassandra, sawBatch bool
	for _, span := range spans {
		for _, a := range span.Attributes() {
			if a.Key == semconv.DBSystemNameKey && a.Value.AsString() == "cassandra" {
				sawCassandra = true
			}
			if a.Key == semconv.DBOperationNameKey && a.Value.AsString() == "BATCH" {
				sawBatch = true
			}
		}
	}
	assert.True(t, sawCassandra, "expected at least one cassandra span")
	assert.True(t, sawBatch, "expected a batch span from ExecuteBatch")
}
