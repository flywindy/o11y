package redis

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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
	"go.opentelemetry.io/otel/trace"
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

func TestWithAttributesCannotOverrideBuiltinKeys(t *testing.T) {
	cfg := config{attrs: []attribute.KeyValue{
		attribute.String("app.name", "checkout"),
		semconv.DBSystemNameKey.String("postgres"), // collides with built-in
	}}
	hook, sr, _ := newTestHook(t, cfg, "127.0.0.1:6379", 0)

	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(context.Background(), goredis.NewCmd(context.Background(), "get", "k"))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	// Custom, non-colliding attribute is kept.
	assertSpanHas(t, spans[0], attribute.String("app.name", "checkout"))
	// Built-in semconv key wins over the colliding user value.
	value, ok := spanAttr(spans[0], semconv.DBSystemNameKey)
	require.True(t, ok)
	assert.Equal(t, "redis", value.AsString())
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

func TestConnectionLifecycleCommandsAreSkipped(t *testing.T) {
	cmds := map[string]goredis.Cmder{
		"auth":                 goredis.NewCmd(context.Background(), "auth", "secret"),
		"hello":                goredis.NewCmd(context.Background(), "hello", "3"),
		"select":               goredis.NewCmd(context.Background(), "select", "2"),
		"readonly":             goredis.NewCmd(context.Background(), "readonly"),
		"client setinfo":       goredis.NewCmd(context.Background(), "client", "setinfo", "lib-name", "go-redis"),
		"client setinfo bytes": goredis.NewCmd(context.Background(), "client", []byte("setinfo"), "lib-name", "go-redis"),
		"client setname":       goredis.NewCmd(context.Background(), "client", "setname", "app"),
	}
	for name, cmd := range cmds {
		t.Run(name, func(t *testing.T) {
			hook, sr, reader := newTestHook(t, config{}, "127.0.0.1:6379", 0)
			err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
				return nil
			})(context.Background(), cmd)
			require.NoError(t, err)

			assert.Empty(t, sr.Ended())
			rm := collectRedisMetrics(t, reader)
			assert.Nil(t, metricByName(rm, "db.client.operation.duration"))
		})
	}
}

func TestDeliberateClientSubcommandIsInstrumented(t *testing.T) {
	// CLIENT LIST is application-issued, not a connection-setup command, so it
	// must still produce telemetry — only CLIENT SETINFO / SETNAME are filtered.
	hook, sr, _ := newTestHook(t, config{}, "127.0.0.1:6379", 0)

	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(context.Background(), goredis.NewCmd(context.Background(), "client", "list"))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "redis.CLIENT", spans[0].Name())
}

func TestWithIgnoredCommandsSkipsNamedCommands(t *testing.T) {
	// Exercise the option wiring, including case-insensitive matching.
	cfg := newConfig([]Option{WithIgnoredCommands("PING", " Info ")})
	hook, sr, reader := newTestHook(t, cfg, "127.0.0.1:6379", 0)

	for _, name := range []string{"ping", "info"} {
		err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
			return nil
		})(context.Background(), goredis.NewCmd(context.Background(), name))
		require.NoError(t, err)
	}
	assert.Empty(t, sr.Ended())
	rm := collectRedisMetrics(t, reader)
	assert.Nil(t, metricByName(rm, "db.client.operation.duration"))

	// A non-ignored command is still instrumented.
	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(context.Background(), goredis.NewCmd(context.Background(), "get", "k"))
	require.NoError(t, err)
	require.Len(t, sr.Ended(), 1)
}

func TestRequireParentSpanDropsUnparentedCommands(t *testing.T) {
	cfg := newConfig([]Option{WithRequireParentSpan(true)})
	hook, sr, reader := newTestHook(t, cfg, "127.0.0.1:6379", 0)

	// No parent span on the context: dropped.
	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(context.Background(), goredis.NewCmd(context.Background(), "get", "k"))
	require.NoError(t, err)
	assert.Empty(t, sr.Ended())
	assert.Nil(t, metricByName(collectRedisMetrics(t, reader), "db.client.operation.duration"))

	// Valid parent span context present: instrumented as a child.
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:  trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), parent)
	err = hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(ctx, goredis.NewCmd(context.Background(), "get", "k"))
	require.NoError(t, err)
	require.Len(t, sr.Ended(), 1)
}

