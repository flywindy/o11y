package mongo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	o11yredis "github.com/flywindy/o11y/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	otelmongo "go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func newTestProviders() (oteltrace.TracerProvider, propagation.TextMapPropagator, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	return tp, prop, sr
}

func TestConnect_Validation(t *testing.T) {
	tp, prop, _ := newTestProviders()
	mp := metricnoop.NewMeterProvider()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name    string
		ctx     context.Context
		uri     string
		tp      oteltrace.TracerProvider
		mp      metric.MeterProvider
		prop    propagation.TextMapPropagator
		wantErr string
	}{
		{"nil ctx", nil, "mongodb://localhost:27017", tp, mp, prop, "context must not be nil"},
		{"canceled ctx", canceled, "mongodb://localhost:27017", tp, mp, prop, context.Canceled.Error()},
		{"empty uri", context.Background(), "", tp, mp, prop, "uri must not be empty"},
		{"nil tracer provider", context.Background(), "mongodb://localhost:27017", nil, mp, prop, "tracer provider must not be nil"},
		{"nil meter provider", context.Background(), "mongodb://localhost:27017", tp, nil, prop, "meter provider must not be nil"},
		{"nil propagator", context.Background(), "mongodb://localhost:27017", tp, mp, nil, "propagator must not be nil"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := Connect(tc.ctx, tc.uri, tc.tp, tc.mp, tc.prop)
			require.Error(t, err)
			assert.Nil(t, client)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestConnect_ReturnsPlainDriverClient(t *testing.T) {
	tp, prop, _ := newTestProviders()

	client, err := Connect(context.Background(), "mongodb://127.0.0.1:1", tp, metricnoop.NewMeterProvider(), prop)
	require.NoError(t, err)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.NoError(t, client.Disconnect(shutdownCtx))
}

func TestConnect_InvalidURI(t *testing.T) {
	tp, prop, _ := newTestProviders()

	client, err := Connect(context.Background(), "://garbage", tp, metricnoop.NewMeterProvider(), prop)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.NotContains(t, err.Error(), "://garbage")
}

func TestInstrument_Validation(t *testing.T) {
	tp, prop, _ := newTestProviders()
	mp := metricnoop.NewMeterProvider()
	opts := options.Client()

	cases := []struct {
		name    string
		opts    *options.ClientOptions
		tp      oteltrace.TracerProvider
		mp      metric.MeterProvider
		prop    propagation.TextMapPropagator
		wantErr string
	}{
		{"nil options", nil, tp, mp, prop, "client options must not be nil"},
		{"nil tracer provider", opts, nil, mp, prop, "tracer provider must not be nil"},
		{"nil meter provider", opts, tp, nil, prop, "meter provider must not be nil"},
		{"nil propagator", opts, tp, mp, nil, "propagator must not be nil"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanup, err := Instrument(tc.opts, tc.tp, tc.mp, tc.prop)
			require.Error(t, err)
			assert.Nil(t, cleanup)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestInstrument_ComposesExistingCommandMonitor(t *testing.T) {
	tp, prop, sr := newTestProviders()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)

	var started, succeeded, failed int
	existing := &event.CommandMonitor{
		Started: func(context.Context, *event.CommandStartedEvent) {
			started++
		},
		Succeeded: func(context.Context, *event.CommandSucceededEvent) {
			succeeded++
		},
		Failed: func(context.Context, *event.CommandFailedEvent) {
			failed++
		},
	}
	opts := options.Client().SetMonitor(existing)

	cleanup, err := Instrument(opts, tp, provider, prop)
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	require.NotSame(t, existing, opts.Monitor)

	ctx := context.Background()
	opts.Monitor.Started(ctx, commandStartedEvent(t, "insert", "o11y_test", "events", 1))
	opts.Monitor.Succeeded(ctx, commandSucceededEvent("insert", "o11y_test", 1))
	opts.Monitor.Failed(ctx, commandFailedEvent("find", "o11y_test", 2, errors.New("boom")))

	assert.Equal(t, 1, started)
	assert.Equal(t, 1, succeeded)
	assert.Equal(t, 1, failed)
	assert.NotEmpty(t, sr.Ended(), "composed o11y monitor should still emit spans")
	assert.NotPanics(t, func() {
		assert.NoError(t, cleanup(context.Background()))
	})

	metric := findMetric(t, collectMongoMetrics(t, reader), "db.client.operation.duration")
	histogram, ok := metric.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "expected MongoDB operation duration histogram")
	assert.NotEmpty(t, histogram.DataPoints)
}

// TestInstrument_ComposesExistingPoolMonitorAndRecordsPoolMetrics verifies
// existing PoolMonitor composition and SDK-owned MongoDB pool metric emission.
func TestInstrument_ComposesExistingPoolMonitorAndRecordsPoolMetrics(t *testing.T) {
	tp, prop, _ := newTestProviders()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)

	var existingEvents []string
	opts := options.Client().
		ApplyURI("mongodb://mongo-a:27017").
		SetPoolMonitor(&event.PoolMonitor{
			Event: func(evt *event.PoolEvent) {
				existingEvents = append(existingEvents, evt.Type)
			},
		})

	cleanup, err := Instrument(opts, tp, provider, prop, WithPoolName("chat-mongo"))
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	require.NotNil(t, opts.PoolMonitor)

	emitPoolEvent(opts.PoolMonitor, event.ConnectionPoolCreated, "mongo-a:27017", 0, &event.MonitorPoolOptions{
		MaxPoolSize: 12,
		MinPoolSize: 2,
	})
	emitPoolEvent(opts.PoolMonitor, event.ConnectionReady, "mongo-a:27017", 150*time.Millisecond, nil)
	emitPoolEvent(opts.PoolMonitor, event.ConnectionCheckOutStarted, "mongo-a:27017", 0, nil)
	emitPoolEvent(opts.PoolMonitor, event.ConnectionCheckOutStarted, "mongo-a:27017", 0, nil)
	emitPoolEvent(opts.PoolMonitor, event.ConnectionCheckedOut, "mongo-a:27017", 0, nil)
	emitPoolEventWithReason(opts.PoolMonitor, event.ConnectionCheckOutFailed, "mongo-a:27017", event.ReasonTimedOut)

	assert.Equal(t, []string{
		event.ConnectionPoolCreated,
		event.ConnectionReady,
		event.ConnectionCheckOutStarted,
		event.ConnectionCheckOutStarted,
		event.ConnectionCheckedOut,
		event.ConnectionCheckOutFailed,
	}, existingEvents)

	rm := collectMongoMetrics(t, reader)
	usedAttrs := []attribute.KeyValue{
		semconv.DBSystemNameMongoDB,
		semconv.DBClientConnectionPoolName("chat-mongo"),
		semconv.ServerAddress("mongo-a"),
		semconv.ServerPort(27017),
		semconv.DBClientConnectionStateUsed,
	}
	idleAttrs := []attribute.KeyValue{
		semconv.DBSystemNameMongoDB,
		semconv.DBClientConnectionPoolName("chat-mongo"),
		semconv.ServerAddress("mongo-a"),
		semconv.ServerPort(27017),
		semconv.DBClientConnectionStateIdle,
	}
	poolAttrs := []attribute.KeyValue{
		semconv.DBSystemNameMongoDB,
		semconv.DBClientConnectionPoolName("chat-mongo"),
		semconv.ServerAddress("mongo-a"),
		semconv.ServerPort(27017),
	}

	assert.Equal(t, int64(1), int64MetricValue(t, findMetric(t, rm, "db.client.connection.count"), usedAttrs...))
	assert.Equal(t, int64(0), int64MetricValue(t, findMetric(t, rm, "db.client.connection.count"), idleAttrs...))
	assert.Equal(t, int64(2), int64MetricValue(t, findMetric(t, rm, "db.client.connection.idle.min"), poolAttrs...))
	assert.Equal(t, int64(12), int64MetricValue(t, findMetric(t, rm, "db.client.connection.max"), poolAttrs...))
	assert.Equal(t, int64(0), int64MetricValue(t, findMetric(t, rm, "db.client.connection.pending_requests"), poolAttrs...))
	assert.Equal(t, int64(1), int64MetricValue(t, findMetric(t, rm, "db.client.connection.timeouts"), poolAttrs...))

	createTime := findMetric(t, rm, "db.client.connection.create_time")
	histogram, ok := createTime.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, histogram.DataPoints, 1)
	assert.Equal(t, uint64(1), histogram.DataPoints[0].Count)
	assert.InDelta(t, 0.15, histogram.DataPoints[0].Sum, 0.0001)
	assert.Equal(t, testHistogramBuckets, histogram.DataPoints[0].Bounds)

	assert.NoError(t, cleanup(context.Background()))
}

// TestInstrument_PoolMetricsDefaultPoolNameAndOmitUnboundedMax verifies
// fallback pool-name derivation and omission of unbounded max pool size.
func TestInstrument_PoolMetricsDefaultPoolNameAndOmitUnboundedMax(t *testing.T) {
	tp, prop, _ := newTestProviders()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)
	opts := options.Client().ApplyURI("mongodb://db-primary:27018")

	cleanup, err := Instrument(opts, tp, provider, prop)
	require.NoError(t, err)
	defer func() { assert.NoError(t, cleanup(context.Background())) }()

	emitPoolEvent(opts.PoolMonitor, event.ConnectionPoolCreated, "db-primary:27018", 0, &event.MonitorPoolOptions{
		MinPoolSize: 3,
		MaxPoolSize: 0,
	})

	rm := collectMongoMetrics(t, reader)
	// Assert the public contract (host-prefixed default name) rather than
	// comparing against defaultPoolName, which would just mirror the production
	// logic and hide a regression in the naming algorithm.
	idAttrs := []attribute.KeyValue{
		semconv.DBSystemNameMongoDB,
		semconv.ServerAddress("db-primary"),
		semconv.ServerPort(27018),
	}
	idleMin := findMetric(t, rm, "db.client.connection.idle.min")
	assert.Equal(t, int64(3), int64MetricValue(t, idleMin, idAttrs...))
	poolName := poolNameValue(t, idleMin, idAttrs...)
	assert.Truef(t, strings.HasPrefix(poolName, "mongo-db-primary-"),
		"default pool name %q should be derived as mongo-<host>-<n>", poolName)
	assertMetricAbsentOrEmpty(t, rm, "db.client.connection.max")
}

