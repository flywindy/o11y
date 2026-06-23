# ADR 0013 — Redis & Valkey Integration

**Status**: Accepted (implemented)
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
- **Metrics.** `redisotel.InstrumentMetrics` is **not** used.
  Its connection-pool instruments carry v1.24-era plural names
  (`db.client.connections.*`, ms histograms) and the labels it
  emits (`pool.name`, legacy `db.system`) cannot be translated
  to the §5 group A allowlist via OTel Go views (`AttributeFilter`
  only retains/drops, it cannot add). The wrapper instead owns
  the pool-metric instrument set end-to-end: it reads
  `*redis.Client.PoolStats()` from each live shard on each
  observation cycle and records under v1.39 singular names
  (`db.client.connection.*`, `s` histograms) with the group A
  label set. See §7 group B for the full instrument list and
  §7 group C for the defensive drop views that keep the v1.39
  contract intact if a consumer also wires up
  `redisotel.InstrumentMetrics` for an unrelated reason.

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
2. **Own the connection-pool metric instrument set directly.**
   The wrapper registers a single async observable callback
   against the supplied MeterProvider that reads
   `*redis.Client.PoolStats()` from each live shard at scrape
   time and records under v1.39 singular names
   (`db.client.connection.idle.{max,min}`,
   `db.client.connection.max`, `db.client.connection.count`
   with `db.client.connection.state="used"|"idle"`,
   `db.client.connection.timeouts`)
   plus a `db.client.connection.create_time` histogram (unit
   `s`) populated from the SDK-owned dial hook. The redisotel
   v1.24-era instruments are not used; §7 group B explains why,
   and group C documents defensive drop views.

`redisotel.InstrumentTracing` is **not** called.

### 3. Public API shape

