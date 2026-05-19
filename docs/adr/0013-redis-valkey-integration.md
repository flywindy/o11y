# ADR 0013 — Redis & Valkey Integration

**Status**: Proposed
**Date**: 2026-05-18

**Applies** ADR 0008 (sourcing policy); **inherits** the metric.View
cardinality controls established by ADR 0009 (any histogram named
`db.client.operation.duration` falls under the SDK-managed allowlist
once this package renames the upstream instrument — see §7).

---

## Context

Two services slated for o11y adoption use Redis/Valkey as their
primary cache. One runs a self-hosted Redis cluster; the other runs
Valkey. **Both services use `github.com/redis/go-redis/v9` as the Go
client.** This is representative: Valkey is a wire-compatible fork of
Redis 7.2.4 (RESP2/RESP3 and the command set are unchanged), and most
Go codebases reach Valkey through the same `go-redis/v9` client they
already use for Redis. A dedicated `valkey-go` client exists for
callers that need RESP3 client-side caching or auto-pipelining; none
of our adopters fit that profile today.

This ADR therefore covers Redis-protocol servers (Redis and Valkey)
through a single client library, `go-redis/v9`. A `valkey-go` facade
is **not in scope**; it is deferred until a concrete consumer needs
RESP3-first features (see §11).

ADR 0008 §5 originally listed Valkey as a separate facade. That row
is amended by this ADR: a separate `valkey-go` package will only be
added when demand exists.

### ADR 0008 §2 evaluation: `github.com/redis/go-redis/extra/redisotel/v9`

Surveyed version: **v9.9.0**, the redisotel module shipped in the
same repo as `go-redis` itself.

| # | Item | Result | Evidence |
|---|---|---|---|
| 1 | ADR 0003 compliance (no global mutation) | ✅ | `extra/redisotel/config.go:57–58` reads `otel.GetTracerProvider()` / `otel.GetMeterProvider()` as fallback only; no `Set*` call anywhere in the package. |
| 2 | Maintenance signal | ✅ | Shipped in the `go-redis` repo's main release cadence; v9.9.0 tagged 2025-05-27. |
| 3 | Semconv alignment with SDK v1.39 pin | ❌ | Imports `semconv/v1.24.0` (15 minor versions behind). Emits legacy `db.system`, `db.statement`, `db.connection_string` instead of stable `db.system.name`, `db.query.text`, etc. |
| 4 | Configurability of names/attributes | ❌ | `WithTracerProvider`, `WithMeterProvider`, `WithDBStatement(bool)`, `WithAttributes(...)` exist, but there is no option to suppress `db.connection_string` or the `code.*` attributes, and OTel Go's `trace.Span` has no `DeleteAttribute`. A downstream hook cannot remove attributes redisotel set on span start. |
| 5 | Framework signal access | ⚠️ | Tracing, dial, pool stats, pipeline, and cluster (via `OnNewNode`) are covered. Pub/Sub is **not instrumented**: subscription receive (`*redis.PubSub.Receive*`) bypasses `ProcessHook` naturally, and the publish-side commands (`PUBLISH` / `SPUBLISH` etc.) which **do** travel through `ProcessHook` are explicitly short-circuited by command-name filter. Acceptable; see §11. |

Item 3 fails strictly. Item 4 also fails on closer inspection: the
OTel Go `trace.Span` API has **no DeleteAttribute** operation, and
redisotel attaches `db.system`, `db.connection_string`, and the
`code.*` attributes inside its own `BeforeProcess`, which runs
before any hook we add downstream. A "post-hook attribute rewrite"
can add or overwrite values but cannot remove the legacy keys, so
the semconv-alignment and PII-protection guarantees this ADR needs
are **not achievable** through a pure-T2 facade over
`redisotel.InstrumentTracing`.

This ADR therefore adopts a **hybrid**: T3 for tracing, T2 for
metrics.

- **Tracing.** We write our own `redis.Hook` (~120–150 LOC, the
  size of the existing `mongo/` wrapper) that emits semconv v1.39
  attributes directly. `redisotel.InstrumentTracing` is **not**
  called. We own the span lifecycle and the attribute set, so
  `db.connection_string` never appears, the `code.*` attributes
  never appear, and `db.query.text` is off unless explicitly
  enabled.
- **Metrics.** `redisotel.InstrumentMetrics` is kept. Its
  connection-pool instruments (`db.client.connections.idle.max` /
  `.usage` / `.timeouts` / etc.) carry stable `db.client.*` names
  that pass through unchanged from v1.24 to v1.39, so there is
  nothing to rewrite. We register one metric.View at `o11y.Init`
  time to drop the upstream `db.client.connections.use_time`
  duration histogram (we replace it with our own seconds-unit
  `db.client.operation.duration` recorded from the tracing hook).