// TestDefaultPoolName_UniquePerClientOptions verifies the fallback pool name is
// host-prefixed and distinct for separate ClientOptions instances, so two
// clients pointed at the same host do not collapse into one metric stream, and
// that WithPoolName still overrides the derivation.
func TestDefaultPoolName_UniquePerClientOptions(t *testing.T) {
	optsA := options.Client().ApplyURI("mongodb://mongo-a:27017")
	optsB := options.Client().ApplyURI("mongodb://mongo-a:27017")

	nameA := defaultPoolName(optsA, "")
	nameB := defaultPoolName(optsB, "")

	assert.Truef(t, strings.HasPrefix(nameA, "mongo-mongo-a-"), "got %q", nameA)
	assert.Truef(t, strings.HasPrefix(nameB, "mongo-mongo-a-"), "got %q", nameB)
	assert.NotEqual(t, nameA, nameB, "distinct ClientOptions must yield distinct pool names")
	assert.Equal(t, "custom", defaultPoolName(optsA, "custom"), "override must win")
}

// TestInstrument_PoolMetricsResetAndCleanup verifies pool clear, close, and
// explicit cleanup behavior for SDK-owned MongoDB pool metrics.
func TestInstrument_PoolMetricsResetAndCleanup(t *testing.T) {
	tp, prop, _ := newTestProviders()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)
	opts := options.Client().ApplyURI("mongodb://mongo-a:27017")

	cleanup, err := Instrument(opts, tp, provider, prop)
	require.NoError(t, err)

	emitPoolEvent(opts.PoolMonitor, event.ConnectionPoolCreated, "mongo-a:27017", 0, &event.MonitorPoolOptions{MaxPoolSize: 10})
	emitPoolEventWithConnectionID(opts.PoolMonitor, event.ConnectionReady, "mongo-a:27017", 1, time.Millisecond, nil)
	emitPoolEvent(opts.PoolMonitor, event.ConnectionCheckOutStarted, "mongo-a:27017", 0, nil)
	emitPoolEventWithConnectionID(opts.PoolMonitor, event.ConnectionCheckedOut, "mongo-a:27017", 1, 0, nil)
	emitPoolEvent(opts.PoolMonitor, event.ConnectionPoolCleared, "mongo-a:27017", 0, nil)

	rm := collectMongoMetrics(t, reader)
	// Match points by the stable server identity; the default pool name is
	// asserted as a contract in TestInstrument_PoolMetricsDefaultPoolName... and
	// TestDefaultPoolName_UniquePerClientOptions instead of being hard-coded here.
	poolAttrs := []attribute.KeyValue{
		semconv.DBSystemNameMongoDB,
		semconv.ServerAddress("mongo-a"),
		semconv.ServerPort(27017),
	}
	usedAttrs := []attribute.KeyValue{
		semconv.DBSystemNameMongoDB,
		semconv.ServerAddress("mongo-a"),
		semconv.ServerPort(27017),
		semconv.DBClientConnectionStateUsed,
	}
	assert.Equal(t, int64(1), int64MetricValue(t, findMetric(t, rm, "db.client.connection.count"), usedAttrs...))
	assert.Equal(t, int64(10), int64MetricValue(t, findMetric(t, rm, "db.client.connection.max"), poolAttrs...))

	emitPoolEventWithConnectionID(opts.PoolMonitor, event.ConnectionClosed, "mongo-a:27017", 1, 0, nil)
	rm = collectMongoMetrics(t, reader)
	assert.Equal(t, int64(0), int64MetricValue(t, findMetric(t, rm, "db.client.connection.count"), usedAttrs...))

	emitPoolEvent(opts.PoolMonitor, event.ConnectionPoolClosed, "mongo-a:27017", 0, nil)
	rm = collectMongoMetrics(t, reader)
	assert.Equal(t, int64(0), int64MetricValue(t, findMetric(t, rm, "db.client.connection.max"), poolAttrs...))

	assert.NoError(t, cleanup(context.Background()))
	emitPoolEvent(opts.PoolMonitor, event.ConnectionReady, "mongo-a:27017", time.Millisecond, nil)
	rm = collectMongoMetrics(t, reader)
	assert.Equal(t, int64(0), int64MetricValue(t, findMetric(t, rm, "db.client.connection.max"), poolAttrs...))
}

