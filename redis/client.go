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

// Wrap installs o11y Redis tracing and metrics on an existing go-redis client.
//
// The returned value is the same client passed in, allowing callers to chain
// setup while retaining full control of go-redis options such as addresses,
// TLS, authentication, pool sizes, and timeouts.
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

	key, ref, err := clientIdentity(rdb)
	if err != nil {
		return rdb, err
	}

	entry := &wrapEntry{ref: ref}
	for {
		actual, loaded := wrappedClients.LoadOrStore(key, entry)
		if !loaded {
			break
		}
		existing := actual.(*wrapEntry)
		existing.mu.Lock()
		switch {
		case existing.ref.matches(rdb) && existing.done:
			existing.mu.Unlock()
			return rdb, nil
		case existing.ref.matches(rdb):
			existing.mu.Unlock()
			return rdb, errors.New("redis wrap: concurrent wrap in progress")
		case !existing.ref.alive() || !existing.ref.matches(rdb):
			existing.mu.Unlock()
			wrappedClients.CompareAndDelete(key, existing)
			continue
		default:
			existing.mu.Unlock()
			return rdb, errors.New("redis wrap: stale client identity collision")
		}
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	cfg := newConfig(opts)
	if cfg.poolName == "" {
		cfg.poolName = fmt.Sprintf("redis-%x", key)
	}

	meter := mp.Meter(instrumentationName, metric.WithSchemaURL(semconv.SchemaURL))
	operationDuration, err := meter.Float64Histogram(
		"db.client.operation.duration",
		metric.WithDescription("Duration of database client operations."),
		metric.WithUnit("s"),
	)
	if err != nil {
		wrappedClients.CompareAndDelete(key, entry)
		return rdb, fmt.Errorf("redis wrap: create operation duration histogram: %w", err)
	}
	pool, err := newPoolMetrics(meter)
	if err != nil {
		wrappedClients.CompareAndDelete(key, entry)
		return rdb, err
	}

	reg, err := pool.register(rdb, cfg.poolName, &entry.disabled)
	if err != nil {
		wrappedClients.CompareAndDelete(key, entry)
		return rdb, err
	}
	entry.reg = reg

	if err := installHooks(context.Background(), rdb, tp, operationDuration, pool.createTime, cfg, &entry.disabled); err != nil {
		entry.done = true
		entry.cleanup = addCleanup(rdb, cleanupArg{key: key, ref: ref})
		return rdb, fmt.Errorf("redis wrap: install hooks: %w", err)
	}

	entry.done = true
	entry.cleanup = addCleanup(rdb, cleanupArg{key: key, ref: ref})
	runtime.KeepAlive(rdb)
	return rdb, nil
}

// Unwrap disables instrumentation previously installed by Wrap.
//
// go-redis does not expose a hook removal API, so Unwrap gates previously
// installed hook closures and unregisters this package's metric callback. A
// later Wrap call can safely install a fresh wrapper.
func Unwrap(rdb goredis.UniversalClient) {
	if rdb == nil {
		return
	}
	key, ref, err := clientIdentity(rdb)
	if err != nil {
		return
	}
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
	entry.cleanup.Stop()
	wrappedClients.CompareAndDelete(key, entry)
	runtime.KeepAlive(ref)
}

func installHooks(
	ctx context.Context,
	rdb goredis.UniversalClient,
	tp trace.TracerProvider,
	operationDuration metric.Float64Histogram,
	connectionCreate metric.Float64Histogram,
	cfg config,
	disabled *atomic.Bool,
) error {
	switch client := rdb.(type) {
	case *goredis.Client:
		addClientHook(client, tp, operationDuration, connectionCreate, cfg, cfg.poolName, disabled)
		return nil
	case *goredis.ClusterClient:
		var seenPtr atomic.Pointer[sync.Map]
		seenPtr.Store(&sync.Map{})
		client.OnNewNode(func(node *goredis.Client) {
			if node == nil || disabled.Load() {
				return
			}
			if seen := seenPtr.Load(); seen != nil {
				if _, loaded := seen.LoadOrStore(clientKey(node), struct{}{}); loaded {
					return
				}
			}
			poolName := shardPoolName(cfg.poolName, node)
			addClientHook(node, tp, operationDuration, connectionCreate, cfg, poolName, disabled)
		})
		err := client.ForEachShard(ctx, func(_ context.Context, node *goredis.Client) error {
			seen := seenPtr.Load()
			if seen == nil {
				return nil
			}
			if _, loaded := seen.LoadOrStore(clientKey(node), struct{}{}); loaded {
				return nil
			}
			poolName := shardPoolName(cfg.poolName, node)
			addClientHook(node, tp, operationDuration, connectionCreate, cfg, poolName, disabled)
			return nil
		})
		seenPtr.Store(nil)
		return err
	case *goredis.Ring:
		var seenPtr atomic.Pointer[sync.Map]
		seenPtr.Store(&sync.Map{})
		client.OnNewNode(func(node *goredis.Client) {
			if node == nil || disabled.Load() {
				return
			}
			if seen := seenPtr.Load(); seen != nil {
				if _, loaded := seen.LoadOrStore(clientKey(node), struct{}{}); loaded {
					return
				}
			}
			poolName := shardPoolName(cfg.poolName, node)
			addClientHook(node, tp, operationDuration, connectionCreate, cfg, poolName, disabled)
		})
		err := client.ForEachShard(ctx, func(_ context.Context, node *goredis.Client) error {
			seen := seenPtr.Load()
			if seen == nil {
				return nil
			}
			if _, loaded := seen.LoadOrStore(clientKey(node), struct{}{}); loaded {
				return nil
			}
			poolName := shardPoolName(cfg.poolName, node)
			addClientHook(node, tp, operationDuration, connectionCreate, cfg, poolName, disabled)
			return nil
		})
		seenPtr.Store(nil)
		return err
	default:
		return fmt.Errorf("unsupported client type %T", rdb)
	}
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

func clientIdentity(rdb goredis.UniversalClient) (uintptr, clientRef, error) {
	switch client := rdb.(type) {
	case *goredis.Client:
		if client == nil {
			return 0, nil, errors.New("redis wrap: client must not be nil")
		}
		return clientKey(client), weakClientRef[goredis.Client]{ptr: weak.Make(client)}, nil
	case *goredis.ClusterClient:
		if client == nil {
			return 0, nil, errors.New("redis wrap: client must not be nil")
		}
		return clientKey(client), weakClientRef[goredis.ClusterClient]{ptr: weak.Make(client)}, nil
	case *goredis.Ring:
		if client == nil {
			return 0, nil, errors.New("redis wrap: client must not be nil")
		}
		return clientKey(client), weakClientRef[goredis.Ring]{ptr: weak.Make(client)}, nil
	default:
		return 0, nil, fmt.Errorf("redis wrap: unsupported client type %T", rdb)
	}
}

func clientKey(client any) uintptr {
	return reflect.ValueOf(client).Pointer()
}

func addCleanup(rdb goredis.UniversalClient, arg cleanupArg) runtime.Cleanup {
	switch client := rdb.(type) {
	case *goredis.Client:
		return runtime.AddCleanup(client, cleanupWrappedClient, arg)
	case *goredis.ClusterClient:
		return runtime.AddCleanup(client, cleanupWrappedClient, arg)
	case *goredis.Ring:
		return runtime.AddCleanup(client, cleanupWrappedClient, arg)
	default:
		return runtime.Cleanup{}
	}
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
