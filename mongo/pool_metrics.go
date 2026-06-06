package mongo

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

const instrumentationName = "github.com/flywindy/o11y/mongo"

type poolMetrics struct {
	meter       metric.Meter
	count       metric.Int64ObservableUpDownCounter
	idleMin     metric.Int64ObservableUpDownCounter
	max         metric.Int64ObservableUpDownCounter
	pending     metric.Int64ObservableUpDownCounter
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
		return nil, fmt.Errorf("create connection count observable: %w", err)
	}
	idleMin, err := meter.Int64ObservableUpDownCounter(
		"db.client.connection.idle.min",
		metric.WithDescription("The minimum number of idle open connections allowed."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create idle min observable: %w", err)
	}
	maxConns, err := meter.Int64ObservableUpDownCounter(
		"db.client.connection.max",
		metric.WithDescription("The maximum number of open connections allowed."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create max observable: %w", err)
	}
	pending, err := meter.Int64ObservableUpDownCounter(
		"db.client.connection.pending_requests",
		metric.WithDescription("The number of pending requests for an open connection."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create pending requests observable: %w", err)
	}
	timeouts, err := meter.Int64ObservableCounter(
		"db.client.connection.timeouts",
		metric.WithDescription("The number of connection timeouts that have occurred trying to obtain a connection from the pool."),
		metric.WithUnit("{timeout}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create timeouts observable: %w", err)
	}
	createTime, err := meter.Float64Histogram(
		"db.client.connection.create_time",
		metric.WithDescription("The time it took to create a new connection."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create connection create_time histogram: %w", err)
	}

	return &poolMetrics{
		meter:      meter,
		count:      count,
		idleMin:    idleMin,
		max:        maxConns,
		pending:    pending,
		timeouts:   timeouts,
		createTime: createTime,
		observables: []metric.Observable{
			count,
			idleMin,
			maxConns,
			pending,
			timeouts,
		},
	}, nil
}

type poolTracker struct {
	mu          sync.Mutex
	metrics     *poolMetrics
	poolName    string
	pools       map[string]*poolState
	reg         metric.Registration
	cleanupOnce sync.Once
	cleanupErr  error
	disabled    atomic.Bool
}

type poolState struct {
	address  string
	total    int64
	used     int64
	pending  int64
	timeouts int64
	min      uint64
	max      uint64
}

func newPoolMonitor(
	opts *options.ClientOptions,
	mp metric.MeterProvider,
	poolName string,
) (*event.PoolMonitor, func(context.Context) error, error) {
	meter := mp.Meter(instrumentationName)
	metrics, err := newPoolMetrics(meter)
	if err != nil {
		return nil, nil, fmt.Errorf("create MongoDB pool metrics: %w", err)
	}

	tracker := &poolTracker{
		metrics:  metrics,
		poolName: defaultPoolName(opts, poolName),
		pools:    make(map[string]*poolState),
	}
	reg, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		tracker.observe(observer)
		return nil
	}, metrics.observables...)
	if err != nil {
		return nil, nil, fmt.Errorf("register MongoDB pool metrics callback: %w", err)
	}
	tracker.reg = reg

	monitor := &event.PoolMonitor{
		Event: tracker.handle,
	}
	cleanup := func(context.Context) error {
		return tracker.cleanup()
	}
	return monitor, cleanup, nil
}

func defaultPoolName(opts *options.ClientOptions, override string) string {
	if override != "" {
		return override
	}
	if opts != nil && len(opts.Hosts) > 0 {
		parsed := parseAddress(opts.Hosts[0])
		if parsed.host != "" {
			return "mongo-" + parsed.host
		}
	}
	return "mongo"
}

func (t *poolTracker) handle(evt *event.PoolEvent) {
	if evt == nil || t.disabled.Load() {
		return
	}

	var recordCreateTime bool
	var createTimeSeconds float64
	var createTimeAddress string
	shouldCleanup := false

	t.mu.Lock()
	state := t.state(evt.Address)
	switch evt.Type {
	case event.ConnectionPoolCreated, event.ConnectionPoolReady:
		state.setOptions(evt.PoolOptions)
	case event.ConnectionReady:
		state.total++
		recordCreateTime = true
		createTimeSeconds = evt.Duration.Seconds()
		createTimeAddress = evt.Address
	case event.ConnectionClosed:
		state.total = decrement(state.total)
		if state.used > state.total {
			state.used = state.total
		}
	case event.ConnectionCheckOutStarted:
		state.pending++
	case event.ConnectionCheckedOut:
		state.pending = decrement(state.pending)
		state.used++
	case event.ConnectionCheckOutFailed:
		state.pending = decrement(state.pending)
		if evt.Reason == event.ReasonTimedOut {
			state.timeouts++
		}
	case event.ConnectionCheckedIn:
		state.used = decrement(state.used)
	case event.ConnectionPoolCleared:
		state.reset()
	case event.ConnectionPoolClosed:
		delete(t.pools, poolKey(evt.Address))
		shouldCleanup = len(t.pools) == 0
	}
	t.mu.Unlock()

	if recordCreateTime {
		t.metrics.createTime.Record(
			context.Background(),
			createTimeSeconds,
			metric.WithAttributes(t.attrs(createTimeAddress)...),
		)
	}
	if shouldCleanup {
		_ = t.cleanup()
	}
}

func (t *poolTracker) cleanup() error {
	t.cleanupOnce.Do(func() {
		t.disabled.Store(true)
		if t.reg != nil {
			t.cleanupErr = t.reg.Unregister()
		}
	})
	return t.cleanupErr
}

func (t *poolTracker) state(address string) *poolState {
	key := poolKey(address)
	state, ok := t.pools[key]
	if !ok {
		state = &poolState{address: address}
		t.pools[key] = state
	}
	return state
}

func (s *poolState) setOptions(opts *event.MonitorPoolOptions) {
	if opts == nil {
		return
	}
	s.min = opts.MinPoolSize
	s.max = opts.MaxPoolSize
}

func (s *poolState) reset() {
	s.total = 0
	s.used = 0
	s.pending = 0
	s.min = 0
	s.max = 0
}

func (t *poolTracker) observe(observer metric.Observer) {
	if t.disabled.Load() {
		return
	}

	t.mu.Lock()
	snapshots := make([]poolState, 0, len(t.pools))
	for _, state := range t.pools {
		snapshots = append(snapshots, *state)
	}
	t.mu.Unlock()

	for _, state := range snapshots {
		attrs := t.attrs(state.address)
		used := clampNonNegative(state.used)
		total := clampNonNegative(state.total)
		idle := total - used
		if idle < 0 {
			idle = 0
		}

		observer.ObserveInt64(t.metrics.count, used, metric.WithAttributes(append(attrs,
			semconv.DBClientConnectionStateUsed,
		)...))
		observer.ObserveInt64(t.metrics.count, idle, metric.WithAttributes(append(attrs,
			semconv.DBClientConnectionStateIdle,
		)...))
		observer.ObserveInt64(t.metrics.idleMin, int64(state.min), metric.WithAttributes(attrs...))
		if state.max > 0 {
			observer.ObserveInt64(t.metrics.max, int64(state.max), metric.WithAttributes(attrs...))
		}
		observer.ObserveInt64(t.metrics.pending, clampNonNegative(state.pending), metric.WithAttributes(attrs...))
		observer.ObserveInt64(t.metrics.timeouts, clampNonNegative(state.timeouts), metric.WithAttributes(attrs...))
	}
}

func (t *poolTracker) attrs(address string) []attribute.KeyValue {
	parsed := parseAddress(address)
	attrs := []attribute.KeyValue{
		semconv.DBSystemNameMongoDB,
		semconv.DBClientConnectionPoolName(t.poolName),
	}
	if parsed.host != "" {
		attrs = append(attrs, semconv.ServerAddress(parsed.host))
	}
	if parsed.port > 0 {
		attrs = append(attrs, semconv.ServerPort(parsed.port))
	}
	return attrs
}

type parsedAddress struct {
	host string
	port int
}

func parseAddress(address string) parsedAddress {
	address = strings.TrimSpace(address)
	if address == "" {
		return parsedAddress{}
	}

	host, portText, err := net.SplitHostPort(address)
	if err == nil {
		return parsedAddress{host: strings.Trim(host, "[]"), port: parsePort(portText)}
	}

	idx := strings.LastIndex(address, ":")
	if idx > 0 && idx < len(address)-1 && strings.Count(address, ":") == 1 {
		return parsedAddress{host: address[:idx], port: parsePort(address[idx+1:])}
	}
	return parsedAddress{host: strings.Trim(address, "[]")}
}

func parsePort(portText string) int {
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

func poolKey(address string) string {
	return strings.TrimSpace(address)
}

func decrement(value int64) int64 {
	if value <= 0 {
		return 0
	}
	return value - 1
}

func clampNonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
