package redis

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
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
)

func TestWrapValidation(t *testing.T) {
	tp, _, mp, _ := newRedisTestProviders()
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})
	defer client.Close()

	_, err := Wrap(nil, tp, mp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client must not be nil")

	got, err := Wrap(client, nil, mp)
	require.Error(t, err)
	assert.Same(t, client, got)
	assert.Contains(t, err.Error(), "tracer provider must not be nil")

	got, err = Wrap(client, tp, nil)
	require.Error(t, err)
	assert.Same(t, client, got)
	assert.Contains(t, err.Error(), "meter provider must not be nil")
}

func TestProcessHookEmitsSemconvSpanAndDuration(t *testing.T) {
	hook, sr, reader := newTestHook(t, config{}, "127.0.0.1:6379", 2)

	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(context.Background(), goredis.NewCmd(context.Background(), "get", "session:123"))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "redis.GET", span.Name())
	assert.Equal(t, codes.Unset, span.Status().Code)
	assertSpanHas(t, span, semconv.DBSystemNameRedis)
	assertSpanHas(t, span, semconv.DBOperationName("GET"))
	assertSpanHas(t, span, semconv.DBNamespace("2"))
	assertSpanHas(t, span, semconv.ServerAddress("127.0.0.1"))
	assertSpanHas(t, span, semconv.ServerPort(6379))
	assertSpanMissingKey(t, span, "db.query.text")
	assertSpanMissingKey(t, span, "db.connection_string")
	assertSpanMissingKey(t, span, "db.system")
	assertSpanMissingKey(t, span, "db.statement")
	assertSpanMissingKey(t, span, "code.function")

	rm := collectRedisMetrics(t, reader)
	m := findMetric(t, rm, "db.client.operation.duration")
	assert.Equal(t, "s", m.Unit)
	assertHistogramPointHas(t, m, semconv.DBOperationName("GET"))
	assertHistogramPointMissingKey(t, m, semconv.ErrorTypeKey)
}

func TestCommandTextIsOptInAndTruncated(t *testing.T) {
	hook, sr, _ := newTestHook(t, config{commandTextEnabled: true}, "127.0.0.1:6379", 0)
	longValue := strings.Repeat("x", maxCommandTextLen+100)

	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(context.Background(), goredis.NewCmd(context.Background(), "set", "key", longValue))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	value, ok := spanAttr(spans[0], semconv.DBQueryTextKey)
	require.True(t, ok)
	assert.Len(t, value.AsString(), maxCommandTextLen)
	assert.True(t, strings.HasPrefix(value.AsString(), "set key "))
}

func TestRedisNilIsRecordedAsSuccess(t *testing.T) {
	hook, sr, reader := newTestHook(t, config{}, "127.0.0.1:6379", 0)

	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return goredis.Nil
	})(context.Background(), goredis.NewCmd(context.Background(), "get", "missing"))
	require.ErrorIs(t, err, goredis.Nil)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Unset, spans[0].Status().Code)
	assert.Empty(t, spans[0].Events())
	assertSpanMissingKey(t, spans[0], "error.type")
	assertSpanMissingKey(t, spans[0], "redis.error.kind")

	rm := collectRedisMetrics(t, reader)
	m := findMetric(t, rm, "db.client.operation.duration")
	assertHistogramPointMissingKey(t, m, semconv.ErrorTypeKey)
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		errType  string
		errKind  string
		wantKind bool
	}{
		{"pool timeout", goredis.ErrPoolTimeout, "*errors.errorString", "pool_timeout", true},
		{"deadline", context.DeadlineExceeded, "context.DeadlineExceeded", "client_timeout", true},
		{"canceled", context.Canceled, "context.Canceled", "client_canceled", true},
		{"other", errors.New("boom"), "*errors.errorString", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook, sr, reader := newTestHook(t, config{}, "127.0.0.1:6379", 0)

			err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
				return tc.err
			})(context.Background(), goredis.NewCmd(context.Background(), "set", "key", "value"))
			require.ErrorIs(t, err, tc.err)

			spans := sr.Ended()
			require.Len(t, spans, 1)
			assert.Equal(t, codes.Error, spans[0].Status().Code)
			assertSpanHas(t, spans[0], semconv.ErrorTypeKey.String(tc.errType))
			if tc.wantKind {
				assertSpanHas(t, spans[0], redisErrorKindKey.String(tc.errKind))
			} else {
				assertSpanMissingKey(t, spans[0], "redis.error.kind")
			}

			rm := collectRedisMetrics(t, reader)
			m := findMetric(t, rm, "db.client.operation.duration")
			assertHistogramPointHas(t, m, semconv.ErrorTypeKey.String(tc.errType))
		})
	}
}

func TestPubSubCommandsAreSkipped(t *testing.T) {
	hook, sr, reader := newTestHook(t, config{}, "127.0.0.1:6379", 0)

	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(context.Background(), goredis.NewCmd(context.Background(), "publish", "events", "payload"))
	require.NoError(t, err)

	assert.Empty(t, sr.Ended())
	rm := collectRedisMetrics(t, reader)
	assert.Nil(t, metricByName(rm, "db.client.operation.duration"))
}

