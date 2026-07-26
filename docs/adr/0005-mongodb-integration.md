# ADR 0005 - MongoDB Integration

**Status**: Accepted (implemented through local wrapper)
**Date**: 2026-04-22
**Updated**: 2026-05-26
**Superseded in part by**: ADR 0021 (§2 instrumentation mechanism → contrib
`otelmongo` CommandMonitor; §4 document trace propagation withdrawn)

---

## Context

The SDK provides MongoDB integration through the local
`github.com/flywindy/o11y/mongo` wrapper package. Before this decision,
services integrated their own `go.mongodb.org/mongo-driver/v2` client, which
meant:

- MongoDB calls are not standardized as Tempo spans.
- Trace/log/metric correlation for DB operations requires ad-hoc code per
  service.
- Semconv compliance for `db.*` attributes is not enforced.

ADR 0014 extends this wrapper with MongoDB operation-duration metrics while
keeping this ADR's tracing and `_oteltrace` document-propagation decisions
intact.

The reference implementation surveyed for this ADR is
`github.com/Marz32onE/instrumentation-go/otel-mongo/v2`. Earlier revisions
were rejected because they mutated OTel globals, could not disable `_oteltrace`
document injection independently, and emitted post-v1.30 DB stable attributes
while this SDK was pinned to semconv v1.27.0.

Those blockers have changed:

- v0.2.10 removed calls to `otel.SetTracerProvider` and
  `otel.SetTextMapPropagator`; the library now reads globals only as fallback
  when explicit options are omitted.
- v0.2.11 supports `WithTracePropagationEnabled(bool)` and
  `OTEL_MONGO_PROPAGATION_ENABLED`, so `_oteltrace` document injection can be
  disabled independently of command spans.
- This SDK has moved its semconv pin to v1.39.0, aligning with the DB stable
  names used by upstream (`db.system.name`, `db.collection.name`,
  `db.operation.name`, ...). Source inspection of upstream `client.go` confirms
  it imports the same `go.opentelemetry.io/otel/semconv/v1.39.0` package, and
  `semconv.go` uses string constants that match the v1.39 catalog (no legacy
  `db.system`).

---

## Decision

### 1. Driver version: v2 only

Only `go.mongodb.org/mongo-driver/v2` is supported. Services still on driver
v1 must migrate before adopting the wrapper.

### 2. Instrumentation mechanism: adopt upstream wrapper, behind o11y API

> **Superseded by ADR 0021.** The Marz wrapper described below was **dropped**.
> MongoDB instrumentation now uses the official contrib `otelmongo`
> `event.CommandMonitor` for both spans and `db.client.operation.duration`, and
> `mongo.Connect` returns a plain driver `*mongo.Client`. The rest of this
> section is retained for historical context only; do not implement against it.

The SDK should adopt `github.com/Marz32onE/instrumentation-go/otel-mongo/v2`
v0.2.11 through a local `mongo/` wrapper package, instead of writing and
maintaining a native `event.CommandMonitor`.

Because the upstream repository tags the module as `otel-mongo/v2/v0.2.11`
instead of a Go module semver tag, `go.mod` pins the corresponding commit with
the pseudo-version `v2.0.0-20260501090829-1aa6610b53de`.

Future upstream version changes require a separate ADR/PR decision with a fresh
global-state, semconv, document-propagation, and synthetic-delivery-tracer audit.

The local wrapper remains important:

- It preserves the short import path `github.com/flywindy/o11y/mongo`.
- It wires `sdk.TracerProvider()` and `sdk.Propagator` explicitly, so global
  OTel fallback paths are not used.
- It enforces o11y defaults, especially `_oteltrace` document propagation off
  unless an application explicitly opts in.
- It gives the SDK a stable boundary if upstream API details change.

### 3. Public API shape

```go
// package mongo (import as o11ymongo "github.com/flywindy/o11y/mongo")

func Connect(
    ctx context.Context,
    uri string,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) (*Client, error)

type Option func(*config)

func WithDocumentTracePropagation(enabled bool) Option
```

The default must be equivalent to upstream `WithTracePropagationEnabled(false)`.

### 4. Document trace propagation remains opt-in

> **Withdrawn by ADR 0021.** `_oteltrace` document injection, the
> `WithDocumentTracePropagation` option, and the synthetic delivery tracer are
> **removed** from this package. Asynchronous trace context is directed to an
> outbox / message-envelope approach instead. This section is historical.