The T3-for-tracing scope is bounded by the `redis.Hook` interface
(four methods) and the closed set of attributes in §5. ADR 0011
established the same precedent for resty: §2 fails on one item, T3
is permitted when the rewrite is bounded and the alternative (forced
emission of wrong-version semconv) is unacceptable. This ADR follows
the same logic.

Relevant files / context:

- ADR 0003 — global state policy
- ADR 0006 — semconv upgrade strategy (this SDK is pinned at v1.39.0)
- ADR 0008 — instrumentation sourcing policy (T2 default)
- ADR 0009 — metric.View cardinality controls (allowlist by instrument name)
- ADR 0011 — resty integration (error-classification pattern referenced in §9)

---

## Decisions

### 1. Client library: `go-redis/v9` only

Only `github.com/redis/go-redis/v9` is supported. Services on the v8
line must migrate before adopting the wrapper. `valkey-go` is **not**
covered by this ADR.

Both Redis-server and Valkey-server backends are supported through
this single client. Span and metric attributes always emit
`db.system.name="redis"` (see §5); the actual backend identity is
recorded in `server.address`. OTel semconv v1.39 does not list a
`"valkey"` value for `db.system.name`, and a future amendment can
revisit the choice if upstream semconv adds one.

### 2. Sourcing tier: hybrid (T3 tracing, T2 metrics)

`Wrap` does two distinct things to the user's client:

1. **Attach an SDK-owned `redis.Hook`** that emits spans and the
   operation-duration histogram directly. Implements
   `redis.Hook`'s `DialHook`, `ProcessHook`, and
   `ProcessPipelineHook`. Owns span start/end, attribute set,
   error recording, and one `db.client.operation.duration`
   histogram in seconds. Pure SDK code; no redisotel dependency on
   this code path.
2. **Call `redisotel.InstrumentMetrics(rdb, redisotel.WithMeterProvider(mp))`**
   for connection-pool observability. Pool stats are valuable and
   their stable `db.client.connections.*` names need no rewrite.
   The one upstream instrument we do not want
   (`db.client.connections.use_time`, the upstream duration
   histogram) is dropped by a metric.View registered at
   `o11y.Init` time (see §7).

`redisotel.InstrumentTracing` is **not** called.

### 3. Public API shape

```go
// package redis (import as o11yredis "github.com/flywindy/o11y/redis")

// Wrap installs tracing + metrics on an existing client. The caller
// retains control over addrs, pool size, TLS, auth, and any other
// go-redis configuration. Returns the same client for chaining.
//
// Wrap accepts any redis.UniversalClient, so single-node,
// ClusterClient, FailoverClient (Sentinel), and Ring all work.
//
// Wrap is idempotent: calling it twice on the same client is a no-op
// after the first call.
//
// Wrap returns a non-nil error when rdb is nil, when the underlying
// concrete type is not supported by redisotel.InstrumentMetrics
// (single / cluster / ring / sentinel-failover are all supported in
// v9.9.0; *redis.SentinelClient — the raw sentinel-monitor client —
// is not), when instrument creation against the supplied
// MeterProvider fails, or when the initial ForEachShard iteration
// over an already-warmed *redis.ClusterClient / *redis.Ring fails
// (see §10 for why the iteration is mandatory). On error the
// original client is returned unmodified so callers can choose to
// proceed without instrumentation; they MUST NOT ignore the error
// silently — log it and surface it to startup readiness, otherwise
// missing Redis spans/metrics become invisible at runtime.
func Wrap(
    rdb redis.UniversalClient,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    opts ...Option,
) (redis.UniversalClient, error)

type Option func(*config)

// WithCommandTextEnabled records the full Redis command (including
// arguments) as db.query.text on each span. OFF by default; commands
// often contain key names and values that are sensitive.
func WithCommandTextEnabled(enabled bool) Option

// WithAttributes appends static attributes (e.g. service-level
// labels) to every emitted span and metric sample.
func WithAttributes(attrs ...attribute.KeyValue) Option

// MetricViews returns the metric.Views this package needs registered
// at MeterProvider construction time (specifically, the drop view
// for the upstream db.client.connections.use_time histogram). o11y.Init
// composes these into its default view set; consumers building their
// own MeterProvider must include them via sdkmetric.WithView.
func MetricViews() []sdkmetric.View
```

The wrapper does **not** accept a `propagation.TextMapPropagator`.
Redis is not a propagation-bearing transport in this SDK's scope:
Redis Streams / Pub/Sub message-header propagation is a separate
problem (out of scope, §11) and the synchronous client API has no
carrier to inject into.

### 4. Servers supported

| Backend | Minimum version | Notes |
|---|---|---|
| Redis | 6.0 (RESP2) / 6.2 (RESP3 optional) | Cluster, Sentinel, standalone all supported. |
| Valkey | 7.2.4 (the fork point) | Same wire protocol as Redis; the wrapper does not detect or branch on backend. |

