package redis

import (
	"context"
	"errors"
	"math"
	"runtime"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// TestDefaultPoolNameBase pins the base Wrap numbers a pool it names itself
// under: the single-node client's host and port or socket path, the lowest
// seed address or shard name for Cluster and Ring, which have no single
// address, and redis-sentinel for a Sentinel client.
func TestDefaultPoolNameBase(t *testing.T) {
	closeLater := func(c goredis.UniversalClient) goredis.UniversalClient {
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	single := func(opt *goredis.Options) singleOps {
		return singleOps{client: closeLater(goredis.NewClient(opt)).(*goredis.Client)}
	}
	for _, tc := range []struct {
		name string
		ops  clientOps
		want string
	}{
		{"single node", single(&goredis.Options{Addr: "10.1.2.3:6379"}), "redis-10.1.2.3:6379"},
		{"single node, hostname", single(&goredis.Options{Addr: "cache.internal:6380"}), "redis-cache.internal:6380"},
		{"single node, IPv6", single(&goredis.Options{Addr: "[::1]:6379"}), "redis-[::1]:6379"},
		{"single node, empty host", single(&goredis.Options{Addr: ":6379"}), "redis"},
		{"unix socket", single(&goredis.Options{Network: "unix", Addr: "/var/run/redis.sock"}), "redis-/var/run/redis.sock"},
		{"sentinel", singleOps{client: closeLater(goredis.NewFailoverClient(&goredis.FailoverOptions{
			MasterName: "mymaster", SentinelAddrs: []string{"10.1.2.3:26379"},
		})).(*goredis.Client)}, "redis-sentinel"},
		{"cluster", clusterOps{client: closeLater(goredis.NewClusterClient(&goredis.ClusterOptions{
			Addrs: []string{"10.1.2.4:7000", "10.1.2.3:7000"},
		})).(*goredis.ClusterClient)}, "redis-cluster-10.1.2.3:7000"},
		{"cluster without seeds", clusterOps{client: closeLater(goredis.NewClusterClient(&goredis.ClusterOptions{
			ClusterSlots: func(context.Context) ([]goredis.ClusterSlot, error) { return nil, nil },
		})).(*goredis.ClusterClient)}, "redis-cluster"},
		{"ring", ringOps{client: closeLater(goredis.NewRing(&goredis.RingOptions{
			Addrs: map[string]string{"shard-b": "10.1.2.3:6379", "shard-a": "10.1.2.4:6379"},
		})).(*goredis.Ring)}, "redis-ring-shard-a"},
		{"ring without shards", ringOps{client: closeLater(goredis.NewRing(&goredis.RingOptions{})).(*goredis.Ring)}, "redis-ring"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.ops.defaultPoolNameBase())
		})
	}
}

// TestPoolNameAllocator pins the numbering: the lowest free number per base,
// bases independent of each other, a returned number handed out again, and a
// caller-chosen name never handed out on top of.
func TestPoolNameAllocator(t *testing.T) {
	a := poolNameAllocator{held: map[string]int{}}

	assert.Equal(t, "redis-a-1", a.acquire("redis-a"))
	assert.Equal(t, "redis-a-2", a.acquire("redis-a"))
	assert.Equal(t, "redis-b-1", a.acquire("redis-b"), "another base's names do not shift this one's")

	a.release("redis-a-1")
	assert.Equal(t, "redis-a-1", a.acquire("redis-a"), "a returned number is reused")

	a.hold("redis-a-3")
	a.hold("redis-a-3")
	assert.Equal(t, "redis-a-4", a.acquire("redis-a"), "a name set with WithPoolName is skipped")
	a.release("redis-a-3")
	assert.Equal(t, "redis-a-5", a.acquire("redis-a"), "a name stays held while any wrapper still holds it")

	for _, name := range []string{"redis-a-1", "redis-a-2", "redis-b-1", "redis-a-3", "redis-a-4", "redis-a-5"} {
		a.release(name)
	}
	assert.Empty(t, a.held, "a name no wrapper holds is dropped")
}

// seriesPoolNames returns the db.client.connection.pool.name values on the
// connection-count series.
func seriesPoolNames(t *testing.T, reader *sdkmetric.ManualReader) map[string]struct{} {
	t.Helper()
	names := map[string]struct{}{}
	m := metricByName(collectRedisMetrics(t, reader), "db.client.connection.count")
	if m == nil {
		return names
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	for _, dp := range sum.DataPoints {
		v, ok := dp.Attributes.Value(semconv.DBClientConnectionPoolNameKey)
		require.True(t, ok)
		names[v.AsString()] = struct{}{}
	}
	return names
}

// TestWrapDefaultPoolNames wraps clients on one address without WithPoolName.
// Each pool gets its own series, named from the host and a number rather than
// from the client's pointer address, which changed on every restart and
// started new series each time; a repeated Wrap keeps its name, and a client
// wrapped after another was unwrapped takes the freed name. The address is
// unique to this test, so the numbers start at 1.
func TestWrapDefaultPoolNames(t *testing.T) {
	tp, _, mp, reader := newRedisTestProviders()
	newClient := func() *goredis.Client {
		client := goredis.NewClient(&goredis.Options{Addr: "pool-name-test.invalid:6379"})
		t.Cleanup(func() { _ = client.Close() })
		t.Cleanup(func() { Unwrap(client) })
		_, err := Wrap(client, tp, mp)
		require.NoError(t, err)
		return client
	}

	first := newClient()
	newClient()
	// A repeated Wrap is an idempotent no-op and must not take a new name.
	_, err := Wrap(first, tp, mp)
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{
		"redis-pool-name-test.invalid:6379-1": {},
		"redis-pool-name-test.invalid:6379-2": {},
	}, seriesPoolNames(t, reader), "two pools on one address keep separate series")

	// A client rebuilt after the first was unwrapped takes over its name and
	// so continues its series.
	Unwrap(first)
	newClient()
	assert.Equal(t, map[string]struct{}{
		"redis-pool-name-test.invalid:6379-1": {},
		"redis-pool-name-test.invalid:6379-2": {},
	}, seriesPoolNames(t, reader))
}