`_oteltrace` document injection is useful for change streams, outbox patterns,
delayed jobs, and other asynchronous readers that need to restore trace
context from MongoDB documents. It is not safe as a hidden default because it:

- Changes persisted document shape.
- May require schema validation changes.
- Adds high-cardinality trace context to every written document.
- Increases storage size.

Services that opt in must update schema validation and explicitly accept those
storage and lifecycle trade-offs.

### 5. Semconv v1.39.0 attributes

The expected MongoDB span attributes are documented in `docs/semconv.md`.
The key migration from the old plan is:

| Old v1.27 key | v1.39 key |
|---|---|
| `db.system` | `db.system.name` |
| `db.operation.name` | unchanged |
| `db.collection.name` | unchanged |
| `db.namespace` | unchanged |

`otel-mongo/v2` still defines several DB keys as string constants, but the
strings match the SDK's v1.39 catalog. The wrapper implementation must include
compatibility tests that assert emitted spans use the documented keys.

---

## Global-State Verification

### Library surveyed: `github.com/Marz32onE/instrumentation-go/otel-mongo/v2`

### Version: `v0.2.11`

### Result: SAFE

Source inspection of `client.go` shows `ConnectWithOptions` uses explicit
`WithTracerProvider` / `WithPropagators` options when provided, and only falls
back to `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()` when they
are omitted. It does not call `otel.SetTracerProvider` or
`otel.SetTextMapPropagator`.

The o11y wrapper must always pass both options.

---

## Upstream Env-Gate Semantics (v0.2.11)

> **No longer applies (ADR 0021).** These env gates belonged to the Marz
> wrapper, which has been dropped. The contrib `otelmongo` monitor reads **no**
> `OTEL_*_ENABLED` env vars; command spans are always-on and governed solely by
> the sampler (ADR 0015 / ADR 0021 §7). Do not set
> `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` / `OTEL_MONGO_TRACING_ENABLED` for
> this package. Retained for historical context.

Source inspection of `client.go` and `env_flags.go` at v0.2.11 shows three env
flags that sit in front of every code path the o11y wrapper relies on. These
were not present when the original ADR was written and change how the wrapper
must be configured.

| Env var | Default when unset | Effect |
|---|---|---|
| `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` | disabled | Master gate. When off, `ConnectWithOptions` swaps the supplied `TracerProvider` for a `noop.NewTracerProvider()`, so no command spans are emitted regardless of `WithTracerProvider`. Also forces document propagation off. |
| `OTEL_MONGO_TRACING_ENABLED` | disabled | Module gate. Both this and the master gate must be truthy for `mongoTracingEnabled()` to return true. |
| `OTEL_MONGO_PROPAGATION_ENABLED` | disabled | Default for `_oteltrace` document propagation. `WithTracePropagationEnabled` overrides this, but the master gate still has to be on. |

Implications for the wrapper:

1. **Command spans require both env vars.** A service that calls
   `o11ymongo.Connect(...)` without setting
   `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true` and
   `OTEL_MONGO_TRACING_ENABLED=true` will silently get a no-op tracer. The
   wrapper must either:
   - document those env vars as deployment requirements, or
   - set them programmatically before calling upstream `ConnectWithOptions`.
     The wrapper should only do this for the env vars it knows about and must
     not mutate them if already set by the operator. Note that environment
     variables are process-global and cannot be scoped to a specific call site;
     `os.Setenv` from the wrapper affects the entire process, so deployment-
     requirement documentation is the preferred option.