The wrapper does not probe `INFO server` to identify the backend.
Caller deployment configuration is the source of truth for
`server.address`; backend type is inferable from the address /
hostname convention rather than from a span attribute.

### 5. Span attributes emitted (semconv v1.39, stable)

Because the wrapper owns the tracing hook (§2), the attribute set is
defined here directly rather than as a translation from upstream.
All keys come from `go.opentelemetry.io/otel/semconv/v1.39.0`.

| Attribute | When set | Value |
|---|---|---|
| `db.system.name` | every span | `"redis"` (single string; see §1 on Valkey backends) |
| `db.operation.name` | every span | uppercased command name (e.g. `"GET"`, `"HSET"`), or `"pipeline"` for batches |
| `db.namespace` | every span | the selected DB number as a string, when known |
| `server.address` | every span | host, from `redis.Options.Addr` / cluster node |
| `server.port` | every span | port, from `redis.Options.Addr` / cluster node |
| `db.query.text` | only if `WithCommandTextEnabled(true)` | the command and arguments, truncated at 1 KiB |
| `db.operation.batch.size` | pipeline spans only | count of commands in the batch |
| `error.type` | error paths | Go type name from `reflect.TypeOf(err).String()` (see §9 for the `redis.Nil` exception) |
| `redis.error.kind` | selected error paths only | one of the closed-enum values in §9 |

Keys **not** emitted (intentionally diverging from what
`redisotel.InstrumentTracing` would have done):

- `db.connection_string` — may carry `redis://user:password@host:port/db`.
  Never set by the wrapper; credential material does not enter the
  trace pipeline. `server.address` + `server.port` cover the
  identification need.
- `code.function`, `code.filepath`, `code.lineno` — describe the
  redisotel hook's call site, not the caller's; misleading without
  upstream changes.
- `db.system` (legacy v1.24 key) — replaced by `db.system.name`.
- `db.statement` (legacy v1.24 key) — replaced by `db.query.text`
  and gated by §6.

Span name: `redis.<METHOD>` for single commands (e.g.
`redis.GET`), `redis.pipeline` for batches. Low cardinality; the
command name is the operation, not the argument.

### 6. Command text (`db.query.text`) is opt-in, default off

The wrapper does **not** emit `db.query.text` by default. Redis
command arguments routinely carry key names with embedded
identifiers, cached values, session tokens, hashed credentials,
etc. — the same data-protection reasoning as ADR 0005 §4 on
`_oteltrace` document propagation.

`WithCommandTextEnabled(true)` turns it on. When enabled, the
wrapper formats the command and arguments and truncates at 1 KiB
to bound span size. The truncation is consistent across single
commands and pipeline batches.

Because the wrapper owns the tracing hook (§2), there is no
`redisotel.WithDBStatement(false)` to pass through — the legacy
`db.statement` key is simply never emitted.

### 7. Metrics

The wrapper produces three groups of metrics:

**A. Operation duration histogram — SDK-owned, recorded in seconds.**

The SDK's tracing hook records each command's wall-clock duration
on a single histogram instrument:

| Name | Type | Unit | Labels |
|---|---|---|---|
| `db.client.operation.duration` | Float64Histogram | `s` | `db.system.name`, `db.operation.name`, `server.address`, `server.port`, `error.type` (on errors only) |

This is the stable v1.39 instrument name with the canonical unit.
Because we record it ourselves rather than rewriting an upstream
histogram, there is no unit drift; sample values are seconds end to
end. The Gemini reviewer suggestion to "record directly within the
SDK's hook in seconds" is what this ADR adopts.

The label set above is the **allowlist** for this instrument.
`db.query.text` and `redis.error.kind` are span-only and never
labels on the histogram (span attributes can be high cardinality;
metric labels cannot). `server.port` is included so services
running multiple Redis processes on the same host (sidecars, dev
environments) can distinguish them, per Gemini's allowlist note.

**B. Connection-pool metrics — passed through from `redisotel.InstrumentMetrics`.**

These instruments already use stable `db.client.connections.*`
names that pass through unchanged from v1.24 to v1.39:

- `db.client.connections.idle.max`
- `db.client.connections.idle.min`
- `db.client.connections.max`
- `db.client.connections.usage` (with `state` attribute)
- `db.client.connections.timeouts`
- `db.client.connections.hits`
- `db.client.connections.misses`
- `db.client.connections.create_time` (histogram, unit `ms`)

`Wrap` calls
`redisotel.InstrumentMetrics(rdb, redisotel.WithMeterProvider(mp))`
to register these. The wrapper does not duplicate them.

**C. Drop upstream `db.client.connections.use_time` via view.**

`redisotel.InstrumentMetrics` also registers a duration histogram
named `db.client.connections.use_time` (unit `ms`), which group A
replaces. This view drops it:

