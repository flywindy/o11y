# ADR 0002 — Metrics Strategy

**Status**: Accepted  
**Date**: 2026-04-18

---

## Context

The o11y SDK needs a metrics pillar that satisfies two requirements simultaneously:

1. **OTel Semantic Conventions compliance** — instrument names, attribute keys, and attribute
   types must match the pinned OTel semconv version so that dashboards and alerts written against the
   specification work without manual field mapping.
2. **Operational simplicity** — the chosen export path must work out of the box with the
   existing Grafana stack without requiring additional OTel Collector pipeline configuration
   beyond what traces already use.

Three export strategies were considered:

| Option | Description | Trade-off |
|--------|-------------|-----------|
| **A — OTLP metrics** | Export metrics via OTLP/HTTP, same path as traces | Requires Collector → Prometheus remote_write or Mimir pipeline; additional config |
| **B — Prometheus pull** | Expose `/metrics` scrape endpoint; Prometheus scrapes it | Native Prometheus workflow; zero Collector involvement for metrics |
| **C — Hybrid** | OTLP for traces/logs, Prometheus for metrics | Matches existing ops tooling; each signal uses its natural egress format |

The team selected **Option C (Hybrid)** for the metrics path.

---

## Decisions

### 1. Prometheus pull model for metrics; OTLP for traces and logs

The existing Grafana stack runs Prometheus. Routing metrics through the OTel Collector into
Prometheus (via remote_write or Mimir) adds an unnecessary hop and requires non-trivial
Collector receiver/exporter configuration. A dedicated `/metrics` HTTP endpoint (default
`:2112`) lets Prometheus scrape the process directly with no collector involvement.

Traces and logs continue to flow through the OTel Collector as before.

### 2. Private Prometheus registry — no global state

`metrics.InitMeter` creates its own `prometheus.Registry` rather than using
`prometheus.DefaultRegisterer`. This is consistent with the **Zero Global State** core
principle: multiple SDK instances (e.g. in tests) cannot interfere with each other through
shared Prometheus state.

### 3. Shared OTel Resource across all three providers

All three providers — `TracerProvider`, `MeterProvider`, and `LoggerProvider` — are
initialized with the same `*resource.Resource` built once by `buildResource()` in
`o11y.Init`. This guarantees that `service.name`, `service.version`,
`deployment.environment.name`, and `service.namespace` are byte-for-byte identical across all signals,
enabling accurate correlation in Grafana's unified explore view.

`metrics.InitMeter` accepts an optional `Config.Resource`; when provided it is used
directly. When `nil` (standalone use in tests), the function builds its own resource from
the remaining Config fields.

### 4. `service.namespace` is a required resource attribute, promoted to a constant Prometheus label

Every metric series carries a `service_namespace` label. This enforces ownership governance:
SRE alert routing and billing attribution require every series to be unambiguously owned.
`Init` returns an error when `WithServiceNamespace` is omitted.

The Prometheus exporter's `WithResourceAsConstantLabels` filter promotes `service.namespace`,
`service.name`, `service.version`, and `deployment.environment.name` from the Resource into
constant labels on every series, including runtime metrics started by
`go.opentelemetry.io/contrib/instrumentation/runtime`.

### 5. Instrument naming and attribute types follow pinned OTel semconv

All instruments and their attributes must conform to the OTel Semantic Conventions
specification at the repository pin, currently **v1.39.0**. Key rules:

| Signal | Rule |
|--------|------|
| Instrument names | Dot-separated OTel names; Prometheus exporter converts dots to underscores automatically (e.g. `http.server.request.duration` → `http_server_request_duration_seconds`) |
| `http.response.status_code` | **`attribute.Int`**, not `attribute.String` — the semconv type is `int` |
| `http.request.method` | `attribute.String` |
| `http.route` | `attribute.String`; must be a normalized route template, never a raw URL path |
| `deployment.environment.name` | Current canonical deployment environment resource key |

### 6. HTTP middleware emits one histogram; `_count` doubles as traffic and error counter

`http.New` records `http.server.request.duration` (seconds, Float64Histogram). The
`_count` series—broken down by `http.response.status_code`—functions as both a traffic
counter and an error rate denominator without requiring a separate counter instrument. This
matches the OTel HTTP server semantic conventions' "Golden Signal" recommendation.

### 7. Cardinality protection is mandatory for all label dimensions with unbounded input

`http.route` is the canonical example: without normalization, every unique URL path becomes
a distinct series. The SDK enforces layered protection:

1. Framework-aware instrumentation must emit route templates rather than raw URL paths.
2. SDK-managed views drop high-cardinality HTTP metric labels before aggregation.
3. A hard export cap (`DefaultMaxUniqueRoutes = 1000`, overridable via
   `WithMaxUniqueRoutes`) collapses additional unseen routes to the literal label
   `"other"` where route labels still reach export.
