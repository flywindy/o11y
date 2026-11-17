package resty

import (
	"reflect"
	"runtime"
	"sync"
	"weak"

	restyclient "github.com/go-resty/resty/v2"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/flywindy/o11y/internal/providers"
)

const instrumentationName = "github.com/flywindy/o11y/resty"

var wrappedClients sync.Map

type wrapEntry struct {
	mu      sync.Mutex
	ref     weak.Pointer[restyclient.Client]
	done    bool
	cleanup runtime.Cleanup
}

type cleanupArg struct {
	key uintptr
	ref weak.Pointer[restyclient.Client]
}

// Wrap installs o11y tracing and metrics on an existing Resty client.
//
// The returned value is the same client passed in, allowing callers to chain
// setup while retaining full control of resty options such as base URL,
// timeouts, retry policy, TLS configuration, and transport.
//
// Wrap is idempotent: calling it twice on the same live client is a no-op after
// the first call. Resty v2 keeps hook chains private, so the implementation
// tracks wrapped client allocations in a weak-reference table and removes stale
// entries when clients are collected.
//
// A nil tp, mp or prop is replaced by one that does nothing: a no-op tracer
// provider (no client span), a no-op meter provider (no metrics), an empty
// propagator (no traceparent or baggage header on the outgoing request). Wrap
// has no error to return, and it never reads the OpenTelemetry globals. Pass
// obs.TracerProvider(), obs.MeterProvider() and obs.Propagator, which are
// never nil.
func Wrap(
	rc *restyclient.Client,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	prop propagation.TextMapPropagator,
	opts ...Option,
) *restyclient.Client {
	if rc == nil {
		return nil
	}
	tp, mp, prop = providers.OrNoop(tp, mp, prop)

	key := clientKey(rc)
	for {
		candidate := &wrapEntry{ref: weak.Make(rc)}
		actual, loaded := wrappedClients.LoadOrStore(key, candidate)
		entry := candidate
		if loaded {
			entry = actual.(*wrapEntry)
		}

		entry.mu.Lock()
		current, ok := wrappedClients.Load(key)
		if !ok || current != entry {
			entry.mu.Unlock()
			continue
		}
		if entry.ref.Value() != rc {
			wrappedClients.CompareAndDelete(key, entry)
			entry.mu.Unlock()
			continue
		}
		if entry.done {
			entry.mu.Unlock()
			return rc
		}

		cfg := newConfig(opts)
		meter := mp.Meter(instrumentationName, metric.WithSchemaURL(semconv.SchemaURL))
		duration, err := meter.Float64Histogram(
			"http.client.request.duration",
			metric.WithDescription("Duration of outbound HTTP client requests."),
			metric.WithUnit("s"),
		)
		if err != nil {
			duration, _ = metricnoop.NewMeterProvider().Meter(instrumentationName).Float64Histogram(
				"http.client.request.duration",
				metric.WithDescription("Duration of outbound HTTP client requests."),
				metric.WithUnit("s"),
			)
		}
		h := newHook(tp, duration, prop, cfg)
		rc.OnBeforeRequest(h.beforeRequest)
		rc.OnSuccess(h.success)
		// The retry hook is what ends an attempt span that resty decided to
		// retry. It is authoritative: resty owns the retry decision, and the
		// request-level half of the condition set is unreadable from here.
		rc.AddRetryHook(h.retry)
		rc.OnError(h.error)
		rc.OnPanic(h.panicked)

		entry.done = true
		entry.cleanup = runtime.AddCleanup(rc, cleanupWrappedClient, cleanupArg{key: key, ref: entry.ref})
		runtime.KeepAlive(rc)
		entry.mu.Unlock()
		return rc
	}
}

// NewClient returns a new Resty client with o11y tracing and metrics installed.
// A nil tp, mp or prop is handled as Wrap handles it.
func NewClient(
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	prop propagation.TextMapPropagator,
	opts ...Option,
) *restyclient.Client {
	return Wrap(restyclient.New(), tp, mp, prop, opts...)
}

func clientKey(client *restyclient.Client) uintptr {
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
	if entry.ref == arg.ref && entry.ref.Value() == nil {
		wrappedClients.CompareAndDelete(arg.key, entry)
	}
}