```go
sdkmetric.NewView(
    sdkmetric.Instrument{Name: "db.client.connections.use_time"},
    sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
)
```

(This matches the OTel Go `sdk/metric` v1.43.0 API the repo already
uses in `internal/metrics.defaultViews`; the old
`view.New` / `aggregation.Drop` types were removed before stabilization.)

**View registration site.** The view in group C must be registered
at `sdkmetric.NewMeterProvider` construction time — OTel Go fixes
views when the MeterProvider is built, so `Wrap` cannot add views
after the fact (Codex reviewer note). This ADR therefore requires
that `o11y.Init` include the drop view in its default view set
(adjacent to the ADR 0009 §2 allowlist views). The redis package
exports the view constructor so `o11y.Init` can compose it without
the redis package needing to know about `o11y.Init`.

The operation-duration histogram in group A falls under the
metric.View allowlist registered at `o11y.Init` per ADR 0009 §2;
the label set above bounds its cardinality.

### 8. Pipeline / batch span model

The wrapper's `ProcessPipelineHook` emits one flat span per
`Pipeline()` / `TxPipeline()` batch:

- Span name: `redis.pipeline`. Low cardinality; the command list is
  not appended to the name.
- Attributes: `db.operation.name="pipeline"`,
  `db.operation.batch.size=<n>`, `db.system.name="redis"`,
  `server.address`, `server.port`.
- One metric sample per batch on `db.client.operation.duration`
  with `db.operation.name="pipeline"`.

Per-command sub-spans inside a pipeline are out of scope. The
`redis.Hook` contract from go-redis surfaces only the batch
boundary; reconstructing per-command timing would require
instrumenting at the protocol layer, which is below the hook
contract.

**Pub/Sub filter inside pipelines.** `pipe.Publish(...)` /
`pipe.SPublish(...)` and pipelined `PUBSUB*` commands travel
through `ProcessPipelineHook`, not `ProcessHook`, so the
single-command filter from §11 is not sufficient on its own. The
pipeline hook applies the same lowercased command-name set
({`publish`, `spublish`, `subscribe`, `unsubscribe`, `psubscribe`,
`punsubscribe`, `ssubscribe`, `sunsubscribe`, `pubsub`}) to each
`cmd` in the batch:

- If **every** command in the batch matches the filter, the hook
  short-circuits — no `redis.pipeline` span, no
  `db.client.operation.duration` sample — and calls
  `next(ctx, cmds)` directly.
- Otherwise the pipeline span and metric sample are recorded, and
  `db.operation.batch.size` reflects the **full** batch length
  (filtered commands included). This is the pragmatic compromise:
  mixed Pub/Sub + normal pipelines are rare, and rebuilding the
  batch-size count from a filtered subset would mislead operators
  about how much work the pipeline actually issued.

### 9. Error handling

Following the operator-friendly classification pattern from ADR 0011
§8, but **scoped tighter**: only the two error classes whose
operational response materially differs are pulled out into a
closed-enum attribute. Everything else relies on stable
`error.type`.

Because the wrapper owns the tracing hook (§2), it controls
whether `span.RecordError` and `span.SetStatus(Error)` are called at
all. The flow on every command's return:

| # | Condition | Action |
|---|---|---|
| 1 | `errors.Is(err, redis.Nil)` | Treat as success: span status stays `Unset`, no `RecordError`, no `error.type`. (`redis.Nil` is the sentinel for "key does not exist"; it is a normal control-flow signal, not a failure.) The duration histogram still records a sample with no `error.type` label. |
| 2 | `errors.Is(err, redis.ErrPoolTimeout)` | `span.RecordError(err)`, `SetStatus(Error)`, `error.type` from `reflect.TypeOf(err).String()` (in go-redis v9.9.0 this evaluates to `*errors.errorString`, because `redis.ErrPoolTimeout` is created via `errors.New`; tests assert pool-timeout classification through `errors.Is`, not through the type string, so an upstream switch to a named type does not break callers), `redis.error.kind="pool_timeout"`. Histogram sample carries `error.type` label. |
| 3 | `errors.Is(err, context.DeadlineExceeded)` | `RecordError`, `SetStatus(Error)`, `error.type="context.DeadlineExceeded"`, `redis.error.kind="client_timeout"`. |
| 4 | `errors.Is(err, context.Canceled)` (and not row 3) | `RecordError`, `SetStatus(Error)`, `error.type="context.Canceled"`, `redis.error.kind="client_canceled"`. |
| 5 | any other non-nil err | `RecordError`, `SetStatus(Error)`, `error.type` from `reflect.TypeOf(err).String()`. Do **not** set `redis.error.kind`. |

Rationale for stopping at three `redis.error.kind` values:

- `pool_timeout` and `client_timeout` need different operator
  responses (scale the pool vs. raise the caller's deadline / fix
  the upstream latency). `error.type` alone does not distinguish
  them clearly.
