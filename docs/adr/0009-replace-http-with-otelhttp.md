# ADR 0009 — Replace `http/` with `otelhttp` Facade

**Status**: Accepted
**Date**: 2026-05-08

**Supersedes parts of** `http/middleware.go` (commit history); **applies** ADR 0008.

---

## Context

`http/middleware.go` was added to the SDK without an explicit decision
on whether to wrap an existing OTel HTTP instrumentation library or
write one from scratch. The current implementation:

- Reimplements ResponseWriter wrapping with a 7-variant switch to
  preserve `http.Flusher` / `http.Hijacker` / `io.ReaderFrom`
  feature-detection (`http/middleware.go:195-285`).
- Implements its own panic recovery and re-raise discipline.
- Records a single histogram (`http.server.request.duration`) with
  `http.request.method`, `http.route`, `http.response.status_code`.
- **Does not create a server-side OTel span.** Inbound `traceparent`
  headers are not extracted; the trace tree is broken at the HTTP
  ingress.
- Implements a `pathLimiter` (`http/middleware.go:291-320`) to defend
  against unbounded `http.route` cardinality. This defense is needed
  because the middleware operates at `http.Handler` level with no
  route concept and falls back to `r.URL.Path`.

ADR 0008 has now established that ecosystem instrumentation libraries
are the default (T2 facade) and self-writing requires explicit
justification. Re-evaluating `http/middleware.go` against the ADR 0008
§2 checklist for `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`:

| Item | Result |
|---|---|
| ADR 0003 compliance | ✅ Reads globals as fallback only; `WithTracerProvider` / `WithMeterProvider` / `WithPropagators` options bypass the fallback. |
| Maintenance signal | ✅ Maintained by OpenTelemetry contrib. |
| Semconv alignment | ✅ Emits stable HTTP attributes through the pinned contrib version. |
| Configurability | ✅ Span name formatter, attribute filters, and metric attribute filter all overridable. |
| Framework signal access | ✅ At handler level there is no extra signal beyond what `otelhttp` exposes. Route extraction for stdlib is via `r.Pattern` (Go 1.22+) or a router-supplied normalizer. |

All five items pass. There is no justification for keeping `http/` as
a T3 self-written package. This ADR decides to replace it.

The blocker that keeps appearing during current SDK adoption attempts —
a service already uses `otelgin` and `genprom`, and our `http/` either
duplicates or conflicts with them — is a direct consequence of the
prior self-build choice. Adopting `otelhttp` removes the conflict
because services already running `otelhttp` (the most common pattern)
align naturally with the SDK.

Relevant existing files:

- `http/middleware.go`, `http/middleware_test.go` — to be removed
- `examples/basic/`, `examples/metrics/` — currently demonstrate `http.New`; to be migrated
- `o11y.go` — exposes `MeterProvider()` and `TracerProvider()` already
- ADR 0006 — semconv v1.39.0 baseline
- ADR 0008 — sourcing policy

---

## Decisions

### 1. Replace `http/middleware.go` with a thin facade over `otelhttp`

The new package shape:

```go
// Package http provides server- and client-side HTTP instrumentation
// for the o11y SDK. It is a Tier-2 facade over
// go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp,
// configured with the SDK's TracerProvider, MeterProvider, and
// Propagator. See docs/adr/0008 (sourcing policy) and 0009.
package http

// NewServerHandler wraps next with otelhttp.NewHandler and the SDK's
// providers. operation is the span name root used by otelhttp for
// requests where no route is known; passing the empty string disables
// the prefix.
func NewServerHandler(
    next http.Handler,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    operation string,
    opts ...Option,
) http.Handler

// NewTransport wraps base with otelhttp.NewTransport for outbound
// HTTP. Pass http.DefaultTransport (or a customized transport) as
// base.
func NewTransport(
    base http.RoundTripper,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) http.RoundTripper
```

`Option` re-exports a curated subset of `otelhttp.Option` (span name
formatter, public-endpoint flag, filter, metric attribute filter)
under the SDK's own option type so that callers do not need to depend
on `otelhttp` directly.

The wrapper does not add ResponseWriter wrapping, panic glue, or
status capture. `otelhttp` already does these correctly and has more
test coverage for edge cases (Hijacker, Flusher, Pusher, ReaderFrom,
trailers) than we can reasonably maintain.

### 2. Cardinality control moves to the metrics pipeline

The `pathLimiter` is removed. Cardinality discipline is implemented
once at SDK init and applies to every instrumentation library that
emits `http.route`-shaped attributes (`http`, `gin`, future `chi`).

Three layers are registered **automatically by `o11y.Init`** (not
caller opt-in). The `o11y.WithDisableDefaultViews()` and
`o11y.WithMaxUniqueRoutes(int)` options exist for callers who need to
override; the defaults assume callers want cardinality protection.

