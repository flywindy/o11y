package cassandra

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

var testBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

func newTestObserver(t *testing.T, cfg config) (*observer, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
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
	meter := mp.Meter(instrumentationName, metric.WithSchemaURL(semconv.SchemaURL))
	inst, err := newInstruments(meter)
	require.NoError(t, err)
	server := serverAddr{host: "127.0.0.1", port: 9042}
	obs := &observer{
		tracer:   tp.Tracer(instrumentationName, oteltrace.WithSchemaURL(semconv.SchemaURL)),
		inst:     inst,
		cfg:      cfg,
		server:   server,
		poolName: poolName(cfg.poolName, server),
	}
	return obs, sr, reader
}

func testHost(t *testing.T) *gocql.HostInfo {
	t.Helper()
	h := (&gocql.HostInfo{}).SetConnectAddress(net.ParseIP("10.0.0.5"))
	h.SetHostID("coordinator-1")
	return h
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	return rm
}

func metricByName(rm metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	for i := range rm.ScopeMetrics {
		for j := range rm.ScopeMetrics[i].Metrics {
			if rm.ScopeMetrics[i].Metrics[j].Name == name {
				return &rm.ScopeMetrics[i].Metrics[j]
			}
		}
	}
	return nil
}

func spanHasKey(span sdktrace.ReadOnlySpan, key attribute.Key) bool {
	for _, a := range span.Attributes() {
		if a.Key == key {
			return true
		}
	}
	return false
}

func TestNewSessionRejectsNilArgs(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	mp := sdkmetric.NewMeterProvider()
	cluster := gocql.NewCluster("127.0.0.1")

	_, err := NewSession(nil, tp, mp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster config must not be nil")

	_, err = NewSession(cluster, nil, mp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracer provider must not be nil")

	_, err = NewSession(cluster, tp, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "meter provider must not be nil")
}

func TestNewSessionRestoresObserversOnConfig(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	mp := sdkmetric.NewMeterProvider()
	// Unroutable TEST-NET-1 host (RFC 5737) with a short dial timeout so
	// CreateSession fails fast without a live cluster.
	cluster := gocql.NewCluster("192.0.2.1")
	cluster.ConnectTimeout = 100 * time.Millisecond
	cluster.Timeout = 100 * time.Millisecond

	_, err := NewSession(cluster, tp, mp)
	require.Error(t, err)
	// The observers must be restored to the caller's originals (nil here) so a
	// retried NewSession on the same *ClusterConfig does not stack another SDK
	// observer layer and emit duplicate spans/metrics.
	assert.Nil(t, cluster.QueryObserver, "QueryObserver must be restored after CreateSession")
	assert.Nil(t, cluster.ConnectObserver, "ConnectObserver must be restored after CreateSession")
}

func TestExecuteBatchNilGuards(t *testing.T) {
	err := ExecuteBatch(context.Background(), nil, gocql.NewBatch(gocql.LoggedBatch)) //nolint:staticcheck // need a *Batch without a live session
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session must not be nil")

	// A non-nil session pointer with a nil batch must also be rejected before
	// any driver call. The session is never dereferenced on this path.
	err = ExecuteBatch(context.Background(), &gocql.Session{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "batch must not be nil")
}

// recordingQueryObserver / recordingConnectObserver capture whether a
// caller-supplied observer is still invoked after NewSession composes the SDK's.
type recordingQueryObserver struct{ calls int }

func (r *recordingQueryObserver) ObserveQuery(context.Context, gocql.ObservedQuery) { r.calls++ }

type recordingConnectObserver struct{ calls int }

func (r *recordingConnectObserver) ObserveConnect(gocql.ObservedConnect) { r.calls++ }

func TestChainObserversPreservesExisting(t *testing.T) {
	obs, _, _ := newTestObserver(t, config{})

	existingQ := &recordingQueryObserver{}
	chainedQ := chainQueryObserver(existingQ, obs)
	chainedQ.ObserveQuery(context.Background(), gocql.ObservedQuery{Statement: "SELECT id FROM rooms"})
	assert.Equal(t, 1, existingQ.calls, "caller's QueryObserver must still run")

	existingC := &recordingConnectObserver{}
	chainedC := chainConnectObserver(existingC, obs)
	chainedC.ObserveConnect(gocql.ObservedConnect{Start: time.Now(), End: time.Now()})
	assert.Equal(t, 1, existingC.calls, "caller's ConnectObserver must still run")

	// With no existing observer, the SDK observer is returned unwrapped.
	assert.Same(t, obs, chainQueryObserver(nil, obs))
	assert.Same(t, obs, chainConnectObserver(nil, obs))
}

func TestObserveQueryEmitsSemconvSpanAndDuration(t *testing.T) {
	obs, sr, reader := newTestObserver(t, config{})

	start := time.Now()
	obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
		Keyspace:  "chat",
		Statement: "SELECT id, body FROM messages_by_room WHERE room_id = ?",
		Start:     start,
		End:       start.Add(3 * time.Millisecond),
		Rows:      7,
		Host:      testHost(t),
	})

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "cassandra.SELECT messages_by_room", span.Name())
	assert.Equal(t, oteltrace.SpanKindClient, span.SpanKind())
	assert.Equal(t, codes.Unset, span.Status().Code)

	assert.Contains(t, span.Attributes(), semconv.DBSystemNameCassandra)
	assert.Contains(t, span.Attributes(), semconv.DBNamespace("chat"))
	assert.Contains(t, span.Attributes(), semconv.DBOperationName("SELECT"))
	assert.Contains(t, span.Attributes(), semconv.DBCollectionName("messages_by_room"))
	assert.Contains(t, span.Attributes(), semconv.DBResponseReturnedRows(7))
	assert.Contains(t, span.Attributes(), semconv.ServerAddress("127.0.0.1"))
	assert.Contains(t, span.Attributes(), semconv.ServerPort(9042))
	// Host-topology attributes are Opt-In (WithHostAttributes); absent by default.
	assert.False(t, spanHasKey(span, semconv.NetworkPeerAddressKey))
	assert.False(t, spanHasKey(span, semconv.CassandraCoordinatorIDKey))
	assert.False(t, spanHasKey(span, semconv.ErrorTypeKey))
	assert.False(t, spanHasKey(span, semconv.DBQueryTextKey), "query text off by default")

	rm := collectMetrics(t, reader)
	dur := metricByName(rm, "db.client.operation.duration")
	require.NotNil(t, dur)
	hist, ok := dur.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, hist.DataPoints, 1)
	dp := hist.DataPoints[0]
	assert.Equal(t, testBuckets, dp.Bounds)
	// MetricViews allow-keys filter drops cassandra.coordinator.*/network.peer.*
	// and keeps the bounded label set.
	assertHasAttr(t, dp.Attributes, semconv.DBSystemNameCassandra)
	assertHasAttr(t, dp.Attributes, semconv.DBOperationName("SELECT"))
	assertHasAttr(t, dp.Attributes, semconv.DBNamespace("chat"))
	assertHasAttr(t, dp.Attributes, semconv.ServerAddress("127.0.0.1"))
	assertMissingKey(t, dp.Attributes, semconv.NetworkPeerAddressKey)
	assertMissingKey(t, dp.Attributes, semconv.CassandraCoordinatorIDKey)
}