func TestPipelineHookHandlesTxFramingAndPubSubFiltering(t *testing.T) {
	hook, sr, _ := newTestHook(t, config{}, "127.0.0.1:6379", 0)
	cmds := []goredis.Cmder{
		goredis.NewCmd(context.Background(), "multi"),
		goredis.NewCmd(context.Background(), "set", "a", "1"),
		goredis.NewCmd(context.Background(), "publish", "events", "payload"),
		goredis.NewCmd(context.Background(), "set", "b", "2"),
		goredis.NewCmd(context.Background(), "exec"),
	}

	err := hook.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error {
		return nil
	})(context.Background(), cmds)
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "redis.pipeline", spans[0].Name())
	assertSpanHas(t, spans[0], semconv.DBOperationName("pipeline"))
	assertSpanHas(t, spans[0], semconv.DBOperationBatchSize(3))
}

func TestPipelinePrefersNonNilCommandError(t *testing.T) {
	hook, sr, reader := newTestHook(t, config{}, "127.0.0.1:6379", 0)
	cacheMiss := goredis.NewCmd(context.Background(), "get", "missing")
	cacheMiss.SetErr(goredis.Nil)
	badCommand := goredis.NewCmd(context.Background(), "incr", "not-an-int")
	badCommand.SetErr(errors.New("ERR value is not an integer"))

	err := hook.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error {
		return goredis.Nil
	})(context.Background(), []goredis.Cmder{cacheMiss, badCommand})
	require.ErrorIs(t, err, goredis.Nil)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assertSpanHas(t, spans[0], semconv.ErrorTypeKey.String("*errors.errorString"))

	rm := collectRedisMetrics(t, reader)
	m := findMetric(t, rm, "db.client.operation.duration")
	assertHistogramPointHas(t, m, semconv.ErrorTypeKey.String("*errors.errorString"))
}

func TestPipelineAllPubSubCommandsAreSkipped(t *testing.T) {
	hook, sr, reader := newTestHook(t, config{}, "127.0.0.1:6379", 0)
	cmds := []goredis.Cmder{
		goredis.NewCmd(context.Background(), "multi"),
		goredis.NewCmd(context.Background(), "publish", "events", "payload"),
		goredis.NewCmd(context.Background(), "spublish", "events", "payload"),
		goredis.NewCmd(context.Background(), "exec"),
	}

	err := hook.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error {
		return nil
	})(context.Background(), cmds)
	require.NoError(t, err)

	assert.Empty(t, sr.Ended())
	rm := collectRedisMetrics(t, reader)
	assert.Nil(t, metricByName(rm, "db.client.operation.duration"))
}

func TestWrapIsIdempotentAndUnwrapAllowsRewrap(t *testing.T) {
	tp, sr, mp, _ := newRedisTestProviders()
	client := goredis.NewClient(&goredis.Options{
		Addr:       "127.0.0.1:1",
		MaxRetries: -1,
	})
	defer client.Close()

	wrapped, err := Wrap(client, tp, mp, WithPoolName("test"))
	require.NoError(t, err)
	assert.Same(t, client, wrapped)
	_, err = Wrap(client, tp, mp, WithPoolName("test"))
	require.NoError(t, err)

	_ = client.Process(context.Background(), goredis.NewCmd(context.Background(), "get", "key"))
	require.Len(t, sr.Ended(), 1)

	Unwrap(client)
	_, err = Wrap(client, tp, mp, WithPoolName("test"))
	require.NoError(t, err)
	_ = client.Process(context.Background(), goredis.NewCmd(context.Background(), "get", "key"))
	require.Len(t, sr.Ended(), 2)
}

