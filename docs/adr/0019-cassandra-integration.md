# ADR 0019 — Cassandra Integration

**Status**: Accepted (implemented)
**Date**: 2026-06-04
**Relates to**: ADR 0003 (global state policy), ADR 0006 (semconv upgrade
strategy), ADR 0008 (instrumentation sourcing policy), ADR 0013 (Redis/Valkey —
reference for the T3 SDK-owned pattern), ADR 0014 (MongoDB metrics — reference
for the operation-duration metric).

---

## Context

The SDK already ships database integrations for MongoDB (`mongo/`, T2 facade)
and Redis/Valkey (`redis/`, T3 SDK-owned). The next target is Apache Cassandra.

The first consumer is the public `github.com/hmchangw/chat` project, whose
`go.mod` pins:

```text
github.com/gocql/gocql v1.7.0
```

Cassandra is load-bearing in that codebase — `history-service/internal/cassrepo`,
`room-service`, and `message-worker` all use `gocql` directly (UDTs, batches,
paging cursors, `Iter.StructScan`, `LocalQuorum` via `pkg/cassutil.Connect`).
Today those calls produce no Tempo spans and no `db.client.*` metrics, and the
service hand-rolls any correlation it needs.

### Driver landscape

| Driver | Module path | State |
|---|---|---|
| `gocql/gocql` | `github.com/gocql/gocql` | Donated to Apache; **low-maintenance mode**. The path the first consumer uses (`v1.7.0`). |
| Apache fork | `github.com/apache/cassandra-gocql-driver/v2` | Active; 2.0.0 renamed the module path (breaking). Future mainline. |

Both expose the **same observer extension points** (`QueryObserver`,
`BatchObserver`, `ConnectObserver`), so instrumentation written against one
ports to the other with an import-path change only.

### Instrumentation landscape (ADR 0008 §2 evaluation)

| Candidate | Result |
|---|---|
| `go.opentelemetry.io/contrib/.../gocql/otelgocql` | **Removed** from contrib in **v1.19.0** (abandoned-module cleanup). Its final release (v0.43.0, 2023-08) emitted pre-stable semconv — `db.name` / `db.cassandra.keyspace`, not `db.namespace` / `db.system.name`. |
| Apache v2 driver built-in OTel | **Does not exist.** The driver exposes only the observer interfaces; no native OTel. |
| Community fork / vendor APMs (uptrace, signoz, dd-trace) | No maintained standalone `otel-gocql` library; vendor docs point at the now-removed contrib package or manual instrumentation. |
| DataStax Go driver | Different driver; not used by the consumer. Out of scope. |

Evaluated against the ADR 0008 §2 checklist, **no candidate passes**:

- **§2.2 Maintenance signal — FAIL.** The only OTel-contrib package was deleted;
  there is no maintained replacement.
- **§2.3 Semconv alignment — FAIL.** The deleted package emitted pre-stable DB
  attributes incompatible with the SDK's v1.39.0 pin (ADR 0006).

This supersedes ADR 0008 §5's forward-looking row "Future: Cassandra → T2 over
`otelgocql`", which was written before the contrib module was removed.

---

## Decisions

### 1. Driver: `github.com/gocql/gocql`

The integration targets `gocql/gocql` as the primary supported driver, matching
the first consumer (`v1.7.0`). The Apache fork
(`apache/cassandra-gocql-driver/v2`) is **not** a v1 target because adopting it
would strand the consumer behind a breaking module-path migration.

Because the observer interfaces are identical across the two drivers, the
package is structured so the only driver-specific surface is the import and the
constructor seam. A future "support Apache v2" change is a localized port
(new build target / module path), tracked by its own ADR amendment — not a
rewrite. The package documents this migration path.

### 2. Sourcing tier: Justified T3 (SDK-owned observers)

Per §"Instrumentation landscape" above, every candidate fails ADR 0008 §2, so
the integration is **T3 self-written**, mirroring the structure of ADR 0013
(Redis): a `cassandra/` package that owns span creation, attribute population,
and metric recording, wired to explicit SDK providers with no OTel global
mutation.

The instrumentation is implemented against the driver's three observer
extension points:

```go
type QueryObserver   interface { ObserveQuery(context.Context, ObservedQuery) }
type BatchObserver   interface { ObserveBatch(context.Context, ObservedBatch) }
type ConnectObserver interface { ObserveConnect(ObservedConnect) }
```