// TestInstrument_PoolMetricsContinueAfterPoolClosedAndRecreated verifies that
// topology churn does not permanently disable pool metric handling.
func TestInstrument_PoolMetricsContinueAfterPoolClosedAndRecreated(t *testing.T) {
	tp, prop, _ := newTestProviders()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)
	opts := options.Client().ApplyURI("mongodb://mongo-a:27017")

	cleanup, err := Instrument(opts, tp, provider, prop)
	require.NoError(t, err)
	defer func() { assert.NoError(t, cleanup(context.Background())) }()

	emitPoolEvent(opts.PoolMonitor, event.ConnectionPoolCreated, "mongo-a:27017", 0, &event.MonitorPoolOptions{MaxPoolSize: 5})
	emitPoolEventWithConnectionID(opts.PoolMonitor, event.ConnectionReady, "mongo-a:27017", 1, time.Millisecond, nil)
	emitPoolEvent(opts.PoolMonitor, event.ConnectionPoolClosed, "mongo-a:27017", 0, nil)
	emitPoolEvent(opts.PoolMonitor, event.ConnectionPoolCreated, "mongo-a:27017", 0, &event.MonitorPoolOptions{MaxPoolSize: 7})
	emitPoolEventWithConnectionID(opts.PoolMonitor, event.ConnectionReady, "mongo-a:27017", 2, time.Millisecond, nil)

	rm := collectMongoMetrics(t, reader)
	poolAttrs := []attribute.KeyValue{
		semconv.DBSystemNameMongoDB,
		semconv.ServerAddress("mongo-a"),
		semconv.ServerPort(27017),
	}
	idleAttrs := append(append([]attribute.KeyValue{}, poolAttrs...), semconv.DBClientConnectionStateIdle)

	assert.Equal(t, int64(1), int64MetricValue(t, findMetric(t, rm, "db.client.connection.count"), idleAttrs...))
	assert.Equal(t, int64(7), int64MetricValue(t, findMetric(t, rm, "db.client.connection.max"), poolAttrs...))
}

