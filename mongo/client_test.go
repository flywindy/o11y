package mongo

import (
	"context"
	"errors"
	"testing"
	"time"

	o11yredis "github.com/flywindy/o11y/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
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
	var _ *drivermongo.Client = client

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

	commandSpan := findSpanWithName(spans, "events.insert")
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

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()

	for i := range rm.ScopeMetrics {
		for j := range rm.ScopeMetrics[i].Metrics {
			if rm.ScopeMetrics[i].Metrics[j].Name == name {
				return rm.ScopeMetrics[i].Metrics[j]
			}
		}
	}
	require.Failf(t, "metric not found", "metric %q not found", name)
	return metricdata.Metrics{}
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
