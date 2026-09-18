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

	"github.com/flywindy/o11y/internal/views"
)

// instrumentationName aliases the scope constant in internal/views so the scope
// this package records its pool metrics under and the scope its pool views
// match cannot drift; the constant lives there because the root package must
// name it without linking the mongo driver (ADR 0026 Option A).
const instrumentationName = views.MongoScope

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
	ready       map[int64]struct{}
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

// defaultPoolNameSeq makes fallback pool names unique per Instrument call so two
// clients pointed at the same host do not collapse into one metric stream. A
// process-local sequence is preferred over the ClientOptions pointer address
// because it is stable and readable across runs.
var defaultPoolNameSeq atomic.Uint64

// defaultPoolName returns the db.client.connection.pool.name label for a
// client: WithPoolName when the caller set it, otherwise the first configured
// host with a sequence number from defaultPoolNameSeq.
func defaultPoolName(opts *options.ClientOptions, override string) string {
	if override != "" {
		return override
	}
	seq := defaultPoolNameSeq.Add(1)
	if opts != nil && len(opts.Hosts) > 0 {
		parsed := parseAddress(opts.Hosts[0])
		if parsed.host != "" {
			return fmt.Sprintf("mongo-%s-%d", parsed.host, seq)
		}
	}
	return fmt.Sprintf("mongo-%d", seq)
}

// handle turns one driver pool event into metric deltas. Every gauge this
// package owns is derived from the event stream rather than a snapshot,
// because the v2 driver exposes no pool-stats call (ADR 0014); the per-address
// poolState holds what that derivation needs to remember.
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
		if state.readyConnection(context.Background(), t.metrics, evt.ConnectionID) {
			recordCreateTime = true
			createTimeSeconds = evt.Duration.Seconds()
		}
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

// state returns the poolState for address, creating it — with its
// pre-built attribute sets — on first sight. The caller holds t.mu.
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
			ready:      make(map[int64]struct{}),
			checkedOut: make(map[int64]struct{}),
		}
		t.pools[key] = state
	}
	return state
}

// setOptions emits the deltas that move db.client.connection.idle.min and
// db.client.connection.max to the pool's configured sizes. It remembers the
// sizes ConnectionPoolCreated reported so a later ConnectionPoolReady, which
// the driver sends with no options, can restate them; an unbounded max
// (MaxPoolSize 0) is taken off the gauge rather than reported as zero, since
// the metric is meant to be absent when there is no limit.
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

// readyConnection records that connectionID finished its handshake and joined
// the pool as an idle connection. It reports whether this was the connection's
// first ConnectionReady, so a repeated event neither double-counts the gauge
// nor records the create-time histogram twice.
func (s *poolState) readyConnection(ctx context.Context, metrics *poolMetrics, connectionID int64) bool {
	if _, ok := s.ready[connectionID]; ok {
		return false
	}
	s.ready[connectionID] = struct{}{}
	s.addConnectionCount(ctx, metrics, 1, s.idleAddOpt)
	return true
}

// checkOutConnection moves connectionID from idle to used. A connection
// already checked out is ignored, and the idle side moves only when there was
// an idle connection to move: a checkout for a connection that never reported
// ready would otherwise push the idle gauge negative.
func (s *poolState) checkOutConnection(ctx context.Context, metrics *poolMetrics, connectionID int64) {
	if _, ok := s.checkedOut[connectionID]; ok {
		return
	}
	wasIdle := len(s.ready) > len(s.checkedOut)
	s.checkedOut[connectionID] = struct{}{}
	s.addConnectionCount(ctx, metrics, 1, s.usedAddOpt)
	if wasIdle {
		s.addConnectionCount(ctx, metrics, -1, s.idleAddOpt)
	}
}

// checkInConnection moves connectionID back from used to idle, ignoring a
// connection this state never saw checked out.
func (s *poolState) checkInConnection(ctx context.Context, metrics *poolMetrics, connectionID int64) {
	if _, ok := s.checkedOut[connectionID]; !ok {
		return
	}
	delete(s.checkedOut, connectionID)
	s.addConnectionCount(ctx, metrics, -1, s.usedAddOpt)
	s.addConnectionCount(ctx, metrics, 1, s.idleAddOpt)
}

// closeConnection unwinds whatever connectionID was counted as. A connection
// the driver closes before it became ready was never counted — the v2 driver
// emits ConnectionCreated then ConnectionClosed with no ConnectionReady when a
// handshake fails — so there is nothing to take off either gauge. Decrementing
// for it would report fewer connections than the pool holds, for good: only
// ConnectionPoolClosed resets this state, so each later failure would drift the
// gauge further.
func (s *poolState) closeConnection(ctx context.Context, metrics *poolMetrics, connectionID int64) {
	if _, ok := s.ready[connectionID]; !ok {
		return
	}
	delete(s.ready, connectionID)
	if _, ok := s.checkedOut[connectionID]; ok {
		delete(s.checkedOut, connectionID)
		s.addConnectionCount(ctx, metrics, -1, s.usedAddOpt)
		return
	}
	s.addConnectionCount(ctx, metrics, -1, s.idleAddOpt)
}

// decrementPending takes one off db.client.connection.pending_requests for a
// checkout that finished, either way it finished. It ignores an event with no
// outstanding request behind it, so a checkout the tracker never saw start
// cannot push the gauge negative.
func (s *poolState) decrementPending(ctx context.Context, metrics *poolMetrics) {
	if s.pending == 0 {
		return
	}
	s.pending--
	metrics.pending.Add(ctx, -1, s.poolAddOpt...)
}

// closePool unwinds everything this state has on the gauges, so a pool the
// driver closes leaves no series stuck at its last value. The tracker drops
// the state afterwards; a pool recreated at the same address starts again from
// zero.
func (s *poolState) closePool(ctx context.Context, metrics *poolMetrics) {
	if used := int64(len(s.checkedOut)); used > 0 {
		s.addConnectionCount(ctx, metrics, -used, s.usedAddOpt)
	}
	if idle := int64(len(s.ready) - len(s.checkedOut)); idle > 0 {
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
