# ADR 0014 — MongoDB Metrics

**Status**: Accepted — Phase 1 (operation duration) implemented; Phase 2
(connection pool metrics) pending (Option A; dual tier annotation at Phase 2)
**Date**: 2026-05-25
**Relates to**: ADR 0005 (MongoDB integration), ADR 0008 (sourcing policy),
ADR 0013 (Redis/Valkey integration — reference for the pool-metric pattern)
**Superseded in part by**: ADR 0021 (resolves the deferred Q1 Option B — spans
move to the contrib monitor; Phase 2 pool-metric lifecycle moves from a wrapper
`Disconnect` override to the cleanup func returned by `Instrument`)

---

## Context

`mongo/` today is a **trace-only** T2 facade over
`github.com/Marz32onE/instrumentation-go/otel-mongo/v2` (ADR 0005). `Connect`
takes `tp` + `prop` and emits command spans (plus optional `_oteltrace`
document propagation). It does **not** take a `MeterProvider` and emits **no
metrics**. By contrast `redis/` emits both spans and the full
`db.client.*` metric set (ADR 0013).

This ADR plans MongoDB metrics: operation latency/error rate and connection
pool health, aligned to semconv v1.39.0 (ADR 0006) and the cardinality
discipline of ADR 0008 §3.

### Library survey (ADR 0008 §6 re-evaluation)

ADR 0005 was written before an OTel-contrib v2 instrumentation existed. The
landscape has changed, so per ADR 0008 §6 we re-survey:

| Candidate | Spans | Metrics | Mechanism | Notes |
|---|---|---|---|---|
| `Marz32onE/.../otel-mongo/v2` (current) | yes (API-wrapping) | **none** | wraps Collection/Database/Cursor methods | owns the `_oteltrace` document-propagation feature ADR 0005 §4 preserves |
| `go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo` | yes | **`db.client.operation.duration` only** | `event.CommandMonitor` | `WithMeterProvider` / `WithTracerProvider`; **no pool metrics, no `PoolMonitor`** |
| mongo-driver/v2 itself | — | — | exposes `SetMonitor` (CommandMonitor) + `SetPoolMonitor` | no built-in OTel; raw event hooks only |

**Verified facts (source inspection):**

- The Marz lib uses **neither** `SetMonitor` nor `SetPoolMonitor` — its spans
  come from API-wrapping. Both driver monitor slots are therefore **free** for
  us to use.
- `ConnectWithOptions` runs `options.MergeClientOptions(opts...)` then
  `mongo.Connect(merged)`, so a `SetMonitor` / `SetPoolMonitor` we set on the
  `*options.ClientOptions` we already pass **survives the merge** and is wired
  into the real driver client.
- The official contrib v2 `otelmongo` creates exactly one instrument,
  `db.client.operation.duration` (histogram), and uses a CommandMonitor only.

**Answer to "is there an off-the-shelf lib":**

- **Operation-duration metric → YES** (official contrib v2 `otelmongo`,
  strongest §2.2 maintenance signal). Hand-rolling it would be an unjustified
  T3 under ADR 0008.
- **Pool metrics → NO.** No library emits `db.client.connection.*` for the Go
  driver. The pool-metric work is a **justified T3** (no candidate passes
  ADR 0008 §2 because none implements a `PoolMonitor`-based observer).

---

## Decision

Add MongoDB metrics in **two phases**, keeping the Marz lib for spans +
document propagation (no regression to ADR 0005 features). Both monitors are
attached on the `*options.ClientOptions` we already build, before handing it to
`ConnectWithOptions`.

### Phase 1 — operation duration (T2, low risk)

- Attach the **official contrib `otelmongo` CommandMonitor in metrics-only
  mode**: `otelmongo.NewMonitor(otelmongo.WithMeterProvider(mp),
  otelmongo.WithTracerProvider(tracenoop.NewTracerProvider()))` via
  `clientOptions.SetMonitor(...)` (`tracenoop` =
  `"go.opentelemetry.io/otel/trace/noop"`, the alias `o11y.go` already uses). The noop tracer suppresses the monitor's own
  spans so they cannot double-emit alongside the Marz API-wrapping spans (and
  cannot bypass the ADR 0005 env-gate); only `db.client.operation.duration`
  flows out.