These are stable, documented seams — the T3 here is **cleaner than Redis**,
which had to manage cluster/ring hook idempotency and weak-pointer identity:
gocql observers are set once on the `*gocql.ClusterConfig` at session creation,
so there is no idempotency map, no `OnNewNode` race window, and no `Unwrap`
problem.

#### Vendoring the removed contrib code as a starting point

The deleted `otelgocql` is Apache-2.0 licensed. The implementation **may vendor
its observer skeleton as a starting point** and modernize it: replace its
pre-stable attributes with semconv v1.39.0, drop its global-provider fallbacks
in favor of required explicit providers (ADR 0003), and align metric shapes
with the SDK's `db.client.*` contract. The result is still T3 — the SDK owns
and maintains the code. The package header records the provenance and license.

### 3. Public API shape

The session is created by the SDK so the observers can be wired before any
query runs (observers cannot be attached to a live session after the fact).
This mirrors `mongo.Connect` (the SDK builds the client) rather than
`redis.Wrap` (the caller builds the client):

```go
// package cassandra (import as o11ycassandra "github.com/flywindy/o11y/cassandra")

func NewSession(
    cluster *gocql.ClusterConfig,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    opts ...Option,
) (*gocql.Session, error)

type Option func(*config)

func WithQueryText(enabled bool) Option        // default: false (see §6)
func WithHostAttributes(enabled bool) Option   // default: false (see §5) — gates network.peer.* / cassandra.coordinator.*
func WithPoolName(name string) Option          // connection-metric pool label; synthesized from the contact point when unset
func WithAttributes(attrs ...attribute.KeyValue) Option
```

- Takes a caller-built `*gocql.ClusterConfig` (preserving full control of
  contact points, consistency, auth, pool sizing, timeouts — `pkg/cassutil`
  already builds one) and returns a normal `*gocql.Session`.
- `tp` and `mp` are **required and rejected if nil** (ADR 0003 — never fall back
  to the global providers, matching `redis.Wrap`).
- No `propagation.TextMapPropagator` parameter: Cassandra is a client-only
  database protocol with no inbound trace context to extract or outbound headers
  to inject (unlike NATS/Mongo document propagation).