// BenchmarkPoolMonitorCheckoutCycle measures the event-monitor hot path used by
// high-throughput applications that frequently check out MongoDB connections.
func BenchmarkPoolMonitorCheckoutCycle(b *testing.B) {
	opts := options.Client().ApplyURI("mongodb://mongo-a:27017")
	monitor, cleanup, err := newPoolMonitor(opts, metricnoop.NewMeterProvider(), "bench-mongo")
	if err != nil {
		b.Fatalf("create pool monitor: %v", err)
	}
	defer func() { _ = cleanup(context.Background()) }()

	ready := &event.PoolEvent{Type: event.ConnectionReady, Address: "mongo-a:27017", ConnectionID: 1}
	started := &event.PoolEvent{Type: event.ConnectionCheckOutStarted, Address: "mongo-a:27017"}
	checkedOut := &event.PoolEvent{Type: event.ConnectionCheckedOut, Address: "mongo-a:27017", ConnectionID: 1}
	checkedIn := &event.PoolEvent{Type: event.ConnectionCheckedIn, Address: "mongo-a:27017", ConnectionID: 1}
	closed := &event.PoolEvent{Type: event.ConnectionClosed, Address: "mongo-a:27017", ConnectionID: 1}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		monitor.Event(ready)
		monitor.Event(started)
		monitor.Event(checkedOut)
		monitor.Event(checkedIn)
		monitor.Event(closed)
	}
}

