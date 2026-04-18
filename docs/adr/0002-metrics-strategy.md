# ADR 0002 — Metrics Strategy

**Status**: Accepted  
**Date**: 2026-04-18

---

## Context

The o11y SDK needs a metrics pillar that satisfies two requirements simultaneously:

1. **OTel Semantic Conventions compliance** — instrument names, attribute keys, and attribute
   types must match OTel semconv v1.27.0 so that dashboards and alerts written against the
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
`deployment.environment.name`, and `team` are byte-for-byte identical across all signals,
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

### 5. Instrument naming and attribute types follow OTel semconv v1.27.0

All instruments and their attributes must conform to the OTel Semantic Conventions
specification at version **v1.27.0**. Key rules:

| Signal | Rule |
|--------|------|
| Instrument names | Dot-separated OTel names; Prometheus exporter converts dots to underscores automatically (e.g. `http.server.request.duration` → `http_server_request_duration_seconds`) |
| `http.response.status_code` | **`attribute.Int`**, not `attribute.String` — the semconv type is `int` |
| `http.request.method` | `attribute.String` |
| `http.route` | `attribute.String`; must be a normalized route template, never a raw URL path |
| `deployment.environment.name` | v1.27.0 key (replaces the deprecated `deployment.environment` from v1.26.0) |

### 6. HTTP middleware emits one histogram; `_count` doubles as traffic and error counter

`http.New` records `http.server.request.duration` (seconds, Float64Histogram). The
`_count` series—broken down by `http.response.status_code`—functions as both a traffic
counter and an error rate denominator without requiring a separate counter instrument. This
matches the OTel HTTP server semantic conventions' "Golden Signal" recommendation.

### 7. Cardinality protection is mandatory for all label dimensions with unbounded input

`http.route` is the canonical example: without normalization, every unique URL path becomes
a distinct series. The middleware enforces two layers:

1. A caller-supplied `WithPathNormalizer` collapses dynamic segments to route templates
   (e.g. `/users/123` → `/users/:id`).
2. A hard cap (`DefaultMaxUniquePaths = 1000`, overridable via `WithMaxUniquePaths`) collapses
   any additional unseen route to the literal label `"other"`.

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
- Shared Resource means `team` appears as a resource attribute on traces and logs as well,
  which is desirable for correlation but adds a field that some log consumers may not expect.
