# ADR 0020 — Elasticsearch Integration

**Status**: Proposed
**Date**: 2026-06-04
**Relates to**: ADR 0003 (global state policy), ADR 0006 (semconv upgrade
strategy), ADR 0008 (instrumentation sourcing policy), ADR 0004 (NATS — the
reference for a trace-only facade with deliberately deferred metrics).

---

## Context

The SDK needs an Elasticsearch integration. The first consumer is the public
`github.com/hmchangw/chat` project, whose `go.mod` pins:

```text
github.com/elastic/go-elasticsearch/v8 v8.19.3
```

Elasticsearch is used there by `search-sync-worker` (bulk-indexes messages into
ES from the JetStream INBOX stream via `pkg/searchengine`) and `search-service`
(query path). Those calls currently produce no Tempo spans.

### Instrumentation landscape — first-party, built into the client

Unlike Cassandra (ADR 0019), Elasticsearch has the **strongest possible
maintenance signal**: OpenTelemetry tracing is **built into the official client
by Elastic**, in the shared transport `github.com/elastic/elastic-transport-go/v8`:

```go
func NewOtelInstrumentation(provider trace.TracerProvider, captureSearchBody bool,
    version string, options ...trace.TracerOption) *ElasticsearchOpenTelemetry
// installed via elastictransport.WithInstrumentation(...),
// surfaced on the client as NewOpenTelemetryInstrumentation(tp, captureSearchBody)
```

It is actively maintained (v8.x, updated 2026), emits `db.elasticsearch.*`
semantic-convention attributes with `SpanKindClient`, captures dynamic URL path
parts, and extracts Elastic Cloud cluster id / node name from response headers.

### ADR 0008 §2 evaluation — passes T2

| Checklist item | Result |
|---|---|
| §2.1 ADR 0003 compliance | **Pass (to confirm by source read).** Takes a `trace.TracerProvider` explicitly; falls back to the OTel global only when `nil` is passed. Our facade always passes the SDK provider, so the fallback never fires. |
| §2.2 Maintenance signal | **Pass.** First-party, maintained by the client vendor itself — the best signal of all. |
| §2.3 Semconv alignment | **Pass with one caveat.** Emits the Elasticsearch DB semconv (`db.elasticsearch.path_parts.*`, `db.system`). The search-body attribute uses the legacy key `db.statement` rather than the current `db.query.text` — see §4. |
| §2.4 Configurability | **Pass.** Span behavior and search-body capture are controllable; we do not need a fork. |
| §2.5 Framework signal access | **Pass.** Endpoint id, index/path parts, and cluster/node identity are populated by the instrumentation. |

The integration is therefore a **T2 facade** — and an unusually clean one,
because the instrumentation ships *inside* the client, so it tracks the client's
semconv automatically with no separate library to pin.

---

## Decisions

### 1. Client: `github.com/elastic/go-elasticsearch/v8`