func TestNewMonitor_EmitsAlwaysOnCommandSpanWithParentContext(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "false")
	t.Setenv("OTEL_MONGO_TRACING_ENABLED", "false")

	tp, _, sr := newTestProviders()
	monitor := NewMonitor(tp, metricnoop.NewMeterProvider())

	parentCtx, parent := tp.Tracer("test").Start(context.Background(), "parent")
	monitor.Started(parentCtx, commandStartedEvent(t, "insert", "o11y_test", "events", 1))
	monitor.Succeeded(parentCtx, commandSucceededEvent("insert", "o11y_test", 1))
	parent.End()

	spans := sr.Ended()
	require.Len(t, spans, 2)

	commandSpan := findSpanWithName(spans, "mongodb.insert events")
	require.NotNil(t, commandSpan, "MongoDB insert span should be recorded")
	parentSpan := findSpanWithName(spans, "parent")
	require.NotNil(t, parentSpan)

	assert.Equal(t, parentSpan.SpanContext().TraceID(), commandSpan.SpanContext().TraceID())
	assert.Equal(t, parentSpan.SpanContext().SpanID(), commandSpan.Parent().SpanID())
	assert.Equal(t, oteltrace.SpanKindClient, commandSpan.SpanKind())

	attrs := commandSpan.Attributes()
	assert.Contains(t, attrs, semconv.DBSystemNameMongoDB)
	assert.Contains(t, attrs, semconv.DBNamespace("o11y_test"))
	assert.Contains(t, attrs, semconv.DBCollectionName("events"))
	assert.Contains(t, attrs, semconv.DBOperationName("insert"))
	assert.Contains(t, attrs, semconv.NetworkPeerAddress("127.0.0.1"))
	assert.Contains(t, attrs, semconv.NetworkPeerPort(27017))
	assert.Contains(t, attrs, semconv.NetworkTransportTCP)
	assertMissingAttributeKey(t, attrs, semconv.ErrorTypeKey)
}