func TestPipelineAllIgnoredCommandsAreSkipped(t *testing.T) {
	cfg := newConfig([]Option{WithIgnoredCommands("ping")})
	hook, sr, reader := newTestHook(t, cfg, "127.0.0.1:6379", 0)
	cmds := []goredis.Cmder{
		goredis.NewCmd(context.Background(), "multi"),
		goredis.NewCmd(context.Background(), "ping"),
		goredis.NewCmd(context.Background(), "ping"),
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

func TestPipelineQueryTextExcludesFilteredCommands(t *testing.T) {
	// A mixed pipeline is still recorded, but a filtered command's arguments
	// (here AUTH's credential) must not leak into db.query.text via the
	// surviving sibling command's span.
	hook, sr, _ := newTestHook(t, config{commandTextEnabled: true}, "127.0.0.1:6379", 0)
	cmds := []goredis.Cmder{
		goredis.NewCmd(context.Background(), "auth", "super-secret"),
		goredis.NewCmd(context.Background(), "get", "k"),
	}

	err := hook.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error {
		return nil
	})(context.Background(), cmds)
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	value, ok := spanAttr(spans[0], semconv.DBQueryTextKey)
	require.True(t, ok)
	assert.Equal(t, "get k", value.AsString())
	assert.NotContains(t, value.AsString(), "super-secret")
	// batch.size still reflects the full user-command count (ADR 0013 §8).
	assertSpanHas(t, spans[0], semconv.DBOperationBatchSize(2))
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

func TestWrapBestEffortOnClusterShardFailure(t *testing.T) {
	tp, _, mp, _ := newRedisTestProviders()
	client := goredis.NewClusterClient(&goredis.ClusterOptions{
		Addrs:       []string{"127.0.0.1:1"},
		MaxRetries:  0,
		DialTimeout: 100 * time.Millisecond,
	})
	defer client.Close()

	// Initial Wrap surfaces the shard-setup error so callers can log it and
	// fail readiness if they choose.
	_, err := Wrap(client, tp, mp, WithPoolName("best-effort"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install hooks")

	// ADR 0013 §10 best-effort: go-redis v9 has no hook-remove API, so the
	// dedup entry is committed despite the error. Retries observe the
	// committed entry and silently no-op rather than appending another
	// OnNewNode callback to the cluster.
	_, found := wrappedClients.Load(clientKey(client))
	assert.True(t, found, "best-effort commit retains the dedup entry")

	_, err = Wrap(client, tp, mp, WithPoolName("best-effort"))
	require.NoError(t, err, "subsequent Wrap is a silent no-op")

	// Unwrap is the canonical way to detach: it disables previously-installed
	// hooks via the shared atomic flag and clears the dedup entry so a future
	// Wrap can re-instrument cleanly.
	Unwrap(client)
	_, found = wrappedClients.Load(clientKey(client))
	assert.False(t, found, "Unwrap clears the dedup entry")
}

func TestSpanOmitsDBNamespaceForDefaultDB(t *testing.T) {
	hook, sr, _ := newTestHook(t, config{}, "127.0.0.1:6379", 0)

	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(context.Background(), goredis.NewCmd(context.Background(), "get", "key"))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assertSpanMissingKey(t, spans[0], "db.namespace")
}

func TestDialHookRecordsCreateTimeOnlyOnSuccess(t *testing.T) {
	tp, _, mp, reader := newRedisTestProviders()
	meter := mp.Meter(instrumentationName, metric.WithSchemaURL(semconv.SchemaURL))
	operationDuration, err := meter.Float64Histogram("db.client.operation.duration", metric.WithUnit("s"))
	require.NoError(t, err)
	pool, err := newPoolMetrics(meter)
	require.NoError(t, err)

	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:6379"})
	t.Cleanup(func() { _ = client.Close() })
	hook := newRedisHook(tp, operationDuration, pool.createTime, config{}, client, "test", &atomic.Bool{})

	// Successful dial: emits one sample.
	okDial := hook.DialHook(func(_ context.Context, _, _ string) (net.Conn, error) {
		c, _ := net.Pipe()
		return c, nil
	})
	conn, err := okDial(context.Background(), "tcp", "127.0.0.1:6379")
	require.NoError(t, err)
	_ = conn.Close()

	// Failed dial: must NOT emit a create_time sample.
	failDial := hook.DialHook(func(_ context.Context, _, _ string) (net.Conn, error) {
		return nil, errors.New("dial refused")
	})
	_, err = failDial(context.Background(), "tcp", "127.0.0.1:6379")
	require.Error(t, err)

	rm := collectRedisMetrics(t, reader)
	m := findMetric(t, rm, "db.client.connection.create_time")
	hist, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.NotEmpty(t, hist.DataPoints)
	var total uint64
	for _, dp := range hist.DataPoints {
		total += dp.Count
	}
	assert.Equal(t, uint64(1), total, "create_time must only count successful dials")
}

func TestCommandTextTruncationIsRuneSafe(t *testing.T) {
	hook, sr, _ := newTestHook(t, config{commandTextEnabled: true}, "127.0.0.1:6379", 0)

	// "set key " is 8 bytes; each "中" rune is 3 bytes; 339 runes produce
	// 1017 bytes. Total command text = 8 + 1017 = 1025 bytes. A naive byte
	// slice at maxCommandTextLen (1024) cuts inside the 339th rune
	// (bytes 1022-1024), so the rune-safe truncation must back up to byte
	// 1022 to keep the result valid UTF-8.
	value := strings.Repeat("中", 339)

	err := hook.ProcessHook(func(context.Context, goredis.Cmder) error {
		return nil
	})(context.Background(), goredis.NewCmd(context.Background(), "set", "key", value))
	require.NoError(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	v, ok := spanAttr(spans[0], semconv.DBQueryTextKey)
	require.True(t, ok)
	text := v.AsString()
	assert.True(t, utf8.ValidString(text), "truncated text must be valid UTF-8")
	assert.LessOrEqual(t, len(text), maxCommandTextLen)
	// Confirm the truncation actually fired (length must shrink from 1025).
	assert.Less(t, len(text), 1025)
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

func TestPoolMetricsOmitMaxWhenUnbounded(t *testing.T) {
	tp, _, mp, reader := newRedisTestProviders()
	// MaxActiveConns == 0 is go-redis's "no upper bound" sentinel. PoolSize is
	// only a steady-state baseline, so the wrapper must omit
	// db.client.connection.max rather than report a misleading value.
	client := goredis.NewClient(&goredis.Options{
		Addr:           "127.0.0.1:6379",
		PoolSize:       11,
		MaxActiveConns: 0,
	})
	defer client.Close()

	_, err := Wrap(client, tp, mp, WithPoolName("unbounded"))
	require.NoError(t, err)

	rm := collectRedisMetrics(t, reader)
	assert.Nil(t, metricByName(rm, "db.client.connection.max"),
		"db.client.connection.max must be omitted when MaxActiveConns is 0 (unbounded)")
	// Confirm the unconditional pool-metric set still reports — only the .max
	// observations (connection.max here, idle.max in its own test) are
	// conditional.
	assert.NotNil(t, metricByName(rm, "db.client.connection.count"))
	assert.NotNil(t, metricByName(rm, "db.client.connection.idle.min"))
}

func TestPoolMetricsOmitIdleMaxWhenUnbounded(t *testing.T) {
	tp, _, mp, reader := newRedisTestProviders()
	// MaxIdleConns == 0 is go-redis's "no idle cap" sentinel: every returned
	// connection is kept idle. Reporting 0 to db.client.connection.idle.max
	// would invert the semconv meaning ("zero idle allowed"), so the wrapper
	// must omit it. MaxActiveConns is set so db.client.connection.max stays
	// present and we can see idle.max is dropped independently.
	client := goredis.NewClient(&goredis.Options{
		Addr:           "127.0.0.1:6379",
		PoolSize:       11,
		MaxActiveConns: 13,
		MaxIdleConns:   0,
	})
	defer client.Close()

	_, err := Wrap(client, tp, mp, WithPoolName("idle-unbounded"))
	require.NoError(t, err)

	rm := collectRedisMetrics(t, reader)
	assert.Nil(t, metricByName(rm, "db.client.connection.idle.max"),
		"db.client.connection.idle.max must be omitted when MaxIdleConns is 0 (unbounded)")
	assert.NotNil(t, metricByName(rm, "db.client.connection.max"),
		"db.client.connection.max stays present when MaxActiveConns is bounded")
	assert.NotNil(t, metricByName(rm, "db.client.connection.idle.min"))
}

func TestMetricViewsDropRedisotelLegacyInstruments(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(MetricViews([]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10})...),
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
		sdkmetric.WithView(MetricViews([]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10})...),
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