func TestObserveQueryHostAttributesOptIn(t *testing.T) {
	obs, sr, reader := newTestObserver(t, config{hostAttributesEnabled: true})

	start := time.Now()
	obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
		Keyspace:  "chat",
		Statement: "SELECT id FROM messages_by_room WHERE room_id = ?",
		Start:     start,
		End:       start.Add(time.Millisecond),
		Host:      testHost(t),
	})

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	// Enabled: the coordinator host topology now appears on the span.
	assert.Contains(t, span.Attributes(), semconv.NetworkPeerAddress("10.0.0.5"))
	assert.Contains(t, span.Attributes(), semconv.CassandraCoordinatorID("coordinator-1"))

	// ...but it is still filtered out of the operation-duration metric labels.
	rm := collectMetrics(t, reader)
	dp := metricByName(rm, "db.client.operation.duration").Data.(metricdata.Histogram[float64]).DataPoints[0]
	assertMissingKey(t, dp.Attributes, semconv.NetworkPeerAddressKey)
	assertMissingKey(t, dp.Attributes, semconv.CassandraCoordinatorIDKey)
}

func TestObserveQueryRecordsError(t *testing.T) {
	obs, sr, reader := newTestObserver(t, config{})

	obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
		Keyspace:  "chat",
		Statement: "INSERT INTO messages_by_room (room_id, id) VALUES (?, ?)",
		Start:     time.Now(),
		End:       time.Now().Add(time.Millisecond),
		Err:       context.DeadlineExceeded,
		Host:      testHost(t),
	})

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Contains(t, span.Attributes(), semconv.ErrorTypeKey.String("context.DeadlineExceeded"))
	require.NotEmpty(t, span.Events())

	rm := collectMetrics(t, reader)
	dur := metricByName(rm, "db.client.operation.duration")
	require.NotNil(t, dur)
	hist, ok := dur.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, hist.DataPoints, 1)
	assertHasAttr(t, hist.DataPoints[0].Attributes, semconv.ErrorTypeKey.String("context.DeadlineExceeded"))
	assert.Equal(t, "cassandra.INSERT messages_by_room", span.Name())
}

func TestObserveQueryQueryTextOptIn(t *testing.T) {
	const stmt = "SELECT * FROM rooms WHERE id = ?"

	obs, sr, _ := newTestObserver(t, config{queryTextEnabled: true})
	obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
		Statement: stmt,
		Start:     time.Now(),
		End:       time.Now(),
	})
	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Contains(t, span.Attributes(), semconv.DBQueryText(stmt))
}

