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
	count      metric.Int64UpDownCounter
	idleMin    metric.Int64UpDownCounter
	max        metric.Int64UpDownCounter
	pending    metric.Int64UpDownCounter
	timeouts   metric.Int64Counter
	createTime metric.Float64Histogram
}

func newPoolMetrics(meter metric.Meter) (*poolMetrics, error) {
	count, err := meter.Int64UpDownCounter(
		"db.client.connection.count",
		metric.WithDescription("The number of connections that are currently in state described by the state attribute."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create connection count up-down counter: %w", err)
	}
	idleMin, err := meter.Int64UpDownCounter(
		"db.client.connection.idle.min",
		metric.WithDescription("The minimum number of idle open connections allowed."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create idle min up-down counter: %w", err)
	}
	maxConns, err := meter.Int64UpDownCounter(
		"db.client.connection.max",
		metric.WithDescription("The maximum number of open connections allowed."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create max up-down counter: %w", err)
	}
	pending, err := meter.Int64UpDownCounter(
		"db.client.connection.pending_requests",
		metric.WithDescription("The number of pending requests for an open connection."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create pending requests up-down counter: %w", err)
	}
	timeouts, err := meter.Int64Counter(
		"db.client.connection.timeouts",
		metric.WithDescription("The number of connection timeouts that have occurred trying to obtain a connection from the pool."),
		metric.WithUnit("{timeout}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create timeouts counter: %w", err)
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
		count:      count,
		idleMin:    idleMin,
		max:        maxConns,
		pending:    pending,
		timeouts:   timeouts,
		createTime: createTime,
	}, nil
}

type poolTracker struct {
	mu          sync.Mutex
	metrics     *poolMetrics
	poolName    string
	pools       map[string]*poolState
	cleanupOnce sync.Once
	disabled    atomic.Bool
}

type poolState struct {
	address     string
	attrs       []attribute.KeyValue
	poolAddOpt  []metric.AddOption
	poolRecOpt  []metric.RecordOption
	usedAddOpt  []metric.AddOption
	idleAddOpt  []metric.AddOption
	total       int64
	pending     int64
	emittedMin  int64
	emittedMax  int64
	checkedOut  map[int64]struct{}
	seenCreated bool
	createdMin  uint64
	createdMax  uint64
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
	suffix := "unknown"
	if opts != nil {
		suffix = fmt.Sprintf("%p", opts)
	}
	if opts != nil && len(opts.Hosts) > 0 {
		parsed := parseAddress(opts.Hosts[0])
		if parsed.host != "" {
			return "mongo-" + parsed.host + "-" + suffix
		}
	}
	return "mongo-" + suffix
}

func (t *poolTracker) handle(evt *event.PoolEvent) {
	if evt == nil || t.disabled.Load() {
		return
	}

	var recordCreateTime bool
	var createTimeSeconds float64

	t.mu.Lock()
	state := t.state(evt.Address)
	switch evt.Type {
	case event.ConnectionPoolCreated, event.ConnectionPoolReady:
		state.setOptions(context.Background(), t.metrics, evt.PoolOptions)
	case event.ConnectionReady:
		state.total++
		state.addConnectionCount(context.Background(), t.metrics, 1, state.idleAddOpt)
		recordCreateTime = true
		createTimeSeconds = evt.Duration.Seconds()
	case event.ConnectionClosed:
		state.closeConnection(context.Background(), t.metrics, evt.ConnectionID)
	case event.ConnectionCheckOutStarted:
		state.pending++
		t.metrics.pending.Add(context.Background(), 1, state.poolAddOpt...)
	case event.ConnectionCheckedOut:
		state.decrementPending(context.Background(), t.metrics)
		state.checkOutConnection(context.Background(), t.metrics, evt.ConnectionID)
	case event.ConnectionCheckOutFailed:
		state.decrementPending(context.Background(), t.metrics)
		if evt.Reason == event.ReasonTimedOut {
			t.metrics.timeouts.Add(context.Background(), 1, state.poolAddOpt...)
		}
	case event.ConnectionCheckedIn:
		state.checkInConnection(context.Background(), t.metrics, evt.ConnectionID)
	case event.ConnectionPoolCleared:
		// The driver closes affected connections asynchronously after a clear.
		// Let the subsequent ConnectionClosed events reconcile counts so
		// checked-out work remains visible and counters do not double-decrement.
	case event.ConnectionPoolClosed:
		state.closePool(context.Background(), t.metrics)
		delete(t.pools, poolKey(evt.Address))
	}
	t.mu.Unlock()

	if recordCreateTime {
		t.metrics.createTime.Record(
			context.Background(),
			createTimeSeconds,
			state.poolRecOpt...,
		)
	}
}

func (t *poolTracker) cleanup() error {
	t.cleanupOnce.Do(func() {
		t.disabled.Store(true)
	})
	return nil
}

func (t *poolTracker) state(address string) *poolState {
	key := poolKey(address)
	state, ok := t.pools[key]
	if !ok {
		attrs := t.attrs(address)
		poolOpt := metric.WithAttributeSet(attribute.NewSet(attrs...))
		usedOpt := metric.WithAttributeSet(attribute.NewSet(appendAttribute(attrs, semconv.DBClientConnectionStateUsed)...))
		idleOpt := metric.WithAttributeSet(attribute.NewSet(appendAttribute(attrs, semconv.DBClientConnectionStateIdle)...))
		state = &poolState{
			address:    address,
			attrs:      attrs,
			poolAddOpt: []metric.AddOption{poolOpt},
			poolRecOpt: []metric.RecordOption{poolOpt},
			usedAddOpt: []metric.AddOption{usedOpt},
			idleAddOpt: []metric.AddOption{idleOpt},
			checkedOut: make(map[int64]struct{}),
		}
		t.pools[key] = state
	}
	return state
}

func (s *poolState) setOptions(ctx context.Context, metrics *poolMetrics, opts *event.MonitorPoolOptions) {
	if opts == nil {
		if s.seenCreated {
			opts = &event.MonitorPoolOptions{
				MinPoolSize: s.createdMin,
				MaxPoolSize: s.createdMax,
			}
		} else {
			return
		}
	} else {
		s.seenCreated = true
		s.createdMin = opts.MinPoolSize
		s.createdMax = opts.MaxPoolSize
	}

	minSize := int64(opts.MinPoolSize)
	if delta := minSize - s.emittedMin; delta != 0 {
		metrics.idleMin.Add(ctx, delta, s.poolAddOpt...)
		s.emittedMin = minSize
	}

	maxSize := int64(opts.MaxPoolSize)
	if maxSize == 0 {
		if s.emittedMax != 0 {
			metrics.max.Add(ctx, -s.emittedMax, s.poolAddOpt...)
			s.emittedMax = 0
		}
		return
	}
	if delta := maxSize - s.emittedMax; delta != 0 {
		metrics.max.Add(ctx, delta, s.poolAddOpt...)
		s.emittedMax = maxSize
	}
}

func (s *poolState) checkOutConnection(ctx context.Context, metrics *poolMetrics, connectionID int64) {
	if _, ok := s.checkedOut[connectionID]; ok {
		return
	}
	wasIdle := s.total > int64(len(s.checkedOut))
	s.checkedOut[connectionID] = struct{}{}
	s.addConnectionCount(ctx, metrics, 1, s.usedAddOpt)
	if wasIdle {
		s.addConnectionCount(ctx, metrics, -1, s.idleAddOpt)
	}
}

func (s *poolState) checkInConnection(ctx context.Context, metrics *poolMetrics, connectionID int64) {
	if _, ok := s.checkedOut[connectionID]; !ok {
		return
	}
	delete(s.checkedOut, connectionID)
	s.addConnectionCount(ctx, metrics, -1, s.usedAddOpt)
	s.addConnectionCount(ctx, metrics, 1, s.idleAddOpt)
}

func (s *poolState) closeConnection(ctx context.Context, metrics *poolMetrics, connectionID int64) {
	hadTotal := s.total > 0
	if s.total > 0 {
		s.total--
	}
	if _, ok := s.checkedOut[connectionID]; ok {
		delete(s.checkedOut, connectionID)
		s.addConnectionCount(ctx, metrics, -1, s.usedAddOpt)
		return
	}
	if hadTotal && s.total >= int64(len(s.checkedOut)) {
		s.addConnectionCount(ctx, metrics, -1, s.idleAddOpt)
	}
}

func (s *poolState) decrementPending(ctx context.Context, metrics *poolMetrics) {
	if s.pending == 0 {
		return
	}
	s.pending--
	metrics.pending.Add(ctx, -1, s.poolAddOpt...)
}

func (s *poolState) closePool(ctx context.Context, metrics *poolMetrics) {
	if used := int64(len(s.checkedOut)); used > 0 {
		s.addConnectionCount(ctx, metrics, -used, s.usedAddOpt)
	}
	if idle := s.total - int64(len(s.checkedOut)); idle > 0 {
		s.addConnectionCount(ctx, metrics, -idle, s.idleAddOpt)
	}
	if s.pending > 0 {
		metrics.pending.Add(ctx, -s.pending, s.poolAddOpt...)
	}
	if s.emittedMin != 0 {
		metrics.idleMin.Add(ctx, -s.emittedMin, s.poolAddOpt...)
	}
	if s.emittedMax != 0 {
		metrics.max.Add(ctx, -s.emittedMax, s.poolAddOpt...)
	}
}

func (s *poolState) addConnectionCount(
	ctx context.Context,
	metrics *poolMetrics,
	delta int64,
	opts []metric.AddOption,
) {
	if delta == 0 {
		return
	}
	metrics.count.Add(ctx, delta, opts...)
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

func appendAttribute(attrs []attribute.KeyValue, attr attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs)+1)
	out = append(out, attrs...)
	out = append(out, attr)
	return out
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