2. **Document propagation option is gated by the master env.**
   `WithDocumentTracePropagation(true)` cannot enable injection while
   `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is unset or false. This is a
   useful safety property (default-off is enforced from two sides) but the
   wrapper docs must call it out so opt-in services do not silently fail.
3. **Test setup must set both env vars** for the unit tests in the Testing
   section to observe spans at all. Without them, "Connect passes the supplied
   tracer provider" would technically pass but emit nothing.

---

## Synthetic Delivery Tracer Policy

> **No longer applies (ADR 0021).** The synthetic delivery tracer was a Marz
> `otel-mongo/v2` behavior; that library has been dropped, so this concern is
> moot. The contrib `otelmongo` monitor creates no independent provider.
> Retained for historical context.

`otel-mongo/v2` can create an independent `TracerProvider` for synthetic
delivery spans when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The o11y wrapper
must disable this path by default so a MongoDB integration cannot silently
create an extra provider outside the SDK-owned provider graph.

In v0.2.11 the upstream call site is now further guarded:

```go
if mongoTracingEnabled() {
    mongoTP, deliverTracer = initMongoProvider(addr, port)
}
```

So three conditions must all be met before the synthetic provider spins up:
`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true`, `OTEL_MONGO_TRACING_ENABLED=true`,
and `OTEL_EXPORTER_OTLP_ENDPOINT` set. This is stricter than the policy
originally feared, and the synthetic provider will not appear under the o11y
default deployment posture.

However, upstream **does not expose a per-client option to disable
`initMongoProvider`** — there is no `WithSyntheticDeliveryTracer(false)` on
the upstream side. The wrapper therefore cannot offer the explicit opt-in
originally proposed; it can only:

- rely on the env-gate default (off) and document the three-env activation,
- and, for services that legitimately need MongoDB tracing but must not get
  the synthetic provider, document that `OTEL_EXPORTER_OTLP_ENDPOINT` must be
  unset in that process (the o11y exporters use their own configuration paths
  and do not require this env var to be set on the consumer process).

This follows ADR 0003's zero global state principle: providers are encapsulated
in structs and lifecycle ownership remains explicit. Environment variables
alone must not enable a hidden second provider through o11y; v0.2.11's three-
env requirement satisfies that bar.

---

## Testing

- Unit tests for the local wrapper:
  - `Connect` rejects canceled contexts before dialing.
  - `Connect` passes the supplied tracer provider and propagator.
  - document trace propagation defaults off.
  - synthetic delivery tracing defaults off even when
    `OTEL_EXPORTER_OTLP_ENDPOINT` is set (relies on the v0.2.11 env-gate; the
    test must assert the gate semantics, not just the wrapper's own knobs).
  - `WithDocumentTracePropagation(true)` maps to upstream
    `WithTracePropagationEnabled(true)`, **and** is a no-op when the master
    env gate is unset (regression guard for the v0.2.11 behavior).
  - tests that need to observe emitted spans must set
    `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true` and
    `OTEL_MONGO_TRACING_ENABLED=true` in `t.Setenv`; tests that assert "no
    spans emitted in default posture" must leave them unset. Because
    `t.Setenv` mutates process-global state, env-gated tests must not call
    `t.Parallel()` and must not share a process with other parallel tests
    that read the same vars; serialize them within the package or isolate
    them in a dedicated build-tag/test binary to avoid flaky span/no-span
    assertions.
- Compatibility tests for emitted MongoDB attributes:
  - `db.system.name="mongodb"`.
  - `db.collection.name`, `db.namespace`, and `db.operation.name` are present
    for representative operations.
  - no legacy `db.system` key is emitted by SDK-owned Mongo integration tests.
- Integration tests may use `testcontainers-go` with MongoDB when CI provides
  Docker. They should remain build-tagged and out of default `go test ./...`.

---

## Consequences

> **Historical (superseded by ADR 0021).** The consequences below described the
> Marz-wrapper design and no longer hold: the SDK no longer depends on the Marz
> wrapper API, `_oteltrace` propagation was withdrawn, and the synthetic
> delivery tracer / env-gate trade-offs are gone. For the current design's
> consequences — single maintained contrib `otelmongo` dependency, plain
> `*mongo.Client`, always-on sampler-governed spans — see ADR 0021.

**Positive**

- Less SDK-owned instrumentation code to maintain.
- Alignment with the upstream NATS integration style.
- DB stable semconv names match the SDK's v1.39 pin.
- `_oteltrace` propagation is available for services that explicitly need it.

**Negative / Trade-offs**

- We depend on upstream wrapper API stability.
- Upstream's synthetic delivery tracer creates an independent `TracerProvider`
  when `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, `OTEL_MONGO_TRACING_ENABLED`,
  and `OTEL_EXPORTER_OTLP_ENDPOINT` are all set. Upstream does not expose a
  per-client option to disable it, so the wrapper cannot ship a
  `WithSyntheticDeliveryTracer(false)` opt-out. The wrapper relies on the
  three-env default-off posture and documents the activation path; services
  that need MongoDB tracing without the synthetic provider must keep
  `OTEL_EXPORTER_OTLP_ENDPOINT` unset in that process.
- Compatibility tests are required because some DB attributes are still emitted
  through upstream string constants rather than typed semconv helpers.
