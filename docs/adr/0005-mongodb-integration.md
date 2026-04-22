# ADR 0005 — MongoDB Integration

**Status**: Accepted (implementation pending)
**Date**: 2026-04-22

---

## Context

The SDK currently has no MongoDB integration, even though `AGENTS.md` lists
MongoDB as the canonical database choice for services built on this SDK.
Services integrate their own `go.mongodb.org/mongo-driver/v2` client today,
which means:

- No standardized command spans → MongoDB calls are invisible in Tempo.
- Trace/log/metric correlation for DB operations requires ad-hoc code per
  service.
- Semconv compliance for `db.*` attributes is not enforced.

The reference implementation surveyed for this ADR is
`github.com/Marz32onE/instrumentation-go/otel-mongo/v2`. That library
provides a full wrapper over `mongo.Client` / `Database` / `Collection` /
`Cursor`, including a distinctive feature: injecting a `_oteltrace`
subdocument (`traceparent` + `tracestate`) into every written document so
that asynchronous readers (change streams, outbox pattern, delayed jobs)
can restore trace context via `ContextFromDocument`.

However, its `ConnectWithOptions` constructor calls
`otel.SetTracerProvider(cfg.TracerProvider)` and
`otel.SetTextMapPropagator(cfg.Propagators)`. Under ADR 0003, that entry
point is therefore unusable without a fork or patch.

---

## Decisions

### 1. Driver version: v2 only

Only `go.mongodb.org/mongo-driver/v2` is supported. The v1 driver is in
legacy support upstream; supporting both would double the wrapper
surface and test matrix for marginal benefit.

Services still on driver v1 must migrate before adopting this wrapper.

### 2. Instrumentation mechanism: native `event.CommandMonitor`, not upstream wrapper

We bypass `Marz32onE/instrumentation-go/otel-mongo/v2` entirely and
instrument the official driver through its built-in extension point:

```go
// mongo/monitor.go (sketch)
func newCommandMonitor(tp trace.TracerProvider, prop propagation.TextMapPropagator) *event.CommandMonitor {
    tracer := tp.Tracer("github.com/flywindy/o11y/mongo")
    // callbacks:
    //   Started    → start span, store in per-RequestID map, attach attrs
    //   Succeeded  → finish span with OK status
    //   Failed     → finish span with error status, record err
    // ...
}

opts := options.Client().ApplyURI(uri).SetMonitor(newCommandMonitor(tp, prop))
client, err := mongo.Connect(opts)
```

**Why not use the upstream library.** It pins globals in its advertised
constructor path (ADR 0003, verification below). Forking would cost
ongoing upstream-tracking work. Writing our own monitor is ~150 LOC,
keeps us in full control of attribute semantics, and maps directly to
OTel's intended extension design for MongoDB.

**What we give up by not using the upstream.**
- `_oteltrace` document injection / `ContextFromDocument`. Not
  implemented initially; see Decision 5.
- `SkipDBOperationsExporter`. Replaced by a simpler in-monitor
  operation filter (Decision 6).
- Upstream bug fixes for command-event edge cases. We take on that
  maintenance burden directly.

### 3. Package layout

```
mongo/
  conn.go           // public API: Connect / Option
  monitor.go        // event.CommandMonitor implementation
  attributes.go     // semconv v1.27.0 attribute helpers
  conn_test.go
  monitor_test.go
examples/
  mongo/main.go
```

Mirrors the shape of `nats/`.

### 4. Public API

```go
// package mongo (import as o11ymongo "github.com/flywindy/o11y/mongo")

func Connect(
    ctx context.Context,
    uri string,
    tp trace.TracerProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) (*mongo.Client, error)

type Option func(*config)

func WithSkippedOperations(ops ...string) Option
func WithMeter(m metric.Meter) Option         // reserved for future DB metrics
// WithDocumentTraceInjection deliberately omitted in v1 — see Decision 5
```

The return type is the official `*mongo.Client`. No wrapping — the
monitor handles all instrumentation; callers keep their existing
`client.Database(...).Collection(...)` usage unchanged.

Typical usage:

```go
client, err := o11ymongo.Connect(ctx, uri, sdk.TracerProvider(), sdk.Propagator)
if err != nil { ... }
defer client.Disconnect(ctx)

coll := client.Database("orders").Collection("events")
_, err = coll.InsertOne(ctx, bson.M{"id": "o-1"})
```

### 5. `_oteltrace` document injection: not implemented in v1

Default **off**, with no opt-in available in the first release. Rationale
for deferring the opt-in:

- Full semantics require cooperative write/read wrapping on every
  operation (Insert, Update, FindOneAndUpdate, BulkWrite, Aggregate
  pipeline $merge, …). That is a large surface to get right and we
  would end up re-implementing most of the upstream wrapper.
- Schema impact is non-trivial (next table) and shouldn't be hidden
  behind a single boolean.
- The use case is niche (change streams / outbox). If a service needs
  it, they can do explicit `prop.Inject` into the document from
  application code using the same `sdk.Propagator`.

**Trade-off documentation** (for reference when the opt-in is added later):

