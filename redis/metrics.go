package redis

import (
	"context"
	"fmt"
	"sync/atomic"
	"weak"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// poolTarget abstracts the per-topology pool observation walk.
// Implementations live in this file; the type switch over go-redis concrete
// client types lives in newClientOps so the rest of the package stays
// polymorphic.

type poolMetrics struct {
	meter       metric.Meter
	count       metric.Int64ObservableUpDownCounter
	idleMax     metric.Int64ObservableUpDownCounter
	idleMin     metric.Int64ObservableUpDownCounter
	max         metric.Int64ObservableUpDownCounter
	timeouts    metric.Int64ObservableCounter
	createTime  metric.Float64Histogram
	observables []metric.Observable
}

func newPoolMetrics(meter metric.Meter) (*poolMetrics, error) {
	count, err := meter.Int64ObservableUpDownCounter(
		"db.client.connection.count",
		metric.WithDescription("The number of connections that are currently in state described by the state attribute."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("redis wrap: create connection count observable: %w", err)
	}
	idleMax, err := meter.Int64ObservableUpDownCounter(
		"db.client.connection.idle.max",
		metric.WithDescription("The maximum number of idle open connections allowed."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("redis wrap: create idle max observable: %w", err)
	}
	idleMin, err := meter.Int64ObservableUpDownCounter(
		"db.client.connection.idle.min",
		metric.WithDescription("The minimum number of idle open connections allowed."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("redis wrap: create idle min observable: %w", err)
	}
	maxConns, err := meter.Int64ObservableUpDownCounter(
		"db.client.connection.max",
		metric.WithDescription("The maximum number of open connections allowed."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("redis wrap: create max observable: %w", err)
	}
	timeouts, err := meter.Int64ObservableCounter(
		"db.client.connection.timeouts",
		metric.WithDescription("The number of connection timeouts that have occurred trying to obtain a connection from the pool."),
		metric.WithUnit("{timeout}"),
	)
	if err != nil {
		return nil, fmt.Errorf("redis wrap: create timeouts observable: %w", err)
	}
	createTime, err := meter.Float64Histogram(
		"db.client.connection.create_time",
		metric.WithDescription("The time it took to create a new connection."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("redis wrap: create connection create_time histogram: %w", err)
	}

	return &poolMetrics{
		meter:      meter,
		count:      count,
		idleMax:    idleMax,
		idleMin:    idleMin,
		max:        maxConns,
		timeouts:   timeouts,
		createTime: createTime,
		observables: []metric.Observable{
			count,
			idleMax,
			idleMin,
			maxConns,
			timeouts,
		},
	}, nil
}

func (p *poolMetrics) register(target poolTarget, disabled *atomic.Bool) (metric.Registration, error) {
	reg, err := p.meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		if disabled.Load() {
			return nil
		}
		target.observe(ctx, p, observer)
		return nil
	}, p.observables...)
	if err != nil {
		return nil, fmt.Errorf("redis wrap: register pool metrics callback: %w", err)
	}
	return reg, nil
}

type poolTarget interface {
	observe(context.Context, *poolMetrics, metric.Observer)
}

type singlePoolTarget struct {
	client   weak.Pointer[goredis.Client]
	poolName string
}

func (t singlePoolTarget) observe(_ context.Context, p *poolMetrics, observer metric.Observer) {
	if client := t.client.Value(); client != nil {
		p.observeClient(observer, client, t.poolName)
	}
}

type clusterPoolTarget struct {
	client   weak.Pointer[goredis.ClusterClient]
	poolName string
}

func (t clusterPoolTarget) observe(ctx context.Context, p *poolMetrics, observer metric.Observer) {
	client := t.client.Value()
	if client == nil {
		return
	}
	_ = client.ForEachShard(ctx, func(_ context.Context, shard *goredis.Client) error {
		p.observeClient(observer, shard, shardPoolName(t.poolName, shard))
		return nil
	})
}

type ringPoolTarget struct {
	client   weak.Pointer[goredis.Ring]
	poolName string
}

func (t ringPoolTarget) observe(ctx context.Context, p *poolMetrics, observer metric.Observer) {
	client := t.client.Value()
	if client == nil {
		return
	}
	_ = client.ForEachShard(ctx, func(_ context.Context, shard *goredis.Client) error {
		p.observeClient(observer, shard, shardPoolName(t.poolName, shard))
		return nil
	})
}

func (p *poolMetrics) observeClient(observer metric.Observer, client *goredis.Client, poolName string) {
	stats := client.PoolStats()
	opt := client.Options()
	attrs := poolAttrs(poolName, opt.Addr)

	used := int64(stats.TotalConns - stats.IdleConns)
	observer.ObserveInt64(p.count, used, metric.WithAttributes(append(attrs,
		attribute.String("db.client.connection.state", "used"),
	)...))
	observer.ObserveInt64(p.count, int64(stats.IdleConns), metric.WithAttributes(append(attrs,
		attribute.String("db.client.connection.state", "idle"),
	)...))
	observer.ObserveInt64(p.idleMax, int64(opt.MaxIdleConns), metric.WithAttributes(attrs...))
	observer.ObserveInt64(p.idleMin, int64(opt.MinIdleConns), metric.WithAttributes(attrs...))
	if maxConns, ok := maxConnections(opt); ok {
		observer.ObserveInt64(p.max, int64(maxConns), metric.WithAttributes(attrs...))
	}
	observer.ObserveInt64(p.timeouts, int64(stats.Timeouts), metric.WithAttributes(attrs...))
}

func poolAttrs(poolName, addr string) []attribute.KeyValue {
	parsed := parseAddress(addr)
	attrs := []attribute.KeyValue{
		semconv.DBSystemNameRedis,
		attribute.String("db.client.connection.pool.name", poolName),
		semconv.ServerAddress(parsed.host),
	}
	if parsed.port > 0 {
		attrs = append(attrs, semconv.ServerPort(parsed.port))
	}
	return attrs
}

// maxConnections returns the hard upper bound on connections that go-redis
// will allocate for the pool, plus a flag indicating whether a hard bound
// exists. go-redis treats MaxActiveConns == 0 as "unbounded" (PoolSize is only
// a steady-state target, not a cap), so falling back to PoolSize would
// under-report db.client.connection.max and mislead saturation dashboards.
// When the pool is unbounded the wrapper omits the observation entirely
// rather than emit a misleading value.
func maxConnections(opt *goredis.Options) (int, bool) {
	if opt.MaxActiveConns > 0 {
		return opt.MaxActiveConns, true
	}
	return 0, false
}