4. The OTel SDK cardinality limit protects in-process aggregation memory when an
   instrument still records too many distinct attribute sets.
5. Callers may extend the `http.server.request.duration` allow-list via
   `WithExtraHTTPServerAttributeKeys(...)` to keep additional caller-controlled
   dimensions (e.g. `app_name`, `bot_name`) on the series. Extending the
   allow-list bypasses layer (2) for the listed keys, so the caller owns
   the cardinality budget for them and must restrict values to an
   enumerable, bounded keyspace.

**Any new label dimension that can grow without bound must apply equivalent protection.**
User IDs, request IDs, trace IDs, and similar high-cardinality values must never appear as
metric label values.

### 8. Runtime metrics are enabled by default

`WithRuntimeMetrics(true)` is the default. Go runtime metrics (goroutines, GC pause, heap
allocations, scheduler latency) cover the **Saturation** golden signal automatically. Teams
that need to disable them for controlled benchmark environments can set
`WithRuntimeMetrics(false)`.

### 9. Pinned histogram boundaries for HTTP server latency

`DefaultLatencyBuckets` (`[5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s, 2.5s, 5s, 10s]`)
are applied specifically to `http.server.*` histograms via an OTel View. User-authored
histograms retain their default exponential boundaries. Standardising HTTP boundaries across
services keeps P99 comparisons directly comparable in Grafana.

---

## Consequences

**Positive**
- Metrics are immediately scrapeable by any Prometheus-compatible system with zero Collector
  configuration change.
- Service identity labels are guaranteed to be consistent across all three observability
  signals.
- OTel semconv compliance means community dashboards (e.g. the official OTel HTTP dashboard)
  work without field remapping.

**Negative / Trade-offs**
- Two distinct egress paths (OTLP for traces/logs, Prometheus pull for metrics) means metrics
  do not flow through the OTel Collector and cannot benefit from Collector-level processing
  (sampling, batching, enrichment). This is an intentional simplicity trade-off.
- The separate metrics HTTP port (`:2112`) must be opened in any firewall or Kubernetes
  `NetworkPolicy` that restricts pod-to-pod traffic.
- Shared Resource means `service.namespace` appears as a resource attribute on traces and logs as well,
  which is desirable for correlation but adds a field that some log consumers may not expect.

---

## Amendment (2026-07-29) — §7 addendum: schema-level labels are bounded, not banned

§7 states the cardinality rule in terms of dimensions with *unbounded* input
(`http.route`, user ids, request ids). It was being read more broadly than
written — as a presumption against any label with more than a handful of values —
and that reading kept a semconv-required attribute off the Cassandra query
metrics (see ADR 0019's 2026-07-29 amendment). This addendum draws the line
explicitly.

**A schema-level label is a distinct category.** Names of database tables,
collections, buckets, or topics are fixed by DDL or configuration, not by request
input. Their value space is set by a deployment's schema, not by its traffic. The
existing integrations already treat them this way — MinIO carries
`object_store.bucket.name` on its operation metric (ADR 0018) — so this is
codifying practice, not creating an exception.

Such a label is **admissible as a metric label** when all four hold:

1. **semconv asks for it.** It is Required, Conditionally Required, or
   Recommended on the instrument, and any condition attached to it is satisfied.
2. **The value space is deployment-fixed**, not request-derived.
3. **The value's provenance is trustworthy** — read from configuration or a
   driver-supplied field, or parsed by a parser whose failure mode is to *omit*
   the label rather than to emit a guess.
4. **A cap layer exists** — an export-boundary cap collapsing overflow to
   `"other"` (§7 layer 3), sized to the expected schema rather than to the
   traffic. The cap is required even when (2) and (3) hold, because it converts
   a parser or configuration defect from an unbounded-cardinality incident into a
   visible, bounded `"other"` bucket.

An allow-keys view (§7 layer 2) does **not** satisfy (4): it bounds which keys
reach the series, not how many values a key takes. The two layers are
complementary and both are needed.

**Default posture.** Where the four conditions hold, the label ships **on by
default**, because a Conditionally-Required attribute gated behind an opt-in flag
is a conformance gap that only the people who already know about it can close.
Integrations expose a per-instance opt-*out* for callers who would rather not
carry the series. Where a condition fails, the label is omitted for that
observation — omission is the correct expression of a semconv condition not being
met, and is never a substitute for the cap in (4).

**Unchanged.** §7's prohibition is untouched for genuinely unbounded dimensions:
user ids, request ids, trace ids, raw URL paths, per-request subjects and reply
inboxes must never be metric label values, regardless of any cap.