Target `go-elasticsearch/v8` (matching the consumer's `v8.19.3`). A `v9` target,
if/when a consumer needs it, is a follow-up amendment; the instrumentation seam
(`elastic-transport-go`) is shared across client majors, so the facade shape is
expected to carry over.

### 2. Sourcing tier: T2 facade over the client's first-party OTel

The `elasticsearch/` package enables the client's built-in instrumentation and
wires the SDK `TracerProvider` into it. The SDK owns no span/attribute code —
it owns the *wiring* and the *defaults* (search-body off by default, explicit
provider, short import path), the same role `nats/` and `mongo/` play.

### 3. Public API shape

The facade builds the client so the instrumentation is wired into the transport
before any request runs (matching `mongo.Connect`):

```go
// package elasticsearch (import as o11yes "github.com/flywindy/o11y/elasticsearch")

func NewClient(
    cfg elasticsearch.Config,
    tp trace.TracerProvider,
    opts ...Option,
) (*elasticsearch.Client, error)

type Option func(*config)

func WithSearchBody(enabled bool) Option   // default: false (§5)
```

- Takes the caller's `elasticsearch.Config` (addresses, TLS, auth, retry — the
  consumer's `pkg/searchengine` already builds one) and returns the standard
  `*elasticsearch.Client`, so existing call sites and the typed/low-level APIs
  are unchanged.
- `tp` is **required and rejected if nil** (ADR 0003 — never fall back to the
  global `TracerProvider`).
- **No `MeterProvider` parameter** (trace-only — see §6).
- **No `propagation.TextMapPropagator` parameter.** Elasticsearch is accessed
  over HTTP as a client; the first-party instrumentation does not propagate
  inbound trace context into ES, and there is no consumer-side context to
  extract. (If a future need arises to inject W3C headers into ES requests for
  an APM-aware cluster, it is added as an option then.)

This signature **deliberately diverges** from the SDK's usual
`(ctx, …, tp, mp, prop, opts)` shape because the upstream instrumentation only
accepts a `TracerProvider`. Forcing an unused `mp`/`prop` through the facade
would misrepresent what the integration does. The divergence is documented on
the constructor and in this ADR.

### 4. Span attributes (semconv v1.39.0) and the `db.statement` caveat

Populated by the first-party instrumentation:

| Attribute | Level | Notes |
|---|---|---|
| `db.system.name` = `elasticsearch` | Required | constant (the SDK target key; see caveat below) |
| `db.operation.name` | Recommended | endpoint id (e.g. `search`, `bulk`, `index`) |
| `db.collection.name` | Recommended | index, when the endpoint addresses one |
| `db.namespace` | — | not generally applicable to ES |
| `db.elasticsearch.path_parts.<key>` | Conditionally Required | dynamic URL path segments mapped to names from the ES endpoint schema |
| `db.elasticsearch.node.name` | Recommended | from response headers (Elastic Cloud) |
| `url.full` / `server.address` / `server.port` | Recommended | request target |
| `http.request.method` | Recommended | underlying HTTP method |
| `error.type` | Conditionally Required | on failed requests |

Span name follows the DB guidance (`db.operation.name` + target).

**`db.statement` vs `db.query.text` caveat.** The upstream search-body attribute
uses the legacy key `db.statement`; current semconv uses `db.query.text`. Two
options, settled at implementation time against the pinned version:

- **(a) Accept the upstream key** and document the drift in `docs/semconv.md`
  (lowest blast radius; inherits an upstream fix for free when Elastic renames).
- **(b) Normalize at the SDK boundary** — but the instrumentation sets the
  attribute on its own span, so the SDK cannot rewrite span attributes without a
  span-processor hack; this is heavier and fragile.

Recommendation: **(a)**, and likewise verify whether the pinned version emits
`db.system` vs `db.system.name` and record the exact emitted set (ADR 0006
catch-up item) rather than assuming.

### 5. Search-body capture is opt-in

`captureSearchBody` is exposed as `WithSearchBody(enabled bool)`, **default
false**, consistent with `redis.WithCommandTextEnabled`,
`mongo.WithDocumentTracePropagation`, and `cassandra.WithQueryText` (ADR 0019).
Search query bodies can be large and may contain user-supplied search terms
(PII); they are captured only on explicit opt-in, and only for the search-family
endpoints the upstream instrumentation supports.

### 6. Metrics: deferred (v1 trace-only), same posture as NATS

The integration is **trace-only** in v1. No `MeterProvider`, no SDK-owned ES
metrics. This mirrors the NATS decision (ADR 0004 amendment), and contrasts with
MongoDB (ADR 0014) and Cassandra (ADR 0019), for concrete reasons:

| Axis | MongoDB / Cassandra (metrics in v1) | Elasticsearch (deferred) |
|---|---|---|
| Off-the-shelf metric? | Mongo: yes (contrib op-duration). Cassandra: cheap via observer. | **No.** The first-party instrumentation is **trace-only**; any metric is self-written. |
| Client snapshot? | redis `PoolStats()`, mongo `PoolMonitor`, gocql observers | `elastictransport` exposes `WithMetrics()` + `Measurable.Metrics()` returning `{Requests, Failures, Responses[status], Connections}` — a **coarse aggregate**, not a per-endpoint/index latency histogram. |
| Server-side coverage | partial (pool health is a client-only gap) | **rich.** `elasticsearch_exporter` + `_nodes/stats` / `_cluster/health` cover indexing/query/JVM/cache/cluster health, like `prometheus-nats-exporter` covers NATS. |

So ES sits with NATS: no library to lean on, and a strong authoritative
server-side metrics story. Per-call latency is already visible as span duration
in v1.

**Revisit triggers** (open a justified-T3 metrics ADR when any holds):

- A concrete need for **client-attributed operation latency / error rate** that
  `elasticsearch_exporter` cannot satisfy — for this consumer the most likely
  trigger is **per-worker bulk-indexing latency/error rate** in
  `search-sync-worker`, correlated with app metrics in the same backend.
- The first-party instrumentation starts emitting OTel metrics (then it becomes
  a T2 metric, like Mongo Phase 1).

At that point the highest-value target is `db.client.operation.duration` derived
from the instrumentation's `BeforeRequest` / `AfterResponse` seam (or a metrics
`http.RoundTripper` on the transport).

### 7. Out of scope (v1)

- `go-elasticsearch/v9` as a target (follow-up amendment).
- Any SDK-owned ES metrics (§6, deferred).
- Trace-context injection into ES requests (no current need; §3).
- OpenSearch (a forked client/protocol; separate evaluation if a consumer
  appears).

