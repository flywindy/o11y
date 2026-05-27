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
	otelmongo "go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	oteltrace "go.opentelemetry.io/otel/trace"
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

func enableMongoTracing(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "true")
	t.Setenv("OTEL_MONGO_TRACING_ENABLED", "true")
}

func TestWithDocumentTracePropagation(t *testing.T) {
	cases := []struct {
		name string
		opt  Option
		want bool
	}{
		{"default off", nil, false},
		{"explicit on", WithDocumentTracePropagation(true), true},
		{"explicit off", WithDocumentTracePropagation(false), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts []Option
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}

			cfg := newConfig(opts)
			assert.Equal(t, tc.want, cfg.documentTracePropagation)
		})
	}
}

func TestConnect_Validation(t *testing.T) {
	tp, prop, _ := newTestProviders()
	mp := noop.NewMeterProvider()
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

func TestConnect_InvalidURI(t *testing.T) {
	tp, prop, _ := newTestProviders()

	client, err := Connect(context.Background(), "://garbage", tp, noop.NewMeterProvider(), prop)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.NotContains(t, err.Error(), "://garbage")
}

func TestConnect_DefaultTracingGateEmitsNoSpans(t *testing.T) {
	tp, prop, sr := newTestProviders()

	client, err := Connect(context.Background(), "mongodb://127.0.0.1:1", tp, noop.NewMeterProvider(), prop)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Disconnect(shutdownCtx)
	}()

	opCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = client.Database("o11y_test").Collection("events").InsertOne(opCtx, bson.M{"name": "default-gate"})

	assert.Empty(t, sr.Ended(), "upstream env gates should keep MongoDB tracing disabled by default")
}

func TestConnect_DocumentPropagationCannotBypassMasterGate(t *testing.T) {
	t.Setenv("OTEL_MONGO_PROPAGATION_ENABLED", "true")

	tp, prop, sr := newTestProviders()

	client, err := Connect(
		context.Background(),
		"mongodb://127.0.0.1:1",
		tp,
		noop.NewMeterProvider(),
		prop,
		WithDocumentTracePropagation(true),
	)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Disconnect(shutdownCtx)
	}()

	opCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = client.Database("o11y_test").Collection("events").InsertOne(opCtx, bson.M{"name": "document-gate"})

	assert.Empty(t, sr.Ended(), "document propagation opt-in must not bypass the upstream master tracing gate")
}

func TestConnect_UsesProvidedTracerProvider(t *testing.T) {
	enableMongoTracing(t)

	tp, prop, sr := newTestProviders()

	client, err := Connect(context.Background(), "mongodb://127.0.0.1:1", tp, noop.NewMeterProvider(), prop)
	require.NoError(t, err)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Disconnect(shutdownCtx)
	}()

	opCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = client.Database("o11y_test").Collection("events").InsertOne(opCtx, bson.M{"name": "provided-tracer"})

	require.NotEmpty(t, sr.Ended(), "MongoDB operation should emit a span through the supplied provider")
	insertSpan := findSpanWithAttribute(sr.Ended(), attribute.String("db.operation.name", "insert"))
	require.NotNil(t, insertSpan, "MongoDB insert span should be recorded")
	attrs := insertSpan.Attributes()
	assert.Contains(t, attrs, attribute.String("db.system.name", "mongodb"))
	assert.Contains(t, attrs, attribute.String("db.namespace", "o11y_test"))
	assert.Contains(t, attrs, attribute.String("db.collection.name", "events"))
	assert.Contains(t, attrs, attribute.String("db.operation.name", "insert"))
	assert.NotContains(t, attrs, attribute.String("db.system", "mongodb"))
}

var testHistogramBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

func TestMetricsMonitor_RecordsOperationDuration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)

	monitor := newMetricsMonitor(provider)
	monitor.Succeeded(context.Background(), &event.CommandSucceededEvent{
		CommandFinishedEvent: event.CommandFinishedEvent{
			Duration:     25 * time.Millisecond,
			CommandName:  "insert",
			DatabaseName: "o11y_test",
			ConnectionID: "127.0.0.1:27017",
			RequestID:    1,
		},
	})

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
// the slice would win the stream-definition conflict — silently applying the
// wrong attribute filter (e.g. MongoDB durations would drop network.peer.* and
// keep the never-set server.*). Scoping each view to its own instrumentation
// keeps the two streams independent.
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

func TestMetricsMonitor_RecordsFailureErrorType(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews(testHistogramBuckets)...),
	)

	monitor := newMetricsMonitor(provider)
	monitor.Failed(context.Background(), &event.CommandFailedEvent{
		CommandFinishedEvent: event.CommandFinishedEvent{
			Duration:     10 * time.Millisecond,
			CommandName:  "find",
			DatabaseName: "o11y_test",
			ConnectionID: "localhost:27018",
			RequestID:    2,
		},
		Failure: errors.New("boom"),
	})

	metric := findMetric(t, collectMongoMetrics(t, reader), "db.client.operation.duration")
	histogram, ok := metric.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "expected MongoDB operation duration histogram")
	require.Len(t, histogram.DataPoints, 1)

	attrs := histogram.DataPoints[0].Attributes.ToSlice()
	assert.Contains(t, attrs, semconv.DBSystemNameMongoDB)
	assert.Contains(t, attrs, semconv.DBOperationName("find"))
	assert.Contains(t, attrs, semconv.NetworkPeerAddress("localhost"))
	assert.Contains(t, attrs, semconv.NetworkPeerPort(27018))
	assertHasAttributeKey(t, attrs, semconv.ErrorTypeKey)
}

func findSpanWithAttribute(spans []sdktrace.ReadOnlySpan, want attribute.KeyValue) sdktrace.ReadOnlySpan {
	for _, span := range spans {
		for _, attr := range span.Attributes() {
			if attr == want {
				return span
			}
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