**Layer A — attribute allowlist views** (default on, applied to both
server and client HTTP histograms):

```go
// Registered internally by o11y.Init; shown here as conceptual API.
sdkmetric.NewView(
    sdkmetric.Instrument{Name: "http.server.request.duration"},
    sdkmetric.Stream{
        AttributeFilter: attribute.NewAllowKeysFilter(
            "http.request.method", "http.route", "http.response.status_code",
        ),
    },
)
sdkmetric.NewView(
    sdkmetric.Instrument{Name: "http.client.request.duration"},
    sdkmetric.Stream{
        AttributeFilter: attribute.NewAllowKeysFilter(
            "http.request.method", "server.address", "server.port",
            "http.response.status_code", "error.type",
        ),
    },
)
```

The server-side allowlist trims to method/route/status. The client-side
allowlist trims to method/server.address/server.port/status/error.type
and deliberately excludes `http.route` (path/route on client metrics
is opt-in per ADR 0011 §3, where the caller takes responsibility for
the keyspace via `WithRouteFromContext`).

`server.port` is included for the client allowlist because in many
deployment topologies (sidecars sharing a host, local dev with
multiple services on `127.0.0.1`, internal services behind a single
cluster IP) `server.address` collapses to identical values across
distinct downstreams; the port is the actual differentiator. Port
cardinality is bounded (a service typically calls a small set of
known ports) so adding the label does not threaten the cardinality
budget.

This trims the attribute set produced by any upstream lib down to the
keys the SDK considers cardinality-safe. `url.full`, `client.address`,
`network.protocol.version`, and similar high-cardinality or
low-signal attributes that some upstream libs emit on metrics by
default get filtered before they reach the exporter.

**Layer B - SDK aggregation cardinality budget** (default on, derived
from `o11y.WithMaxUniqueRoutes(int)`):

The OTel Go SDK does not expose a public hook that can mutate an
attribute value after the view filter and before aggregation:

- `sdkmetric.Reader` has unexported methods, so external wrappers
  cannot be registered with `sdkmetric.WithReader`.
- `sdkmetric.Stream.AttributeFilter` can keep or drop keys, but cannot
  rewrite `http.route` to another value.
- The SDK does not expose a custom aggregator plugin point.

Therefore in-process memory protection uses the supported
`sdkmetric.WithCardinalityLimit` hook. The SDK derives a per-instrument
datapoint budget from `WithMaxUniqueRoutes` and applies it to the
MeterProvider. If a service records more distinct attribute sets than
that budget, OTel aggregates the excess into the SDK-defined overflow
series `otel.metric.overflow=true`. This is intentionally lower-level
than `http.route`: it protects the aggregator map even for future
misconfigured instruments, at the cost of losing label detail once the
overflow guard trips.

**Layer C - export-boundary `http.route` presentation cap** (default
on, with `o11y.WithMaxUniqueRoutes(int)` to override):

For Prometheus pull and OTLP push, the SDK wraps the export boundary.
For each `http.route` value that still reaches export, after the
configured cap the wrapper rewrites the attribute to the literal
`"other"` before forwarding the data point. The cap is per
`(instrument name, attribute key)` pair so different histograms do not
share a budget.

The export-boundary implementation lives in `internal/metricscap`.
Tests cover: under-cap pass-through, at-cap collapse, attribute set
re-hashing on collapse, Prometheus histogram merge, and concurrency.
This layer keeps Prometheus/Grafana queryability stable. It is not the
memory defense; the SDK aggregation cardinality budget above is.

The cap's existence is **defense in depth**. Framework-aware
instrumentation already keeps `http.route` bounded by the route table.
The cap protects against pathological cases (a 404 handler that uses
`r.URL.Path` as its `http.route`, a misconfigured custom span name
formatter) without the SDK having to police every middleware.

### 3. Migration

`http.New` is removed in the same PR that adds `NewServerHandler` /
`NewTransport`. No deprecation period. Justification: the SDK has no
external consumers; a deprecation period serves no one and prolongs
two-API confusion in examples and docs.

In-tree consumers updated in the same PR:

- `examples/basic/main.go` — switch to `NewServerHandler`
- `examples/metrics/main.go` — switch to `NewServerHandler`
- `README.md` — update "Using the SDK" section
- `AGENTS.md` — update package layout section
- `CHANGELOG.md` — record the breaking change

### 4. Compliance with ADR 0003

`otelhttp` reads `otel.GetTracerProvider()` / `otel.GetMeterProvider()` /
`otel.GetTextMapPropagator()` only as fallback when the corresponding
options are not supplied. The facade always supplies all three:

```go
return otelhttp.NewHandler(next, operation,
    otelhttp.WithTracerProvider(tp),
    otelhttp.WithMeterProvider(mp),
    otelhttp.WithPropagators(prop),
    otelhttp.WithSpanNameFormatter(defaultServerSpanName),
    // user-supplied opts last so they can override SDK defaults
    convertedOpts...,
)
```

