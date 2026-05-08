# ADR 0009 — Replace `http/` with `otelhttp` Facade

**Status**: Proposed
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
| Semconv alignment | ✅ Emits v1.39.0 stable HTTP attributes. |
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

### 2. Cardinality control moves to the MeterProvider via `metric.View`

The `pathLimiter` is removed. Cardinality discipline is implemented
once at SDK init and applies to every instrumentation library that
emits `http.route`-shaped attributes (`http`, `gin`, future `chi`).

Two layers, both opt-in via `o11y.Option`:

**Layer A — attribute allowlist view** (default on):

```go
o11y.WithMetricView(sdkmetric.NewView(
    sdkmetric.Instrument{Name: "http.server.request.duration"},
    sdkmetric.Stream{
        AttributeFilter: attribute.NewAllowKeysFilter(
            "http.request.method", "http.route", "http.response.status_code",
        ),
    },
))
```

This trims the attribute set produced by any upstream lib down to the
keys the SDK considers cardinality-safe. `url.full`, `client.address`,
and similar high-cardinality attributes that some upstream libs emit
on metrics by default get filtered before they reach the exporter.

**Layer B — distinct-value cap on `http.route`** (default on, with
`o11y.WithMaxUniqueRoutes(int)` to override):

A custom `metric.Reader` wrapper interposes between the SDK and the
exporter. For each `http.route` value seen, after the configured cap
the reader rewrites the attribute to the literal `"other"` before
forwarding the data point. The cap is per `(instrument name, attribute
key)` pair so different histograms do not share a budget.

The reader implementation lives at `internal/metricscap/reader.go`.
Tests cover: under-cap pass-through, at-cap collapse, attribute set
re-hashing on collapse, concurrency.

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
    // user-supplied opts last so they can override SDK defaults
    convertedOpts...,
)
```

The ADR 0003 §"Approved integrations" table is updated in the same PR:

| Library | Version | Verified | Behavior | Notes |
|---|---|---|---|---|
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | (pinned) | ✅ | Reads globals as fallback only; never sets. | See ADR 0009 |

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

### Why move cardinality control to the MeterProvider

Three reasons:

1. **DRY across instrumentation packages.** A cap implemented in the
   `http/` package would not protect metrics emitted by `gin/`,
   `chi/`, or future router integrations. A cap at the MeterProvider
   protects all of them at once.
2. **Cap independence from instrumentation.** The cap is a property
   of the metrics pipeline (Prometheus cannot afford >N series), not
   of any one HTTP middleware. Co-locating it with the pipeline
   reflects the actual ownership.
3. **Reuse for non-HTTP integrations.** `messaging.destination.name`
   in NATS subjects, `db.collection.name` in MongoDB — the same
   keyspace-explosion failure mode applies. The reader-level cap
   generalizes.

---

## Consequences

**Positive**

- ~320 lines of self-maintained HTTP middleware deleted, replaced by
  ~50 lines of facade plus ~100 lines of MeterProvider-level cap
  reader (the cap is reused beyond HTTP).
- HTTP server tracing works for the first time — inbound
  `traceparent` is extracted, server spans are created and propagated
  to handlers, slog ↔ trace correlation works at the entry point.
- HTTP client tracing is now in scope (`NewTransport`) without
  extra design work.
- Consumer services already running `otelhttp` align naturally with
  the SDK; they configure providers via `o11y.Init` and stop their
  own `otel.SetX` setup.
- Cardinality control becomes uniform and applies to every future
  HTTP-shaped integration without per-package reimplementation.

**Negative / Trade-offs**

- Hard breaking change to `http.New`. Acceptable while the SDK has no
  external consumers.
- The `WithPathNormalizer` callback API is gone; users with
  non-stdlib, non-popular routers must supply route extraction
  themselves via `otelhttp` options. We will document the chi and
  echo recipes in the README to reduce friction.
- The MeterProvider-level cap is a new piece of code we own. Its
  surface area is small (~100 LOC) and it is testable in isolation.
  It is still self-written code — but it is cross-cutting
  infrastructure, not instrumentation, and properly belongs to T1
  (Core SDK) under ADR 0008's tier model.

---

## Open questions

- **Default cap value.** `DefaultMaxUniquePaths = 1000` was the
  current value. Keep it, or revisit based on production label
  budgets? Lean: keep 1000 until concrete evidence demands change.
- **Per-attribute cap configuration.** The cap reader supports a
  per-attribute budget. Should we expose `WithAttributeCap(string, int)`
  or a single `WithDefaultAttributeCap(int)` for simplicity? Lean:
  start with the global default and add per-attribute as concrete
  needs arise.
- **Eviction policy when cap is hit.** "First N unique values stick,
  the rest collapse to 'other'" is the simplest. An alternative is
  LRU. Lean: stick with first-N (matches current `pathLimiter`
  behavior, predictable cardinality, no eviction churn in metrics).