- **No `context.Context` parameter** (matching `redis.Wrap`, ADR 0013 §3, and
  for a driver-specific reason). gocql v1.7.0's `NewSession(cfg ClusterConfig)`
  takes no context and builds its session context from `context.TODO()`
  internally (`session.go`: `ctx, cancel := context.WithCancel(context.TODO())`,
  with the upstream comment *"TODO: we should take a context in here at some
  point"*); the initial connection setup in `s.init()` runs under that context,
  not a caller-supplied one. Accepting a `ctx` here would therefore be a **false
  cancellation guarantee** — it could not cancel or deadline the initial dial.
  Dial bounds come from the `ClusterConfig` (`ConnectTimeout` / `Timeout`)
  instead. `NewSession` is a one-shot startup bootstrap, not a request-path call.
  (If a future gocql adds a context-aware constructor, a cancellable seam is an
  amendment then.)

### 4. Span model

The gocql v1.7.0 observer seam delivers a **completed-observation snapshot**
(`Start`/`End` already set), not a live span handle. Verified against the v1.7.0
tag, `ObservedQuery` and `ObservedBatch` carry **no query/batch identity field**
(the `Query *Query` / `Batch *Batch` pointers exist only on later gocql trunk).
The span model is shaped by what that seam can actually deliver:

**Queries — `ObserveQuery` fires once per *attempt*, and once per page.**
Verified against the v1.7.0 source, not the doc-comment summary alone:
`queryExecutor.do` runs the retry/next-host loop and calls `attemptQuery` for
each attempt; `attemptQuery` calls `qry.attempt(...)` after **every** driver
attempt; and `(*Query).attempt` immediately invokes `q.observer.ObserveQuery(...)`
(`query_executor.go` lines 55–63, 129–196; `session.go` `(*Query).attempt`).
`ObservedQuery.Attempt` is documented as *"the index of attempt at executing this
query. The first attempt is number zero and any retries have non-zero attempt
number"* — i.e. retried and speculative executions deliver **multiple** callbacks,
one per attempt, each a completed `Start`/`End` snapshot for the host actually
contacted. The earlier "retries are collapsed into one callback" claim was wrong.

Two seam-honest options follow, and v1 takes (a):

- **(a) One CLIENT span per `ObserveQuery` callback** = one span per attempt and
  per page. This is exactly what the historical `otelgocql` contrib emitted, and
  it is the only model the bare observer can implement faithfully: each callback
  is a standalone completed snapshot with no shared identity and no way to mutate
  the caller's context for the next attempt. A retried/speculative query thus
  produces **sibling attempt spans**, each carrying its `cassandra.coordinator.*`
  host and its `Attempt` index, so retries and token-aware host changes are
  *visible* rather than hidden. A paged read likewise produces one span per page
  (each page is a real round trip), all sharing the caller's context.
- **(b) One logical-operation span spanning all attempts** would require an
  **SDK-owned query seam** (a wrapper that opens the parent span before execution
  and threads it through retries), because the observer payload carries no query
  identity and the callback cannot reach forward to later attempts. Deferred — it
  is the same seam §4 introduces for batches, and is out of scope for v1.

Span name follows the cross-package convention (ADR 0023):
`{db.system.name}.{db.operation.name} {target}`, e.g.
`cassandra.SELECT messages_by_room`, falling back to `cassandra.{operation}`
when the table cannot be parsed. Because this is a T3 SDK-owned seam, the
wrapper sets the span name directly and conforms without an upstream
formatter. A unit test pins the callback/span multiplicity for the pinned
gocql version (one span per attempt and per page, not one per logical query).

**Batches.** `ObserveBatch` "gets called on every batch query to cassandra. It
also gets called once for each query in a batch", and v1.7.0 exposes **no batch
identity** to deduplicate those `(1 + N)` callbacks. Coalescing them into one
span from the observer payload alone is therefore not reliable. The integration
resolves this with an **SDK-owned batch-execution seam** — thin
`ExecuteBatch(ctx, session, batch)` / `ExecuteBatchCAS` / `MapExecuteBatchCAS`
helpers that own exactly one span per logical batch (feeding
`db.operation.batch.size` from the statement count) and treat the driver's
batch-observer callbacks as supplementary timing only. The CAS forms cover
lightweight-transaction batches, which gocql executes through separate session
methods and would otherwise have no instrumented entry point. Pure observer-only
batch instrumentation is **out of scope for v1** precisely because the v1.7.0
payload cannot identify the logical batch. The callback multiplicity for the
pinned gocql version is pinned by a unit test.

**Per-query observers.** gocql stores a single query observer per `Query`
(inherited from the session, overwritten by `(*Query).Observer`). Callers must
therefore configure any additional query/connect observer on the `ClusterConfig`
— where `NewSession` composes it with the SDK's — rather than per query, which
would replace the SDK observer and silently drop that query's telemetry. This
does **not** apply to batches: their telemetry comes from the `ExecuteBatch*`
seams, not the driver's `BatchObserver` (which the package does not install), so a
`(*Batch).Observer` the caller sets runs independently and does not affect the SDK
batch spans. Documented on `NewSession` and in the guide.

### 5. Span attributes (semconv v1.39.0)

Sourced from `ObservedQuery` / `ObservedBatch` / `HostInfo` / `hostMetrics`.

| Attribute | Level | Source |
|---|---|---|
| `db.system.name` = `cassandra` | Required | constant |
| `db.namespace` | Conditionally Required | the keyspace actually addressed: an explicit `keyspace.table` qualifier parsed from the statement (authoritative, overrides the session keyspace), else `ObservedQuery.Keyspace`. Covers DML and qualified `CREATE/ALTER/DROP/TRUNCATE TABLE` and `CREATE/DROP KEYSPACE`. A batch resolves per statement and omits `db.namespace` when it spans multiple keyspaces. |
| `db.operation.name` | Recommended | parsed from statement verb (SELECT/INSERT/…) / "BATCH" |
| `db.collection.name` | Recommended | parsed table when a single table is addressed; for a batch, only when every statement targets the same fully-qualified `keyspace.table` |
| `db.query.text` | Opt-In | `ObservedQuery.Statement` (batch: statements joined with `; `), **only when `WithQueryText(true)`** (§6) |
| `db.response.returned_rows` | Recommended | `ObservedQuery.Rows` |
| `cassandra.coordinator.id` | Opt-In | `ObservedQuery.Host` (coordinating node id), **only when `WithHostAttributes(true)`** |
| `cassandra.coordinator.dc` | Opt-In | `ObservedQuery.Host.DataCenter()`, **only when `WithHostAttributes(true)`** |
| `server.address` / `server.port` | Recommended | the node that served the operation (`ObservedQuery.Host`), falling back to the configured contact point [1] |
| `network.peer.address` / `network.peer.port` | Opt-In | the actual contacted coordinator from `HostInfo`, **only when `WithHostAttributes(true)`** [1] |
| `error.type` | Conditionally Required | set on `ObservedQuery.Err != nil` |

**[1] `server.*` and `WithHostAttributes`.** `server.address`/`server.port` are
the primary peer keys (consistent with Redis, ADR 0013, and Elasticsearch,
ADR 0020) and now identify the node that actually served the operation —
`ObservedQuery.Host` under token-aware routing — falling back to the contact
point. `WithHostAttributes(true)` additionally records the coordinator's
`cassandra.coordinator.id` / `.dc` (node UUID and datacenter, which `server.*`
does not carry) and `network.peer.address`/`network.peer.port`; for a direct
gocql connection the latter mirror `server.*`, so they are Opt-In and off by
default rather than always duplicated. These topology details are kept Opt-In so
the package does not expose per-node UUIDs/DCs unless asked.

**Cassandra-specific key namespace.** In the pinned
`go.opentelemetry.io/otel/semconv/v1.39.0` Go package these keys live in the
**`cassandra.*`** namespace — `cassandra.consistency.level`,
`cassandra.coordinator.dc` / `.id`, `cassandra.page.size`,
`cassandra.query.idempotent`, `cassandra.speculative_execution.count` — **not**
the old `db.cassandra.*` prefix, which is deprecated (and `db.cassandra.table`
became `db.collection.name`). The implementation must source them from the
semconv constants (e.g. `semconv.CassandraConsistencyLevelKey`), never
hardcoded literals. The connect observer keys its spans/metrics by `server.*`
(actual peer in `network.peer.*`, Opt-In), matching the query path.

**Not available from the bare gocql v1.7.0 observer seam.**
`cassandra.consistency.level`, `cassandra.page.size`, `cassandra.query.idempotent`,
and `cassandra.speculative_execution.count` describe per-query *settings* carried
on `*gocql.Query` before execution; v1.7.0's `ObservedQuery` does not expose them
(it has no `Query` field). They are **out of scope for v1** and would require the
SDK-owned query/batch seam (§4) — or a newer gocql that adds `ObservedQuery.Query`
— to populate. The table lists only what the v1.7.0 observer payload actually
exposes; `cassandra.coordinator.*` survive because they come from
`ObservedQuery.Host`.

### 6. Query text is opt-in

`db.query.text` (the CQL statement) is **off by default**, matching
`redis.WithCommandTextEnabled` and the MongoDB posture. CQL statements are
parameterized (values are bound separately), so the statement text is low-PII,
but it can still be high-cardinality and reveal schema/table topology. Services
opt in with `WithQueryText(true)`. Bound values are never captured.

### 7. Metrics

Cassandra metrics sit between MongoDB (worth doing) and NATS (deferred): there
is **no off-the-shelf library** (so any metric is self-written), but the
observer seam yields the high-value signals **cheaply**, and the genuinely
client-only signals (latency attribution, retries) are not reconstructable from
server-side exporters.

**A. Operation duration (v1, high value)**

| Metric | Type | Unit | Labels |
|---|---|---|---|
| `db.client.operation.duration` | Float64 histogram | s | `db.system.name`, `db.operation.name`, `db.namespace`, `server.address`, `server.port`, `error.type` (on errors only) |

Computed from `ObservedQuery.End - ObservedQuery.Start` (and the batch
equivalent). This is the same instrument MongoDB (ADR 0014) and Redis (ADR 0013)
emit; the SDK provides `cassandra.MetricViews()` composed into `o11y.Init` via
`internal/metrics.Config.ExtraViews` (same wiring as redis at `o11y.go:233`),
pinning histogram buckets to the SDK default set for cross-integration
consistency and applying an allow-keys filter to bound cardinality. The same
allow-keys backstop is applied to both SDK-owned attempt counters
(`cassandra.query.attempts`, `cassandra.connection.attempts`); their label set is
already bounded by construction, but the view guarantees a stray attribute can
never leak in.

`server.address` / `server.port` identify the node that actually served the
operation: `ObservedQuery.Host` for queries (the coordinator chosen by
token-aware routing) and `ObservedConnect.Host` for connects, falling back to the
configured contact point when the driver supplies no host (and for the batch
seam, which has no per-statement host). This is the semconv-conformant meaning of
`server.*` for `db.client.operation.duration`, and it keeps the query and connect
metrics consistent. Cardinality stays bounded because the value is one of the
cluster's nodes — a fixed set per deployment, not a per-request address
(ADR 0008 §3). Callers who want to keep these labels collapsed to the contact
point can still aggregate them away downstream; the actual coordinator id/dc are
additionally available on spans via `WithHostAttributes`.

**B. Retry / speculative-execution counter (v1 or fast-follow, Cassandra-unique)**

| Metric | Type | Source |
|---|---|---|
| `cassandra.query.attempts` (SDK-owned name, pending a stable semconv equivalent) | counter | **`+1` per `ObserveQuery` callback** (each callback = exactly one attempt) |

**Increment by a fixed `1` per callback — do *not* add `Metrics.Attempts`.**
Because `ObserveQuery` fires once per attempt (§4) and `(*Query).attempt` calls
`q.metrics.attempt(1, ...)`, each callback represents exactly **one** attempt, so
a plain `counter.Add(ctx, 1)` per callback yields the true attempt total. By
contrast `ObservedQuery.Metrics.Attempts` (and `ObservedQuery.Attempt`) is a
**cumulative per-host snapshot** — `queryMetrics.attempt` does
`updateHostMetrics.Attempts += addAttempts` and returns a copy, and the field is
documented as *"count of how many times this query has been attempted for this
host"* (incremented by retries **and** by fetching the next page). Adding that
snapshot on every callback would record `1 + 2 + 3 = 6` for a 3-attempt
same-host query instead of `3`, corrupting the retry metric. The cumulative
`Metrics.Attempts` is therefore used only as an *attribute* on the per-attempt
span (current driver-side attempt index), never summed into the counter.

These fields are reachable from our package: `Attempt` and `Metrics` are
exported fields of `ObservedQuery`, and although the `hostMetrics` type name is
unexported in gocql v1.7.0, its `Attempts` / `TotalLatency` fields are exported,
so `q.Metrics.Attempts` compiles and reads correctly.

This is the signal server-side exporters **cannot** provide: client-side
token-aware routing, retries, and speculative execution are driver decisions
invisible to Cassandra's own metrics. Kept as an explicitly SDK-named metric
(not a fabricated semconv key) so it is easy to retire/rename if semconv later
standardizes one.

**C. Connection attempts (optional, medium value)**

`ConnectObserver` yields connect success/failure and connect latency. A connect
counter + a `db.client.connection.create_time`-style histogram MAY be emitted.

**Out of scope: connection-pool gauges.** Unlike go-redis (`PoolStats()`
snapshot) and the Mongo driver (`event.PoolMonitor` deltas), **gocql exposes no
public pool-stats snapshot or pool lifecycle events**. `db.client.connection.count{state}`
and similar gauges are therefore **not emitted** — they cannot be derived
cheaply or reliably. Cluster/node operational health (read/write latency
percentiles, pending compactions, heap, dropped messages) is obtained from the
**server-side** `cassandra-exporter` / Prometheus JMX exporter / MCAC scraped by
the OpenTelemetry Collector, complementary to the SDK's client-side spans and
operation-duration metric.

### 8. Out of scope (v1)

- Apache `cassandra-gocql-driver/v2` as a build target (port, future amendment).
- Connection-pool gauges (no driver snapshot; see §7).
- `gocql.Tracer` (server-side `system_traces` CQL tracing) — a separate,
  heavyweight Cassandra feature, not OTel tracing.
- Schema/prepared-statement cache metrics.

---

## Global-state verification

The integration imports **no third-party OTel instrumentation library** — it is
SDK-owned and uses only the SDK-supplied `tp`/`mp`. The `gocql` driver itself
does not import OpenTelemetry. Therefore:

- There is no upstream constructor that could call `otel.SetTracerProvider` /
  `otel.SetTextMapPropagator`.
- The package contains no `otel.SetX` call (enforced by the ADR 0008 §7.3 grep
  gate).

If the implementation vendors the removed `otelgocql` skeleton (§2), the
vendored code's global-provider fallbacks must be removed so the SDK always
threads explicit providers; the audit is a source read of the vendored file,
recorded in the implementing PR.

---

## Required policy artifacts (ADR 0008)

- **`cassandra/doc.go`** carries `// Tier: T3 SDK-owned observers over
  github.com/gocql/gocql` and a provenance note if the otelgocql skeleton is
  vendored.
- **ADR 0008 §7.2 gate**: add `cassandra` to the gate's `integrationDirs`
  include-list (`scripts/check_integrations.go`) in the implementing PR; this
  ADR satisfies the "T3 package must be mentioned by an ADR" requirement.
- **ADR 0008 §5**: the "Future: Cassandra → T2 over `otelgocql`" row is
  corrected to "Justified T3 — see ADR 0019" (amendment applied alongside this
  ADR).
- **ADR 0003**: no Approved-integrations row is required — there is no
  third-party OTel instrumentation dependency to verify (the gate only checks
  `go.opentelemetry.io/contrib/...` and the corporate prefix). The `gocql`
  driver is a pure DB client, like `go-resty/resty` under ADR 0011.

---

## Testing

The observers are plain structs of methods, so most behavior is unit-testable
without a live cluster by constructing synthetic `ObservedQuery` /
`ObservedBatch` / `ObservedConnect` values:

- `NewSession` rejects nil `tp` / nil `mp`.
- A successful `ObservedQuery` yields one span with `db.system.name=cassandra`,
  `db.namespace`, `db.operation.name`, `db.response.returned_rows`, and
  `server.*`; no `error.type`. `network.peer.*` / `cassandra.coordinator.*` are
  absent by default and present only under `WithHostAttributes(true)`.
- A failed `ObservedQuery` sets `error.type` and records the span error.
- `db.query.text` is absent by default and present (statement only, no bound
  values) under `WithQueryText(true)`.
- A multi-attempt `ObservedQuery` sequence produces **one span per attempt**
  (sibling attempt spans, each with its `Attempt` index, plus the coordinator
  host when `WithHostAttributes(true)`),
  matching the gocql per-attempt callback semantics — not one collapsed span and
  not a fabricated single logical span (§4 option (a)).
- `db.client.operation.duration` is recorded with the documented labels and the
  SDK default histogram buckets; `MetricViews()` drops keys outside the
  allow-set.
- The attempts counter increments by exactly `1` per `ObserveQuery` callback; a
  3-attempt same-host sequence records `3` (verifying the counter does **not**
  sum the cumulative `ObservedQuery.Metrics.Attempts` snapshot, §7.B).
- Batch path mirrors the query path.
- Integration test (build-tagged, `testcontainers-go` Cassandra, matching the
  consumer's own test posture) for the healthy path; kept out of default
  `go test ./...`.

---

## Consequences

**Positive**

- Cassandra calls become first-class Tempo spans with semconv v1.39.0
  attributes, correlated with logs and traces like MongoDB and Redis.
- The highest-value client-only metric (`db.client.operation.duration`) and a
  Cassandra-unique retry/speculative signal land cheaply off the observer seam.
- The observer-based T3 is simpler than the Redis hook T3 (no idempotency/race
  machinery) and ports to the Apache v2 driver with an import-path change.
- No new third-party dependency to pin or re-audit.

**Negative / Trade-offs**

- Self-written instrumentation: the SDK owns semconv catch-up for Cassandra
  (the cost ADR 0008 §2 accepts when no maintained library exists).
- No connection-pool gauges (driver limitation); pool/cluster health relies on
  the server-side exporter.
- Targeting `gocql/gocql` (low-maintenance) ties v1 to a driver in maintenance
  mode; the Apache v2 port is deferred until a consumer needs it.
- `db.cassandra.*` keys are experimental; a future stabilization may require an
  attribute rename (localized, Opt-In keys only).

---

## Open questions

1. Should the retry/speculative counter (§7.B) ship in v1 or as a fast-follow
   once the operation-duration metric is validated against the consumer's load
   profile?
2. Confirm whether `pkg/cassutil.Connect`'s consumers can route session creation
   through `cassandra.NewSession`, or whether a `Wrap`-style attach-to-existing
   variant is also needed (it cannot attach observers to an already-built
   session, so a config-time seam is required either way).