func TestNewMonitor_NilProvidersUseNoopProviders(t *testing.T) {
	monitor := NewMonitor(nil, nil)
	require.NotNil(t, monitor)
	require.NotNil(t, monitor.Started)
	require.NotNil(t, monitor.Succeeded)
	require.NotNil(t, monitor.Failed)

	assert.NotPanics(t, func() {
		monitor.Started(context.Background(), commandStartedEvent(t, "ping", "admin", "", 1))
		monitor.Succeeded(context.Background(), commandSucceededEvent("ping", "admin", 1))
	})
}

var testHistogramBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

func TestNewMonitor_RecordsOperationDuration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)

	monitor := NewMonitor(tracenoop.NewTracerProvider(), provider)
	monitor.Succeeded(context.Background(), commandSucceededEvent("insert", "o11y_test", 1))

	metric := findMetric(t, collectMongoMetrics(t, reader), "db.client.operation.duration")
	histogram, ok := metric.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "expected MongoDB operation duration histogram")
	require.Len(t, histogram.DataPoints, 1)

	dp := histogram.DataPoints[0]
	assert.Equal(t, uint64(1), dp.Count)
	assert.Equal(t, testHistogramBuckets, dp.Bounds)
	assert.Contains(t, dp.Attributes.ToSlice(), semconv.DBSystemNameMongoDB)
	assert.Contains(t, dp.Attributes.ToSlice(), semconv.DBOperationName("insert"))
	assert.Contains(t, dp.Attributes.ToSlice(), semconv.NetworkPeerAddress("127.0.0.1"))
	assert.Contains(t, dp.Attributes.ToSlice(), semconv.NetworkPeerPort(27017))
	assertMissingAttributeKey(t, dp.Attributes.ToSlice(), semconv.DBNamespaceKey)
	assertMissingAttributeKey(t, dp.Attributes.ToSlice(), semconv.NetworkTransportKey)
	assertMissingAttributeKey(t, dp.Attributes.ToSlice(), semconv.ErrorTypeKey)
}

// redisInstrumentationName mirrors the meter scope the Redis wrapper uses
// (redis.instrumentationName, unexported). Kept here so this test can record an
// instrument under that scope without importing an internal symbol.
const redisInstrumentationName = "github.com/flywindy/o11y/redis"

// TestMetricViews_ScopedPerInstrumentation guards against the Redis and MongoDB
// db.client.operation.duration views matching each other's instrument. o11y.Init
// installs both view sets together; if either view matched by name only it would
// also catch the other integration's instrument, and the first matching view in
// the slice would win the stream-definition conflict, silently applying the
// wrong attribute filter.
func TestMetricViews_ScopedPerInstrumentation(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(append(o11yredis.MetricViews(testHistogramBuckets), MetricViews(testHistogramBuckets)...)...),
	)

	record := func(scope string) {
		meter := provider.Meter(scope)
		hist, err := meter.Float64Histogram("db.client.operation.duration", metric.WithUnit("s"))
		require.NoError(t, err)
		hist.Record(context.Background(), 0.01, metric.WithAttributes(
			semconv.NetworkPeerAddress("10.0.0.1"),
			semconv.ServerAddress("10.0.0.1"),
			semconv.DBNamespace("o11y_test"),
		))
	}
	record(otelmongo.ScopeName)
	record(redisInstrumentationName)

	rm := collectMongoMetrics(t, reader)

	mongoAttrs := scopedDurationAttrs(t, rm, otelmongo.ScopeName)
	assert.Contains(t, mongoAttrs, semconv.NetworkPeerAddress("10.0.0.1"))
	assertMissingAttributeKey(t, mongoAttrs, semconv.ServerAddressKey)
	assertMissingAttributeKey(t, mongoAttrs, semconv.DBNamespaceKey)

	redisAttrs := scopedDurationAttrs(t, rm, redisInstrumentationName)
	assert.Contains(t, redisAttrs, semconv.ServerAddress("10.0.0.1"))
	assertMissingAttributeKey(t, redisAttrs, semconv.NetworkPeerAddressKey)
	assertMissingAttributeKey(t, redisAttrs, semconv.DBNamespaceKey)
}