- `client_canceled` is included because it is the common false-error
  in graceful shutdown paths — operators must be able to filter it
  out of error-rate dashboards.
- `auth`, `loading`, `oom`, `readonly`, `cluster_redirect` are not
  in the enum because their detection requires string-matching the
  server reply text (`*redis.Error` carries the message as a
  string), which is fragile to upstream wording changes. If a
  consumer requests them, a separate ADR amendment adds them with
  pinned test fixtures against specific redis-server versions.
- `redis.Nil` does not appear as a kind because it is **not** an
  error in this taxonomy; row 1 explicitly clears the recorded
  error and status.

The `redis.error.kind` attribute is span-only (not a metric label) so
it does not contribute to histogram cardinality.

### 10. Topology modes

The wrapper accepts `redis.UniversalClient`, so single-node
`*redis.Client`, `*redis.ClusterClient`, `*redis.FailoverClient`
(Sentinel), and `*redis.Ring` all flow through one entry point.

Cluster-specific concerns:

- **`server.address` per node.** `Wrap` registers the SDK-owned
  `redis.Hook` on `*redis.ClusterClient` / `*redis.Ring` through
  **both** an `OnNewNode` callback (covers future nodes created
  after `Wrap` returns) **and** an immediate iteration over the
  already-materialised shards via `ForEachShard` (Cluster: via
  `ForEachMaster` + `ForEachSlave`; Ring: `ForEachShard`) calling
  `AddHook` on each per-node `*redis.Client`. The dual approach
  is required because in go-redis v9.9.0 `OnNewNode` only appends
  the callback to a slice that runs from the cluster pool's
  `GetOrCreate`, so a warmed `ClusterClient` passed to `Wrap`
  would otherwise execute commands on existing nodes without the
  hook, silently losing spans and operation metrics until the
  next topology refresh. Single-node `*redis.Client` uses
  `AddHook` directly. All three code paths read `server.address`
  / `server.port` from the per-connection options at span-start
  time. The iteration must be guarded by a per-node sentinel
  marker so a second `Wrap` call (or a refreshed shard that
  happens to be re-presented to `ForEachShard`) does not
  double-hook: each `*redis.Client` is tagged with a no-op marker
  hook on first `AddHook`, and the iteration skips clients whose
  hook chain already contains that marker.

  **Ordering and the unmodified-on-error contract.** To honour §3's
  "on error the original client is returned unmodified" promise,
  the wrapper performs the work in this order: (1) construct the
  metric instruments and the hook value in local variables — no
  mutation of `rdb` yet; (2) run `ForEachShard` (Cluster/Ring)
  collecting the per-node `*redis.Client` references but **not**
  calling `AddHook` during traversal; (3) once traversal completes
  cleanly, install `OnNewNode` and then `AddHook` on each
  collected node, in that order, treating the combined step as
  the single commit point. If any step before commit fails — nil
  `rdb`, unsupported type, instrument creation, or `ForEachShard`
  returning an error — the wrapper returns the original client
  with no callbacks installed and no shards hooked. This means
  callers who decide to proceed after a non-nil error from `Wrap`
  see a uniformly uninstrumented client (no surprise spans from
  shards that happened to be created before the failure point);
  startup readiness can therefore key off the single error return
  without worrying about partial telemetry.
- **MOVED / ASK redirects.** go-redis handles redirects internally
  by re-issuing the command against the correct node. The hook
  emits **one span per attempt**, so a redirected command produces
  two sibling spans (the first with the wrong-node address, the
  second with the right one). This is documented behavior, not a
  bug; the second span carries the successful response.
- **Sentinel / failover (`NewFailoverClient`).** `NewFailoverClient`
  returns a `*redis.Client` whose `Options().Addr` is the literal
  placeholder `"FailoverClient"` — the actual master/replica
  endpoint is selected inside the dialer and is not exposed on the
  client's `Options`. Reading `server.address` from `Options.Addr`
  therefore yields `"FailoverClient"` with no real port, both
  steady-state and across failover. The wrapper records this
  placeholder as-is and does **not** claim to surface the current
  master address; resolving to a real endpoint requires capturing
  it inside `Options.Dialer`, which is out of scope for this ADR
  and tracked under §Open questions. Sentinel-monitor RPCs (the
  `*redis.SentinelClient` returned by `NewSentinelClient`) are not
  instrumented at all — `Wrap` returns an error for it.
- **Replica reads (`RouteByLatency`, `RouteRandomly`,
  `ReadOnly`).** Replica node addresses appear in `server.address`
  naturally. No `redis.cluster.node.role` attribute is emitted by
  the wrapper today; if dashboards need primary-vs-replica split, a
  future amendment can derive it from the cluster topology.

