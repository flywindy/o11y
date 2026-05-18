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
| 4 | Configurability of names/attributes | ✅ | `WithTracerProvider`, `WithMeterProvider`, `WithDBStatement(bool)`, `WithAttributes(...)` cover what we need; the legacy attribute keys can be rewritten via an additional hook (see §5). |
| 5 | Framework signal access | ⚠️ | Tracing, dial, pool stats, pipeline, and cluster (via `OnNewNode`) are covered. Pub/Sub is **not instrumented** (no hook fires for `Subscribe` / `Publish`). Acceptable; see §11. |

Item 3 fails strictly. Under ADR 0008 §2 that puts T3 on the table.
A pure T3 (reimplement on top of `redis.Hook` from scratch) would be
~300–400 LOC and would duplicate the dial/pool/cluster wiring that
redisotel already gets right. Item 3 is fixable with a thin
attribute-rewrite layer in front of redisotel (§5), so this ADR
chooses a **T2-with-rewrite** path rather than T3. The rewrite layer
is ~30 LOC and bounded to the legacy-key translation table; if
redisotel later upgrades its semconv pin, the rewrite layer becomes a
no-op and can be deleted.

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

### 2. Sourcing tier: T2 facade over redisotel/v9 + attribute rewrite

The wrapper installs `redisotel.InstrumentTracing` and
`redisotel.InstrumentMetrics` on the user's client, then adds a small
SDK-owned `redis.Hook` that runs **after** redisotel's hook in the
chain. The SDK-owned hook does three things:

1. Translates legacy attribute keys to stable v1.39 names (§5).
2. Drops attributes the SDK does not want to emit (`db.connection_string`).
3. Classifies errors (§9).

A metric.View registered at `o11y.Init` rewrites the upstream
instrument names to stable forms (§7).

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
func Wrap(
    rdb redis.UniversalClient,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    opts ...Option,
) redis.UniversalClient

type Option func(*config)

// WithCommandTextEnabled records the full Redis command (including
// arguments) as db.query.text on each span. OFF by default; commands
// often contain key names and values that are sensitive.
func WithCommandTextEnabled(enabled bool) Option

// WithAttributes appends static attributes (e.g. service-level
// labels) to every emitted span and metric sample.
func WithAttributes(attrs ...attribute.KeyValue) Option
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

### 5. Attribute rewrite to semconv v1.39

redisotel v9.9.0 emits legacy keys. The SDK-owned post-hook rewrites
on every span:

| Legacy key (redisotel @ v1.24) | Rewrite | Notes |
|---|---|---|
| `db.system` ("redis") | `db.system.name` ("redis") | Drop the legacy key after copying. |
| `db.statement` | `db.query.text` | Only if `WithCommandTextEnabled(true)`. Otherwise drop both. |
| `db.connection_string` | (dropped) | May contain `redis://user:password@host:port/db`. Replaced functionally by `server.address` + `server.port`, which redisotel also sets. |
| `db.redis.num_cmd` | `db.operation.batch.size` | Stable v1.39 name. |
| `server.address` | unchanged | Already stable. |
| `server.port` | unchanged | Already stable. |
| `code.function`, `code.filepath`, `code.lineno` | (dropped) | These describe the redisotel hook's call site, not the caller's; misleading. |

The rewrite happens in the wrapper hook's `AfterProcess` /
`AfterProcessPipeline` equivalents (go-redis v9's hook model wraps
process via context-passing `next`; the wrapper composes around the
return). The set of legacy keys is closed and lives in a single
table in the wrapper for easy audit on redisotel bumps.

### 6. Command text (`db.query.text`) is opt-in, default off

redisotel's `WithDBStatement(true)` is **default-on** upstream and
emits full command text including arguments. This is unsafe as a
default for the same reasons as `_oteltrace` document propagation in
ADR 0005: Redis arguments routinely contain key names with embedded
identifiers, cached values, session tokens, hashed credentials, etc.

The wrapper passes `redisotel.WithDBStatement(false)` to upstream
unconditionally, then conditionally re-emits `db.query.text` from
its own hook when `WithCommandTextEnabled(true)`. Re-emission, not
pass-through, lets the wrapper apply consistent length truncation
(implementation PR sets a 1 KiB cap to bound span size).

Services that need full command text (debug environments, query
profilers) must opt in explicitly and accept the data-protection
consequences.

### 7. Metrics: instrument names rewritten via metric.View

redisotel v1.24 emits `db.client.connections.use_time` (histogram,
unit `ms`). Stable v1.39 names this metric
`db.client.operation.duration` (unit `s`). Connection-pool gauges
(`db.client.connections.idle.max` / `.usage` / `.timeouts` / etc.)
are unchanged between v1.24 and v1.39 — those pass through.

The wrapper does not rename in code; it registers metric.Views at
`Wrap` time (or composes views the host SDK injects via
`o11y.Init`):

