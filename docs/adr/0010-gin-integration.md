# ADR 0010 — Gin Integration

**Status**: Proposed
**Date**: 2026-05-08

**Applies** ADR 0008 (sourcing policy); **builds on** ADR 0009
(`otelhttp` facade and metric.View cardinality control).

---

## Context

The SDK needs first-party gin instrumentation. ADR 0008 establishes
that the default sourcing strategy is a T2 facade over a vetted
upstream library; ADR 0009 has already reset `http/` to follow this
pattern. This ADR applies the same policy to gin.

A separate motivating issue arose during early SDK adoption: services
using `c.AbortWithError(...)` push errors into `*gin.Context.Errors`,
which is **not visible** to a `RoundTripper`-level or `http.Handler`-level
middleware. Either the gin instrumentation surfaces `c.Errors`, or the
trace and metric records will silently miss the failure signal.

ADR 0008 §2 checklist applied to
`go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin`:

| Item | Result |
|---|---|
| ADR 0003 compliance | ✅ Reads globals as fallback only; `WithTracerProvider`, `WithPropagators`, `WithMeterProvider` options bypass the fallback. |
| Maintenance signal | ✅ Maintained by OpenTelemetry contrib; releases track gin and OTel SDK semver. |
| Semconv alignment | ✅ Recent versions emit v1.30+ stable HTTP attributes; we pin to a release that emits v1.39.0 to match ADR 0006. If no such release exists at adoption time, we either wait or pin one minor version behind and document the gap. |
| Configurability | ✅ Span name formatter, attribute injector, and filter all overridable. |
| Framework signal access | ⚠️ `otelgin` does **not** populate `c.Errors` onto the span. This is the single gap; it is closed by a small in-tree middleware (see §2). |

Four full passes plus one gap closable in <30 lines of facade code.
T2 adoption is justified.

Relevant existing files / context:

- ADR 0008 — sourcing policy
- ADR 0009 — `otelhttp` facade and metric.View cardinality
- `nats/conn.go` — precedent for "thin facade taking `tp` / `prop`"

---

## Decisions

### 1. Adopt `otelgin` as the upstream and ship a thin facade

Package layout:

```
gin/
├── middleware.go       // Middleware: otelgin.Middleware + ErrorRecorder
├── errors.go           // ErrorRecorder gin.HandlerFunc
├── doc.go              // Tier and policy reference
└── *_test.go
```

Public API:

```go
package gin

import (
    "github.com/gin-gonic/gin"
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/trace"
)

// Middleware returns the canonical chain: otelgin instrumentation
// configured with the SDK's providers, followed by an ErrorRecorder
// that surfaces *gin.Context.Errors onto the active server span.
//
// The returned slice is intended to be spread into r.Use:
//
//   r.Use(o11ygin.Middleware(serviceName, tp, mp, prop)...)
//
// For callers who want finer control (different ordering, custom
// otelgin options), use otelgin directly with WithTracerProvider /
// WithMeterProvider / WithPropagators and add ErrorRecorder()
// separately.
func Middleware(
    service string,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) []gin.HandlerFunc

// ErrorRecorder returns a gin.HandlerFunc that records any errors
// pushed via c.Error / c.AbortWithError onto the active OTel server
// span using span.RecordError, and sets the span status to Error
// when len(c.Errors) > 0. It must run after the otelgin middleware
// in the chain so that a server span exists in c.Request.Context().
func ErrorRecorder() gin.HandlerFunc
```

`Option` exposes a curated subset:

- `WithSpanNameFormatter(func(*http.Request) string)` — passes through to otelgin
- `WithFilter(func(*http.Request) bool)` — passes through to otelgin
- `WithMetricAttributesFn(...)` — passes through

### 2. ErrorRecorder semantics

Pseudocode (concrete implementation in PR):

```go
func ErrorRecorder() gin.HandlerFunc {
    return func(c *gin.Context) {
        c.Next()
        if len(c.Errors) == 0 {
            return
        }
        span := trace.SpanFromContext(c.Request.Context())
        if !span.SpanContext().IsValid() {
            return // no otelgin span; ErrorRecorder is no-op
        }
        for _, ge := range c.Errors {
            span.RecordError(ge.Err,
                trace.WithAttributes(
                    attribute.String("gin.error.type", ge.Type.String()),
                ),
            )
        }
        // OTel semconv: server-side 5xx is Error; for 4xx we still
        // record the errors above but leave status Unset to match
        // ADR 0009 / otelgin convention.
        if c.Writer.Status() >= 500 {
            span.SetStatus(codes.Error, c.Errors.Last().Error())
        }
    }
}
```

