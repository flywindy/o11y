package redis

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"weak"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

var wrappedClients sync.Map

type wrapEntry struct {
	mu       sync.Mutex
	ref      clientRef
	done     bool
	disabled atomic.Bool
	reg      metric.Registration
	cleanup  runtime.Cleanup
}

type cleanupArg struct {
	key uintptr
	ref clientRef
}

type clientRef interface {
	alive() bool
	matches(goredis.UniversalClient) bool
}

type weakClientRef[T any] struct {
	ptr weak.Pointer[T]
}

func (r weakClientRef[T]) alive() bool {
	return r.ptr.Value() != nil
}

func (r weakClientRef[T]) matches(client goredis.UniversalClient) bool {
	want, ok := any(client).(*T)
	return ok && r.ptr.Value() == want
}

// clientOps captures the per-concrete-type behaviour of the three supported
// go-redis client types. Centralising the type switch here keeps the rest of
// the package free of concrete-type branches.
type clientOps interface {
	key() uintptr
	ref() clientRef
	target(poolName string) poolTarget
	install(ctx context.Context, p hookParams) error
	addCleanup(arg cleanupArg) runtime.Cleanup
}

type hookParams struct {
	tp                trace.TracerProvider
	operationDuration metric.Float64Histogram
	connectionCreate  metric.Float64Histogram
	cfg               config
	poolName          string
	disabled          *atomic.Bool
}

// newClientOps validates the concrete client type and returns an ops handle
// that downstream code uses without further type switching.
func newClientOps(rdb goredis.UniversalClient) (clientOps, error) {
	switch c := rdb.(type) {
	case *goredis.Client:
		if c == nil {
			return nil, errors.New("redis wrap: client must not be nil")
		}
		return singleOps{client: c}, nil
	case *goredis.ClusterClient:
		if c == nil {
			return nil, errors.New("redis wrap: client must not be nil")
		}
		return clusterOps{client: c}, nil
	case *goredis.Ring:
		if c == nil {
			return nil, errors.New("redis wrap: client must not be nil")
		}
		return ringOps{client: c}, nil
	default:
		return nil, fmt.Errorf("redis wrap: unsupported client type %T", rdb)
	}
}

type singleOps struct {
	client *goredis.Client
}

func (o singleOps) key() uintptr { return clientKey(o.client) }
func (o singleOps) ref() clientRef {
	return weakClientRef[goredis.Client]{ptr: weak.Make(o.client)}
}
func (o singleOps) target(poolName string) poolTarget {
	return singlePoolTarget{client: weak.Make(o.client), poolName: poolName}
}
func (o singleOps) install(_ context.Context, p hookParams) error {
	addClientHook(o.client, p.tp, p.operationDuration, p.connectionCreate, p.cfg, p.poolName, p.disabled)
	return nil
}
func (o singleOps) addCleanup(arg cleanupArg) runtime.Cleanup {
	return runtime.AddCleanup(o.client, cleanupWrappedClient, arg)
}

type clusterOps struct {
	client *goredis.ClusterClient
}

func (o clusterOps) key() uintptr { return clientKey(o.client) }
func (o clusterOps) ref() clientRef {
	return weakClientRef[goredis.ClusterClient]{ptr: weak.Make(o.client)}
}
func (o clusterOps) target(poolName string) poolTarget {
	return clusterPoolTarget{client: weak.Make(o.client), poolName: poolName}
}
func (o clusterOps) install(ctx context.Context, p hookParams) error {
	return installShardedHooks(ctx, p, o.client.OnNewNode, o.client.ForEachShard)
}
func (o clusterOps) addCleanup(arg cleanupArg) runtime.Cleanup {
	return runtime.AddCleanup(o.client, cleanupWrappedClient, arg)
}

type ringOps struct {
	client *goredis.Ring
}

func (o ringOps) key() uintptr { return clientKey(o.client) }
func (o ringOps) ref() clientRef {
	return weakClientRef[goredis.Ring]{ptr: weak.Make(o.client)}
}
func (o ringOps) target(poolName string) poolTarget {
	return ringPoolTarget{client: weak.Make(o.client), poolName: poolName}
}
func (o ringOps) install(ctx context.Context, p hookParams) error {
	return installShardedHooks(ctx, p, o.client.OnNewNode, o.client.ForEachShard)
}
func (o ringOps) addCleanup(arg cleanupArg) runtime.Cleanup {
	return runtime.AddCleanup(o.client, cleanupWrappedClient, arg)
}