// scopedDurationAttrs returns the attribute set of the single
// db.client.operation.duration stream emitted under the named instrumentation
// scope, failing if that scope has zero or more than one such stream.
func scopedDurationAttrs(t *testing.T, rm metricdata.ResourceMetrics, scope string) []attribute.KeyValue {
	t.Helper()

	var matches []metricdata.Metrics
	for i := range rm.ScopeMetrics {
		if rm.ScopeMetrics[i].Scope.Name != scope {
			continue
		}
		for j := range rm.ScopeMetrics[i].Metrics {
			if rm.ScopeMetrics[i].Metrics[j].Name == "db.client.operation.duration" {
				matches = append(matches, rm.ScopeMetrics[i].Metrics[j])
			}
		}
	}
	require.Lenf(t, matches, 1, "scope %q: expected exactly one db.client.operation.duration stream", scope)

	hist, ok := matches[0].Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, hist.DataPoints, 1)
	return hist.DataPoints[0].Attributes.ToSlice()
}

func TestNewMonitor_RecordsFailureErrorType(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)

	monitor := NewMonitor(tracenoop.NewTracerProvider(), provider)
	monitor.Failed(context.Background(), commandFailedEvent("find", "o11y_test", 2, errors.New("boom")))

	metric := findMetric(t, collectMongoMetrics(t, reader), "db.client.operation.duration")
	histogram, ok := metric.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "expected MongoDB operation duration histogram")
	require.Len(t, histogram.DataPoints, 1)

	attrs := histogram.DataPoints[0].Attributes.ToSlice()
	assert.Contains(t, attrs, semconv.DBSystemNameMongoDB)
	assert.Contains(t, attrs, semconv.DBOperationName("find"))
	assert.Contains(t, attrs, semconv.NetworkPeerAddress("127.0.0.1"))
	assert.Contains(t, attrs, semconv.NetworkPeerPort(27017))
	assertHasAttributeKey(t, attrs, semconv.ErrorTypeKey)
}

func TestSpanName(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		collection string
		want       string
	}{
		{name: "find with collection", command: "find", collection: "users", want: "mongodb.find users"},
		{name: "insert with collection", command: "insert", collection: "events", want: "mongodb.insert events"},
		{name: "collection with dot", command: "find", collection: "a.b", want: "mongodb.find a.b"},
		{name: "no collection (admin)", command: "ping", collection: "", want: "mongodb.ping"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evt := commandStartedEvent(t, tc.command, "o11y_test", tc.collection, 1)
			if got := spanName(evt); got != tc.want {
				t.Errorf("spanName() = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("nil event", func(t *testing.T) {
		if got := spanName(nil); got != "mongodb" {
			t.Errorf("spanName(nil) = %q, want %q", got, "mongodb")
		}
	})
}

func commandStartedEvent(t *testing.T, command, database, collection string, requestID int64) *event.CommandStartedEvent {
	t.Helper()

	doc := bson.D{{Key: command, Value: collection}}
	if collection == "" {
		doc = bson.D{{Key: command, Value: 1}}
	}
	raw, err := bson.Marshal(doc)
	require.NoError(t, err)
	return &event.CommandStartedEvent{
		Command:      raw,
		DatabaseName: database,
		CommandName:  command,
		RequestID:    requestID,
		ConnectionID: "127.0.0.1:27017",
	}
}

func commandSucceededEvent(command, database string, requestID int64) *event.CommandSucceededEvent {
	return &event.CommandSucceededEvent{
		CommandFinishedEvent: event.CommandFinishedEvent{
			Duration:     25 * time.Millisecond,
			CommandName:  command,
			DatabaseName: database,
			ConnectionID: "127.0.0.1:27017",
			RequestID:    requestID,
		},
	}
}

func commandFailedEvent(command, database string, requestID int64, failure error) *event.CommandFailedEvent {
	return &event.CommandFailedEvent{
		CommandFinishedEvent: event.CommandFinishedEvent{
			Duration:     10 * time.Millisecond,
			CommandName:  command,
			DatabaseName: database,
			ConnectionID: "127.0.0.1:27017",
			RequestID:    requestID,
		},
		Failure: failure,
	}
}

func emitPoolEvent(
	monitor *event.PoolMonitor,
	eventType string,
	address string,
	duration time.Duration,
	poolOptions *event.MonitorPoolOptions,
) {
	monitor.Event(&event.PoolEvent{
		Type:        eventType,
		Address:     address,
		Duration:    duration,
		PoolOptions: poolOptions,
	})
}

func emitPoolEventWithConnectionID(
	monitor *event.PoolMonitor,
	eventType string,
	address string,
	connectionID int64,
	duration time.Duration,
	poolOptions *event.MonitorPoolOptions,
) {
	monitor.Event(&event.PoolEvent{
		Type:         eventType,
		Address:      address,
		ConnectionID: connectionID,
		Duration:     duration,
		PoolOptions:  poolOptions,
	})
}

func emitPoolEventWithReason(monitor *event.PoolMonitor, eventType, address, reason string) {
	monitor.Event(&event.PoolEvent{
		Type:    eventType,
		Address: address,
		Reason:  reason,
	})
}

func findSpanWithName(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	return nil
}

func collectMongoMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
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

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()

	metric := metricByName(rm, name)
	require.NotNilf(t, metric, "metric %q not found", name)
	return *metric
}