- Wire a `MeterProvider` into `mongo.Connect` (API change — see below).
- Add `mongo.MetricViews()` and compose it into `o11y.Init` via the existing
  `internal/metrics.Config.ExtraViews` mechanism (the same wiring redis uses at
  `o11y.go:233`).

> **Decision point for the reviewer.** Phase 1 keeps two libs (Marz = spans,
> contrib = metric). The clean-architecture alternative is to **migrate spans
> to the contrib `otelmongo` too** (single maintained lib, drop the noop-tracer
> trick) — but that **loses `_oteltrace` document propagation** (ADR 0005 §4)
> and changes span shape. Recommended only if document propagation is confirmed
> unused. Default recommendation: keep Marz for spans, add the contrib monitor
> for the metric.

### Phase 2 — connection pool metrics (justified T3, medium risk)

Attach an **SDK-owned `event.PoolMonitor`** via
`clientOptions.SetPoolMonitor(...)` that maintains per-server-address counters
and feeds an async observable callback registered on `mp`. This is the
genuinely custom part; it mirrors `redis/metrics.go` but is **event-stream
based** (deltas) because the Go driver exposes **no pool-stats snapshot** like
go-redis's `PoolStats()`.

#### Metric mapping (semconv v1.39.0)

| Metric | Type | Source (driver `event.PoolEvent` / options) |
|---|---|---|
| `db.client.connection.count` `{state=used\|idle}` | observable up-down counter | running counters: `ConnectionReady`−`ConnectionClosed` = total; `ConnectionCheckedOut`−`ConnectionCheckedIn` = used; idle = total−used |
| `db.client.connection.max` | observable up-down counter | `PoolOptions.MaxPoolSize` (from `ConnectionPoolCreated`/`Ready`); omit if 0 (unbounded) — same rule we just applied to redis |
| `db.client.connection.idle.min` | observable up-down counter | `PoolOptions.MinPoolSize` |
| `db.client.connection.idle.max` | — | **omit**; MongoDB has no max-idle concept |
| `db.client.connection.timeouts` | observable counter | count of `ConnectionCheckOutFailed` with `Reason == event.ReasonTimedOut` |
| `db.client.connection.create_time` | histogram (s) | `ConnectionReady.Duration` |
| `db.client.connection.pending_requests` | observable up-down counter | `ConnectionCheckOutStarted` − (`ConnectionCheckedOut` + `ConnectionCheckOutFailed`) |

- **Attributes**: `db.system.name=mongodb`, `db.client.connection.pool.name`
  (synthesized like redis, or `WithPoolName`), `server.address` + `server.port`
  parsed from `PoolEvent.Address`. Bounded by topology size (replica set /
  shards), so cardinality is naturally small.
- **State reset** on `ConnectionPoolCleared` / `ConnectionPoolClosed` to avoid
  drift after a pool is torn down.
- `wait_time` / `use_time` deferred (not cleanly derivable from v2 events;
  dropped by `MetricViews` like redis does).

#### Lifecycle

- Register the observable callback at `Connect`; **unregister on `Disconnect`**.
  Our `mongo.Client` embeds `*otelmongo.Client` (which already overrides
  `Disconnect`); we add our own `Disconnect` that calls through and then
  `reg.Unregister()`. No weak-pointer/idempotency machinery (unlike redis Wrap)
  is needed — the client is constructed by us and has a single owner.

---

## API change

```go
// before
func Connect(ctx, uri string, tp trace.TracerProvider,
    prop propagation.TextMapPropagator, opts ...Option) (*Client, error)

// after  (breaking — minor bump, pre-1.0 per CHANGELOG policy)
func Connect(ctx, uri string, tp trace.TracerProvider, mp metric.MeterProvider,
    prop propagation.TextMapPropagator, opts ...Option) (*Client, error)
```

- `mp` required and rejected if nil (mirrors redis `Wrap`, ADR 0003 — never
  fall back to the global `MeterProvider`).
- Call sites pass `obs.MeterProvider()` (already exposed at `o11y.go:107`).
- New option `WithPoolName(string)` for Phase 2, matching redis.

---

## Tier classification & policy artifacts (ADR 0008)