```go
view.New(
    view.MatchInstrumentName("db.client.connections.use_time"),
    view.WithRename("db.client.operation.duration"),
    view.WithUnit("s"),
    view.WithAggregation(/* divide-by-1000 not natively supported,
                           so we accept the unit drift on this view
                           and document it; see Open Questions */),
)
```

Unit conversion (`ms` → `s`) is **not** done by the View — OTel's
View API does not transform sample values. The wrapper documents
that the rewritten histogram emits values in milliseconds with unit
label `s`, **or** keeps the upstream `ms` unit label and forgoes the
v1.39 unit alignment. The implementation PR picks one and documents
the choice; this ADR's preference is to keep `ms` (Option B) and
accept the unit drift as a known compatibility note, since rewriting
unit labels without rewriting values is worse than admitting the
unit. The histogram name still aligns with v1.39 so dashboard
queries are correct; the unit label is the only point of drift.

Once a future redisotel version upgrades its semconv pin and uses
seconds natively, the View can be retired.

The renamed `db.client.operation.duration` then falls under the
metric.View allowlist registered at `o11y.Init` per ADR 0009 §2, so
its label cardinality is bounded the same way HTTP duration is.

### 8. Pipeline / batch span model

redisotel produces **one span per pipeline batch** with
`db.redis.num_cmd` recording the command count (rewritten to
`db.operation.batch.size` per §5). The wrapper preserves this flat
model:

- Span name: `redis.pipeline` (renamed from upstream's
  `redis.pipeline <summary>` to drop the summary, which has unbounded
  cardinality from concatenated command names).
- Attributes: `db.operation.name="pipeline"`,
  `db.operation.batch.size=<n>`, `db.system.name="redis"`,
  `server.address`, `server.port`.
- One metric sample per batch on `db.client.operation.duration` with
  `db.operation.name="pipeline"`. Per-command timing is not
  available from redisotel and the wrapper does not reconstruct it.

Per-command sub-spans inside a pipeline are out of scope. redisotel
does not surface per-command timing during the batch, and emulating
it would either require forking redisotel or instrumenting at a
lower layer than the hook contract permits.

### 9. Error handling

Following the operator-friendly classification pattern from ADR 0011
§8, but **scoped tighter**: only the two error classes whose
operational response materially differs are pulled out into a
closed-enum attribute. Everything else relies on stable
`error.type`.

The wrapper hook runs after redisotel and adjusts the span:

| # | Condition | Action |
|---|---|---|
| 1 | `errors.Is(err, redis.Nil)` | **Unset** the Error status redisotel sets. Drop the recorded error. Do not emit `error.type`. (`redis.Nil` is the sentinel for "key does not exist"; it is a normal control-flow signal, not a failure.) |
| 2 | `errors.Is(err, redis.ErrPoolTimeout)` | Set `error.type="*redis.PoolError"` and `redis.error.kind="pool_timeout"`. Leave Error status as set by redisotel. |
| 3 | `errors.Is(err, context.DeadlineExceeded)` | Set `error.type="context.DeadlineExceeded"` and `redis.error.kind="client_timeout"`. |
| 4 | `errors.Is(err, context.Canceled)` (and not row 3) | Set `error.type="context.Canceled"` and `redis.error.kind="client_canceled"`. |
| 5 | any other non-nil err | Set `error.type` from `reflect.TypeOf(err).String()`. Do **not** set `redis.error.kind`. |

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

- **`server.address` per node.** redisotel's
  `InstrumentTracing(rdb)` registers an `OnNewNode` callback on
  `*redis.ClusterClient` / `*redis.Ring`, so each node's hook chain
  carries that node's address. The wrapper relies on this directly;
  no extra cluster-aware code is needed.
- **MOVED / ASK redirects.** go-redis handles redirects internally
  by re-issuing the command against the correct node. redisotel
  emits **one span per attempt**, so a redirected command produces
  two sibling spans (the first with the wrong-node address, the
  second with the right one). This is documented behavior, not a
  bug; the second span carries the successful response.
- **Failover.** When a primary fails over, go-redis refreshes the
  topology and `OnNewNode` fires for the new primary. redisotel
  does not cache topology state, so the new primary's spans carry
  the correct `server.address` from the first hook invocation. The
  wrapper relies on this and adds no failover-specific logic.
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
- **Pub/Sub instrumentation.** redisotel does not instrument
  `Subscribe` / `Publish`. The wrapper does not backfill it. When
  Pub/Sub becomes a target, the right model is `messaging.*`
  semconv (with `messaging.system="redis"`), which is structurally
  unlike the `db.*` model this ADR commits to.
- **Redis Streams (`XADD` / `XREAD` / consumer groups).** Same
  reasoning: `messaging.*` semconv, separate ADR.
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