// installShardedHooks installs OnNewNode (covering nodes materialised after
// Wrap returns) and walks the warmed shard set (covering already-existing
// nodes), deduping across the two paths so a node that appears mid-walk is
// hooked exactly once.
//
// A ForEachShard error is returned to the caller, which treats it as a
// best-effort outcome rather than a hard failure: go-redis v9 has no
// hook-remove API, so any per-node hooks installed before the error stay
// attached and continue to emit telemetry for the healthy shards.
func installShardedHooks(
	ctx context.Context,
	p hookParams,
	onNewNode func(func(*goredis.Client)),
	forEachShard func(context.Context, func(context.Context, *goredis.Client) error) error,
) error {
	var seenPtr atomic.Pointer[sync.Map]
	seenPtr.Store(&sync.Map{})

	hook := func(node *goredis.Client) {
		if node == nil || p.disabled.Load() {
			return
		}
		if seen := seenPtr.Load(); seen != nil {
			if _, loaded := seen.LoadOrStore(clientKey(node), struct{}{}); loaded {
				return
			}
		}
		poolName := shardPoolName(p.poolName, node)
		addClientHook(node, p.tp, p.operationDuration, p.connectionCreate, p.cfg, poolName, p.disabled)
	}

	onNewNode(hook)
	err := forEachShard(ctx, func(_ context.Context, node *goredis.Client) error {
		hook(node)
		return nil
	})
	// Release the setup-phase dedup map. Subsequent OnNewNode firings rely on
	// go-redis only invoking the callback for genuinely new nodes, so no
	// further deduplication is needed and we avoid retaining a reference to
	// shard clients beyond the setup window.
	seenPtr.Store(nil)
	return err
}

// Wrap installs o11y Redis tracing and metrics on an existing go-redis client.
//
// The returned value is the same client passed in, allowing callers to chain
// setup while retaining full control of go-redis options such as addresses,
// TLS, authentication, pool sizes, and timeouts.
//
// Hook ordering matters. go-redis runs hooks outermost-first in the order they
// were added (hook-1 start -> hook-2 start -> exec -> hook-2 end -> hook-1
// end), so the span Wrap opens only encloses hooks added after it. Call Wrap
// before any client.AddHook(...) of your own if you want the span and its
// recorded duration to cover those hooks' work.
//
// Wrap is idempotent: calling it twice on the same client is a no-op after the
// first successful (or best-effort) call. Options passed to a subsequent Wrap
// call are ignored — the configuration (pool name, attributes, command-text
// setting) from the first call stands. To change options, Unwrap first and then
// Wrap again. A best-effort call is one where the
// initial per-shard setup walk on a warmed Cluster/Ring returned an error
// (e.g. an unreachable shard). In that case the metric callback and any
// per-shard hooks installed before the error stay live, the dedup entry is
// committed, and subsequent Wrap calls on the same client return (rdb, nil) so
// they do not double-hook nodes that v9 cannot un-hook. Use Unwrap to fully
// tear down and re-Wrap if recovery is required.
//
// Wrap returns a non-nil error for:
//   - nil rdb / nil tp / nil mp;
//   - unsupported concrete client type (anything that is not *redis.Client,
//     *redis.ClusterClient, or *redis.Ring);
//   - failure to create the wrapper's metric instruments or register the pool
//     metrics callback — these are strict pre-commit errors that leave no
//     state behind and can be retried cleanly;
//   - per-shard setup failure on a warmed Cluster/Ring — this is the
//     best-effort case described above.
func Wrap(
	rdb goredis.UniversalClient,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	opts ...Option,
) (goredis.UniversalClient, error) {
	if rdb == nil {
		return nil, errors.New("redis wrap: client must not be nil")
	}
	if tp == nil {
		return rdb, errors.New("redis wrap: tracer provider must not be nil")
	}
	if mp == nil {
		return rdb, errors.New("redis wrap: meter provider must not be nil")
	}

	ops, err := newClientOps(rdb)
	if err != nil {
		return rdb, err
	}
	key := ops.key()

	for {
		candidate := &wrapEntry{ref: ops.ref()}
		actual, loaded := wrappedClients.LoadOrStore(key, candidate)
		entry := candidate
		if loaded {
			entry = actual.(*wrapEntry)
		}
		entry.mu.Lock()

		// Canonicity: a sibling goroutine's strict pre-commit failure may have
		// CompareAndDeleted this entry while we were blocked on the mutex,
		// leaving us holding an orphan. Restart the loop so a fresh
		// LoadOrStore can install a new placeholder.
		current, ok := wrappedClients.Load(key)
		if !ok || current != entry {
			entry.mu.Unlock()
			continue
		}

		// Identity: the dedup entry must still resolve to this exact client
		// allocation. A mismatch means the Go allocator reused the address for
		// a different client after the previous one was GC'd but before its
		// runtime.AddCleanup callback evicted the stale entry. Tear the stale
		// state down deterministically and restart.
		if !entry.ref.matches(rdb) {
			entry.disabled.Store(true)
			if entry.reg != nil {
				_ = entry.reg.Unregister()
			}
			if entry.done {
				entry.cleanup.Stop()
			}
			wrappedClients.CompareAndDelete(key, entry)
			entry.mu.Unlock()
			continue
		}

		// Steady-state idempotent hit (also serves best-effort retries).
		if entry.done {
			entry.mu.Unlock()
			return rdb, nil
		}

		cfg := newConfig(opts)
		if cfg.poolName == "" {
			cfg.poolName = fmt.Sprintf("redis-%x", key)
		}

		meter := mp.Meter(instrumentationName, metric.WithSchemaURL(semconv.SchemaURL))
		operationDuration, instErr := meter.Float64Histogram(
			"db.client.operation.duration",
			metric.WithDescription("Duration of database client operations."),
			metric.WithUnit("s"),
		)
		if instErr != nil {
			wrappedClients.CompareAndDelete(key, entry)
			entry.mu.Unlock()
			return rdb, fmt.Errorf("redis wrap: create operation duration histogram: %w", instErr)
		}
		pool, instErr := newPoolMetrics(meter)
		if instErr != nil {
			wrappedClients.CompareAndDelete(key, entry)
			entry.mu.Unlock()
			return rdb, instErr
		}
		reg, instErr := pool.register(ops.target(cfg.poolName), &entry.disabled)
		if instErr != nil {
			wrappedClients.CompareAndDelete(key, entry)
			entry.mu.Unlock()
			return rdb, instErr
		}
		entry.reg = reg

		installErr := ops.install(context.Background(), hookParams{
			tp:                tp,
			operationDuration: operationDuration,
			connectionCreate:  pool.createTime,
			cfg:               cfg,
			poolName:          cfg.poolName,
			disabled:          &entry.disabled,
		})

		// Commit the entry on both nil and best-effort errors. go-redis v9 has
		// no public hook-remove API, so per-shard hooks installed before a
		// shard-collection failure stay attached; rolling back would leave a
		// retry-loop where each retry adds a fresh OnNewNode callback and a
		// fresh hook on the already-touched shards. Instead, the metric
		// callback keeps observing the live shard set, retries are no-ops via
		// the entry.done check above, and Unwrap remains the canonical way to
		// fully detach.
		entry.done = true
		entry.cleanup = ops.addCleanup(cleanupArg{key: key, ref: entry.ref})
		runtime.KeepAlive(rdb)
		entry.mu.Unlock()
		if installErr != nil {
			return rdb, fmt.Errorf("redis wrap: install hooks: %w", installErr)
		}
		return rdb, nil
	}
}