func TestObserveQueryPerAttemptSpans(t *testing.T) {
	// Enable host attributes so each sibling span carries its coordinator host.
	obs, sr, reader := newTestObserver(t, config{hostAttributesEnabled: true})

	start := time.Now()
	for attempt := 0; attempt < 3; attempt++ {
		obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
			Keyspace:  "chat",
			Statement: "SELECT id FROM messages_by_room WHERE room_id = ?",
			Start:     start,
			End:       start.Add(time.Millisecond),
			Attempt:   attempt,
			Host:      testHost(t),
		})
	}

	spans := sr.Ended()
	require.Len(t, spans, 3, "one span per attempt, not one collapsed logical span")
	for i, span := range spans {
		assert.Contains(t, span.Attributes(), attemptKey.Int(i))
		assert.Contains(t, span.Attributes(), semconv.CassandraCoordinatorID("coordinator-1"))
	}

	// The attempts counter increments by exactly 1 per callback: a 3-attempt
	// same-host sequence records 3, not 1+2+3 (ADR 0019 §7.B).
	rm := collectMetrics(t, reader)
	cnt := metricByName(rm, "cassandra.query.attempts")
	require.NotNil(t, cnt)
	sum := cnt.Data.(metricdata.Sum[int64])
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	assert.Equal(t, int64(3), total)
}

func TestBatchAttrsMirrorQueryPath(t *testing.T) {
	obs, sr, reader := newTestObserver(t, config{})

	// session.NewBatch needs a live session; NewBatch is the only way to build a
	// *gocql.Batch in a unit test without a cluster.
	batch := gocql.NewBatch(gocql.LoggedBatch) //nolint:staticcheck // see comment above
	batch.Query("INSERT INTO messages_by_room (room_id, id) VALUES (?, ?)", "r1", "m1")
	batch.Query("INSERT INTO messages_by_room (room_id, id) VALUES (?, ?)", "r1", "m2")

	attrs := obs.batchAttrs(batch)
	start := time.Now()
	obs.record(context.Background(), spanName("BATCH", ""), "BATCH", "chat", start, start.Add(time.Millisecond), nil, attrs)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "cassandra.BATCH", span.Name())
	assert.Contains(t, span.Attributes(), semconv.DBSystemNameCassandra)
	assert.Contains(t, span.Attributes(), semconv.DBOperationName("BATCH"))
	assert.Contains(t, span.Attributes(), semconv.DBOperationBatchSize(2))

	rm := collectMetrics(t, reader)
	require.NotNil(t, metricByName(rm, "db.client.operation.duration"))
}

func TestObserveConnectMetrics(t *testing.T) {
	obs, _, reader := newTestObserver(t, config{})

	now := time.Now()
	// Successful connect to a discovered node: server.* must reflect that node
	// (10.0.0.5), not the configured contact point (127.0.0.1).
	obs.ObserveConnect(gocql.ObservedConnect{Host: testHost(t), Start: now, End: now.Add(2 * time.Millisecond)})
	obs.ObserveConnect(gocql.ObservedConnect{Start: now, End: now, Err: context.Canceled})

	rm := collectMetrics(t, reader)
	cnt := metricByName(rm, "cassandra.connection.attempts")
	require.NotNil(t, cnt)
	var total int64
	for _, dp := range cnt.Data.(metricdata.Sum[int64]).DataPoints {
		total += dp.Value
	}
	assert.Equal(t, int64(2), total)

	create := metricByName(rm, "db.client.connection.create_time")
	require.NotNil(t, create, "create_time recorded only for successful connects")
	createPoints := create.Data.(metricdata.Histogram[float64]).DataPoints
	require.Len(t, createPoints, 1)
	// Actual contacted node + synthesized pool name from the contact point.
	assertHasAttr(t, createPoints[0].Attributes, semconv.ServerAddress("10.0.0.5"))
	assertHasAttr(t, createPoints[0].Attributes, semconv.DBClientConnectionPoolName("cassandra/127.0.0.1:9042"))
}

func TestPoolNameSynthesisAndOverride(t *testing.T) {
	// Synthesized from the contact point when WithPoolName is not set.
	assert.Equal(t, "cassandra/10.0.0.1:9042", poolName("", serverAddr{host: "10.0.0.1", port: 9042}))
	assert.Equal(t, "cassandra/10.0.0.1", poolName("", serverAddr{host: "10.0.0.1"}))
	assert.Equal(t, "cassandra", poolName("", serverAddr{}))
	// WithPoolName overrides the synthesized value.
	assert.Equal(t, "chat-cluster", poolName("chat-cluster", serverAddr{host: "10.0.0.1", port: 9042}))
}

func assertHasAttr(t *testing.T, set attribute.Set, want attribute.KeyValue) {
	t.Helper()
	v, ok := set.Value(want.Key)
	require.True(t, ok, "missing attribute %s", want.Key)
	assert.Equal(t, want.Value, v)
}

func assertMissingKey(t *testing.T, set attribute.Set, key attribute.Key) {
	t.Helper()
	_, ok := set.Value(key)
	assert.False(t, ok, "attribute %s should have been filtered out", key)
}