---

## Global-state verification

### Library surveyed: `github.com/elastic/elastic-transport-go/v8` (OTel instrumentation)

### Version: to pin at implementation time (consumer is on `go-elasticsearch/v8 v8.19.3`)

### Expected result: SAFE — to confirm by source read

`NewOtelInstrumentation(provider, …)` stores the supplied `TracerProvider` and
is documented to fall back to the OTel global only when `provider == nil`. The
facade always passes the SDK provider, so the fallback never fires, and no
`otel.SetTracerProvider` call is expected on the constructor path. The
implementing PR must record the exact source-inspection finding (the verification
command and outcome) before this ADR moves to Accepted, per ADR 0003.

---

## Required policy artifacts (ADR 0003 / 0008)

- **`elasticsearch/doc.go`** carries `// Tier: T2 facade over the
  github.com/elastic/go-elasticsearch/v8 first-party OpenTelemetry
  instrumentation`.
- **ADR 0003 Approved-integrations table**: add a row for the
  `elastic-transport-go/v8` OTel instrumentation (version, global-state grep
  result, semconv note) once source inspection is done.
- **ADR 0008 §7.1 gate**: the gate currently keys on
  `go.opentelemetry.io/contrib/...` and the corporate prefix. The ES
  instrumentation lives under `github.com/elastic/...`, so the gate's matched-
  prefix set is extended (or the row requirement is satisfied via the ADR 0003
  table) in the implementing PR — decide which when wiring the gate.
- **ADR 0008 §7.2 gate**: add `elasticsearch` to `integrationDirs`.
- **ADR 0008 §5**: add an "Elasticsearch → T2 over the go-elasticsearch
  first-party OTel instrumentation (trace-only; metrics deferred per ADR 0004
  posture)" row (amendment applied alongside this ADR).

---

## Testing

- `NewClient` rejects nil `tp`.
- A request against a stub/`httptest` ES endpoint emits one CLIENT span with
  `db.system.name=elasticsearch` (or the upstream's emitted key — assert the
  actual value, §4), `db.operation.name`, and `db.elasticsearch.path_parts.*`
  for an index-addressing endpoint.
- Search-body attribute is absent by default and present under
  `WithSearchBody(true)`.
- A failed request sets `error.type` / records the span error.
- Provider wiring: the facade passes the SDK `TracerProvider` (assert spans land
  on the supplied provider, not the global) and never calls `otel.SetX`.
- A compatibility test pins the **exact attribute keys** the upstream emits
  (especially `db.statement` vs `db.query.text`, `db.system` vs
  `db.system.name`) so an upstream bump that changes them is caught (ADR 0006).
- Integration test (build-tagged, `testcontainers-go` Elasticsearch, matching
  the consumer's `testutil.Elasticsearch`) for the healthy bulk + search paths;
  out of default `go test ./...`.

---

## Consequences

**Positive**

- ES calls become first-class Tempo spans with the best-possible maintenance
  signal — first-party instrumentation that tracks the client's semconv with no
  separate library to pin or re-audit.
- Minimal SDK-owned code: wiring + defaults only.
- Search bodies are off by default; opt-in is one option, consistent across the
  DB integrations.
- The trace-only scope is honest and small; metrics are added later only with a
  concrete client-attribution need, avoiding the "false coverage" trap NATS
  identified.

**Negative / Trade-offs**

- The facade signature diverges from the SDK norm (`tp` only, no `mp`/`prop`)
  because the upstream API does so; this must be documented to avoid surprise.
- An upstream attribute-key drift (`db.statement`, possibly `db.system`) is
  inherited rather than corrected at the boundary; the compatibility test makes
  the drift visible, and `docs/semconv.md` documents it.
- No client-side ES metrics in v1; operators rely on `elasticsearch_exporter`
  for ES health and on span duration for per-call latency until a metrics ADR is
  opened.
- We take on upstream behavioral-change exposure (an instrumentation bump could
  rename attributes); pinned-version compatibility tests mitigate this (ADR 0008
  §6 quarterly health check).

---

## Open questions

1. `db.statement` handling — confirm option (a) accept-and-document vs (b)
   boundary-normalize against the pinned `elastic-transport-go` version (§4).
2. Does `search-sync-worker`'s bulk-indexing path want client-attributed metrics
   soon enough to fold a justified-T3 metrics layer into v1, or is span duration
   sufficient until a backend dashboard need appears (§6 revisit trigger)?
3. Confirm the gate-wiring choice for the `github.com/elastic/...` instrumentation
   prefix (extend the §7.1 matched set vs rely on the ADR 0003 row).