Integration tests under cluster topology use `testcontainers-go`
with the official `redis:7-cluster` (or `valkey/valkey:7.2-cluster`)
image, build-tagged out of default `go test ./...`.

### 11. Out of scope

The following are explicitly **not** in this ADR. Each is a
candidate for a separate ADR if demand appears:

- **`valkey-go` client.** RESP3-first features (client-side caching,
  pubsub via `Receive`) and auto-pipelining are valkey-go's primary
  selling points. None of the current adopters need them.
- **Pub/Sub instrumentation.** `Subscribe` / `Publish` are not
  instrumented. The subscription receive path (`Receive` /
  `ReceiveMessage` on `*redis.PubSub`) bypasses `ProcessHook`
  entirely, so it is naturally excluded. `Publish` / `SPublish` /
  `PubSubChannels` / `PubSubNumSub` / `PubSubNumPat`, however,
  travel through the normal command path and **do** invoke
  `ProcessHook` in v9.9.0; if left unfiltered the wrapper would
  emit `db.*` spans for them despite Pub/Sub being out of scope.
  The hook therefore short-circuits — no span, no metric sample,
  invoke `next(ctx, cmd)` directly — when `cmd.Name()` (lowercased)
  matches the set `{publish, spublish, subscribe, unsubscribe,
  psubscribe, punsubscribe, ssubscribe, sunsubscribe, pubsub}`.
  The same set is also applied inside `ProcessPipelineHook` so
  that pipelined `Publish` does not leak `redis.pipeline` spans
  — see §8 for the pipeline-level short-circuit rules.
  When Pub/Sub becomes a target the right model is `messaging.*`
  semconv (with `messaging.system="redis"`), which is structurally
  unlike the `db.*` model this ADR commits to, so the filter stays
  in place even then.
- **Redis Streams as `messaging.*` semconv.** `XADD` / `XREAD` /
  `XREADGROUP` / `XGROUP` / `XACK` / `XCLAIM` / `XAUTOCLAIM` /
  `XPENDING` / `XRANGE` / `XREVRANGE` / `XLEN` / `XINFO` / `XDEL`
  / `XTRIM` / `XSETID` are normal go-redis commands and **do**
  invoke `ProcessHook`, so unlike Pub/Sub they cannot simply be
  "not instrumented" without an explicit filter. This ADR
  therefore covers them as ordinary `db.*` operations: each
  Streams command produces one `db.client.operation.duration`
  sample and one span carrying `db.operation.name="XADD"`,
  `"XREAD"`, etc. — uppercased, matching the §5 rule that
  applies to every command.
  The `messaging.system="redis"` model with producer/consumer
  spans, message-id linking, and consumer-group lag metrics is a
  follow-up ADR; that ADR would add a Streams-command filter to
  the `db.*` hook (mirroring the Pub/Sub mechanism) and emit
  `messaging.*` telemetry from a separate code path. Until then,
  Streams users get duration/error coverage but no
  Streams-specific attributes (no `messaging.message.id`, no
  consumer-group lag).
- **Client-side caching observability.** Tracking cache hit/miss
  rate, invalidation events, RESP3 client-tracking — out of scope
  until a consumer uses it.
- **Lua script execution attributes.** `EVAL` / `EVALSHA` are
  treated like any other command; no `db.script.*` attributes are
  emitted.

---

## Global-state verification

### Library: `github.com/redis/go-redis/extra/redisotel/v9`

### Version: `v9.9.0`

### Result: ✅ SAFE — does not set globals

**Verification method.** Source inspection of
`extra/redisotel/config.go`, `tracing.go`, and `metrics.go` at tag
`v9.9.0` (commit `c935f96`).

Findings:

- `config.go:57–58` reads `otel.GetTracerProvider()` and
  `otel.GetMeterProvider()` only as fallback when explicit
  `WithTracerProvider` / `WithMeterProvider` options are omitted.
- No `otel.SetTracerProvider`, `otel.SetMeterProvider`, or
  `otel.SetTextMapPropagator` call appears in the package.
- No `os.Setenv` / `os.LookupEnv` in instrumentation paths. There
  is **no env-gate** of the kind seen in `otel-mongo` v0.2.11; if
  `Wrap` is called and providers are passed, command spans are
  emitted unconditionally.

**Wrapper discipline.** `Wrap` calls `redisotel.InstrumentMetrics`
with an explicit `redisotel.WithMeterProvider(mp)`. The fallback
branch is never reached. `redisotel.InstrumentTracing` is not
called, so the tracer-provider fallback path is moot for this
package.

### Audit discipline for upstream bumps

On every `go-redis` / `redisotel` version change:

1. Re-grep the package for `otel.Set*` calls.
2. Re-check the semconv import path. If it advances past v1.24 to a
   stable v1.x ≥ 1.39, evaluate whether `redisotel.InstrumentTracing`
   becomes viable and whether the SDK-owned tracing hook can be
   retired in a future ADR amendment.