`gin.ErrorType` values (`ErrorTypeBind`, `ErrorTypeRender`,
`ErrorTypePublic`, `ErrorTypePrivate`, `ErrorTypeAny`) map to the
`gin.error.type` attribute as the type's string form. This is gin-
specific; it is intentionally **not** mapped to OTel's `error.type`
attribute because OTel's `error.type` is for transport / protocol
class names (e.g. `*net.OpError`), not framework-level error tags.

The attribute appears on the span only. Metric labels do not include
gin error type — adding it would explode cardinality on services that
push diverse `c.Error(err)` calls. If a future requirement justifies a
metric signal for "request had a gin error", a boolean
`gin.had_error="true"` label is the right shape, not a typed one.

### 3. Cardinality control

This package emits no metric instruments of its own. The
`http.server.request.duration` histogram is registered by `otelgin`
into the SDK's MeterProvider, which already carries the
`metric.View` configured in ADR 0009. Cardinality control is
inherited.

If `otelgin` emits attribute keys outside the allowlist (`net.peer.*`,
`url.full`), they are filtered by the view at the SDK level. No work
on this package's side.

### 4. Cardinality at `http.route` is bounded by gin's route table

`otelgin` populates `http.route` from `c.FullPath()`, which is the
matched route template. The cap reader from ADR 0009 §2 still applies
as defense in depth (in particular, gin's `NoRoute` handler can write
arbitrary paths if user code abuses `c.Request.URL.Path`).

### 5. Recovery interaction and middleware ordering

`otelgin.Middleware`, `ErrorRecorder`, and `gin.Recovery()` form a
three-layer stack whose ordering determines correctness. The
**recommended canonical order** that `Middleware(...)` returns is:

```go
r.Use(o11ygin.Middleware("svc", tp, mp, prop)...) // [0] otelgin span open
                                                  // [1] ErrorRecorder (defer)
r.Use(gin.Recovery())                             // [2] panic recover (innermost)
// ... user handlers below
```

Execution order on a request:

1. `[0] otelgin` runs first → opens server span, replaces
   `c.Request.Context()`.
2. `[1] ErrorRecorder` enters → registers its `defer`, calls `c.Next()`.
3. `[2] gin.Recovery` enters → registers its `defer`, calls `c.Next()`.
4. Handler runs.
5. Unwind: `gin.Recovery` defer (recover if panic), `ErrorRecorder`
   defer (read `c.Errors`, annotate span), `otelgin` defer (set
   span attributes from `c.Writer.Status()`, end span).

This ordering produces the expected behavior on every documented
case in the matrix below. If a service inverts the order (e.g.
`gin.Recovery()` outermost), the matrix changes — most notably,
panic-induced 500s do not get a span because the span has already
been closed by the time control returns. The package godoc and
`AGENTS.md` explicitly call this out, and `Middleware(...)` returns
the slice in the canonical order to make the wrong order require
manual effort.

#### Middleware-ordering test matrix (ship in the implementation PR)

The PR adds tests covering each row. Each test asserts the recorded
span attributes, span status, recorded errors, and the
`http.server.request.duration` sample for the request.

| # | Scenario | Recovery present | Custom recovery | Handler outcome | Expected span status | Expected `c.Errors` surfaced | Expected metric `status_code` |
|---|---|---|---|---|---|---|---|
| 1 | Happy path | `gin.Recovery()` | n/a | 200 OK | Unset | none | 200 |
| 2 | 4xx via `c.JSON(400, ...)` | `gin.Recovery()` | n/a | 400 | Unset | none | 400 |
| 3 | 4xx via `c.AbortWithError(400, err)` | `gin.Recovery()` | n/a | 400 | Unset (4xx is not server error per OTel) | one error recorded with `gin.error.type` | 400 |
| 4 | 5xx via `c.AbortWithError(500, err)` | `gin.Recovery()` | n/a | 500 | Error, message = err.Error() | one error recorded | 500 |
| 5 | Multiple `c.Error(err1)` + `c.AbortWithError(500, err2)` | `gin.Recovery()` | n/a | 500 | Error, message = err2.Error() | both errors recorded in order | 500 |
| 6 | Handler panics, default Recovery | `gin.Recovery()` | n/a | 500 | Error, message from panic value | none (`gin.Recovery` does not push panic into `c.Errors`) | 500 |
| 7 | Handler panics, custom Recovery that **does** push into `c.Errors` | none | custom that calls `c.Error(panicErr); c.AbortWithStatus(500)` | 500 | Error | one error recorded with the panic value | 500 |
| 8 | Handler panics, custom Recovery that swallows the panic and writes 200 | none | custom that recovers and `c.JSON(200, fallback)` | 200 | Unset | none | 200 |
| 9 | `c.Abort()` without error, status 204 | `gin.Recovery()` | n/a | 204 | Unset | none | 204 |
| 10 | Inverted order: `gin.Recovery()` outermost, then `Middleware(...)` | `gin.Recovery()` | n/a | 500 (panic) | **No span at all** (otelgin span never opened because Recovery short-circuited) | none | not recorded |

