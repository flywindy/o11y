# ADR 0005 - MongoDB Integration

**Status**: Accepted (implementation pending; upstream adoption candidate)
**Date**: 2026-04-22
**Updated**: 2026-05-02

---

## Context

The SDK currently has no MongoDB integration, even though `AGENTS.md` lists
MongoDB as the canonical database choice for services built on this SDK.
Services integrate their own `go.mongodb.org/mongo-driver/v2` client today,
which means:

- MongoDB calls are not standardized as Tempo spans.
- Trace/log/metric correlation for DB operations requires ad-hoc code per
  service.
- Semconv compliance for `db.*` attributes is not enforced.

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
- This SDK is moving its semconv pin to v1.39.0, aligning with the DB stable
  names used by upstream (`db.system.name`, `db.collection.name`,
  `db.operation.name`, ...).

---

## Decision

### 1. Driver version: v2 only

Only `go.mongodb.org/mongo-driver/v2` is supported. Services still on driver
v1 must migrate before adopting the wrapper.

### 2. Instrumentation mechanism: adopt upstream wrapper, behind o11y API

The SDK should adopt `github.com/Marz32onE/instrumentation-go/otel-mongo/v2`
v0.2.11 or newer through a local `mongo/` wrapper package, instead of writing
and maintaining a native `event.CommandMonitor`.

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
    prop propagation.TextMapPropagator,
    opts ...Option,
) (*otelmongo.Client, error)

type Option func(*config)

func WithDocumentTracePropagation(enabled bool) Option
```

The default must be equivalent to upstream `WithTracePropagationEnabled(false)`.

### 4. Document trace propagation remains opt-in

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

## Testing

- Unit tests for the local wrapper:
  - `Connect` rejects canceled contexts before dialing.
  - `Connect` passes the supplied tracer provider and propagator.
  - document trace propagation defaults off.
  - `WithDocumentTracePropagation(true)` maps to upstream
    `WithTracePropagationEnabled(true)`.
- Compatibility tests for emitted MongoDB attributes:
  - `db.system.name="mongodb"`.
  - `db.collection.name`, `db.namespace`, and `db.operation.name` are present
    for representative operations.
  - no legacy `db.system` key is emitted by SDK-owned Mongo integration tests.
- Integration tests may use `testcontainers-go` with MongoDB when CI provides
  Docker. They should remain build-tagged and out of default `go test ./...`.

---

## Consequences

**Positive**

- Less SDK-owned instrumentation code to maintain.
- Alignment with the upstream NATS integration style.
- DB stable semconv names match the SDK's v1.39 pin.
- `_oteltrace` propagation is available for services that explicitly need it.

**Negative / Trade-offs**

- We depend on upstream wrapper API stability.
- Upstream's optional synthetic deliver tracer creates an independent
  `TracerProvider` when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The local wrapper
  and docs must decide whether to expose or disable that behavior.
- Compatibility tests are required because some DB attributes are still emitted
  through upstream string constants rather than typed semconv helpers.