3. Re-check the set of metric instruments emitted by
   `redisotel.InstrumentMetrics`. If new instruments appear, audit
   their names and cardinality and update §7 group B / C as needed.
4. Re-check the `redis.Hook` interface for additions or signature
   changes. The wrapper's hook implementation must implement the
   full current interface; CI runs the wrapper against the pinned
   go-redis version to catch this.
5. Update the ADR 0003 approved-integrations table with the new
   version.

---

## Testing

- Unit tests for the wrapper:
  - `Wrap` is idempotent (calling twice does not double-instrument
    and returns `(rdb, nil)` on the second call). For cluster/ring
    clients, idempotency is asserted per node via the sentinel
    marker described in §10 (after the second `Wrap`, each
    per-node `*redis.Client` still has exactly one tracing hook
    in its chain).
  - On a warmed `*redis.ClusterClient` / `*redis.Ring` (one whose
    `ForEachShard` already yields ≥1 node before `Wrap` is
    called), every existing shard's per-node `*redis.Client`
    receives the hook before `Wrap` returns, and a subsequent
    command routed to a pre-existing shard emits a span. This
    asserts §10's "dual mechanism" contract; a regression where
    only `OnNewNode` is installed would fail this test.
  - `Wrap` returns a non-nil error for a nil client, for
    `*redis.SentinelClient`, when given a MeterProvider whose
    instrument creation has been forced to fail, and when
    `ForEachShard` returns an error mid-traversal (the original
    client is returned unmodified in each case). The
    unmodified-on-error contract is asserted by issuing a
    command on the returned client after a forced failure and
    confirming the in-memory exporter records **no** spans and
    **no** `db.client.operation.duration` samples — i.e. no
    `OnNewNode` callback and no partial per-shard hook installation
    survives the error path.
  - The hook short-circuits Pub/Sub commands (§11): asserting that
    `PUBLISH`, `SPUBLISH`, `SUBSCRIBE`, `PSUBSCRIBE`, `PUBSUB`
    produce no span and no `db.client.operation.duration` sample.
  - **Pipeline Pub/Sub filtering (§8).** A pipeline of only
    `pipe.Publish(...)` calls produces no `redis.pipeline` span
    and no duration sample. A mixed pipeline (e.g. one `Publish`
    plus two `Set` calls) records exactly one `redis.pipeline`
    span with `db.operation.batch.size=3` (full length).
  - **Redis Streams are covered as `db.*` (§11).** `XADD`,
    `XREAD`, `XREADGROUP`, `XGROUP CREATE`, `XACK`, and
    `XPENDING` each emit a span with `db.operation.name` set to
    the uppercased command name (per §5: `"XADD"`, `"XREAD"`,
    etc.) and produce a duration sample; no `messaging.*`
    attributes are present.
  - Sentinel-failover clients (`NewFailoverClient`) emit
    `server.address="FailoverClient"` (placeholder), matching the
    limitation documented in §10 and the open question above.
  - Spans emit only the §5 attributes — assert presence of
    `db.system.name`, `db.operation.name`, `server.address`,
    `server.port` and absence of `db.connection_string`, `db.system`,
    `db.statement`, `code.function`, `code.filepath`, `code.lineno`.
  - `db.query.text` is absent by default and present (truncated to
    1 KiB) when `WithCommandTextEnabled(true)`.
  - Error classification table-driven tests for §9 rows 1–5, using
    real fixture errors (`redis.Nil` from `Get` against an empty
    server, `redis.ErrPoolTimeout` from an exhausted test pool,
    `context.DeadlineExceeded` from an expired
    `context.WithTimeout`, `context.Canceled` from an explicitly
    canceled `context.WithCancel`, a `*net.OpError` from a closed
    listener).
  - `redis.Nil` does not call `RecordError`, does not set Error
    status, does not emit `error.type`, but does record a duration
    sample (asserting the histogram count increments).
  - `db.client.operation.duration` is recorded in **seconds** (unit
    "s") and the label set matches §7 group A.
  - `db.client.connections.use_time` is dropped by the view (no
    instrument under that name appears in collected metrics).
  - Pipeline span: name is exactly `redis.pipeline`, attribute
    `db.operation.batch.size` matches the command count, no
    per-command child spans appear.

- Compatibility tests against an in-process `miniredis` instance
  cover single-node behavior end to end (cheap, no Docker).

- Integration tests with `testcontainers-go` cover:
  - 3-node Redis cluster: span addresses match cluster nodes,
    MOVED redirect produces two sibling spans.
  - Valkey single-node (`valkey/valkey:7.2`): wrapper behaves
    identically to Redis single-node; `db.system.name="redis"` is
    asserted (not `"valkey"`).
  - Sentinel failover: spans emitted before, during, and after a
    forced failover all carry the placeholder
    `server.address="FailoverClient"` (per §10's documented
    limitation), no spans are dropped, and the wrapper does not
    panic across the topology refresh.
  - All integration tests are build-tagged out of default
    `go test ./...`.

