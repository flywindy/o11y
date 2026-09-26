# ADR 0010 — Gin Integration

**Status**: Accepted
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
| Framework signal access | ⚠️ Modern `otelgin` writes `c.Errors.String()` as a single concatenated `gin.errors` string attribute *(it does not: v0.68.0 uses that string as the span status description)*, but does **not** call `span.RecordError` per-error nor classify by `gin.ErrorType`. ErrorRecorder (§2) **enhances** rather than replaces this: it adds typed `RecordError` calls with `gin.error.type` so each error becomes a span event queryable in TraceQL. *Wrong at the pinned version: otelgin v0.68.0 does call `span.RecordError` per error — see the 2026-09-26 amendment.* |

Adoption-time verification pinned `otelgin` v0.68.0, which already emits
`http.server.request.duration` and already records `c.Errors` as span error
events. The remaining SDK-owned gap is typed classification: upstream does
not add `gin.error.type`, so ErrorRecorder adds typed error events while
leaving upstream span and metric ownership intact.

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

```text
gin/
├── middleware.go       // Middleware: otelgin.Middleware + chain error handler (amended 2026-09-26)
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
// span using span.RecordError with a gin.error.type attribute, and sets
// the span status to Error for 5xx responses. It must run after the otelgin middleware
// in the chain so that a server span exists in c.Request.Context().
func ErrorRecorder() gin.HandlerFunc
```

`Option` exposes a curated subset. Adoption note: `otelgin` v0.68.0's
span-name formatter receives `*gin.Context`, so the concrete facade
signature is `WithSpanNameFormatter(func(*gin.Context) string)`.

- `WithSpanNameFormatter(func(*gin.Context) string)` passes through to otelgin
- `WithFilter(func(*http.Request) bool)` passes through to otelgin
- `WithMetricAttributesFn(...)` passes through to otelgin

### 2. ErrorRecorder semantics

> Superseded for the canonical `Middleware` chain by the
> [2026-09-26 amendment](#amendment-2026-09-26--one-exception-per-gin-error-recorded-by-the-chain).
> What follows still describes `ErrorRecorder` used on its own.

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
                    attribute.String("gin.error.type", ginErrorTypeString(ge.Type)),
                ),
            )
        }
        // OTel semconv: server-side 5xx is Error. Note that otelgin
        // v0.68.0 also sets Error when c.Errors is non-empty, including
        // 4xx responses produced with AbortWithError.
        if c.Writer.Status() >= 500 {
            span.SetStatus(codes.Error, c.Errors.Last().Error())
        }
    }
}
```

`gin.ErrorType` values (`ErrorTypeBind`, `ErrorTypeRender`,
`ErrorTypePublic`, `ErrorTypePrivate`, `ErrorTypeAny`) map to the
`gin.error.type` attribute as `bind`, `render`, `public`, `private`,
and `any`; combined bitmasks are joined with `|`. This is gin-
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

Callers that need additional caller-controlled dimensions on the
series (e.g. `app_name`, `bot_name` for multi-tenant API gateways) can
extend the allowlist via `o11y.WithExtraHTTPServerAttributeKeys(...)`
and inject the values per request through
`o11ygin.WithMetricAttributesFn`. Cardinality of the new keys is the
caller's responsibility — the SDK does not separately cap them, so
prefer values drawn from an enumerable, bounded keyspace.

### 4. Cardinality at `http.route` is bounded by gin's route table

`otelgin` populates `http.route` from `c.FullPath()`, which is the
matched route template. The metrics-pipeline cardinality controls from
ADR 0009 §2 still apply as defense in depth (in particular, gin's
`NoRoute` handler can write arbitrary paths if user code abuses
`c.Request.URL.Path`).

### 5. Recovery interaction and middleware ordering

> Updated by the 2026-09-26 amendment: slot `[1]` is the chain's error
> handler, no longer `ErrorRecorder`. It records each `c.Errors` entry
> as the exception event and keeps `c.Errors` from otelgin, which would
> record each entry again; slot `[0]` puts them back once otelgin returns.

`otelgin.Middleware`, `ErrorRecorder`, and `gin.Recovery()` form a
three-layer stack whose ordering determines correctness. The
**recommended canonical order** that `Middleware(...)` returns is:

```go
r.Use(o11ygin.Middleware("svc", tp, mp, prop)...) // [0] otelgin span open
                                                  // [1] chain error handler (post-c.Next)