// TestCollectedClientReturnsPoolName drops a wrapped client without Unwrap and
// checks the runtime cleanup gives its name back once the client is collected.
func TestCollectedClientReturnsPoolName(t *testing.T) {
	tp, _, mp, _ := newRedisTestProviders()
	const name = "redis-pool-name-gc.invalid:6379-1"
	func() {
		client := goredis.NewClient(&goredis.Options{Addr: "pool-name-gc.invalid:6379"})
		_, err := Wrap(client, tp, mp)
		require.NoError(t, err)
		poolNames.mu.Lock()
		held := poolNames.held[name]
		poolNames.mu.Unlock()
		require.Equal(t, 1, held, "Wrap holds the name")
		require.NoError(t, client.Close())
	}()
	require.Eventually(t, func() bool {
		runtime.GC()
		poolNames.mu.Lock()
		defer poolNames.mu.Unlock()
		return poolNames.held[name] == 0
	}, 5*time.Second, 10*time.Millisecond, "the collected client's name must be given back")
}

// forwardingMeterProvider is a distinct decorator value over a shared provider.
type forwardingMeterProvider struct{ metric.MeterProvider }

// TestWrapDefaultPoolNamesAcrossDecorators wraps two clients on one address
// through two decorators of one MeterProvider. Both pools reach the same
// exporter, so they must not share a name: the shared series would carry two
// points with identical attributes.
func TestWrapDefaultPoolNamesAcrossDecorators(t *testing.T) {
	tp, _, mp, reader := newRedisTestProviders()
	const addr = "pool-name-decorator.invalid:6379"
	for range 2 {
		client := goredis.NewClient(&goredis.Options{Addr: addr})
		t.Cleanup(func() { _ = client.Close() })
		t.Cleanup(func() { Unwrap(client) })
		_, err := Wrap(client, tp, &forwardingMeterProvider{MeterProvider: mp})
		require.NoError(t, err)
	}
	assert.Equal(t, map[string]struct{}{
		"redis-" + addr + "-1": {},
		"redis-" + addr + "-2": {},
	}, seriesPoolNames(t, reader))
}

// TestWrapExplicitPoolNameIsHeld checks Wrap holds a WithPoolName name, so a
// default is not allocated on top of it, and gives it back on Unwrap.
func TestWrapExplicitPoolNameIsHeld(t *testing.T) {
	tp, _, mp, reader := newRedisTestProviders()
	const addr = "pool-name-explicit.invalid:6379"
	wrap := func(opts ...Option) *goredis.Client {
		client := goredis.NewClient(&goredis.Options{Addr: addr})
		t.Cleanup(func() { _ = client.Close() })
		t.Cleanup(func() { Unwrap(client) })
		_, err := Wrap(client, tp, mp, opts...)
		require.NoError(t, err)
		return client
	}

	explicit := wrap(WithPoolName("redis-" + addr + "-1"))
	wrap()
	assert.Equal(t, map[string]struct{}{
		"redis-" + addr + "-1": {},
		"redis-" + addr + "-2": {},
	}, seriesPoolNames(t, reader), "the default skips the name the caller chose")

	Unwrap(explicit)
	wrap()
	assert.Equal(t, map[string]struct{}{
		"redis-" + addr + "-1": {},
		"redis-" + addr + "-2": {},
	}, seriesPoolNames(t, reader), "Unwrap gives the caller's name back")
}

type failingMeterProvider struct{ noop.MeterProvider }

func (failingMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return failingMeter{}
}

type failingMeter struct{ noop.Meter }

func (failingMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errors.New("histogram unavailable")
}

// TestWrapFailureReturnsPoolName checks a Wrap that fails before committing
// gives its default name's number back, so a retry, and every pool named after
// it, gets the name a clean run would have.
func TestWrapFailureReturnsPoolName(t *testing.T) {
	tp, _, mp, reader := newRedisTestProviders()
	client := goredis.NewClient(&goredis.Options{Addr: "pool-name-failure.invalid:6379"})
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { Unwrap(client) })

	_, err := Wrap(client, tp, failingMeterProvider{})
	require.Error(t, err)
	_, err = Wrap(client, tp, mp)
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"redis-pool-name-failure.invalid:6379-1": {}}, seriesPoolNames(t, reader))
}

// TestConnectionCounts pins the used and idle counts against go-redis's two
// separately locked reads: when IdleConns momentarily exceeds TotalConns, idle
// is capped at the total and used is zero, not a uint32 wrap to about 4.29e9.
func TestConnectionCounts(t *testing.T) {
	for _, tc := range []struct {
		name               string
		total, idle        uint32
		wantUsed, wantIdle int64
	}{
		{"some in use", 5, 2, 3, 2},
		{"all idle", 4, 4, 0, 4},
		{"empty", 0, 0, 0, 0},
		{"idle read ahead of total", 2, 5, 0, 2},
		{"large pool", math.MaxUint32, 0, math.MaxUint32, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			used, idle := connectionCounts(&goredis.PoolStats{TotalConns: tc.total, IdleConns: tc.idle})
			assert.Equal(t, tc.wantUsed, used, "used")
			assert.Equal(t, tc.wantIdle, idle, "idle")
		})
	}
}