---

## Consequences

**Positive**

- Single client library covers both Redis and Valkey backends;
  callers do not change code when switching.
- Span attributes are full stable semconv v1.39 from the source,
  not a rewrite of legacy keys: the trace pipeline never carries
  `db.system`, `db.connection_string`, `db.statement`, or the
  misleading `code.*` keys at all.
- `db.connection_string` never enters the trace pipeline, so the
  `redis://user:password@host:port/db` credential-leak path is
  closed at the source rather than relying on a downstream filter.
- `db.client.operation.duration` is recorded in seconds end to end,
  matching v1.39 canonical units; no unit-drift compatibility note
  needed.
- `db.query.text` is off by default, closing the redisotel
  data-protection footgun.
- `redis.Nil` is treated as success: span status `Unset`, no
  `RecordError` event in the timeline, no `error.type` label on
  the duration histogram. Error-rate dashboards are not inflated by
  cache-miss control flow.
- `pool_timeout` vs `client_timeout` distinction lets operators
  separate "scale the pool" from "raise the deadline" responses.
- Cluster, Sentinel-failover, and Ring topologies are supported
  through one `redis.UniversalClient`-accepting API. Cluster and
  Ring carry the hook to each node automatically via `OnNewNode`;
  single-node and Sentinel-failover use `AddHook`. (Sentinel
  spans surface a placeholder `server.address` — see §10.)
- Pool-stats instruments come from `redisotel.InstrumentMetrics`
  without modification — we do not reimplement what upstream gets
  right.

**Negative / Trade-offs**

- ~120–150 LOC of SDK-owned tracing code (the `redis.Hook`
  implementation), versus the ~80 LOC a pure-T2 facade would have
  needed. Justified by the impossibility of removing attributes
  through a post-hook; see §2.
- The wrapper must track the `redis.Hook` interface across go-redis
  versions. If go-redis v10 changes the interface shape, the
  wrapper's hook must follow. CI pins go-redis and asserts
  interface compatibility.
- Pub/Sub gap is real. Services that rely on `PUBLISH` /
  `SUBSCRIBE` for non-trivial workflows will see no spans for
  those operations under this ADR: subscription receive paths
  (`*redis.PubSub.Receive*`) bypass `redis.Hook.ProcessHook`
  entirely, and the publish-side commands (`PUBLISH` / `SPUBLISH`
  / `PUBSUB*`) — which **do** travel through `ProcessHook` in
  v9.9.0 — are intentionally short-circuited by the
  command-name filter documented in §11.
- `redis.error.kind` is intentionally narrower than
  `resty.error.kind`. Operators looking for `auth` / `loading` /
  `oom` distinctions must read the error message or wait for an
  amendment.
- `valkey-go` users are not served. If RESP3 client-side caching
  becomes a target, a sibling ADR is required.
- The view that drops `db.client.connections.use_time` must be
  registered at `o11y.Init` time (OTel Go views are fixed at
  MeterProvider construction). The redis package exports the view
  constructor; `o11y.Init` must include it. A consumer that builds
  their own MeterProvider without composing this view will see the
  upstream instrument alongside the SDK's `db.client.operation.duration`,
  reporting roughly the same data twice under different names.

---

## Open questions

- **Per-command sub-spans inside pipelines.** Some users will
  eventually ask for them. The `redis.Hook` interface fires once
  per batch, not per command; reconstructing per-command timing
  requires instrumenting at the protocol layer. Defer until a
  concrete consumer asks.
- **`redis.cluster.node.role` (`primary` / `replica`).** Not
  emitted today. Adding it requires consulting cluster topology
  state, which the wrapper does not currently hold. Defer.
- **Real `server.address` for sentinel-failover clients.** As noted
  in §10, `*redis.Client` from `NewFailoverClient` exposes
  `Options.Addr == "FailoverClient"`; the actual master endpoint
  is selected inside `Options.Dialer`. Surfacing the real address
  on spans requires wrapping the dialer to capture the resolved
  `host:port` and threading it through to the hook via a
  per-connection context value or a `sync.Map` keyed by net.Conn
  identity. Both approaches add complexity (and the latter risks
  leaks); defer until a consumer demonstrates that distinguishing
  masters across failover from telemetry alone is required.
- **Retiring the SDK-owned tracing hook.** If a future redisotel
  release upgrades its semconv pin past v1.39, stops emitting
  `db.connection_string`, and flips `db.statement` to default-off,
  the architectural reasons for owning the tracing hook disappear.
  At that point a follow-up ADR can amend §2 back to a pure-T2
  facade and delete the SDK-owned hook. Track redisotel releases
  per the "Audit discipline for upstream bumps" section above.