func TestWrapCleansUpAfterInstallHookFailure(t *testing.T) {
	tp, _, mp, _ := newRedisTestProviders()
	client := goredis.NewClusterClient(&goredis.ClusterOptions{
		Addrs:       []string{"127.0.0.1:1"},
		MaxRetries:  0,
		DialTimeout: 10 * time.Millisecond,
	})
	defer client.Close()

	_, err := Wrap(client, tp, mp, WithPoolName("broken-cluster"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install hooks")

	_, found := wrappedClients.Load(clientKey(client))
	assert.False(t, found, "failed wrap must not poison future wrap attempts")

	_, err = Wrap(client, tp, mp, WithPoolName("broken-cluster"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install hooks")
}

func TestPoolMetricsEmitV139Names(t *testing.T) {
	tp, _, mp, reader := newRedisTestProviders()
	client := goredis.NewClient(&goredis.Options{
		Addr:           "127.0.0.1:6379",
		PoolSize:       11,
		MinIdleConns:   2,
		MaxIdleConns:   5,
		MaxActiveConns: 13,
	})
	defer client.Close()

	_, err := Wrap(client, tp, mp, WithPoolName("sessions-cache"))
	require.NoError(t, err)

	rm := collectRedisMetrics(t, reader)
	for _, name := range []string{
		"db.client.connection.count",
		"db.client.connection.idle.max",
		"db.client.connection.idle.min",
		"db.client.connection.max",
		"db.client.connection.timeouts",
	} {
		m := findMetric(t, rm, name)
		assertMetricPointHas(t, m, attribute.String("db.client.connection.pool.name", "sessions-cache"))
		assertMetricPointHas(t, m, semconv.DBSystemNameRedis)
		assertMetricPointHas(t, m, semconv.ServerAddress("127.0.0.1"))
		assertMetricPointHas(t, m, semconv.ServerPort(6379))
	}
	assert.Nil(t, metricByName(rm, "db.client.connections.use_time"))
}

func TestMetricViewsDropRedisotelLegacyInstruments(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews()...),
	)
	meter := mp.Meter("redisotel")

	legacyCounter, err := meter.Int64Counter("db.client.connections.hits")
	require.NoError(t, err)
	legacyCounter.Add(context.Background(), 1)

	legacyHistogram, err := meter.Float64Histogram("db.client.connections.create_time")
	require.NoError(t, err)
	legacyHistogram.Record(context.Background(), 0.001)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	assert.Nil(t, metricByName(rm, "db.client.connections.hits"))
	assert.Nil(t, metricByName(rm, "db.client.connections.create_time"))
}

func newRedisTestProviders() (*sdktrace.TracerProvider, *tracetest.SpanRecorder, *sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews()...),
	)
	return tp, sr, mp, reader
}

func newTestHook(t *testing.T, cfg config, addr string, db int) (*redisHook, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	tp, sr, mp, reader := newRedisTestProviders()
	meter := mp.Meter(instrumentationName, metric.WithSchemaURL(semconv.SchemaURL))
	operationDuration, err := meter.Float64Histogram("db.client.operation.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	pool, err := newPoolMetrics(meter)
	require.NoError(t, err)
	client := goredis.NewClient(&goredis.Options{Addr: addr, DB: db})
	t.Cleanup(func() { _ = client.Close() })
	return newRedisHook(tp, operationDuration, pool.createTime, cfg, client, "test", &atomic.Bool{}), sr, reader
}

func collectRedisMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
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
	m := metricByName(rm, name)
	require.NotNil(t, m, "metric %s not found", name)
	return *m
}

func spanAttr(span sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, attr := range span.Attributes() {
		if attr.Key == key {
			return attr.Value, true
		}
	}
	return attribute.Value{}, false
}

func assertSpanHas(t *testing.T, span sdktrace.ReadOnlySpan, want attribute.KeyValue) {
	t.Helper()
	assert.Contains(t, span.Attributes(), want)
}

func assertSpanMissingKey(t *testing.T, span sdktrace.ReadOnlySpan, key attribute.Key) {
	t.Helper()
	for _, attr := range span.Attributes() {
		assert.NotEqual(t, key, attr.Key)
	}
}

func assertHistogramPointHas(t *testing.T, m metricdata.Metrics, want attribute.KeyValue) {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "%s should be a float64 histogram", m.Name)
	require.NotEmpty(t, hist.DataPoints)
	assert.True(t, hist.DataPoints[0].Attributes.HasValue(want.Key))
	got, _ := hist.DataPoints[0].Attributes.Value(want.Key)
	assert.Equal(t, want.Value, got)
}

func assertHistogramPointMissingKey(t *testing.T, m metricdata.Metrics, key attribute.Key) {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "%s should be a float64 histogram", m.Name)
	require.NotEmpty(t, hist.DataPoints)
	assert.False(t, hist.DataPoints[0].Attributes.HasValue(key))
}

func assertMetricPointHas(t *testing.T, m metricdata.Metrics, want attribute.KeyValue) {
	t.Helper()
	set := firstMetricAttributes(t, m)
	require.True(t, set.HasValue(want.Key), "missing %s on %s", want.Key, m.Name)
	got, _ := set.Value(want.Key)
	assert.Equal(t, want.Value, got)
}

func firstMetricAttributes(t *testing.T, m metricdata.Metrics) attribute.Set {
	t.Helper()
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		require.NotEmpty(t, data.DataPoints)
		return data.DataPoints[0].Attributes
	case metricdata.Sum[float64]:
		require.NotEmpty(t, data.DataPoints)
		return data.DataPoints[0].Attributes
	case metricdata.Gauge[int64]:
		require.NotEmpty(t, data.DataPoints)
		return data.DataPoints[0].Attributes
	case metricdata.Gauge[float64]:
		require.NotEmpty(t, data.DataPoints)
		return data.DataPoints[0].Attributes
	default:
		t.Fatalf("unsupported metric data type %T for %s", m.Data, m.Name)
		return attribute.Set{}
	}
}