// Unwrap disables instrumentation previously installed by Wrap.
//
// go-redis does not expose a hook removal API, so Unwrap gates previously
// installed hook closures via the shared disabled flag, unregisters this
// package's metric callback, and cancels the cleanup hook. A later Wrap call
// on the same live client installs a fresh wrapper with its own disabled flag;
// the previously-attached hooks remain in the client's hook chain but observe
// disabled=true and emit nothing.
func Unwrap(rdb goredis.UniversalClient) {
	if rdb == nil {
		return
	}
	ops, err := newClientOps(rdb)
	if err != nil {
		return
	}
	key := ops.key()
	actual, ok := wrappedClients.Load(key)
	if !ok {
		return
	}
	entry := actual.(*wrapEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.ref.matches(rdb) {
		return
	}
	entry.disabled.Store(true)
	if entry.reg != nil {
		_ = entry.reg.Unregister()
	}
	if entry.done {
		entry.cleanup.Stop()
	}
	wrappedClients.CompareAndDelete(key, entry)
	runtime.KeepAlive(rdb)
}

func addClientHook(
	client *goredis.Client,
	tp trace.TracerProvider,
	operationDuration metric.Float64Histogram,
	connectionCreate metric.Float64Histogram,
	cfg config,
	poolName string,
	disabled *atomic.Bool,
) {
	client.AddHook(newRedisHook(tp, operationDuration, connectionCreate, cfg, client, poolName, disabled))
}

func clientKey(client any) uintptr {
	return reflect.ValueOf(client).Pointer()
}

func cleanupWrappedClient(arg cleanupArg) {
	actual, ok := wrappedClients.Load(arg.key)
	if !ok {
		return
	}
	entry := actual.(*wrapEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.ref.alive() && entry.ref == arg.ref {
		entry.disabled.Store(true)
		if entry.reg != nil {
			_ = entry.reg.Unregister()
		}
		wrappedClients.CompareAndDelete(arg.key, entry)
	}
}

func shardPoolName(base string, client *goredis.Client) string {
	return base + "/" + client.Options().Addr
}