Row 10 documents the failure mode of the wrong order; the test
asserts the failure as a regression guard. The README's gin section
references this row to discourage the inversion.

The custom-recovery cases (rows 7 and 8) are the load-bearing tests
because they prove `ErrorRecorder` composes correctly with
non-default recovery patterns common in production services
(structured-logging recoveries, Sentry-style recoveries that capture
the panic and continue, fallback handlers).

### 6. Compliance with ADR 0003

`otelgin`'s relevant constructors accept `WithTracerProvider`,
`WithMeterProvider`, and `WithPropagators` options. The facade always
supplies all three. `gin-gonic/gin` itself does not import OTel and
does not touch globals.

The ADR 0003 §"Approved integrations" table is updated in the same PR:

| Library | Version | Verified | Behavior | Notes |
|---|---|---|---|---|
| `go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin` | (pinned) | ✅ | Reads globals as fallback only; never sets. | See ADR 0010 |
| `github.com/gin-gonic/gin` | (pinned) | ✅ | Pure HTTP framework; no OTel coupling. | See ADR 0010 |

---

## Implementation plan (informative)

PR sequence after ADRs are accepted:

1. ADR 0009's PR lands first (`otelhttp` facade + metric.View
   cardinality).
2. This ADR's PR adds `gin/`, the `examples/gin/` example, README and
   AGENTS.md updates.
3. ADR 0011's PR (`resty/`) follows independently.

This package depends on the metric.View infrastructure from ADR 0009
but not on the `http/` facade itself.

---

## Consequences

**Positive**

- Single short package (~100 LOC including ErrorRecorder + tests
  glue) instead of ~300 LOC of self-written gin middleware.
- `c.Errors` and `c.AbortWithError(...)` are visible in traces — the
  primary motivating gap is closed.
- Inherits `otelgin`'s server span correctness (route extraction,
  status codes, propagation) and any future improvements automatically.
- Service migration is straightforward for teams already using
  `otelgin` directly: they only swap their option setup for the
  SDK's facade.

**Negative / Trade-offs**

- The SDK's `go.mod` gains `github.com/gin-gonic/gin` and the
  `otelgin` contrib package as direct dependencies. Same trade-off
  accepted for `nats.go` in ADR 0004; mitigated by Go's lazy module
  loading (consumers who do not import `o11y/gin` do not link gin
  into their binary).
- We track upstream `otelgin` releases for semconv alignment. If a
  release lags `otelgin`'s metric naming behind the SDK's ADR 0006
  pin, we either wait, contribute upstream, or pin a transitional
  version with the gap noted in this ADR.
- `ErrorRecorder` ordering relative to `gin.Recovery()` is a footgun
  if reversed. Documented in godoc and AGENTS.md.

---

## Open questions

- **otelgin metric registration.** Some `otelgin` versions emit
  `http.server.duration` (old name) and some emit
  `http.server.request.duration` (current). The version we pin must
  be the one that emits the current name, or the metric.View must
  rename it. To verify and document at PR time.
- **`gin.error.type` attribute key.** Use exactly that string, or
  follow OTel-style `gin.error_type`? Lean: dot-form `gin.error.type`
  to mirror OTel conventions.
- **Convenience overload `MiddlewareFromSDK(obs *o11y.SDK, ...)`.**
  Adds a back-edge from `gin/` to the root package. Lean: defer until
  user feedback shows the four-arg form is friction.
