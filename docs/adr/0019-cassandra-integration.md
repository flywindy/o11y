# ADR 0019 — Cassandra Integration

**Status**: Proposed
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
    ctx context.Context,
    cluster *gocql.ClusterConfig,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    opts ...Option,
) (*gocql.Session, error)

type Option func(*config)

func WithQueryText(enabled bool) Option        // default: false (see §6)
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

### 4. Span model

One CLIENT span per observed query, and one per observed batch. Span name
follows the DB span guidance (`db.operation.name` + target, e.g.
`SELECT messages_by_room`), falling back to the operation name alone when the
table cannot be determined.

`ObservedQuery.Attempt` distinguishes retries and page fetches: attempt 0 opens
the logical span; subsequent attempts are recorded as events/attributes on the
same logical operation rather than as separate top-level spans, so a retried
query is one span with a visible retry count, not N spans.

**Batch callback multiplicity.** gocql's `ObserveBatch` doc comment warns it
"gets called on every batch query to cassandra. It also gets called once for
each query in a batch", so a single logical batch can invoke the observer more
than once. The implementation therefore emits exactly **one** batch span and
one metric sample per execution, deduplicated by the `ObservedBatch.Batch`
pointer identity (plus `Start`); the surplus per-statement callbacks are
collapsed into that one logical span and feed `db.operation.batch.size` (the
statement count), not extra spans. The exact callback multiplicity for
gocql v1.7.0 is pinned by a unit test so an upstream change is caught.

### 5. Span attributes (semconv v1.39.0)

Sourced from `ObservedQuery` / `ObservedBatch` / `HostInfo` / `hostMetrics`.

| Attribute | Level | Source |
|---|---|---|
| `db.system.name` = `cassandra` | Required | constant |
| `db.namespace` | Conditionally Required | `ObservedQuery.Keyspace` |
| `db.operation.name` | Recommended | parsed from statement verb (SELECT/INSERT/…) / "BATCH" |
| `db.collection.name` | Recommended | parsed table when a single table is addressed |
| `db.query.text` | Opt-In | `ObservedQuery.Statement`, **only when `WithQueryText(true)`** (§6) |
| `db.response.returned_rows` | Recommended | `ObservedQuery.Rows` |
| `cassandra.consistency.level` | Opt-In | query consistency when available |
| `cassandra.coordinator.id` | Opt-In | `HostInfo` (coordinating node id) |
| `cassandra.coordinator.dc` | Opt-In | `HostInfo.DataCenter()` |
| `cassandra.page.size` | Opt-In | page size when known |
| `cassandra.query.idempotent` | Opt-In | query idempotence flag when known |
| `cassandra.speculative_execution.count` | Opt-In | derived from attempt accounting |
| `server.address` / `server.port` | Recommended | the configured contact point / logical server [1] |
| `network.peer.address` / `network.peer.port` | Opt-In | the actual contacted coordinator from `HostInfo` [1] |
| `error.type` | Conditionally Required | set on `ObservedQuery.Err != nil` |

**[1] `server.*` vs `network.peer.*`.** `server.address`/`server.port` are the
primary peer keys, consistent with the Redis (ADR 0013) and Elasticsearch
(ADR 0020) integrations and the `db.client.operation.duration` metric labels
(§7). `network.peer.*` captures the *actual* contacted coordinator (useful
under token-aware routing) but is kept Opt-In so the SDK-owned package leads
with the repo's conformant `server.*` convention — the inverse of the Mongo T2
case, where the upstream contrib library forces `network.peer.*`.

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
consistency and applying an allow-keys filter to bound cardinality.

`server.address` / `server.port` are safe as labels because they are the
client's configured contact points — a small, fixed set per client instance,
not per-request peer addresses — so they do not explode label cardinality
(ADR 0008 §3). They are included (rather than dropped) so metrics remain
attributable in shared-address topologies such as sidecars, provided the
contact point parses into host and port.

**B. Retry / speculative-execution counter (v1 or fast-follow, Cassandra-unique)**

| Metric | Type | Source |
|---|---|---|
| `cassandra.query.attempts` (SDK-owned name, pending a stable semconv equivalent) | counter | `ObservedQuery.Attempt` and `ObservedQuery.Metrics.Attempts` |

Both sources are accessible to the observer: `Attempt` and `Metrics` are
exported fields of `ObservedQuery`, and although the `hostMetrics` type name is
unexported in gocql v1.7.0, its `Attempts` / `TotalLatency` fields are exported,
so `q.Metrics.Attempts` compiles and reads correctly from our package.

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
  `network.peer.*`; no `error.type`.
- A failed `ObservedQuery` sets `error.type` and records the span error.
- `db.query.text` is absent by default and present (statement only, no bound
  values) under `WithQueryText(true)`.
- A multi-attempt `ObservedQuery` sequence produces one logical span with the
  expected retry/speculative count, not N spans.
- `db.client.operation.duration` is recorded with the documented labels and the
  SDK default histogram buckets; `MetricViews()` drops keys outside the
  allow-set.
- The attempts counter reflects `ObservedQuery.Metrics.Attempts`.
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
