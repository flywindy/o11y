# ADR 0020 — Elasticsearch Integration

**Status**: Accepted
**Date**: 2026-06-04 (accepted 2026-06-14, on implementation)
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
semantic-convention attributes (the now-deprecated namespace — see §4) with
`SpanKindClient`, captures dynamic URL path parts, and extracts Elastic Cloud
cluster id / node name from response headers.

### ADR 0008 §2 evaluation — passes T2

| Checklist item | Result |
|---|---|
| §2.1 ADR 0003 compliance | **Pass (to confirm by source read).** Takes a `trace.TracerProvider` explicitly; falls back to the OTel global only when `nil` is passed. Our facade always passes the SDK provider, so the fallback never fires. |
| §2.2 Maintenance signal | **Pass.** First-party, maintained by the client vendor itself — the best signal of all. |
| §2.3 Semconv alignment | **Pass with a documented drift.** The pinned `elastic-transport-go/v8` (≈ v8.8.0 for the consumer's `go-elasticsearch/v8 v8.19.3`) predates DB stabilization and is expected to emit **legacy core** keys — `db.system` and `db.operation` (not `db.system.name` / `db.operation.name`), `db.statement` (not `db.query.text`) — plus the deprecated `db.elasticsearch.*` namespace (`node.name` → `elasticsearch.node.name`, `cluster.name` → `db.namespace`, `path_parts.*` → `db.operation.parameter.*`). Accepted and documented per §4 (same posture as the Mongo T2 attribute exception); a compatibility test pins the exact emitted keys. |
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

// Typed API: go-elasticsearch v8 keeps the typed client behind a separate
// constructor; it is not reachable from *elasticsearch.Client. The facade
// offers a parallel constructor so typed-API call sites are not forced back
// to the low-level API. Both wire the same instrumentation into the shared
// elastictransport.
func NewTypedClient(
    cfg elasticsearch.Config,
    tp trace.TracerProvider,
    opts ...Option,
) (*elasticsearch.TypedClient, error)

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

### 4. Span attributes (semconv v1.39.0) and the upstream attribute-drift caveat

The **target** (current v1.39.0) keys are below. Note that the whole
`db.elasticsearch.*` namespace was deprecated and dispersed in the DB semconv
stabilization, exactly as `db.cassandra.*` was (ADR 0019):

| Target attribute (v1.39.0) | Level | Notes / deprecated predecessor |
|---|---|---|
| `db.system.name` = `elasticsearch` | Required | constant (assert the emitted value — see caveat) |
| `db.operation.name` | Recommended | endpoint id (e.g. `search`, `bulk`, `index`) |
| `db.collection.name` | Recommended | **NOT emitted by upstream** — see ‡ |
| `db.namespace` | Conditionally Required | ES cluster name — **replaces deprecated `db.elasticsearch.cluster.name`** |
| `db.operation.parameter.<key>` | Conditionally Required | dynamic URL path segments mapped to names — **replaces deprecated `db.elasticsearch.path_parts.<key>`** |
| `elasticsearch.node.name` | Recommended | node/instance the request was routed to (Elastic Cloud) — **replaces deprecated `db.elasticsearch.node.name`** (no `db.` prefix) |
| `url.full` / `server.address` / `server.port` | Recommended | request target |
| `http.request.method` | Recommended | underlying HTTP method |
| `error.type` | Conditionally Required | **NOT emitted by upstream** — see † |

Span name is emitted by the upstream `elastic-transport-go` instrumentation
and is **not** under facade control: a T2 facade only wires providers and has
no span-name seam (unlike `otelmongo`, which exposes `WithSpanNameFormatter`).
The cross-package convention `{db.system.name}.{operation} {target}` (ADR 0023)
therefore **does not apply** to Elasticsearch in v1; this is a recorded,
accepted divergence (the same class as the §4 legacy-attribute drift). Revisit
if upstream adds a span-name formatter or the facade is promoted to a T3 seam.

**† `error.type` is not set by the pinned instrumentation.** Verified against
`elastic-transport-go/v8 v8.8.0` (`elastictransport/instrumentation.go`):
`RecordError` does only `span.SetStatus(codes.Error, …)` + `span.RecordError(err)`
— it records an exception event and sets the span status, but sets **no**
`error.type` attribute (no such constant exists in the package). A failed request
is therefore observable via span **status = Error** and the recorded exception,
not via an `error.type` attribute. Per the (a) Mongo-T2 posture the SDK accepts
this rather than adding a normalization span-processor in v1; the compatibility
test asserts *status-Error-without-`error.type`*, and `docs/semconv.md` records
the gap. (A backend that needs `error.type` for ES is the trigger to add an
SDK-owned normalization seam — same escalation path as the deferred metrics, §6.)

**‡ `db.collection.name` is not set by the pinned instrumentation either.** The
index is recorded **only** through `RecordPathPart` as the dynamic path variable
`db.elasticsearch.path_parts.index` (current-semconv `db.operation.parameter.index`);
the transport never emits `db.collection.name`. It is listed as the *semconv
target* for completeness, but collection-keyed dashboards/queries will not match
ES spans in v1 unless an SDK-owned seam derives `db.collection.name` from the
`index` path part — out of scope for v1 (same accept-and-document stance as †).

**Upstream attribute-drift caveat.** The first-party `elastic-transport-go`
instrumentation predates this stabilization and is expected to emit the
**deprecated** spellings for both the core and ES-specific keys — `db.system`
(current `db.system.name`), `db.operation` (current `db.operation.name`),
`db.statement` for the search body (current `db.query.text`),
`db.elasticsearch.path_parts.<key>` (current `db.operation.parameter.<key>`),
`db.elasticsearch.node.name` (current `elasticsearch.node.name`), and
`db.elasticsearch.cluster.name` (current `db.namespace`). Because the instrumentation sets
these on its own span, the SDK cannot rewrite them without a span-processor hack
(heavier and fragile). The decision, per the §2.3/§2.4 trade-off:

- **(a) Accept the upstream keys** and document the drift in `docs/semconv.md`,
  inheriting an upstream fix for free when Elastic catches up — **recommended**
  (lowest blast radius), consistent with the Mongo T2 stance on attributes we
  cannot override.
- **(b) Normalize at the SDK boundary** via a span processor — rejected for v1
  as disproportionate.

A compatibility test (§Testing) pins the **exact** keys the pinned version
emits — including `db.system` vs `db.system.name` and each `db.elasticsearch.*`
vs its replacement — so this drift is recorded as fact, not assumed, and any
upstream change is caught (ADR 0006).

### 5. Search-body capture is opt-in

`captureSearchBody` is exposed as `WithSearchBody(enabled bool)`, **default
false**, consistent with `redis.WithCommandTextEnabled`,
and `cassandra.WithQueryText` (ADR 0019).
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

### Version: `elastic-transport-go/v8 v8.8.0` (via `go-elasticsearch/v8 v8.19.3`)

### Result: SAFE — confirmed by source read

Source inspected at `elastic-transport-go/v8@v8.8.0`
`elastictransport/instrumentation.go:87-101`:

```go
func NewOtelInstrumentation(provider trace.TracerProvider, captureSearchBody bool, version string, options ...trace.TracerOption) *ElasticsearchOpenTelemetry {
    if provider == nil {
        provider = otel.GetTracerProvider()
    }
    // ...
    return &ElasticsearchOpenTelemetry{tracer: provider.Tracer(tracerName, options...), recordBody: captureSearchBody}
}
```

`provider` is read from `otel.GetTracerProvider()` **only** when it is `nil`;
otherwise the supplied provider is stored on the tracer and the global is never
touched. The facade rejects a nil `tp` (`elasticsearch/client.go` `instrument`),
so the fallback can never fire. A grep of the package for the forbidden setters
confirms none exist:

```text
$ grep -rn 'otel\.\(SetTracerProvider\|SetTextMapPropagator\|SetMeterProvider\|SetLoggerProvider\)' \
    $(go env GOMODCACHE)/github.com/elastic/elastic-transport-go/v8@v8.8.0
(no matches)
```

`TestProviderWiring` (`elasticsearch/client_test.go`) backs this at runtime: it
sets a sentinel global provider, builds the facade client with a *different*
SDK provider, and asserts every span lands on the supplied provider and the
global records none. The integration is therefore SAFE under ADR 0003.

The same source read confirms the §4 attribute caveats as fact: `AfterRequest`
sets `db.system` / `db.operation` (legacy core keys); `RecordRequestBody` sets
`db.statement`; `RecordPathPart` sets `db.elasticsearch.path_parts.<key>`;
`AfterResponse` sets `db.elasticsearch.cluster.name` / `.node.name`; and
`RecordError` sets only span status `codes.Error` + `RecordError(err)` with no
`error.type`. These are pinned by the compatibility assertions in
`elasticsearch/client_test.go`.

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
  actual value, §4), `db.operation.name`, and the path-parts attribute the
  upstream actually emits (deprecated `db.elasticsearch.path_parts.*` or current
  `db.operation.parameter.*`) for an index-addressing endpoint.
- Search-body attribute is absent by default and present under
  `WithSearchBody(true)`.
- A failed request sets span **status = Error** and records the exception
  (via the upstream `RecordError`); the test asserts there is **no** `error.type`
  attribute, matching the pinned `elastic-transport-go/v8 v8.8.0` behavior (§4 †).
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

_All resolved at implementation (2026-06-14):_

1. `db.statement` handling — **resolved: option (a) accept-and-document.** The
   facade inherits the legacy upstream keys and pins them with a compatibility
   test; `docs/semconv.md` records the drift. No boundary normalization in v1.
2. Client-attributed bulk-indexing metrics — **resolved: deferred (v1
   trace-only).** Span duration is sufficient for now; `elasticsearch_exporter`
   covers ES health. A justified-T3 metrics ADR is the path when a concrete
   per-worker dashboard need appears (§6 revisit triggers).
3. Gate wiring — **resolved: both.** `scripts/check_integrations.go` matches
   `github.com/elastic/go-elasticsearch/v8` and
   `github.com/elastic/elastic-transport-go/v8`, and both have rows in ADR
   0003's Approved-integrations table.