func assertHasAttributeKey(t *testing.T, attrs []attribute.KeyValue, key attribute.Key) {
	t.Helper()

	for _, attr := range attrs {
		if attr.Key == key {
			return
		}
	}
	assert.Failf(t, "attribute missing", "attribute key %q missing from %v", key, attrs)
}

func assertMissingAttributeKey(t *testing.T, attrs []attribute.KeyValue, key attribute.Key) {
	t.Helper()

	for _, attr := range attrs {
		if attr.Key == key {
			assert.Failf(t, "attribute present", "attribute key %q unexpectedly present in %v", key, attrs)
			return
		}
	}
}

func int64MetricValue(t *testing.T, metric metricdata.Metrics, attrs ...attribute.KeyValue) int64 {
	t.Helper()

	var points []metricdata.DataPoint[int64]
	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		points = data.DataPoints
	case metricdata.Gauge[int64]:
		points = data.DataPoints
	default:
		t.Fatalf("unsupported int64 metric data type %T for %s", metric.Data, metric.Name)
	}
	for _, point := range points {
		if attributesContain(point.Attributes, attrs...) {
			return point.Value
		}
	}
	t.Fatalf("metric %s missing point with attrs %v", metric.Name, attrs)
	return 0
}

func attributesContain(set attribute.Set, attrs ...attribute.KeyValue) bool {
	for _, attr := range attrs {
		got, ok := set.Value(attr.Key)
		if !ok || got != attr.Value {
			return false
		}
	}
	return true
}

// poolNameValue returns the db.client.connection.pool.name attribute of the
// first int64 data point in metric whose attributes contain all of attrs.
func poolNameValue(t *testing.T, metric metricdata.Metrics, attrs ...attribute.KeyValue) string {
	t.Helper()

	var points []metricdata.DataPoint[int64]
	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		points = data.DataPoints
	case metricdata.Gauge[int64]:
		points = data.DataPoints
	default:
		t.Fatalf("unsupported int64 metric data type %T for %s", metric.Data, metric.Name)
	}
	for _, point := range points {
		if !attributesContain(point.Attributes, attrs...) {
			continue
		}
		value, ok := point.Attributes.Value(semconv.DBClientConnectionPoolNameKey)
		require.Truef(t, ok, "metric %s point missing pool name", metric.Name)
		return value.AsString()
	}
	t.Fatalf("metric %s missing point with attrs %v", metric.Name, attrs)
	return ""
}

func assertMetricAbsentOrEmpty(t *testing.T, rm metricdata.ResourceMetrics, name string) {
	t.Helper()

	metric := metricByName(rm, name)
	if metric == nil {
		return
	}
	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		assert.Empty(t, data.DataPoints)
	case metricdata.Gauge[int64]:
		assert.Empty(t, data.DataPoints)
	case metricdata.Histogram[float64]:
		assert.Empty(t, data.DataPoints)
	default:
		t.Fatalf("unsupported metric data type %T for %s", metric.Data, metric.Name)
	}
}