```go
// package redis (import as o11yredis "github.com/flywindy/o11y/redis")

// Wrap installs tracing + metrics on an existing client. The caller
// retains control over addrs, pool size, TLS, auth, and any other
// go-redis configuration. Returns the same client for chaining.
//
// Wrap accepts any redis.UniversalClient, so single-node,
// *redis.Client (including the Sentinel-failover variant returned
// by NewFailoverClient — go-redis v9.9.0 returns a regular
// *redis.Client there, distinguishable only by its Options().Addr
// placeholder), *redis.ClusterClient, and *redis.Ring all work.
//
// Wrap is idempotent: calling it twice on the same client is a no-op
// after the first call.
//
// Wrap returns a non-nil error when:
//   - rdb is nil;
//   - tp (trace.TracerProvider) is nil;
//   - mp (metric.MeterProvider) is nil;
//   - the underlying concrete type is not one of the three
//     supported variants of redis.UniversalClient (single /
//     cluster / ring — Sentinel-failover is a *redis.Client, so
//     it falls under "single"). Note: *redis.SentinelClient (the
//     raw sentinel-monitor RPC client from NewSentinelClient)
//     does NOT implement redis.UniversalClient at all in go-redis
//     v9.9.0, so passing it to Wrap is a compile-time error, not
//     a runtime one — no error class is needed for it;
//   - creation of the wrapper's own metric instruments
//     (db.client.operation.duration in §5 group A and the
//     pool-stat instrument set in §7 group B) against the
//     supplied MeterProvider fails;
//   - the initial shard-collection pass over an already-warmed
//     *redis.ClusterClient / *redis.Ring fails.
//
// Wrap does NOT fall back to otel.GetTracerProvider() /
// otel.GetMeterProvider() when the arguments are nil — the
// no-OTel-globals discipline (ADR 0008) requires that
// providers be passed explicitly. Nil providers are a
// configuration bug, surfaced as a strict error.
//
// The first four error classes (nil rdb / nil tp / nil mp /
// instrument-creation) preserve the strict
// unmodified-on-error contract: the original client is
// returned with no callbacks installed and the dedup-map entry
// stays open for retry.
//
// The remaining class — shard-collection failure on a warmed
// Cluster/Ring — is best-effort. By the time it is reached,
// the wrapper has already installed OnNewNode (which closes
// the v9.9.0 race window for nodes created mid-iteration; see
// §10) and may have hooked some per-node *redis.Clients via
// the callback. go-redis v9.9.0 exposes no public hook-remove
// API, so the wrapper cannot unwind. Wrap therefore returns
// (rdb, err), sets the dedup-map done=true, registers
// runtime.AddCleanup, and the caller observes a partially-
// instrumented client. Retries are silent no-ops to avoid
// double-hooking the nodes already covered. Pool metrics on
// these partially-hooked nodes still emit correctly — the
// pool-stat observer iterates the live shard set at scrape
// time, not the in-flight collection state.
//
// Callers MUST NOT ignore Wrap's error silently — log it and
// surface it to startup readiness so missing Redis telemetry is
// observable; whether to fail open or fail closed on a metrics-
// phase error is a deployment decision.
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

// WithAttributes appends extra attributes to every emitted **span**.
// These never flow to metric samples, regardless of cardinality —
// the metric label set is fixed by §7 group A and ADR 0009 §2 so
// that per-user / per-request / per-key labels cannot inflate
// db.client.operation.duration or the redisotel pool-stat
// instruments. Intended for span-only enrichment such as
// service-level tags that a tracing backend can index but a
// metrics backend would treat as cardinality.
func WithAttributes(attrs ...attribute.KeyValue) Option

// WithPoolName sets the application-defined identifier emitted as
// db.client.connection.pool.name on every pool-metric sample (§7
// group B). The OTel database-metrics semconv marks this attribute
// as required for db.client.connection.* instruments.
//
// When omitted, the wrapper synthesises a process-unique default of
// "redis-<uintptr-hex>" derived from the concrete client pointer.
// For Cluster / Ring topologies the per-shard label is the
// composed value "<top-level-pool-name>/<shard-Addr>" — see §7
// group B for why per-shard discrimination is required even when
// shards share a base name.
//
// Supply WithPoolName when you want stable, human-readable labels
// across deployments (e.g. "sessions-cache", "rate-limiter"). Two
// Wrap calls in the same process MUST supply distinct pool-names
// if they wrap distinct logical pools that may resolve to the same
// host/port (sidecar deployments, multiple db numbers, fixtures),
// otherwise the OTel async-callback uniqueness contract is
// violated.
func WithPoolName(name string) Option

// MetricViews returns the metric.Views this package needs registered
// at MeterProvider construction time. The set contains:
//   - the §5 group A allowlist views for db.client.operation.duration
//     (label set bounded to db.system.name, db.operation.name,
//     server.address, server.port, error.type);
//   - defensive drop views for every v1.24-era
//     db.client.connections.* instrument that redisotel v9.9.0
//     would emit, in case a consumer also wires up
//     redisotel.InstrumentMetrics — see §7 group C for why.
//
// o11y.Init composes these into its default view set; consumers
// building their own MeterProvider must include them via
// sdkmetric.WithView, or the v1.39 contract is not preserved.
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
| `server.address` | every span | host portion of `redis.Options.Addr` for single-node / cluster / ring; the literal `"FailoverClient"` placeholder for Sentinel-failover (see §10 limitation) |
| `server.port` | when `Options.Addr` splits cleanly into `host:port` | numeric port from `redis.Options.Addr`. **Omitted** when the address has no port — specifically for Sentinel-failover (`Options.Addr == "FailoverClient"`) and for any UDS path that may surface in future. Span tests assert presence only for the topologies that can supply it. |
| `db.query.text` | only if `WithCommandTextEnabled(true)` | the command and arguments, truncated at 1 KiB |
| `db.operation.batch.size` | pipeline spans only, **and only when the effective user-command count is ≥ 2** | command count in the hook-invocation batch — equals the caller's batch for single-node / sentinel-failover; equals the per-shard subset for cluster / ring (§8). Omitted when the per-hook count is 0 or 1, per the semconv v1.39 rule that batch operations are batches of two or more. |
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
command name is the operation, not the argument. This is already the
`{system.name}.{operation}` shape later standardized across all data-store
integrations in ADR 0023 (redis has no target — the key is high-cardinality),
so no change was needed here.

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
| `db.client.operation.duration` | Float64Histogram | `s` | `db.system.name`, `db.operation.name`, `server.address`, `server.port` (when `Options.Addr` splits — i.e. **omitted** for Sentinel-failover whose `Addr` is the `"FailoverClient"` placeholder), `error.type` (on errors only) |

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

**B. Connection-pool metrics — owned by the wrapper, emitted in semconv v1.39.**

The ADR originally planned to call
`redisotel.InstrumentMetrics(...)` and translate its emissions
to v1.39 via OTel Go views. That approach is **not viable**: in
OTel Go `sdk/metric` v1.43.0, `Stream.AttributeFilter` only
chooses which already-recorded attributes are retained — it
cannot add `db.system.name`, `server.address`, or `server.port`
to redisotel's `pool.name` / legacy `db.system` label set. The
wrapper therefore does **not** call `redisotel.InstrumentMetrics`
at all. It owns the pool-metric instrument set end-to-end,
reads `*redis.Client.PoolStats()` from each per-node client at
observation time, and records under v1.39 names with a
**dedicated pool-metric label set** described below — distinct
from the §7 group A operation-duration allowlist because the
pool metrics need pool identity but not `db.operation.name`.

**Pool-metric label set.** The OTel database-metrics semconv
marks `db.client.connection.pool.name` as **required** for
every `db.client.connection.*` instrument, and the OTel
async-instrument rules require each callback to produce a
unique attribute set per observation — without a pool identity
label, two `Wrap` calls on different clients pointing at the
same host/port (sidecar deployments, multiple `db` numbers,
fixtures) would collapse into one time series and violate both
the semconv and the SDK uniqueness contract. The wrapper
therefore emits, for every pool-metric sample:

- `db.system.name = "redis"`
- `db.client.connection.pool.name = <pool-name>` — application-
  unique, see derivation below
- `server.address = <host>` (taken from the per-shard
  `Options().Addr`)
- `server.port = <port>` when `Options().Addr` splits cleanly
  (omitted for the Sentinel-failover placeholder, same rule as
  §5)

**Pool-name derivation.** The wrapper accepts a
`WithPoolName(string)` option on `Wrap` carrying an
application-defined identifier (mirroring the semconv intent —
e.g. `"sessions-cache"`, `"rate-limiter"`). If supplied, the
top-level pool-name is that string; otherwise the wrapper
synthesises a default of `redis-<uintptr-hex>` using the
concrete client pointer (the same value the §10 dedup map keys
on), which guarantees uniqueness across `Wrap` calls in the
same process. For Cluster / Ring, the per-shard pool-name
appends the shard's address as a suffix:
`<top-level-pool-name>/<shard-Addr>`. Within a single
`*redis.ClusterClient`, distinct shards already have distinct
addresses; the prefix discriminates between two `Wrap`s on
different cluster clients whose topologies happen to overlap.

Instruments registered once per `Wrap` call (async / observable
where appropriate):

- `db.client.connection.idle.max` — Int64ObservableUpDownCounter
- `db.client.connection.idle.min` — Int64ObservableUpDownCounter
- `db.client.connection.max` — Int64ObservableUpDownCounter
- `db.client.connection.count` (carrying
  `db.client.connection.state="used"|"idle"` — the full semconv
  v1.39 attribute key, not the shorthand `state`) —
  Int64ObservableUpDownCounter
- `db.client.connection.timeouts` — Int64ObservableCounter
- `db.client.connection.create_time` (unit `s`) — Float64Histogram,
  recorded synchronously from inside the SDK-owned dial hook
  (the only timing point where the start/end timestamps are
  available).

All async instruments are registered with a single
`RegisterCallback` whose closure captures a
`weak.Pointer[T]` to the underlying concrete client (one of
`*redis.Client` / `*redis.ClusterClient` / `*redis.Ring`) plus
the resolved top-level pool-name. On each observation cycle the
callback:

1. Dereferences the weak pointer; if `nil` (the client has been
   GC'd between scrape cycles), the callback **returns without
   observing and without calling `Unregister`**. Calling
   `Registration.Unregister()` from inside the callback would
   deadlock OTel Go `sdk/metric` v1.43.0 — `pipeline.produce`
   holds the pipeline lock while invoking registered callbacks,
   and `Unregister` re-takes the same lock. The actual
   unregistration is performed from outside the callback by the
   `runtime.AddCleanup` hook the wrapper installed on commit
   success (which also deletes the dedup-map entry); that hook
   runs in its own goroutine after the client is reclaimed and
   is therefore not under the pipeline lock. Until the cleanup
   fires, the callback simply no-ops on each scrape — cheap and
   safe.
2. For single-node, reads `PoolStats()` once and records the
   gauges under the pool-metric label set above, with
   `db.client.connection.pool.name` set to the top-level
   pool-name and `server.address` / `server.port` taken from
   `Options()`.
3. For Cluster / Ring, iterates the current shard set —
   `*redis.ClusterClient.ForEachMaster` + `ForEachSlave` for
   Cluster, `*redis.Ring.GetShardClients()` for Ring (the same
   APIs §10's setup pass uses, with the same down-shard
   coverage caveat resolved by `GetShardClients`). For each
   per-node `*redis.Client`, reads `PoolStats()` and records
   under the same label set, with `db.client.connection.pool.name`
   set to `<top-level-pool-name>/<shard.Options().Addr>` and
   `server.address` / `server.port` taken from that node's
   `Options()`.

Reading pool stats at observation time rather than registering
per-node callbacks eliminates redisotel's shared-`conf` bug
(every observation builds its label set fresh from the current
shard's `Options`), removes the in-flight race window entirely
(no setup-phase per-node InstrumentMetrics call exists), and
keeps the dedup map free of per-node `*redis.Client`
references. The observation cost is one `PoolStats()` call per
shard per scrape interval, which is a few atomic-counter reads
inside go-redis — well within the metrics SDK's observation
budget.

**Implication for §10's commit sequence.** Because the wrapper
no longer calls `redisotel.InstrumentMetrics`, §10's
"best-effort metrics phase" no longer needs the
`metricsInstalled` CAS gate, the `metricsErr` field, the shared-
`conf` workaround, or the warmed-Cluster `ForEachShard`-failure
carve-out for metrics. Instrument creation (the async
registrations and the `create_time` histogram) is a single
strict pre-commit step that either succeeds or fails as one
unit, joining the existing strict error class. The detailed
walk-through in §10 carries the simplified flow; this group B
section is the authoritative description of what the metrics
path actually does.

**C. Defensive drop views for redisotel emissions.**

The wrapper itself does not call `redisotel.InstrumentMetrics`
(see group B), so in a default `Wrap` + `o11y.Init` deployment
no `db.client.connections.*` instruments are registered against
the MeterProvider. However, an end-user who composes their own
MeterProvider and **also** wires up `redisotel.InstrumentMetrics`
on the side (perhaps for a non-Redis-via-this-wrapper code
path, or by accident) would re-introduce the v1.24-named
plural family. To keep the v1.39 contract intact for any
MeterProvider that includes `MetricViews()`, the export
registers a defensive drop view for every redisotel pool
instrument:

- `db.client.connections.usage`
- `db.client.connections.idle.max`
- `db.client.connections.idle.min`
- `db.client.connections.max`
- `db.client.connections.timeouts`
- `db.client.connections.hits`
- `db.client.connections.misses`
- `db.client.connections.create_time`
- `db.client.connections.use_time`

Each is dropped via
`sdkmetric.NewView(sdkmetric.Instrument{Name: <plural-name>},
sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}})`.
This is a no-op if redisotel was never registered, and prevents
mixed v1.24/v1.39 emissions if it was. The full set lives in
the redis package's `MetricViews()` export and matches the OTel
Go `sdk/metric` v1.43.0 API the repo already uses in
`internal/metrics.defaultViews`.

**View registration site.** The group A allowlist views from §5
and the group C defensive drops here must be registered at
`sdkmetric.NewMeterProvider` construction time — OTel Go fixes
views when the MeterProvider is built, so `Wrap` cannot add
views after the fact. This ADR therefore requires that
`o11y.Init` include the redis view set in its default view set
(adjacent to the ADR 0009 §2 allowlist views). The redis
package exports `MetricViews()` so `o11y.Init` can compose it
without the redis package needing to know about `o11y.Init`.

The operation-duration histogram in group A falls under the
metric.View allowlist registered at `o11y.Init` per ADR 0009 §2;
the label set above bounds its cardinality.

### 8. Pipeline / batch span model

The wrapper's `ProcessPipelineHook` emits one flat span per
**hook invocation**:

- Span name: `redis.pipeline`. Low cardinality; the command list is
  not appended to the name.
- Attributes: `db.operation.name="pipeline"`,
  `db.operation.batch.size=<n>` **when the effective user-
  command count is ≥ 2** (semconv v1.39 reserves batch.size
  for true batches; the attribute is omitted when a clustered
  pipeline routes a single command to one shard, when an
  internal split happens to yield a 1-command per-node hook
  invocation, or any other case where the per-hook count is
  0 or 1), `db.system.name="redis"`, `server.address`, and
  `server.port` **when `Options.Addr` splits cleanly** (omitted
  otherwise, per §5 / §7 — Sentinel-failover pipelines whose
  `Addr == "FailoverClient"` carry no port).
- One metric sample per hook invocation on
  `db.client.operation.duration` with
  `db.operation.name="pipeline"`.

**Single-node / Sentinel-failover** (both are `*redis.Client`,
differentiated only by `Options().Addr`): one hook invocation per
`Pipeline()` / `TxPipeline()` call, so one span and one sample per
caller batch.

For `Pipeline()`, the hook sees exactly the caller-issued
commands and `db.operation.batch.size` matches that count.

For `TxPipeline()`, go-redis v9.9.0 wraps the user's commands
with synthetic `MULTI` and `EXEC` via `wrapMultiExec` **before**
`ProcessPipelineHook` fires, so the hook's `cmds` slice is
`[MULTI, user1, user2, …, userN, EXEC]`. The wrapper compensates
in two ways:

- **`db.operation.batch.size` excludes framing.** When the first
  cmd is `MULTI` and the last is `EXEC`, the wrapper subtracts
  two from the count it reports as `db.operation.batch.size` so
  the attribute reflects the user-issued command count, not the
  on-wire RESP-level count. Dashboards that group by
  `batch.size` therefore see `TxPipeline(3)` and `Pipeline(3)`
  as the same size.
- **Pub/Sub all-match short-circuit ignores framing.** When
  evaluating "every command in the batch matches the Pub/Sub
  filter" (the §11 set), the wrapper treats a leading `MULTI`
  and trailing `EXEC` as transparent and checks only the
  user-issued commands in between. Without this, an all-`Publish`
  `TxPipeline` would carry MULTI/EXEC (not in the filter set),
  the all-match check would fail, and `redis.pipeline`
  telemetry would leak for what is conceptually a Pub/Sub-only
  transaction. With this carve-out, an all-`Publish` `TxPipeline`
  short-circuits cleanly (no span, no duration sample).

**Cluster / Ring** (`*redis.ClusterClient`, `*redis.Ring`): go-redis
v9.9.0 splits the caller batch by destination shard and invokes
`ProcessPipelineHook` **once per node group**, on each node's
per-node `*redis.Client` (which §10 hooked individually). A
caller-issued pipeline that crosses K shards therefore produces
**K sibling `redis.pipeline` spans** (each carrying the
node-specific `server.address`) and K duration samples; each
span's `db.operation.batch.size` (when present) is the subset
size routed to that shard (with the same MULTI/EXEC
compensation as the single-node case if the caller used
`TxPipeline`), not the original batch length. **When a shard's
subset contains only one user command** — common for a small
caller pipeline that happens to fan out one command per
shard — the span omits `db.operation.batch.size` entirely,
per the semconv v1.39 rule that batch.size MUST NOT be set
when the operation is not actually a batch. The span itself
still emits as `redis.pipeline` because the wire-level fact is
a pipeline invocation; only the size attribute is conditional.
Summing the per-node `batch.size` across the spans that have
it, plus counting the spans that omitted it (each representing
one user command), reconstructs the caller-visible total. This
is the honest description of what the hook contract surfaces in
v9.9.0: hooking the top-level `ClusterClient` to own a single
batch span is not supported because
`ClusterClient.processPipeline` performs the shard split before
any pipeline hook fires.

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

The wrapper accepts `redis.UniversalClient`. The three supported
concrete types are `*redis.Client` (which includes the Sentinel-
failover variant returned by `NewFailoverClient` — go-redis v9.9.0
does not expose a distinct `*redis.FailoverClient` type, only a
`*redis.Client` whose `Options().Addr == "FailoverClient"`),
`*redis.ClusterClient`, and `*redis.Ring`. The Sentinel-failover
sub-case is detected via `rdb.Options().Addr == "FailoverClient"`,
not via a type switch.

`*redis.SentinelClient` — the raw sentinel-monitor RPC client
returned by `NewSentinelClient` — does **not** implement
`redis.UniversalClient` in go-redis v9.9.0 (it lacks `Watch`,
`Pipeline`, etc.). It therefore cannot reach `Wrap` at all; a
call like `Wrap(redis.NewSentinelClient(...), tp, mp)` is a
compile-time type error, not a runtime one. No runtime rejection
path is needed.

The wrapper still uses a type switch on the supported concretes
to drive topology-specific behaviour (cluster/ring iterate
shards, single-node `*redis.Client` does not), and the type
switch's default case returns an error to defend against any
hypothetical future `redis.UniversalClient` implementation that
the wrapper does not know how to walk.

Cluster-specific concerns:

- **`server.address` per node.** `Wrap` registers the SDK-owned
  `redis.Hook` on `*redis.ClusterClient` / `*redis.Ring` through
  **both** an `OnNewNode` callback (covers future nodes created
  after `Wrap` returns) **and** an immediate iteration over the
  already-materialised shards. The iteration API differs by
  topology:
  - **Cluster** uses `ForEachMaster` followed by `ForEachSlave`;
    both walk the cluster's current topology view.
  - **Ring** uses `GetShardClients()`, **not** `ForEachShard`.
    `Ring.ForEachShard` in go-redis v9.9.0 silently skips any
    shard whose `shard.IsDown()` reports true — but the
    underlying per-shard `*redis.Client` is already materialised
    inside the Ring and continues to live there. If the shard
    later recovers (`IsDown` flips back), no `OnNewNode`
    callback fires because the shard client was never re-created,
    leaving that shard's commands forever un-hooked. The
    `GetShardClients()` getter returns every shard regardless of
    health, so each can be hooked + metric-instrumented during
    the initial pass.

  `AddHook` is then called on each per-node `*redis.Client`
  yielded by the chosen iteration API. The dual approach
  is required because in go-redis v9.9.0 `OnNewNode` only appends
  the callback to a slice that runs from the cluster pool's
  `GetOrCreate`, so a warmed `ClusterClient` passed to `Wrap`
  would otherwise execute commands on existing nodes without the
  hook, silently losing spans and operation metrics until the
  next topology refresh. Single-node `*redis.Client` uses
  `AddHook` directly. All three code paths read `server.address`
  / `server.port` from the per-connection options at span-start
  time.

  **Idempotency without inspecting hook chains.** go-redis v9.9.0's
  `AddHook` only appends to the unexported `hooksMixin.slice` and
  exposes no public accessor for already-registered hooks, so the
  wrapper cannot ask "is my marker already in this client's chain?"
  Idempotency is therefore enforced at the wrapper layer using a
  package-private `sync.Map` (Go's standard `sync.Map` is the
  non-generic `type Map struct` even in Go 1.25 — the ADR's
  bracketed notation below should be read as "logically
  `map[uintptr]*entry`, accessed via type-asserted `Load` /
  `LoadOrStore` / `Store` / `CompareAndDelete` calls on the
  untyped `sync.Map`"; an implementation may wrap it in a small
  typed helper like `type entryMap struct{ m sync.Map }` with
  typed methods to keep call sites tidy). The map is keyed by
  the concrete pointer of the client (extracted via a
  `switch v := rdb.(type)` over the three supported concretes
  `*redis.Client`, `*redis.ClusterClient`, `*redis.Ring`, then
  `uintptr(unsafe.Pointer(v))`). Each branch of the type switch
  **also tests `v == nil`** and returns `ErrNilClient` from the
  strict pre-commit class before any key derivation runs —
  catching the typed-nil case (`var c *redis.Client = nil;
  Wrap(c, ...)`) which slips past the earlier
  `rdb redis.UniversalClient == nil` check because the
  interface value is non-nil even though its dynamic value is.
  Without this check the wrapper would derive a zero `uintptr`
  key and later panic on `v.Options()` / `v.AddHook(...)`.
  Keying by `uintptr` rather than by the interface value keeps
  the map from retaining a Go reference to the client — see the
  lifetime discussion below. But `uintptr` alone is not a safe
  long-term identity, because Go's allocator can reuse the
  underlying memory once the original client is unreachable and
  before the `runtime.AddCleanup` callback has had a chance to
  delete the entry. A new short-lived client allocated at the
  same address would `LoadOrStore` the **stale** entry with
  `done = true` and skip instrumentation entirely. Generation
  counters (which were proposed earlier in this section) only
  prevent an outdated cleanup from deleting a newer entry —
  they do not stop the newer entry from inheriting the old one.

  To rule this out, each entry also carries a
  `weak.Pointer[T]` (Go 1.24's `weak` package) pointing at the
  underlying concrete client. On every `Wrap` call, after
  `LoadOrStore` returns the entry, the loop checks
  `entry.weakClient.Value() == currentConcretePointer`. A
  mismatch (or a `nil` weak `Value`, meaning the original
  client has been GC'd but cleanup has not yet run) means the
  entry is stale: the wrapper `CompareAndDelete`s it and
  restarts the `LoadOrStore`. This handles the address-reuse
  window deterministically — the stale entry is always
  detected and replaced on the next `Wrap` call, regardless of
  whether GC cleanup has run yet.

  Each `entry` therefore holds a `sync.Mutex`, a `done bool`,
  and a `weak.Pointer[T]` for the concrete client type. The
  flow on every `Wrap` call:

  The flow is a small retry loop, not a straight sequence, because
  a pre-commit failure removes the entry from the map and any
  goroutine that was blocked on that entry's mutex must be
  redirected through a fresh `LoadOrStore` rather than continue
  on the orphaned entry.

  ```go
  for {
      e, _ := m.LoadOrStore(key, newEntry(currentClient))
      e.mu.Lock()
      // (a) Canonicity: a pre-commit failure on a sibling
      //     goroutine may have CompareAndDeleted while we were
      //     blocked, leaving us with an orphan entry.
      current, ok := m.Load(key)
      if !ok || current != e {
          e.mu.Unlock()
          continue            // restart at LoadOrStore
      }
      // (b) Identity: the entry might be a stale survivor from
      //     a previous client whose memory was reused before
      //     AddCleanup ran.
      if got := e.weakClient.Value(); got == nil || got != currentClient {
          m.CompareAndDelete(key, e)
          e.mu.Unlock()
          continue            // restart at LoadOrStore with fresh entry
      }
      if e.done {
          e.mu.Unlock()
          return rdb, nil     // steady-state idempotent hit
      }
      err := doCommitSequence(rdb, e)   // §10 / §7
      switch classify(err) {
      case errStrictPreCommit:
          // nil rdb / nil tp / nil mp / unsupported type /
          // wrapper-owned instrument creation. Nothing was
          // mutated: remove the placeholder so a retry can
          // re-enter cleanly.
          m.CompareAndDelete(key, e)
          e.mu.Unlock()
          return rdb, err
      case errBestEffort:
          // Warmed-Cluster/Ring shard-collection failure.
          // OnNewNode is already installed and may have hooked
          // some nodes during the failed iteration; v9.9.0 has
          // no public hook-remove API. Commit the dedup entry
          // so retries are silent no-ops and don't double-hook
          // the already-touched nodes. Release the race-window
          // map so post-return OnNewNode firings take the
          // steady-state, no-retention branch.
          seenNodesPtr.Store(nil)
          e.done = true
          runtime.AddCleanup(clientObj, deleteEntry, identity)
          e.mu.Unlock()
          return rdb, err
      case nil:
          // Step 7's seenNodesPtr.Store(nil) ran during the commit
          // sequence; here we just finalise the entry.
          e.done = true
          runtime.AddCleanup(clientObj, deleteEntry, identity)
          e.mu.Unlock()
          return rdb, nil
      }
  }
  ```

  Step-by-step:

  1. `LoadOrStore` an `*entry` for the client. The freshly-
     constructed entry has `weakClient = weak.Make(currentClient)`
     so any future reader can verify it is for this specific
     client allocation.
  2. Lock the entry's mutex. This serialises concurrent `Wrap`
     calls on the same client (a second goroutine blocks until
     the first either commits or fails).
  3. **Canonicity check.** After acquiring the lock, re-`Load`
     the key. If the value is missing or not the same `*entry`
     pointer, the entry is **orphaned** — a prior pre-commit
     failure on another goroutine ran `CompareAndDelete` after
     we did `LoadOrStore` but before we got the lock. We unlock
     and restart the loop. Without this check, a waiter could
     successfully commit on an entry no longer in the dedup
     map, and a later `Wrap` call would insert a new placeholder
     and double-register hooks/metrics despite the idempotency
     contract.
  4. **Identity check.** Compare `entry.weakClient.Value()`
     against the current client pointer. If `nil` (original
     client GC'd, cleanup not yet run) or mismatched (memory
     reused for a new client allocation), the entry is stale —
     `CompareAndDelete` it and restart the loop. The next
     iteration's `LoadOrStore` will install a fresh entry whose
     `weakClient` points at the current client. Without this
     check, a new short-lived client allocated at a recycled
     address would inherit the previous client's `done = true`
     entry and skip instrumentation entirely.
  5. Under the lock and on the canonical, identity-matched
     entry, check `done`. If true, release and return
     `(rdb, nil)` — steady-state hit.
  6. If false, run the §10 / §7 commit sequence (instrument
     creation including the pool-metric observable registration
     → `OnNewNode` install → shard-collection pass with per-node
     `AddHook` → release `seenNodesPtr`). Classify the result:
       - **Success** → set `done = true`, register
         `runtime.AddCleanup`, unlock, return `(rdb, nil)`.
       - **Strict pre-commit error** (nil rdb, unsupported type,
         wrapper-owned instrument creation): nothing has been
         mutated. **`CompareAndDelete` the entry** using the
         `*entry` pointer as the witness so the placeholder is
         removed iff nothing else replaced it; unlock; return
         `(rdb, err)`. A retry can then re-enter cleanly.
       - **Best-effort error** (warmed-Cluster/Ring
         shard-collection failure): `OnNewNode` is already
         installed and may have hooked some nodes during the
         failing iteration, and v9.9.0 has no public hook-remove
         API. Commit the dedup entry anyway:
         `seenNodesPtr.Store(nil)` so any post-return
         `OnNewNode` firing takes the steady-state no-retention
         branch (without this step the callback would keep
         capturing every refreshed node in the map and leak),
         then set `done = true`, register `runtime.AddCleanup`,
         unlock, return `(rdb, err)`. A retry on the same client
         therefore short-circuits instead of double-hooking the
         already-touched nodes, and steady-state node creations
         do not leak. Pool metrics still emit correctly because
         the §7 group B observable was registered before
         `OnNewNode` and scrapes the live shard set regardless
         of the collection-pass outcome.

  The canonicity check in step 3 makes concurrent waiters safe
  against the strict-pre-commit `CompareAndDelete` path: any
  blocked-on-this-entry goroutine observes the deletion on its
  next lock acquisition and restarts the loop. Concurrent
  waiters on a best-effort error path simply observe
  `done = true` after acquiring the lock and return `(rdb, nil)`
  via the steady-state hit in step 5 — they correctly inherit
  the partially-instrumented state without re-running the work.

  Marking the client as wrapped only at a commit point (success
  or best-effort error in step 6) is essential: storing the
  marker on entry (step 1) would leave a failed-but-marked
  client that future retries would silently no-op past even on
  the strict-pre-commit class where retry should work, and a
  concurrent second `Wrap` would also see the marker and report
  success before any hook is installed.

  The cluster-pool-refresh case where `ForEachShard` may re-present
  the same per-node `*redis.Client` on a later `Wrap` call is
  handled by the same gate: the second `Wrap` finds `done = true`
  and short-circuits before any per-shard work runs. The
  `sync.Map` is local state of the redis instrumentation package —
  it does **not** violate ADR 0008's no-OTel-globals policy (which
  targets `otel.SetTracerProvider` / `otel.SetMeterProvider` and
  the global propagator).

  **The dedup map does not keep clients alive.** A naive
  `sync.Map` keyed by `redis.UniversalClient` (logically
  `map[redis.UniversalClient]*entry`) would hold the interface
  value (and therefore the underlying `*redis.Client` /
  `*redis.ClusterClient` / `*redis.Ring` pointer) for the lifetime
  of the process, blocking GC of any client the caller has dropped
  — turning a small idempotency aid into unbounded global state,
  which would be especially painful in tests, multi-tenant
  processes, and services that create short-lived clients. To
  avoid that, the wrapper keys the map by the **concrete pointer
  as `uintptr`** extracted via type switch (one of three branches:
  `*redis.Client`, `*redis.ClusterClient`, `*redis.Ring`) and
  registers a `runtime.AddCleanup(client, deleteEntry, id)` on
  first successful commit. The cleanup function deletes the
  dedup-map entry **and** calls
  `entry.poolMetricRegistration.Unregister()` on the §7 group B
  observable so the MeterProvider drops it. Running the
  unregister from `runtime.AddCleanup`'s goroutine (rather than
  from inside the callback when the weak pointer is observed
  nil) is essential: the callback runs under
  `sdk/metric.pipeline.produce`'s pipeline lock, and
  `Registration.Unregister()` re-takes the same lock —
  unregistering from inside would deadlock. The cleanup
  goroutine is not under the lock, so it is safe. The map
  therefore stores no reference that could keep the client live;
  when the caller drops the client and it becomes unreachable,
  `runtime.AddCleanup` fires and the entry plus the observable
  are removed.

  The address-reuse window — a new client allocated at the same
  address before the previous client's cleanup has run — is
  handled by the **weak-pointer identity check** in the retry
  loop above, not by a generation counter. The check
  (`entry.weakClient.Value() == currentClient`) on every `Wrap`
  call detects the mismatch before `LoadOrStore` can inherit the
  stale `done = true` entry, `CompareAndDelete`s the stale
  entry, and restarts the loop with a fresh placeholder. The
  `runtime.AddCleanup` callback itself uses
  `m.CompareAndDelete(key, *entry)` with the original `*entry`
  pointer as the witness, so an outdated cleanup cannot wrongly
  evict a fresh entry installed for a reused address. Earlier
  drafts of this section proposed a `generation uint64` field
  for the same purpose, but a generation counter only protects
  the cleanup-side deletion — it does not stop a new client
  from inheriting the stale entry on its own `LoadOrStore`. The
  weak-pointer check supersedes it.

  An explicit `Unwrap(rdb redis.UniversalClient)` helper is
  exported for tests that need deterministic eviction without
  waiting for GC (e.g. table-driven tests over many cluster
  fixtures). `Unwrap` must remain safe under a subsequent
  `Wrap(rdb)` on the same live Cluster/Ring client, because
  `OnNewNode` in v9.9.0 only appends callbacks and exposes no
  removal API — a naive `Unwrap` that just dropped the dedup
  entry would leave the previous callback in place, and the
  next `Wrap` would append a **second** callback, so any future
  node would be hooked and metric-instrumented twice.

  The wrapper closes this gap with a per-`Wrap`-call
  `disabled atomic.Bool` captured by both the tracing hook and
  the `OnNewNode` closure for that call. The dedup entry holds
  a reference to the flag. `Unwrap` performs three steps under
  the entry's mutex: set `disabled = true` (the still-attached
  hooks become no-ops, the still-registered `OnNewNode`
  callback returns immediately without touching new nodes),
  `CompareAndDelete` the dedup entry, and unlock. A subsequent
  `Wrap(rdb)` creates a fresh entry with its own fresh
  `disabled` flag and registers a new callback; the old
  callback is still in the cluster's callback slice but
  observes `disabled = true` on every invocation, so no
  double-installation occurs. Existing per-node tracing hooks
  attached during the first `Wrap` remain attached but emit no
  spans (their wrapper sees `disabled = true`); the second
  `Wrap` re-hooks each node via the same `seenNodes` flow as
  normal, and those new (non-disabled) hooks emit spans
  correctly.

  **Unwrap cleanly handles the wrapper-owned pool observable.**
  Each `Wrap` call's async pool-metric callback is returned
  from `RegisterCallback` as a `Registration`. The wrapper
  stores that `Registration` on the dedup entry; `Unwrap`
  calls `Registration.Unregister()` before deleting the
  entry, so a subsequent `Wrap` registers a fresh callback
  and there is no double-emission of pool metrics. This
  cleanly resolves the limitation earlier drafts of this
  section called out for redisotel-based metrics (which had
  no removal API).

  **Ordering and the unmodified-on-error contract.** Naively, one
  would gather shards first and install `OnNewNode` second, but
  that opens a race window for warmed clusters already serving
  traffic: a topology refresh or MOVED/ASK redirect during the
  gap can materialise a node that is neither in the gathered slice
  (created after `ForEachShard` returned) nor caught by
  `OnNewNode` (created before it was installed), so the node
  stays unhooked indefinitely. The wrapper therefore installs the
  callback **before** iterating shards and uses a per-call
  `seenNodes` set keyed by the per-node `*redis.Client`'s
  `uintptr` to deduplicate. Steps:

  1. Construct the wrapper's own metric instruments (the
     `db.client.operation.duration` histogram from §5 group A
     **and** the pool-metric instrument set from §7 group B,
     including registering the async observable callback against
     the supplied MeterProvider) and the hook value in local
     variables — no mutation of `rdb` yet. Any failure here is
     strict pre-commit per the failure-path discussion below.
  2. Allocate a `seenNodes` `sync.Map` (logically keyed by
     `uintptr` to `*nodeState` — same untyped-`sync.Map` caveat
     as the idempotency map above), and wrap it behind a
     `seenNodesPtr atomic.Pointer[sync.Map]` so the map can be
     dropped for GC once steady-state begins (see step 6
     below — the map's purpose is purely to dedup the in-flight
     race window, and retaining strong `*redis.Client`
     references in it for the rest of the process would leak
     every node go-redis ever materialised after a topology
     refresh). The pointer is initialised to the fresh map and
     captured by both the upcoming `OnNewNode` closure and the
     `ForEachShard` loop, where the node-state struct is:

     ```go
     type nodeState struct {
         client        *redis.Client
         hookInstalled atomic.Bool   // CAS gate for AddHook
     }
     ```

     The wrapper no longer drives `redisotel.InstrumentMetrics`
     per-node — pool metrics are emitted by a single
     wrapper-owned observable callback that iterates the live
     shard set at scrape time (§7 group B). The race-window
     dedup therefore only needs to cover hook installation;
     there is no metrics-side CAS or error field on
     `nodeState`.

     Both code paths `LoadOrStore` a `*nodeState` keyed by the
     node's `uintptr`. The `hookInstalled` CAS gate makes hook
     installation single-shot per node regardless of which code
     path (callback vs iterator) reaches it first. Storing in
     the map rather than a slice also handles the v9.9.0 fact
     that `ForEachShard` invokes its callback **concurrently**
     per shard — `sync.Map` writes from N goroutines are safe.
  3. Install the wrapper's own
     `OnNewNode(newNode *redis.Client)` callback. It only
     installs the tracing hook — pool metrics come from a single
     wrapper-owned observable that scrapes the live shard set
     (§7 group B), not from per-node registrations. The closure
     branches on whether the in-flight race window is still
     open (`seenNodesPtr` non-nil):

     ```go
     onNewNode := func(newNode *redis.Client) {
         if disabled.Load() { return }     // Unwrap gate, §10
         if m := seenNodesPtr.Load(); m != nil {
             // Setup phase: dedup against ForEachShard pass.
             ns := &nodeState{client: newNode}
             actual, _ := m.LoadOrStore(addr(newNode), ns)
             state := actual.(*nodeState)
             if state.hookInstalled.CompareAndSwap(false, true) {
                 AddHook(newNode, traceHook)
             }
             return
         }
         // Steady state (post-Wrap-return): no dedup needed —
         // go-redis fires OnNewNode exactly once per node-pool
         // creation, and there is no concurrent ForEachShard
         // pass to race with. Install hook directly without
         // retaining a reference.
         AddHook(newNode, traceHook)
     }
     ```

     The callback is now in place to catch any node created
     from this moment on. There is no shared-`conf` bug to
     sidestep because the wrapper does not call
     `redisotel.InstrumentMetrics` at all.
  4. Run the shard-collection pass (Cluster:
     `ForEachMaster` + `ForEachSlave`; Ring: `GetShardClients`).
     The per-shard callback executes the same CAS dance on
     `hookInstalled` (so a node race-caught by step 3's
     callback during the iteration window is not re-hooked).
     Iteration errors fail `Wrap` here on the best-effort path
     described below — by this point step 3 may have already
     installed hooks on nodes created during the iteration
     window, and there is no public API to remove them.
  5. Once the shard pass returns cleanly, the **tracing commit
     point** has been reached: every node that exists now is
     hooked, every node created from this moment on will be
     hooked. `done = true` is set after step 6. The pool-metric
     observable registered back in step 1 (§7 group B) has been
     emitting since the next MeterProvider scrape cycle began —
     no separate metrics commit point exists.
  6. **Release the race-window map.** `seenNodesPtr.Store(nil)`.
     From this point on the `OnNewNode` closure takes the
     steady-state branch shown in step 3, which does not touch
     `seenNodes` at all. The previously-allocated `sync.Map`
     becomes unreachable and is GC-eligible, releasing every
     `*redis.Client` reference it accumulated during the race
     window. Without this swap, the closure would hold a strong
     reference to every node go-redis ever materialised after a
     topology refresh, leaking pool memory until the whole
     Cluster/Ring client is collected. After this step, set
     `done = true` and register `runtime.AddCleanup` per the
     idempotency loop's commit path.

  The race-closing property: because `OnNewNode` is registered
  **before** the shard-collection pass runs, any node materialised
  during iteration is guaranteed to fire the callback. The
  `seenNodes` dedup + `hookInstalled` CAS gate make the order of
  "callback fires" vs "iterator visits" on the same node
  irrelevant — whichever wins the CAS installs the hook, the
  loser skips. There is no remaining window where a new node
  can sneak in unhooked, and no node is hooked more than once.
  Pool metrics are independent of this race because the §7
  group B observable scrapes the live shard set at observation
  time, not from the wrapper's `seenNodes`.

  **No post-return metric-error sink needed.** With the
  wrapper owning the pool-metric observable (§7 group B) and
  per-node `redisotel.InstrumentMetrics` removed entirely, there
  is no longer a class of "post-return per-node metric failure"
  to surface. The observable runs against a single set of
  instruments registered once during `Wrap`; if those
  registrations succeed at `Wrap` time, every subsequent
  observation scrapes the live shard set with no further
  failable registrations. Earlier drafts of this ADR carried a
  `slog`-based error sink for post-`Wrap` `InstrumentMetrics`
  failures — that path no longer exists.

  **Failure paths.** The wrapper has two classes:

  - **Strict pre-commit failure** (nil `rdb`, nil `tp`, nil
    `mp`, unsupported type, wrapper-owned instrument creation
    — which now includes both the
    `db.client.operation.duration` histogram from §5 group A
    and the pool-metric instrument set from §7 group B): nothing
    has been mutated. The wrapper returns the original client,
    runs the `CompareAndDelete` placeholder cleanup from the
    idempotency section, and `done = false`. Retry is safe.
    Provider-nil checks run **first** in step 1 — before any
    allocation, lock acquisition, or `LoadOrStore` — so a
    nil-provider call doesn't even create a dedup-map entry to
    clean up. Instrument creation runs entirely before step 3
    (`OnNewNode` install), so a failure here also leaves the
    callback uninstalled — no partial state.

  - **Warmed Cluster/Ring shard-collection failure** (step 4
    iteration error): the awkward case created by closing the
    race window. Because `OnNewNode` was already installed in
    step 3, any node materialised during the failed iteration
    may already have a tracing hook attached via the callback.
    The wrapper cannot remove these — go-redis v9.9.0 exposes
    no public hook-remove API. The wrapper treats this as a
    **best-effort failure**: report the error via the return
    value, leave installed hooks in place, run step 6
    (`seenNodesPtr.Store(nil)`) to release the race-window map
    and switch the callback to its steady-state branch, set
    `done = true`, and register `runtime.AddCleanup` so retries
    are silent no-ops rather than risk double-hooking the nodes
    the callback already touched. Pool metrics still work
    correctly via the §7 group B observable, which scrapes
    whatever shards the topology yields at observation time —
    so the post-`Wrap` state is: tracing partially installed
    (every node the callback fired on during the window is
    covered; nodes the failing iteration didn't reach are not),
    pool metrics fully operational, non-nil error return.

  Callers who insist on all-or-nothing instrumentation should
  treat any non-nil `Wrap` error as terminal for the process;
  callers who can tolerate degraded telemetry can log and
  continue. The §3 doc-comment enumerates which error class
  each return belongs to so startup readiness can decide
  per-class.
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
  `*redis.SentinelClient` returned by `NewSentinelClient`) are
  not instrumented at all — that type does not implement
  `redis.UniversalClient` in v9.9.0, so a call to `Wrap` with it
  is a compile-time error, not a runtime one (see §10).
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

## Amendment (2026-06-23) — command-noise filtering

Operational experience showed that several command classes that
travel through `ProcessHook` are pure noise in a `db.*` trace: they
are not application units of work, yet they produce spans and inflate
the `db.client.operation.duration` series. This amendment extends the
§11 Pub/Sub short-circuit into a small, layered filtering model. The
split follows a single principle: **the SDK filters what is
*structurally* never application work; it makes *preference-based*
suppression opt-in.**

### A. Always filtered — connection-lifecycle commands

In addition to the §11 Pub/Sub set, the hook now unconditionally
short-circuits the connection-setup commands the go-redis client
issues itself on connect — never the application as a unit of work:

- `AUTH`, `HELLO`, `SELECT`, `COMMAND` (matched by verb);
- `CLIENT SETINFO` / `CLIENT SETNAME` (matched by subcommand —
  go-redis v9 auto-issues `CLIENT SETINFO lib-name/lib-ver` on every
  new connection, and `CLIENT SETNAME` when `ClientName` is set).
  Deliberate `CLIENT` subcommands (`LIST`, `KILL`, …) stay
  instrumented.

These are treated exactly like Pub/Sub: no span, no duration sample,
`next` invoked directly, in both `ProcessHook` and (all-match)
`ProcessPipelineHook`. Filtering `AUTH` also removes the risk of
credentials reaching `db.query.text` when `WithCommandTextEnabled` is
on. Rationale for hardcoding rather than making this configurable: a
`db.*` span for a handshake misrepresents application activity in
every deployment, so there is no legitimate "keep it" choice to expose
— the same reasoning §11 applies to Pub/Sub.

### B. Opt-in suppression — `WithIgnoredCommands`

Commands that *are* legitimate `db.*` operations but are frequently
noise — health-check / keepalive `PING`, periodic `INFO` polls,
`ECHO`, `CLUSTER *` topology probes — are **emitted by default** and
suppressed only when the caller lists them:

```go
func WithIgnoredCommands(names ...string) Option
```

Names match case-insensitively against `cmd.Name()` (the verb), so one
entry covers all invocations regardless of arguments. A listed command
is filtered identically to category A (no span, no sample; all-match
short-circuit in pipelines). These are *not* hardcoded because
reasonable deployments disagree: `PING` latency is a clean server-RTT
signal some teams want to keep, so the policy belongs to the
application while the SDK supplies the mechanism (which the application
cannot implement itself — a downstream go-redis hook cannot suppress a
span this package's hook already owns).

**Known limitation.** Name matching cannot distinguish a background
health-check `PING` from an application-issued `PING`;
`WithIgnoredCommands("ping")` drops both. For probe-only suppression,
use category C or a dedicated unwrapped client for the probe.

### C. Opt-in scoping — `WithRequireParentSpan`

```go
func WithRequireParentSpan(enabled bool) Option
```

When enabled, a command (or pipeline) whose context carries no valid
span context produces neither a span nor a duration sample. This
targets background noise that name matching cannot isolate:
health-check probes, pool keepalive, and topology refreshes typically
run on a fresh context, while genuine application commands run within a
server/consumer span and are kept. Off by default because legitimate
unparented background work (scheduled jobs, warmup) would also be
dropped — so this is a deliberate, caller-made trade-off.

### Out-of-SDK layer (documentation only)

Cross-service, fleet-wide suppression remains a Collector concern: an
OTTL `filter` processor (e.g. drop spans where
`attributes["db.operation.name"] == "PING"`) is the vendor-neutral way
to enforce policy centrally without redeploying applications. The SDK
options above cover the in-process case the Collector cannot (dropping
before serialization/export cost) and the per-client case
(`WithRequireParentSpan`); the Collector covers org-wide defaults. The
two compose.

This amendment updates §3 (public API gains `WithIgnoredCommands` and
`WithRequireParentSpan`) and §11 (the Pub/Sub bullet is now one case of
the general "always filtered" category A).

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

**Wrapper discipline.** `Wrap` does not call either
`redisotel.InstrumentMetrics` or `redisotel.InstrumentTracing`
(see §7 group B and §2 for why), so neither
`redisotel.WithTracerProvider` nor `redisotel.WithMeterProvider`
fallback paths in redisotel can ever execute on this code path.
The wrapper interacts with the SDK only via its own
`tp.Tracer(...)` and `mp.Meter(...)` calls using the providers
passed to `Wrap`.

### Audit discipline for upstream bumps

On every `go-redis` / `redisotel` version change:

1. Re-grep the package for `otel.Set*` calls.
2. Re-check the semconv import path. If it advances past v1.24 to a
   stable v1.x ≥ 1.39, evaluate whether `redisotel.InstrumentTracing`
   becomes viable and whether the SDK-owned tracing hook can be
   retired in a future ADR amendment.
3. Re-check the set of metric instruments emitted by
   `redisotel.InstrumentMetrics` (even though we no longer call
   it). If new instruments appear, add corresponding entries to
   §7 group C's defensive drop list so a consumer that registers
   redisotel separately cannot leak v1.24-era names past the
   v1.39 contract.
4. Re-check `*redis.Client.PoolStats()`'s field set. If go-redis
   adds or removes pool counters, mirror the change in §7 group B
   (the wrapper-owned observable) and the corresponding §12
   exporter assertion.
5. Re-check the `redis.Hook` interface for additions or signature
   changes. The wrapper's hook implementation must implement the
   full current interface; CI runs the wrapper against the pinned
   go-redis version to catch this.
6. Update the ADR 0003 approved-integrations table with the new
   version.

---

## Testing

- Unit tests for the wrapper:
  - `Wrap` is idempotent (calling twice does not double-instrument
    and returns `(rdb, nil)` on the second call). For cluster/ring
    clients, idempotency is asserted by the dedup-map's
    `done` gate described in §10 (after the second `Wrap`, each
    per-node `*redis.Client` still has exactly one tracing hook
    in its chain).
  - On a warmed `*redis.ClusterClient` / `*redis.Ring` (one
    whose shard iteration already yields ≥1 node before `Wrap`
    is called), every existing shard's per-node `*redis.Client`
    receives the tracing hook before `Wrap` returns. A
    subsequent command routed to a pre-existing shard emits a
    span, and the in-memory metric exporter records at least
    one `db.client.connection.count` sample (with `state`
    attribute) for that shard's pool on the next observation
    cycle — sourced from the wrapper-owned §7 group B
    observable, not from any per-node redisotel registration. A
    regression where only `OnNewNode` is installed for the
    tracing path would fail this test on the pre-existing
    shard's spans.
  - **Down Ring shard is still instrumented (§10 regression).**
    Construct a `*redis.Ring` whose shard-down probe reports
    `IsDown() == true` for one shard at `Wrap` time, while the
    underlying per-shard `*redis.Client` is already
    materialised. `Wrap` succeeds; the down shard is then
    flipped to healthy via the test fixture (no `OnNewNode`
    fires because the shard client was never re-created), and
    a command routed to that shard must emit a span and a
    `db.client.operation.duration` sample. A regression that
    iterates Ring shards via `Ring.ForEachShard` instead of
    `GetShardClients()` would skip the down shard during the
    initial pass and fail this test.
  - **Per-shard pool-metric labelling (§7 group B regression
    test).** Integration test with a 3-shard cluster: after
    `Wrap`, force a topology refresh that materialises two new
    nodes back-to-back. On the next scrape cycle, assert each
    shard's `db.client.connection.count` series carries that
    shard's own `server.address` (and `server.port` when split),
    **not** any sibling's address — proving the wrapper-owned
    observable iterates the live shard set per-observation and
    builds the label set freshly from each shard's `Options`. A
    regression that re-introduced redisotel's per-node
    `InstrumentMetrics` (which closes over a shared `conf` in
    v9.9.0) would fail this assertion because every post-first
    node would inherit the first node's `pool.name`.
  - **`ForEachShard` / `OnNewNode` race window is closed.** Unit
    test using a fake `*redis.ClusterClient` (or a real one with
    a controllable topology) that injects a brand-new node
    creation **between** the `OnNewNode` install and the
    `ForEachShard` iteration of a `Wrap` call. Assert that the
    injected node ends up with exactly one tracing hook (not
    zero, not two): zero would mean the race left it unhooked,
    two would mean `seenNodes` dedup failed. A command routed to
    that node must emit exactly one span. The whole test runs
    under `go test -race`; concurrent writes to `seenNodes` from
    the per-shard callback goroutines (v9.9.0's `ForEachShard`
    fans out one goroutine per shard) must not trigger the race
    detector.
  - **Strict pre-commit failure cleans up the dedup placeholder.**
    Force a wrapper-owned instrument-creation error (e.g. a
    MeterProvider whose first instrument registration returns
    an error) on a fresh client; after `Wrap` returns the error,
    assert the package dedup map does not contain an entry for
    that client pointer (via the test affordance from the
    lifecycle test above). Without the `CompareAndDelete`
    cleanup, the placeholder would leak permanently. Note that
    `ForEachShard` mid-traversal failure is **not** tested here
    because §10 classifies it as best-effort once `OnNewNode` is
    installed (the entry stays committed with `done=true`); that
    behaviour is asserted by the warmed-Cluster carve-out test
    below.
  - **Concurrent `Wrap` with a failing first call (orphan-entry
    safety).** Two goroutines `Wrap` the same client. The first
    one is wired to fail pre-commit (e.g. instrument creation
    rigged to error); the second is wired to succeed. The
    failure path's `CompareAndDelete` removes the placeholder
    while the second goroutine is still blocked on the entry's
    mutex. Assert: (a) the second goroutine returns `(rdb, nil)`
    with the client fully instrumented; (b) the client has
    exactly **one** tracing hook per node (not zero, not two);
    (c) the dedup map contains exactly one entry for the client
    at the end, owned by the second goroutine's fresh
    `LoadOrStore`. Run under `go test -race`. A regression where
    the canonicity-check loop in §10 is removed (waiter commits
    on the orphaned entry, then a third `Wrap` call inserts a
    second placeholder and double-hooks) would fail this test.
  - `Wrap` returns a non-nil error for a nil interface client,
    for a typed-nil concrete client (e.g.
    `var c *redis.Client = nil; Wrap(c, tp, mp)` — the
    interface value is non-nil but the dynamic value is, so the
    untyped check passes; the per-concrete check in §10 catches
    it), for a nil `trace.TracerProvider`, for a nil
    `metric.MeterProvider`, and when wrapper-owned instrument
    creation fails — the strict unmodified-on-error paths per
    §10. The nil-provider cases are asserted explicitly: pass
    `Wrap(redis.NewClient(...), nil, mp)` and
    `Wrap(redis.NewClient(...), tp, nil)`, confirm each
    returns a non-nil error with no panic. The typed-nil case
    is asserted for each of the three supported concretes
    (`*redis.Client`, `*redis.ClusterClient`, `*redis.Ring`),
    confirming no panic in any branch and that the dedup map is
    unchanged for all of these (no placeholder created — strict
    checks short-circuit before `LoadOrStore`). The broader contract is asserted
    by issuing a command on the returned client after a
    forced failure and confirming the in-memory exporter
    records **no** spans and **no**
    `db.client.operation.duration` samples — no `OnNewNode`
    callback and no partial per-shard hook installation
    survives these error paths.
    (`ForEachShard` mid-traversal failures on warmed
    Cluster/Ring are best-effort per §10's carve-out, not
    strict; that path is covered by the warmed-Cluster
    `ForEachShard`-failure carve-out test below.
    `*redis.SentinelClient` is not tested here because it does
    not implement `redis.UniversalClient` in v9.9.0 and
    therefore fails to compile at the call site — there is no
    runtime rejection path to exercise.)
  - **Pool-metric observable emits v1.39 instruments (§7 group B).**
    `Wrap` a single-node `*redis.Client` with
    `WithPoolName("test")` against a MeterProvider with a
    periodic in-memory reader. Force at least one PoolStats
    observation cycle. Assert the exporter sees
    `db.client.connection.idle.{max,min}`,
    `db.client.connection.max`, `db.client.connection.count`
    (with `db.client.connection.state="used"|"idle"`),
    `db.client.connection.timeouts`,
    and `db.client.connection.create_time` (unit `s`) — each
    carrying `db.system.name="redis"`,
    `db.client.connection.pool.name="test"`, `server.address`,
    and (when `Options.Addr` splits) `server.port`. The legacy
    `db.system` attribute and any plural-form
    `db.client.connections.*` instruments must be absent. Repeat
    for `*redis.ClusterClient` with two shards using
    `WithPoolName("cluster")`, asserting each shard contributes
    its own labelled samples with
    `db.client.connection.pool.name="cluster/<shard-Addr>"`.
  - **Pool-name default + uniqueness (§7 group B).** Wrap two
    distinct single-node `*redis.Client`s pointing at the same
    `host:port` (a sidecar / dual-db scenario), neither call
    supplying `WithPoolName`. On the next observation cycle,
    assert that two distinct `db.client.connection.count` time
    series exist — one per `Wrap` — each carrying its own
    synthesised `db.client.connection.pool.name = "redis-<hex>"`
    derived from the concrete pointer. A regression that
    omitted pool-name entirely would collapse the two pools
    into a single colliding series and violate the OTel
    async-callback uniqueness contract.
  - **Pool observable does not unregister from inside the
    callback (§7 group B).** Wrap a short-lived `*redis.Client`,
    drop the reference, and `runtime.GC()` to make the weak
    pointer go nil. Trigger a `ManualReader.Collect(ctx)` on the
    MeterProvider. Assert (a) the collect call completes within
    a short deadline (e.g. 200ms) — not deadlocked; (b) the
    observation cycle records nothing for the GC'd client; (c)
    the `runtime.AddCleanup` hook subsequently fires and the
    eventual exporter snapshot no longer lists the client's
    callback registration. A regression that called
    `Registration.Unregister()` from inside the callback would
    block on the pipeline lock and time the test out.
  - **Strict pre-commit retry-after-failure.** Force a strict
    pre-commit failure (wrapper-owned instrument-creation error
    on a non-cluster client, where `ForEachShard` isn't called).
    A second `Wrap` with a healthy MeterProvider must succeed,
    return `(rdb, nil)`, and subsequent commands must emit spans.
    This proves the strict failure path correctly removed its
    placeholder so retry actually performs the work. (Retry for
    the best-effort `ForEachShard`/metrics-phase classes is
    asserted to be a no-op by the warmed-cluster carve-out test
    above, not by this one.)
  - **Concurrent `Wrap`.** Launching N goroutines that call `Wrap`
    on the same warmed `*redis.ClusterClient` simultaneously
    results in exactly one successful commit (the per-entry
    mutex serialises the racers) and zero double-hooked nodes:
    after the dust settles, a single command emits exactly one
    span, not N.
  - **`seenNodes` map is released after Wrap returns.**
    Integration test: `Wrap` a `*redis.ClusterClient`, then
    cycle the topology so `OnNewNode` fires for K new nodes
    after `Wrap` returned. Assert (via an internal test
    affordance that exposes the wrapper's `seenNodesPtr.Load()`)
    that the map pointer is `nil` after step 7 and that the K
    new node clients become unreachable once go-redis drops
    them from the active topology (verified with a
    `runtime.SetFinalizer` reachability probe on one of the K).
    A regression that kept `seenNodesPtr` populated, or that
    failed to take the steady-state branch in the callback,
    would keep at least K `*redis.Client` references reachable
    and fail this test.
  - **`Unwrap` + re-`Wrap` does not double-emit spans or pool
    metrics.** On a warmed cluster: `Wrap`, run a `GET` (one
    span recorded), `Unwrap`, `Wrap` again with the same
    MeterProvider, run another `GET`. Assert exactly **one new
    span** appears in the exporter — not two — proving the
    disabled-flag gate on the old hook closure no-ops
    correctly. On the next pool-metric observation cycle,
    assert exactly **one** `db.client.connection.count` time
    series per shard — not two — proving §10's
    `Registration.Unregister()` step in `Unwrap` correctly
    detached the prior observable before the second `Wrap`
    registered its own.
  - **Dedup map does not pin clients.** Create K short-lived
    `*redis.Client` instances, `Wrap` each, then drop all
    references. After `runtime.GC` + a brief settle (or after an
    explicit `Unwrap` per client), the package's dedup map size
    drops back to 0 (asserted via an exported `testHookMapLen`
    or equivalent test affordance). A regression that re-stored
    the interface value as the map key — or that omitted the
    `runtime.AddCleanup` registration — would leave the map at
    K and fail this assertion.
  - **Address-reuse identity check.** A unit test using
    `runtime.SetFinalizer` to assert reachability windows
    constructs a `*redis.Client` C1, `Wrap`s it, drops the
    reference, allocates a new `*redis.Client` C2 at the same
    address (via repeatedly allocating until size-class match,
    or via an unsafe test-only helper). `Wrap(C2)` is called
    **before** C1's `runtime.AddCleanup` has been allowed to
    run. Assert that C2's `Wrap` does **not** return early via
    the stale entry's `done = true`, that C2 ends up fully
    instrumented, and that a `GET` on C2 emits a span. A
    regression that drops the `weak.Pointer` identity check
    would treat C2 as already-wrapped and emit zero spans.
  - **No double-hook in race window.** Construct a fake
    `*redis.ClusterClient` whose shard-collection pass yields
    one existing node and, mid-iteration, injects a new node
    via `OnNewNode`. After `Wrap` returns, assert the new node
    has exactly **one** tracing hook — not two. A regression
    that removes the `hookInstalled` CAS gate would emit two
    duplicate spans for any command routed to the raced node.
    Pool metrics are not part of this test because they come
    from the wrapper-owned observable (§7 group B) which
    iterates the live shard set at scrape time, not from per-
    node registrations — no double-registration is possible
    from this code path.
  - **Warmed-Cluster shard-collection-failure carve-out.**
    Force the shard-collection pass (Cluster: `ForEachMaster`;
    Ring: `GetShardClients` fixture) to error after the
    wrapper's `OnNewNode` has already hooked one freshly-
    materialised node during the failing iteration. Assert:
    (a) `Wrap` returns a non-nil error of class
    "warmed-cluster best-effort"; (b) the pre-touched node
    still has its tracing hook (visible via a subsequent `GET`
    emitting a span); (c) the dedup map entry has
    `done = true` and `runtime.AddCleanup` is registered, so a
    second `Wrap` call is a silent no-op even with a healthy
    iteration; (d) `seenNodesPtr.Load() == nil` after the error
    return — proving the best-effort failure path released the
    race-window map and post-return `OnNewNode` firings will
    take the steady-state branch; (e) the pool-metric
    observable emits non-zero samples on the next scrape (the
    §7 group B path is independent of the failed collection
    pass). A regression that forgets to `Store(nil)` on the
    best-effort path would fail (d); a regression that ties
    pool metrics to the collection pass would fail (e).
  - The hook short-circuits Pub/Sub commands (§11): asserting that
    `PUBLISH`, `SPUBLISH`, `SUBSCRIBE`, `PSUBSCRIBE`, `PUBSUB`
    produce no span and no `db.client.operation.duration` sample.
  - **Pipeline Pub/Sub filtering (§8, single-node case).** Against
    a single-node `*redis.Client`, a pipeline of only
    `pipe.Publish(...)` calls produces no `redis.pipeline` span
    and no duration sample. A mixed pipeline (e.g. one `Publish`
    plus two `Set` calls) records exactly one `redis.pipeline`
    span with `db.operation.batch.size=3` (full length). Cluster
    pipeline semantics are covered separately by the integration
    test below — the §8 contract there is per-node-group, not
    single-batch.
  - **`TxPipeline` MULTI/EXEC handling (§8).** Three sub-cases
    against a single-node `*redis.Client`:
    (a) `TxPipeline` with three user `Set` calls records exactly
        one `redis.pipeline` span whose `db.operation.batch.size`
        is `3`, not `5` — proving the MULTI/EXEC framing is
        subtracted from the reported batch size.
    (b) `TxPipeline` whose user commands are all `Publish` (so
        the wire is `MULTI, PUBLISH, PUBLISH, EXEC`) records
        **no** span and no duration sample — proving the all-
        match Pub/Sub short-circuit ignores the framing commands.
    (c) `TxPipeline` mixing `Publish` and `Set` records exactly
        one `redis.pipeline` span with `db.operation.batch.size`
        equal to the user-issued count, mirroring the mixed-
        pipeline case above.
    A regression that fails to special-case `TxPipeline` would
    fail (a) by reporting `batch.size=5` and fail (b) by
    emitting a span the ADR says should be absent.
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
    `db.system.name`, `db.operation.name`, `server.address`
    (single-node fixture also asserts `server.port`; Sentinel
    fixture asserts `server.port` is **absent** because
    `Options.Addr == "FailoverClient"` does not split) and
    absence of `db.connection_string`, `db.system`,
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
  - **Pool metrics are v1.39-named (§7 group C).** With the
    package's `MetricViews()` registered, the exporter sees the
    singular-form, second-based instruments
    (`db.client.connection.count` with
    `db.client.connection.state` attribute,
    `db.client.connection.timeouts`,
    `db.client.connection.idle.max` / `idle.min`,
    `db.client.connection.max`,
    `db.client.connection.create_time` in `s`) and **no**
    plural-form `db.client.connections.*` instrument appears in
    the exporter snapshot. A regression that fails to register
    the rename views would leave the redisotel v1.24 names
    visible and fail this assertion. The drop+re-emit of
    `create_time` is asserted by confirming exactly one
    `db.client.connection.create_time` histogram (unit `s`)
    appears and the redisotel `db.client.connections.create_time`
    (unit `ms`) does not.
  - Pipeline span (single-node): name is exactly `redis.pipeline`,
    `db.operation.batch.size` equals the caller-issued command
    count, exactly one such span per `Pipeline()` call, no
    per-command child spans appear.
  - Pipeline spans (cluster, integration test under
    `testcontainers-go`): a 6-command pipeline whose keys
    intentionally fan out across two shards (3+3) produces
    exactly two `redis.pipeline` sibling spans, each carrying
    its node's `server.address`, and the sum of their
    `db.operation.batch.size` (both present, both = 3) equals
    6. Asserts the §8 per-node-group contract.
  - **Cluster pipeline batch.size omitted for size-1 subsets
    (§8).** Same fixture, but with a 2-command pipeline whose
    keys land one on each of two shards. Assert exactly two
    `redis.pipeline` sibling spans, each carrying its node's
    `server.address` — and assert each span has **no**
    `db.operation.batch.size` attribute set, per the semconv
    v1.39 rule that batch.size MUST NOT be present when the
    per-hook user-command count is less than 2. A regression
    that always emitted `batch.size=1` would fail this test
    and produce non-compliant series.

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
- Pool-stats instruments are emitted by a wrapper-owned async
  observable that scrapes `*redis.Client.PoolStats()` on each
  shard, recording under v1.39 singular names with a dedicated
  pool-metric label set (`db.system.name`,
  `db.client.connection.pool.name`, `server.address`,
  `server.port`). `redisotel.InstrumentMetrics` is not used
  (its v1.24 names and `pool.name` label cannot be translated
  to v1.39 via OTel Go views; see §7 group B).

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