**Wrapper discipline.** `Wrap` always passes both
`redisotel.WithTracerProvider(tp)` and
`redisotel.WithMeterProvider(mp)`. The fallback branch is never
reached in practice.

### Audit discipline for upstream bumps

On every `go-redis` / `redisotel` version change:

1. Re-grep the package for `otel.Set*` calls.
2. Re-check the semconv import path. If it advances past v1.24, the
   §5 rewrite table can shrink; if any new attribute is added,
   evaluate whether it needs rewriting.
3. Re-check `db.statement` default in `config.go` — if upstream
   flips the default to off, §6's defensive override becomes
   redundant but harmless.
4. Update the ADR 0003 approved-integrations table with the new
   version.

---

## Testing

- Unit tests for the wrapper:
  - `Wrap` is idempotent (calling twice does not double-instrument).
  - `Wrap` rejects a nil client.
  - Attribute rewrite produces v1.39 keys and drops the legacy
    originals (table-driven, one row per §5 entry).
  - `db.connection_string` is never present on any emitted span.
  - `db.query.text` is absent by default and present (truncated)
    when `WithCommandTextEnabled(true)`.
  - Error classification table-driven tests for §9 rows 1–5, using
    real fixture errors (`redis.Nil`, `redis.ErrPoolTimeout` from a
    test pool, `context.DeadlineExceeded` from a canceled context,
    a `*net.OpError` from a closed listener).
  - `redis.Nil` does not set `error.type`, does not increment any
    error counter, and leaves span status `Unset`.
  - metric.View renaming
    `db.client.connections.use_time` → `db.client.operation.duration`
    is registered and produces a histogram under the new name.
  - Pipeline span: name is `redis.pipeline`, attribute
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
  - Sentinel failover: spans emitted before, during, and after
    failover all carry a current `server.address` (no stale
    addresses after topology refresh).
  - All integration tests are build-tagged out of default
    `go test ./...`.

---

## Consequences

**Positive**

- Single client library covers both Redis and Valkey backends;
  callers do not change code when switching.
- Less SDK-owned instrumentation code (~80–120 LOC wrapper) than a
  T3 reimplementation would require.
- Attribute names align with semconv v1.39 despite redisotel's v1.24
  pin, so dashboards keyed on `db.system.name` / `db.query.text` /
  `db.operation.batch.size` work uniformly with other DB
  integrations.
- `db.query.text` is off by default, closing the redisotel
  data-protection footgun.
- `db.connection_string` is dropped, eliminating a credential-leak
  path through span attributes.
- `redis.Nil` no longer inflates error-rate dashboards.
- `pool_timeout` vs `client_timeout` distinction lets operators
  separate "scale the pool" from "raise the deadline" responses.
- Cluster, Sentinel, and Ring topologies are supported through one
  `redis.UniversalClient`-accepting API with no extra code.

**Negative / Trade-offs**

- The semconv-rewrite layer is dead weight the moment redisotel
  upgrades its semconv pin. Until then it must be maintained.
- The histogram unit drift (`ms` retained while name aligns to
  v1.39 `db.client.operation.duration` which canonically uses `s`)
  is a documented compatibility quirk. Dashboards reading the
  histogram must know the unit.
- Pub/Sub gap is real. Services that rely on `PUBLISH` /
  `SUBSCRIBE` for non-trivial workflows will see no spans for
  those operations under this ADR.
- `redis.error.kind` is intentionally narrower than
  `resty.error.kind`. Operators looking for `auth` / `loading` /
  `oom` distinctions must read the error message or wait for an
  amendment.
- `valkey-go` users are not served. If RESP3 client-side caching
  becomes a target, a sibling ADR is required.
- Adding a post-hook after redisotel's hook means the wrapper's
  hook ordering on `*redis.Client.AddHook` matters. The
  implementation PR documents the ordering invariant and asserts
  it in a test.

---

## Open questions

- **Histogram unit drift on `db.client.operation.duration`.** §7
  currently keeps the upstream `ms` unit while renaming the
  instrument to the v1.39 name (which canonically expects `s`).
  Alternatives: (a) accept the drift and document, (b) wait for a
  redisotel semconv bump, (c) intercept records via a custom
  exporter wrapper that divides by 1000. (c) is invasive enough to
  warrant its own ADR. Lean: (a) for now.
- **Per-command sub-spans inside pipelines.** Some users will
  eventually ask for them. Doing it correctly requires either
  forking redisotel or recording timings at the resp3 protocol
  level. Defer until a concrete consumer asks.
- **`redis.cluster.node.role` (`primary` / `replica`).** Not
  emitted today. Adding it requires consulting cluster topology
  state, which the wrapper does not currently hold. Defer.
- **redisotel's `db.statement` default flipping upstream.** If a
  future redisotel release flips its default to off, §6's
  defensive `WithDBStatement(false)` becomes redundant. Keep it
  anyway as defense in depth; document the redundancy at that
  point.