| Aspect | OFF (this release) | ON (future opt-in) |
|---|---|---|
| Document size | unchanged | +100–120 bytes per document (`_oteltrace` subdoc with `traceparent` + `tracestate`) |
| Schema | unaffected | Must update schema validation (`$jsonSchema`) to permit `_oteltrace` |
| Projections | unaffected | Must avoid explicit exclusion (`{_oteltrace: 0}`) in read queries if downstream trace restoration is desired; or explicitly include when needed |
| Indexes | unaffected | Never index `_oteltrace` — it is by design high-cardinality |
| Cross-boundary trace continuity | broken across async/stream consumers | preserved (change streams, outbox readers, delayed jobs) |
| Synchronous request-reply trace | fully preserved (via normal ctx) | fully preserved |
| Storage cost | baseline | ~100 bytes × N documents — evaluate at million-document scale |
| Migration safety | trivial | write path must roll out before read path, else readers see missing field |
| Typical fit | CRUD services, OLTP | event-sourced systems, CQRS read models, streaming pipelines |

When the opt-in is added, the expected shape is
`WithDocumentTraceInjection(bool)` gated by a matching write-path hook
inside the monitor; a new section of this ADR (or a follow-up ADR)
must be written at that time.

### 6. Default skipped operations

The command monitor skips span emission for these operation names by
default:

- `getMore` — every cursor batch emits one; noisy, low-signal.
- `killCursors` — internal cleanup.
- `ping` — driver health check.
- `hello` / `isMaster` — handshake, emitted at high frequency.

Override via `WithSkippedOperations("foo", "bar")` (replaces the default
list entirely — explicit is better than merge for skip lists).

### 7. Semconv v1.27.0 attributes

The monitor attaches the following on every emitted span (names pinned
to `go.opentelemetry.io/otel/semconv/v1.27.0`):

| Attribute | Type | Source |
|---|---|---|
| `db.system` | string | constant `"mongodb"` |
| `db.namespace` | string | `event.DatabaseName` |
| `db.collection.name` | string | first BSON arg (`event.Command` lookup; see below) |
| `db.operation.name` | string | `event.CommandName` |
| `server.address` | string | `event.ConnectionID` host portion |
| `server.port` | int | `event.ConnectionID` port portion |
| `network.peer.address` / `network.peer.port` | string / int | same as `server.*`, but mandatory under semconv `client` spans |

**Explicitly NOT emitted by default:**

- `db.query.text` / `db.statement` — query documents routinely contain
  PII and secrets. If a service needs this, add `WithCapturedQueryText(true)`
  in a future revision with an opt-in warning.
- Response document contents.

Span name convention: `{db.operation.name} {db.namespace}.{db.collection.name}`
(e.g. `find orders.events`). Falls back to `{db.operation.name}` when
collection is unknown.

Span kind: `trace.SpanKindClient`.

### 8. Future metrics (reserved)

DB-side metrics (e.g. `db.client.operation.duration`,
`db.client.connection.count`) are not part of this ADR's scope but the
`Option` surface already reserves `WithMeter(m metric.Meter)` so a
later ADR can add them without an API break.

### 9. Example and documentation

- `examples/mongo/main.go` — insert → find → aggregate happy path; run
  against a local Mongo (or the kind cluster once a MongoDB manifest is
  added).
- `README.md` gains a MongoDB section mirroring the NATS section.
- `AGENTS.md` adds a "Do NOT" item: "Do not call
  `mongo.Connect` directly for services using this SDK; route through
  `o11ymongo.Connect` so the CommandMonitor and providers are wired."

### 10. Testing

- **Unit**: mock `event.CommandMonitor` events (Started/Succeeded/Failed)
  and assert span attributes. Table-driven for operation-name routing,
  namespace parsing, skip-list behavior.
- **Integration**: build-tagged `integration` tests using
  `testcontainers-go` with `mongo:7`. Not included in default
  `go test ./...`. CI invokes with `-tags=integration` when MongoDB is
  available.

---

## Global-state verification

### Library surveyed: `github.com/Marz32onE/instrumentation-go/otel-mongo/v2`
### Result: ❌ NOT SAFE at `ConnectWithOptions` entry point

`ConnectWithOptions` calls
`otel.SetTracerProvider(cfg.TracerProvider)` and
`otel.SetTextMapPropagator(cfg.Propagators)` when those options are
supplied. This violates ADR 0003. Confirmed by source inspection of
`otel-mongo/v2/client.go`.

**Decision consequence.** We do not add this module to `go.mod`. All
MongoDB instrumentation is implemented through the official driver's
`event.CommandMonitor` extension point as specified in Decision 2.

### Library used: `go.mongodb.org/mongo-driver/v2`
### Result: ✅ SAFE

The official driver is OTel-agnostic. It does not read or write OTel
globals. All instrumentation is opt-in via `SetMonitor`.

---

## Consequences

**Positive**

- Zero globals touched; ADR 0003 upheld.
- Full control over span attributes and naming; direct OTel semconv
  v1.27.0 compliance without depending on upstream update cadence.
- Small dependency surface — no new third-party library in `go.mod` for
  MongoDB instrumentation.
- Same ctx-based correlation pattern as the rest of the SDK; no
  surprises for service developers.
- `_oteltrace` absence means Mongo documents are untouched by default —
  easier to adopt in services with existing schemas.

**Negative / Trade-offs**

- ~150 lines of command-monitor code to maintain, including handling of
  edge cases around command redaction (`saslStart`, `saslContinue`,
  `copydbgetnonce`, `getnonce`, `authenticate`) that must not land their
  arguments in spans.
- No built-in cross-boundary trace continuity through documents. Teams
  who need it either add it explicitly in application code or wait for
  a future ADR re-evaluating document injection.
- We assume upstream-driver CommandMonitor API stability; a major driver
  update may require adjustments.
- Forgoing upstream's tested `SkipDBOperationsExporter` means our skip
  logic lives at the monitor layer and must be kept in sync with any
  new noisy operation names added to MongoDB server releases.
