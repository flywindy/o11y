package mongo

import (
	"context"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// driverConnectionID matches the identifier the v2 driver builds in
// x/mongo/driver/topology/connection.go: the server address followed by a
// bracketed, signed, process-global connection counter.
var driverConnectionID = regexp.MustCompile(`^.+\[-\d+\]$`)

// TestDriverConnectionIDFormat pins the upstream shape this package normalizes
// against. Synthetic fixtures cannot do that job: they were carrying a bare
// "host:port" — a shape the driver never emits — which is exactly why the
// counter leaking into network.peer.address went unnoticed. If a driver
// upgrade changes the identifier, this test says so instead of the change
// quietly reaching a metric label.
func TestDriverConnectionIDFormat(t *testing.T) {
	fake := startFakeMongod(t)

	var mu sync.Mutex
	var ids []string
	opts := options.Client().ApplyURI(fake.uri())
	opts.SetMonitor(&event.CommandMonitor{
		Succeeded: func(_ context.Context, evt *event.CommandSucceededEvent) {
			mu.Lock()
			defer mu.Unlock()
			ids = append(ids, evt.ConnectionID)
		},
	})

	client := connectFake(t, opts)
	pingConcurrently(t, client, 8)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, ids)

	distinct := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		assert.Regexpf(t, driverConnectionID, id, "driver connection identifier changed shape: %q", id)
		distinct[id] = struct{}{}
	}
	assert.Greater(t, len(distinct), 1,
		"expected concurrent commands to span several connections; the fixture no longer exercises pool growth")
}

// TestInstrument_PeerAddressBoundedAgainstRealDriver is the end-to-end form of
// the cardinality regression: several real connections to one server must
// produce exactly one attribute set, carrying the listener's real host and
// port rather than a connection identifier and otelmongo's hardcoded 27017.
func TestInstrument_PeerAddressBoundedAgainstRealDriver(t *testing.T) {
	fake := startFakeMongod(t)
	host, port := fake.hostPort()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)

	opts := options.Client().ApplyURI(fake.uri())
	cleanup, err := Instrument(opts, tracenoop.NewTracerProvider(), provider, propagation.TraceContext{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanup(context.Background()) })

	client := connectFake(t, opts)
	pingConcurrently(t, client, 8)

	metric := findMetric(t, collectMongoMetrics(t, reader), "db.client.operation.duration")
	histogram, ok := metric.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "expected MongoDB operation duration histogram")

	pings := make([]metricdata.HistogramDataPoint[float64], 0, 1)
	for _, dp := range histogram.DataPoints {
		if operation, ok := dp.Attributes.Value(semconv.DBOperationNameKey); ok && operation.AsString() == "ping" {
			pings = append(pings, dp)
		}
	}
	require.Len(t, pings, 1, "every connection to one server must share one attribute set")

	attrs := pings[0].Attributes.ToSlice()
	assert.Contains(t, attrs, semconv.NetworkPeerAddress(host))
	assert.Contains(t, attrs, semconv.NetworkPeerPort(port))
	for _, attr := range attrs {
		assertBoundedLabelValue(t, attr)
	}
}

// assertBoundedLabelValue fails on a label value that carries the driver's
// per-connection decoration, whichever attribute it lands on.
func assertBoundedLabelValue(t *testing.T, attr attribute.KeyValue) {
	t.Helper()

	if attr.Value.Type() != attribute.STRING {
		return
	}
	assert.NotContainsf(t, attr.Value.AsString(), "[",
		"metric label %q carries a per-connection identifier", attr.Key)
}

func connectFake(t *testing.T, opts *options.ClientOptions) *drivermongo.Client {
	t.Helper()

	client, err := drivermongo.Connect(opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return client
}

// pingConcurrently issues n pings at once so the driver has to grow its pool
// past a single connection.
func pingConcurrently(t *testing.T, client *drivermongo.Client, n int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- client.Database("admin").RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