- Phase 1 metric → **T2** (contrib lib). Phase 2 pool metrics → **justified
  T3** (no candidate implements a pool observer).
- `mongo/doc.go` Tier line must be updated to reflect the mixed sourcing once
  Phase 2 lands (T2 facade for spans + duration; T3 for the SDK-owned pool
  observer, justified in this ADR per §2). Use a **dual annotation**
  (`// Tier: T2` + `// Tier: T3`) — the CI gate (ADR 0008 §7.2) accepts both
  lines, as decided in Q3 below; no gate change is required.
- **ADR 0003**: add the contrib `otelmongo` module to the Approved-integrations
  table (global-state grep + semconv row), per ADR 0008 §4 / §7.1.
- **ADR 0005**: cross-reference this ADR; note metrics now augment the trace
  facade.
- **CHANGELOG**: `[Unreleased]` entry for the new `mp` param, `mongo` metrics,
  and `WithPoolName`.
- **README / examples/mongodb**: update the `Connect` call to pass
  `obs.MeterProvider()`.

---

## Testing

- **Phase 1**: assert `db.client.operation.duration` is recorded with
  `db.system.name=mongodb` / `db.operation.name` / `network.peer.address` /
  `network.peer.port` / `error.type` on failure (the contrib emits
  `network.peer.*`, not `server.*` — see Decision analysis and Implementation
  specifics #2); assert the contrib monitor's spans do **not** appear (noop tracer);
  assert provider wiring rejects nil `mp`.
- **Phase 2**: unit-testable without a real server — `event.PoolMonitor` and
  `event.CommandMonitor` are plain structs of funcs, so feed synthetic
  `*event.PoolEvent` sequences and assert the observable callback reports the
  expected `count{used,idle}`, `timeouts`, `create_time`, and that
  `idle.max` is omitted and `max` is omitted when `MaxPoolSize==0`.
- Integration test (build-tagged, `testcontainers-go`) for the healthy path,
  consistent with ADR 0005's testing posture.

---

## Consequences

**Positive**

- MongoDB reaches metric parity with redis (operation duration + pool health).
- Phase 1 leans on a maintained OTel-contrib instrument (ADR 0008 default),
  not hand-rolled code.
- Both signals are independently testable from synthetic events.

**Negative / Trade-offs**

- Phase 1 composes two libs (spans via Marz, metric via contrib) with a
  noop-tracer trick — a deviation from the clean single-lib T2 model that must
  be documented at the call site. (Avoidable only by the full-migration
  alternative, which sacrifices document propagation.)
- Phase 2 pool tracking is **stateful** (event deltas, not a snapshot), so it
  carries drift risk if events are missed; reset-on-clear mitigates this.
- One new dependency (contrib `otelmongo`) to pin and re-audit (ADR 0008 §6).

---

## Decision analysis (the three open questions)

Additional verified facts feeding this analysis (source inspection of the
pinned contrib `otelmongo` `mongo.go` and the local repo):

- Contrib `otelmongo` is **global-state SAFE** (reads providers as fallback
  only; the only global touch is `otel.Handle(err)`) → qualifies for an
  ADR 0003 row.
- It records `db.client.operation.duration` **unconditionally** in
  `Succeeded`/`Failed`, independent of span sampling → **the noop-tracer trick
  in Phase 1 Option A genuinely works** (metric emits with a no-op tracer).
- It reads **no `OTEL_*_ENABLED` env vars** (unlike the Marz lib, whose spans
  are gated behind `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` +
  `OTEL_MONGO_TRACING_ENABLED`, ADR 0005).
- Its attributes use **`network.peer.address` / `network.peer.port`**, *not*
  `server.address` / `server.port`, and there is **no option to override metric
  attributes** (only `WithSpanNameFormatter`, `WithCommandAttributeDisabled`,
  `WithMeterProvider`, `WithTracerProvider`). *(Verify the exact set against the
  pinned version at implementation time.)*
- `_oteltrace` document propagation is **shipped and demonstrated**:
  `examples/mongodb/main.go:72` and `mongo/client_test.go:125`.
- The ADR 0008 §7 gate is **implemented** (`scripts/check_integrations.go`,
  `make adr-check`, `.github/workflows/adr-check.yml`). It requires: (a) every
  contrib/Marz module in `go.mod` has an ADR 0003 row; (b) each integration
  `doc.go` contains `// Tier: T2` **or** `// Tier: T3`; (c) any `T3` package is
  mentioned by some ADR. A `doc.go` carrying **both** tier lines passes.
- `ExtraViews` is a single assignment today (`o11y.go:233`,
  `ExtraViews: o11yredis.MetricViews()`); adding mongo means
  `append(o11yredis.MetricViews(), o11ymongo.MetricViews()...)` — a T1 edit.

### Q1 — Phase 1 shape: keep Marz + contrib metric (A) vs full migration (B)

| | **Option A — Marz spans + contrib metric (noop tracer)** | **Option B — migrate spans+metric to contrib** |
|---|---|---|
| Span shape | unchanged (no dashboard breakage) | **changes** (`network.peer.*`, formatter span names, attr diffs) — breaks existing Tempo queries/alerts |
| `_oteltrace` doc propagation | **kept** | **lost** (shipped feature: examples + tests + ADR 0005 §4) |
| Marz env-gate footgun | still applies to spans; metric is independent | **removed** entirely |
| Synthetic-delivery-tracer concern (ADR 0005) | unchanged | **removed** entirely |
| Dependencies | **two** mongo libs to pin/audit | **one** maintained OTel-contrib lib |
| `noop` tracer trick | yes (small, isolated) | none |
| Tier | T2 (metric) over a maintained lib | clean single-lib T2 |
| Metric attrs | inherits contrib `network.peer.*` | same |
| Blast radius | **additive / low** | **trace rewrite / high** |

- **Recommendation: Option A**, unless document propagation is confirmed unused
  by every consumer (it currently is used in-tree). A is additive: existing
  traces are untouched, the highest-value metric lands, and we stay
  ADR 0008-compliant by using the contrib instrument for the metric.
- **Shared wart (both options)**: the contrib metric labels with
  `network.peer.*`, inconsistent with redis pool metrics (`server.*`) and with
  our Phase-2 pool observer. We cannot override it (ADR 0008 §2.4 tension). To
  keep mongo internally consistent we either (i) emit `server.*` on our Phase-2
  pool metrics and accept that mongo's operation vs pool metrics differ on the
  address key, or (ii) mirror `network.peer.*` on the pool observer and accept
  divergence from redis. Lean (i): match the semconv DB-metrics spec for the
  part we own; document the contrib quirk.
- **Future impact** — A: one added dep, no signal regression. B: trace behavior
  change for every mongo consumer + feature removal; same *class* of breakage
  as the v0.2.0 OpenMetrics `le` change.
- **Past-ADR changes** — A: ADR 0003 (+1 row, contrib `otelmongo`, verified
  SAFE), ADR 0005 (cross-ref + "metrics augment the trace facade" note), this
  ADR. B: **substantial ADR 0005 rewrite** (§2 mechanism, §4 doc propagation,
  Synthetic-Delivery-Tracer section, Testing) plus ADR 0003 row swap — much
  larger doc churn.

### Q2 — First-PR scope: Phase 1 only vs Phase 1 + Phase 2 together

- The `mp` parameter is the **only breaking change** and is needed by both
  phases, so front-loading it in Phase 1 means **Phase 2 is purely additive**
  (`WithPoolName` + the observer). Two phases ≠ two breaking changes.

| | **Phase 1 only first** | **Phase 1 + 2 together** |
|---|---|---|
| Reviewability | small, T2, one breaking API | large: T2 + justified-T3 + stateful event tracking + `Disconnect` override |
| Risk isolation | risky stateful pool observer bakes separately | easy win blocked if pool design iterates |
| Releases | two `[Unreleased]` entries | one |
| ADR dependency | T2 needs only the ADR 0003 row | T3 needs **this ADR Accepted** before merge (gate) |

- **Recommendation: Phase 1 first; Phase 2 as a follow-up PR.** Front-load the
  `mp` break, ship the high-value latency/error metric at low risk, let the
  stateful T3 observer land on its own once this ADR is Accepted.

### Q3 — `doc.go` tier annotation for the mixed package

- **Phase 1** leaves the package **T2** (spans via Marz + metric via a
  maintained contrib lib). Only change: update the annotation text to name both
  libs, and add the contrib row to ADR 0003 (gate requirement (a)).
- **Phase 2** introduces the SDK-owned pool observer = self-written
  instrumentation → the package becomes mixed.
  - **Recommendation: dual annotation.** Keep `// Tier: T2 facade ...` and add
    `// Tier: T3 SDK-owned pool-metric observer — see ADR 0014`. The gate
    accepts both lines (it passes on either, and the `T3` line is satisfied by
    this ADR mentioning `mongo`). **No gate code change required.**
  - Rejected: T2-only (under-declares self-written code, against ADR 0008
    spirit); separate package (the gate's `integrationDirs` list is fixed —
    `{http,nats,mongo,gin,resty,redis}` — so a new dir is unchecked, creating a
    gate gap, and fragments the package for no benefit).
- **Future impact**: contrib `otelmongo` joins the ADR 0008 §6 quarterly
  health-check; the mongo pool observer joins the annual T3 escape-hatch review.

### Recommended path (summary)

Option **A** + Phase **1 first** + **dual annotation** at Phase 2. ADR edits:
ADR 0003 (+1 row), ADR 0005 (cross-ref note), this ADR (→ Accepted before
Phase 2), CHANGELOG, README, `examples/mongodb`. This is the lowest-blast-radius
path that still satisfies ADR 0008's "prefer a maintained lib" default.

---

## Remaining implementation-level specifics

These do not change the decisions above but must be settled in the
implementing PRs:

1. **Histogram buckets (consistency)** — the contrib instrument bakes its own
   explicit boundaries (`0.001 … 10`), which differ from the SDK's configurable
   `HistogramBuckets` (`o11y.go:231`) used elsewhere. **Resolution:**
   `mongo.MetricViews()` sets an explicit-bucket aggregation view on
   `db.client.operation.duration` so the rendered buckets match the SDK policy
   (and redis), overriding the lib's baked boundaries. **Confirmed:** align to
   the SDK's default histogram bucket set (identical to redis).
2. **`MetricViews()` allow-keys** — for `db.client.operation.duration` allow
   exactly: `db.system.name`, `db.operation.name`, `network.peer.address`,
   `network.peer.port`, `error.type`; **drop `network.transport`** (constant
   `tcp`, no signal). Pool instruments keep the redis-style allow-set with
   `server.address` / `server.port`. Net: within mongo, the operation metric is
   keyed by `network.peer.*` and the pool metrics by `server.*` — accepted (see
   Q1 wart); document it on `MetricViews`.
3. **Default pool name (confirmed)** — redis derives `redis-%x` from the client
   pointer; our `mongo.Connect` builds the client, so synthesize
   `mongo-<primary-host>` from the parsed URI and let `WithPoolName` override.
   The pool observer keys samples by `server.address` regardless, so the name is
   a grouping label, not an identity key.
4. **`error.type` style divergence (accepted)** — the contrib metric derives
   `error.type` via `semconv.ErrorType(evt.Failure)`, which will not match
   redis's custom sentinel/reflection mapping (`redis/hook.go errorType`). We
   cannot override it without forking, so mongo and redis `error.type` values
   will differ in style. Accepted T2 trade-off; note it in package docs.
5. **Metrics-without-spans deployment posture (accepted)** — because the contrib
   monitor reads no env gate while Marz spans are gated behind
   `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` + `OTEL_MONGO_TRACING_ENABLED`
   (ADR 0005), a process that omits those env vars gets **operation metrics but
   no command spans**. This is acceptable (metrics should not hide behind a
   tracing flag) but must be documented so the asymmetry is not mistaken for a
   bug.
6. **`Disconnect` override (Phase 2)** — defining `Disconnect` on our `*Client`
   shadows the embedded `*otelmongo.Client.Disconnect`; the override must call
   `c.Client.Disconnect(ctx)` through and then `reg.Unregister()` the pool
   observable. Pool counters are updated from concurrent driver goroutines, so
   per-address state must be atomic and tolerate transient negative `used`
   (clamp at 0) between `CheckedOut`/`CheckedIn` ordering.