The ADR 0003 §"Approved integrations" table is updated in the same PR:

| Library | Version | Verified | Behavior | Notes |
|---|---|---|---|---|
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.68.0 | ✅ | Reads globals as fallback only; never sets. | See ADR 0009 |

### 5. Migration checklist (ship in the implementation PR)

This ADR records a behavioral substitution, not just a code swap.
The implementation PR must verify each item below and attach the
results to its description before merge. Items are grouped by the
three observability data planes that downstream consumers
(dashboards, alerts, log queries) bind to.

**Spans** — `otelhttp` and the old `http/` middleware differ in span
emission:

- [ ] `otelhttp` **creates** a server span; old `http/` did not.
      Confirm the trace tree now includes the entry-point span. Logs
      from `slog.InfoContext(r.Context(), ...)` inside handlers must
      carry the **upstream's `traceId`** (continuity across services
      is the whole point of `traceparent` propagation) and the
      **new server span's `spanId`** (fresh span, fresh id). For
      ingress requests with no upstream `traceparent`, both ids are
      newly generated by the server span.
- [ ] Default span name. `otelhttp` uses the `operation` argument as
      span name root, optionally augmented by the route via the span
      name formatter. Verify the chosen formatter produces names
      with bounded cardinality (`{METHOD} {route}` for matched
      routes, `{METHOD}` otherwise) and document the choice in
      `examples/basic/`.
- [ ] Span attributes. Capture the attribute set produced by
      `otelhttp` for one matched-route request, one 404, one 500,
      and one panicked handler; compare against the old set
      (`http.request.method`, `http.route`, `http.response.status_code`).
      Differences become a row in the migration notes; user-visible
      changes go into CHANGELOG.

**Metrics** — `otelhttp` and old `http/` both emit
`http.server.request.duration`, but the attribute set and bucket
boundaries can differ:

- [ ] Histogram name and unit are unchanged
      (`http.server.request.duration`, seconds). If `otelhttp`'s
      pinned version uses a different name or unit, the
      `metric.View` from §2 renames before export so existing
      Prometheus queries continue to match.
- [ ] Default histogram boundaries. Compare `otelhttp`'s default
      bucket boundaries to the OTel SDK's default; document any
      change. If the new boundaries materially shift latency
      percentiles, configure a `metric.View` to override.
- [ ] Attribute keyspace under the allowlist view. Run a smoke
      service through the migration and dump
      `:2112/metrics`; the label set on `http_server_request_duration_seconds`
      must equal `{method, route, status_code}` exactly. No
      `url_full`, `client_address`, `network_protocol_version`, or
      similar high-cardinality leaks.
- [ ] Route cap behavior. Force the export route cap by issuing
      requests against `MaxUniqueRoutes + 5` distinct synthetic paths;
      verify that the first N appear with their own labels and the
      rest collapse to `route="other"` without dropping samples. Also
      force the lower SDK cardinality budget and verify excess
      datapoints aggregate under `otel.metric.overflow=true`.

**Queryability and dashboards** — the migration must not silently
break downstream queries:

- [ ] Loki LogQL: log lines emitted from inside handlers retain the
      same shape (`traceId`, `spanId`, `service.name`, the JSON keys
      from `slog`'s structured fields). Run a representative log
      query before and after.
- [ ] Tempo TraceQL: at least one search clause used in existing
      dashboards (e.g. `{ name = "GET /users/:id" && status = error }`)
      still returns the expected traces. The span-name formatter
      choice is the load-bearing factor here.
- [ ] Grafana panels referencing `http_server_request_duration_seconds`:
      the rate, p95, error-rate panels in the example Grafana
      provisioning produce non-empty results against a smoke service
      after the migration.
- [ ] Alerts: any alert rule referencing the metric or its labels
      continues to fire on the same conditions. The default
      cardinality allowlist (`method`, `route`, `status_code`)
      preserves the most common alert shapes; if an alert depends on
      a now-filtered label, the migration PR explicitly extends the
      allowlist or deletes the alert.

**Code paths removed** — confirm the old behaviors gone with `http/`
are either replaced or intentionally dropped:

- [ ] ResponseWriter wrapping (`recF`/`recH`/`recR`/...).
      `otelhttp` handles `http.Flusher`, `http.Hijacker`, `io.ReaderFrom`,
      `http.Pusher`, and trailers. Smoke-test SSE and WebSocket
      upgrade paths through a wrapped handler.
- [ ] Panic re-raise. `otelhttp` records the panic on the span and
      re-panics so `http.Server`'s default recovery still runs.
      Confirm with a panicking handler test.
- [ ] `WithPathNormalizer` callers in `examples/`. Each callsite is
      either deleted (route comes from `r.Pattern`) or rewritten to
      use `otelhttp.WithSpanNameFormatter`.

The PR description embeds this checklist with each item ticked, plus
the captured before/after attribute and metric snapshots.

---

## Rationale

### What we lose by removing `http/middleware.go`

The current implementation has three differentiators worth examining:

1. **Cardinality cap (`pathLimiter`).** Replaced by a stronger,
   cross-cutting mechanism at the MeterProvider. Net upgrade.
2. **7-variant ResponseWriter feature detection.** `otelhttp` does
   this correctly already (and additionally handles `http.Pusher`,
   trailers, and `http.NewResponseController` interactions that our
   code does not). Net upgrade.
3. **Logging instrument-creation failure to slog instead of returning
   error.** `otelhttp` panics on instrument creation failure, which is
   acceptable: instrument creation failure indicates programmer error
   (duplicate name with conflicting type), not runtime degradation.
   The SDK's slog fallback was over-engineered. Net neutral.

What we lose:
- The bespoke `WithPathNormalizer` callback. Replaced by `otelhttp`'s
  span name formatter and Go 1.22+ `r.Pattern` for stdlib mux. For
  routers without a native pattern (chi v4), router-specific glue
  (e.g. `otelchi`) covers it.

### Why no deprecation period

The SDK has not been adopted by any service yet. The team currently
attempting first integration is the audience for this ADR; serving
them is better done by giving them the final API now than by giving
them a transitional one. Future SDK consumers benefit from finding
exactly one HTTP API on first read.

### Why move cardinality control to the metrics pipeline

Three reasons:

1. **DRY across instrumentation packages.** A cap implemented only in
   the `http/` package would not protect metrics emitted by `gin/`,
   `chi/`, or future router integrations. A MeterProvider cardinality
   budget protects every instrument before aggregation.
2. **Cap independence from instrumentation.** The cap is a property
   of the metrics pipeline (the SDK cannot afford unbounded
   in-process datapoints, and Prometheus cannot afford >N route
   series), not of any one HTTP middleware. Co-locating both guards
   with the pipeline reflects the actual ownership.
3. **Reuse for non-HTTP integrations.** `messaging.destination.name`
   in NATS subjects, `db.collection.name` in MongoDB — the same
   keyspace-explosion failure mode applies. The SDK cardinality budget
   generalizes; export-boundary presentation caps can then be added
   where the backend has a stable label to rewrite.

---

## Consequences

**Positive**

- ~320 lines of self-maintained HTTP middleware deleted, replaced by
  ~50 lines of facade plus a metrics-pipeline cardinality budget and
  export-boundary route presentation cap.
- HTTP server tracing works for the first time — inbound
  `traceparent` is extracted, server spans are created and propagated
  to handlers, slog ↔ trace correlation works at the entry point.
- HTTP client tracing is now in scope (`NewTransport`) without
  extra design work.
- Consumer services already running `otelhttp` align naturally with
  the SDK; they configure providers via `o11y.Init` and stop their
  own `otel.SetX` setup.
- In-process cardinality control becomes uniform and applies to every
  future integration without per-package reimplementation.
- Exported HTTP server metrics keep the queryable `http.route="other"`
  bucket for ordinary route overflow on both Prometheus and OTLP paths.

**Negative / Trade-offs**

- Hard breaking change to `http.New`. Acceptable while the SDK has no
  external consumers.
- The `WithPathNormalizer` callback API is gone; users with
  non-stdlib, non-popular routers must supply route extraction
  themselves via `otelhttp` options. We will document the chi and
  echo recipes in the README to reduce friction.
- The export-boundary route presentation cap is a new piece of code we
  own. Its surface area is small and it is testable in isolation. It
  is still self-written code, but it is cross-cutting infrastructure,
  not instrumentation, and properly belongs to T1 (Core SDK) under
  ADR 0008's tier model.
- If the SDK cardinality guard trips before the export-boundary route
  cap, OTel preserves totals under `otel.metric.overflow=true` and
  route detail is intentionally lost for the overflowed datapoints.

---

## Open questions

- **Default cap value.** `DefaultMaxUniqueRoutes = 1000` is the
  current value. Keep it, or revisit based on production label
  budgets? Lean: keep 1000 until concrete evidence demands change.
- **Per-attribute cap configuration.** The export-boundary cap supports a
  per-attribute export budget. Should we expose
  `WithAttributeCap(string, int)` or a single
  `WithDefaultAttributeCap(int)` for simplicity? Lean: start with the
  global default and add per-attribute as concrete needs arise.
- **Eviction policy when cap is hit.** "First N unique values stick,
  the rest collapse to 'other'" is the simplest. An alternative is
  LRU. Lean: stick with first-N (matches current `pathLimiter`
  behavior, predictable cardinality, no eviction churn in metrics).