r.Use(gin.Recovery())                             // [2] panic recover (innermost)
// ... user handlers below
```

Execution order on a request:

1. `[0] otelgin` runs first → opens server span, registers
   `defer span.End()`, replaces `c.Request.Context()`, calls `c.Next()`.
2. `[1]` the chain's error handler enters → notes the span, calls
   `c.Next()` directly (no defer; it inspects `c.Errors` after `c.Next()`
   returns).
3. `[2] gin.Recovery` enters → registers a `defer { recover() ... }`,
   calls `c.Next()`.
4. Handler runs.
5. Unwind:
   - `gin.Recovery`'s `defer` recovers any panic and writes 500 to
     the response writer. Control then returns up the chain
     **normally** (the default `gin.Recovery` does not re-panic).
   - The chain's error handler's post-`c.Next()` code runs, reads
     `c.Errors`, records one `exception` event per entry carrying
     `gin.error.type`, sets status Error with `c.Errors.String()`, and
     moves `c.Errors` aside. With the default `gin.Recovery`, `c.Errors`
     is empty after a panic (Recovery does not push the panic into
     `c.Errors`), so the handler is a no-op for that case.
   - `otelgin`'s post-`c.Next()` code reads `c.Writer.Status()` (now
     500), sets the OTel span status from it (for a 5xx this replaces
     the description with an empty one) and the
     `http.response.status_code` attribute. It finds `c.Errors` empty,
     so it records no exception. Then `defer span.End()` fires.
   - Slot `[0]` puts `c.Errors` back before control leaves the chain.

The end-state span for a panic in canonical order: status `Error`
(from the 500 status code), `http.response.status_code=500`, but
**no panic value or stack trace** captured — because the panic was
swallowed by `gin.Recovery` before any tracing layer saw it. To
capture the panic value itself, callers either replace
`gin.Recovery` with a custom recovery that pushes the panic into
`c.Errors` (then the chain records it) or move the recovery
**outside** of otelgin, accepting the trade-off in row 10 below.

If a service inverts the order (e.g. `gin.Recovery()` outermost,
otelgin inner), behavior changes substantially — see row 10.
`Middleware(...)` returns the slice in the canonical order so
inversion requires manual effort.

#### Middleware-ordering test matrix (ship in the implementation PR)

The PR adds tests covering each row. Each test asserts the recorded
span attributes, span status, the typed exception events the chain records,
and the `http.server.request.duration` sample for the request.

Because the chain sets span status to Error whenever `c.Errors` is
non-empty, as `otelgin` v0.68.0 would, `AbortWithError(400, err)` produces
an Error span even though plain 4xx responses remain Unset. On a 5xx,
`otelgin`'s status-code-derived status (Error, empty description) replaces
that description; the messages are on the exception events.

| # | Scenario | Recovery present | Custom recovery | Handler outcome | Expected span status | Expected `c.Errors` surfaced | Expected metric `status_code` |
|---|---|---|---|---|---|---|---|
| 1 | Happy path | `gin.Recovery()` | n/a | 200 OK | Unset | none | 200 |
| 2 | 4xx via `c.JSON(400, ...)` | `gin.Recovery()` | n/a | 400 | Unset | none | 400 |
| 3 | 4xx via `c.AbortWithError(400, err)` | `gin.Recovery()` | n/a | 400 | Error, message = `c.Errors.String()` | one `exception` event with `gin.error.type` | 400 |
| 4 | 5xx via `c.AbortWithError(500, err)` | `gin.Recovery()` | n/a | 500 | Error, description empty (5xx) | one `exception` event with `gin.error.type` | 500 |
| 5 | Multiple `c.Error(err1)` + `c.AbortWithError(500, err2)` | `gin.Recovery()` | n/a | 500 | Error, description empty (5xx) | one typed `exception` event per error, in order | 500 |
| 6 | Handler panics, default Recovery | `gin.Recovery()` | n/a | 500 | Error (from status code), description **empty** — panic was swallowed by inner Recovery before any tracing layer saw it | none (`gin.Recovery` does not push panic into `c.Errors`) | 500 |
| 7 | Handler panics, custom Recovery that **does** push into `c.Errors` | none | custom that calls `c.Error(panicErr); c.AbortWithStatus(500)` | 500 | Error | one typed `exception` event for the panic value | 500 |
| 8 | Handler panics, custom Recovery that swallows the panic and writes 200 | none | custom that recovers and `c.JSON(200, fallback)` | 200 | Unset | none | 200 |
| 9 | `c.Abort()` without error, status 204 | `gin.Recovery()` | n/a | 204 | Unset | none | 204 |
| 10 | Inverted order: `gin.Recovery()` outermost, then `Middleware(...)` (handler panics) | `gin.Recovery()` | n/a | 500 (panic, recovered by outer Recovery) | **Span exists but is incomplete**: opened by otelgin's pre-`c.Next()` code, ended by its `defer span.End()`, but `status_code` attribute and span status are **not set** because otelgin's post-`c.Next()` code was skipped by the propagating panic. The chain's error handler is also skipped; the OTel SDK's `span.End` records the propagating panic as one `exception` event. | none | **not recorded** (otelgin's metric is also written post-`c.Next()`, which never ran) |

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

## Resolved questions

- **otelgin metric registration.** Verified at adoption time:
  `otelgin` v0.68.0 emits `http.server.request.duration`, so no
  transitional rename view is required.
- **`gin.error.type` attribute key.** Chosen exactly as
  `gin.error.type`, using dot-form to mirror OTel attribute naming.
- **Convenience overload `MiddlewareFromSDK(obs *o11y.SDK, ...)`.**
  Deferred. The gin package must not import the root
  `github.com/flywindy/o11y` package until user feedback shows the
  four-argument provider form is too much friction.

---

## Amendment (2026-09-26) — one exception per gin error, recorded by the chain

### Finding

The §Context checklist row "Framework signal access" says `otelgin` does
not call `span.RecordError` per error. At the pinned v0.68.0 it does
(`gin.go`, after `c.Next()`: status Error with `c.Errors.String()`, then
one `span.RecordError(err.Err)` per `c.Errors` entry). `ErrorRecorder`
runs inside `otelgin` and also called `RecordError` per entry, so every
`c.Error` / `c.AbortWithError` in the canonical chain produced **two**
`exception` events: one with `gin.error.type` (ours) and one without
(otelgin's). Exception counts in Tempo, and any alert built on them, read
twice the real number. The matrix tests counted only events carrying
`gin.error.type`, so they could not see the duplicate.

`otelgin` offers no option to turn its recording off, and an exception
event cannot be amended once recorded, so `gin.error.type` cannot be
added to otelgin's event after the fact.

### Decision

1. **The chain records the exception; `otelgin` does not see `c.Errors`.**
   `Middleware`'s second handler records each `c.Errors` entry as one
   `exception` event carrying `gin.error.type` — the event shape v0.13.0
   already emitted — and sets status Error with `c.Errors.String()`, as
   `otelgin` would. It then moves `c.Errors` under a key private to the
   chain and leaves `c.Errors` empty, so `otelgin`'s unwind records
   nothing. The chain's first handler wraps `otelgin` and, in a `defer`,
   puts the errors back (ahead of anything appended since) as soon as
   `otelgin` returns or panics, so every middleware outside the chain —
   gin's logger, an error-response middleware — sees `c.Errors` exactly
   as the handlers left them. Nothing but `otelgin`'s own unwind runs
   while they are aside.
2. **What this relies on in `otelgin`.** v0.68.0 reads `c.Errors` in one
   place only, the status-and-`RecordError` block after `c.Next()`; its
   metrics and attributes do not use it. The matrix pins the outcome
   rather than the reading: it counts every event on the span and
   requires exactly one typed `exception` per error, and a test checks an
   outer middleware sees every error. An `otelgin` upgrade that reads
   `c.Errors` elsewhere, or stops relying on it, fails those tests
   instead of changing what services emit.
3. **5xx status description.** `otelgin` sets the status from the HTTP
   code before it looks at `c.Errors`; for a 5xx that is Error with an
   empty description, which replaces the chain's (equal codes do not
   keep the earlier one). Before this amendment `otelgin`'s own
   `c.Errors.String()` came after and won. So a 5xx span with gin errors
   now has an empty status description; the messages remain on its
   exception events. A 4xx keeps the chain's description, because
   `otelgin`'s Unset does not replace Error.
4. **Requests the chain filters out are not instrumented by it.** On a
   request excluded by `WithFilter` / `WithSkipPaths`, `otelgin` opens no
   span and records nothing, yet an outer layer's span
   (`o11yhttp.NewServerHandler`, an application span, an enclosing
   `Middleware` on the engine) may be active. The handler does the same
   as `otelgin`: nothing, and it leaves `c.Errors` alone. The facade's
   `WithFilter` no longer hands filters to `otelgin` one by one;
   `Middleware` wraps all of them in a single `otelgin.WithGinFilter` that
   evaluates them once per request (all must pass, as `otelgin` requires)
   and, when the request is traced, marks it under a key private to the
   chain. The two cannot disagree, even for a filter whose answer changes
   between calls (per-request sampling), and an excluded request, such as
   a probe, writes nothing. A chain with no filters installs no gin
   filter.
5. **The handler reads the span before `c.Next()`,** while the request
   context is the one `otelgin` handed on: a downstream middleware may
   replace it without restoring it. It does no recording on a span that
   is not recording, and skips entries whose `Err` is nil
   (`c.Error(&gin.Error{...})` appends one).
6. **`ErrorRecorder` on its own** keeps its behaviour: one `exception`
   event per error carrying `gin.error.type`, and span status Error on a
   5xx response. That is the case where nothing else reads `c.Errors`.
   Its godoc now says not to add it after `Middleware`. Two crash-level
   fixes apply to it as well: a nil-`Err` entry is skipped instead of
   panicking the 5xx status path, and the status description comes from
   the last entry that has an error.

The §5 matrix and walkthrough above are updated accordingly. Row 10 now
also asserts the one `exception` event the OTel SDK's `span.End` records
for the panic that unwinds through `otelgin`, which the old helper
ignored because it carried no `gin.error.type`.

### Alternatives rejected in review

A first implementation left `otelgin`'s exception alone and added a
separate `gin.error` event carrying the type. Six review rounds refined
it and it was dropped, because every variant lost information or needed
a query migration:

- **Pair the n-th `gin.error` with the n-th `exception`.** Breaks when
  application code records exceptions on the server span, and when the
  event limit evicts the older `gin.error` events first.
- **Copy the error text onto `gin.error`** (as `exception.message` or an
  SDK-owned key) to pair by content. Duplicates `exception.message`
  against semconv v1.39.0's guidance (`error.message` note), moves the
  double count onto queries that detect exceptions by attribute, and
  widens what redaction has to cover.
- **Type only, no pairing.** Loses which error had which type on any
  request with several, and every query on
  `event:name = "exception" && event.gin.error.type` stops matching.

Two ways of handling filtered requests were rejected as well:
recording on whatever span is active (a second exception whenever that
span belongs to an enclosing `Middleware`, whose `otelgin` records the
same errors), and detecting whether `otelgin` opened a span by comparing
span contexts, or by re-evaluating the filters in the handler (wrong on
nested chains; two answers from a non-deterministic filter).

### Consequences

- Exception-event volume from gin errors halves on rollout. Dashboards
  and alerts that count `exception` events read the real number from
  then on; a before/after comparison shows a step down that is not an
  improvement in error rate.
- Queries need no change: the typed `exception` event is the one v0.13.0
  emitted. Only the untyped duplicate disappears.
- A 5xx span with gin errors has an empty status description (Decision 3).
- A request a filter excludes (for example `/healthz` under
  `WithSkipPaths`) no longer gets exception events on an outer span;
  before this amendment `ErrorRecorder` recorded them there.
