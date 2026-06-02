# ADR 0017 — Echo Integration

**Status**: Accepted
**Date**: 2026-06-02

**Applies** ADR 0008 (sourcing policy); **builds on** ADR 0009
(`otelhttp` facade and `metric.View` cardinality control); **mirrors**
ADR 0010 (gin integration).

> **Scope note.** This PR makes the decision and records this ADR only.
> No code, `go.mod`, README, AGENTS.md, ADR 0003, or ADR 0008 changes
> land here. The implementation plan in the "Implementation plan
> (informative)" section is the contract for the follow-up PR, which is
> where the ADR 0003 / ADR 0008 table rows and the `echo/` package are
> added.

---

## Context

Several candidate services use [Echo](https://echo.labstack.com/)
(`github.com/labstack/echo/v4`) rather than gin. Today the SDK ships a
first-party gin facade (ADR 0010) but nothing for echo, so an echo
service either drops down to the framework-agnostic `http/` facade
(ADR 0009) — losing echo's route template as a bounded `http.route`
source and losing visibility into echo's returned-error flow — or wires
`otelecho` by hand with its own provider plumbing, re-deriving the
ADR 0003 no-globals discipline each time.

ADR 0008 establishes that the default sourcing strategy for a new
integration is a **T2 facade** over a vetted upstream library, and that
self-writing (T3) is the exception that an ADR must justify. ADR 0010
applied this to gin via
`go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin`.
This ADR applies the same policy to echo via
`go.opentelemetry.io/contrib/instrumentation/github.com/labstack/echo/otelecho`,
which lives in the same OpenTelemetry contrib monorepo and ships on the
same release train (v0.68.0) already pinned for `otelgin`, `otelhttp`,
and the runtime metrics package.

### Why echo is not just "gin with different imports"

Two framework differences drive the design and are the reason this ADR
exists rather than a one-line "do what 0010 did":

1. **Error model.** Gin handlers do not return errors; they push them
   into the side-channel `*gin.Context.Errors` via `c.Error` /
   `c.AbortWithError`, which a `RoundTripper`- or `http.Handler`-level
   middleware cannot see. ADR 0010 closed that gap with a self-written
   `ErrorRecorder` middleware because `otelgin` exposes no error hook.
   Echo is the opposite: handlers **return** `error`, the error
   propagates back up the middleware chain, and a centralized
   `Echo.HTTPErrorHandler` turns it into a response. `otelecho`
   **already exposes** this via the `WithOnError(func(echo.Context, error))`
   option. The SDK therefore does **not** need a separate recorder
   middleware — it configures `otelecho`'s native hook instead (see
   Decision 2). This is a deliberate, documented divergence from
   ADR 0010's slice-of-handlers shape.

2. **Status is committed by a centralized handler that runs *outside*
   the span scope.** When an echo handler returns an error without
   having written a response, the response status is set by
   `Echo.HTTPErrorHandler`, which `Echo.ServeHTTP` invokes **after** the
   middleware chain (and therefore after `otelecho`'s span has closed).
   `otelecho`'s own documentation warns that the span "depends on
   calling `c.Error()` to ensure the span contains valid response data".
   This timing is the load-bearing correctness concern for the echo
   facade and is addressed in Decision 4.

### ADR 0008 §2 checklist applied to `otelecho`

| Item | Result |
|---|---|
| ADR 0003 compliance | ✅ Reads globals as fallback only; `WithTracerProvider`, `WithPropagators`, `WithMeterProvider` options bypass the fallback. `github.com/labstack/echo/v4` itself does not import OTel. |
| Maintenance signal | ✅ Maintained by OpenTelemetry contrib; releases track echo and the OTel SDK on the same cadence as `otelgin`. |
| Semconv alignment | ✅ v0.68.0 emits stable v1.30+ HTTP server attributes and `http.server.request.duration`, matching the train pinned in ADR 0006 / ADR 0009. Verify the emitted keyset against `docs/semconv.md` at adoption (it also emits `http.server.request.body.size` / `http.server.response.body.size`; see Decision 8). |
| Configurability of names and attributes | ⚠️ **Partial.** Metric/attribute config is overridable (`WithMetricAttributeFn`, `WithEchoMetricAttributeFn`), and span names default to the bounded route template `c.Path()`. But v0.68.0 has **no** `WithSpanNameFormatter` (unlike `otelgin`). §2 item 4 is satisfied by "correct out of the box" — names are bounded by default — not by "overridable". See Decision 5. |
| Framework signal access | ✅ The returned `error` and echo's `*echo.HTTPError` (with its hidden `.Internal` cause) are reachable through `WithOnError`. The facade uses this to surface what the client never sees. See Decision 2. |

Five passes (item 4 on the "correct by default" branch) with one
echo-specific timing concern closable inside the `WithOnError` hook in
<30 lines of facade code. **T2 adoption is justified**; no T3 self-write
is warranted.

Relevant existing files / context:

- ADR 0008 — sourcing policy (T2 default, §2 checklist)
- ADR 0009 — `otelhttp` facade and `metric.View` cardinality
- ADR 0010 — gin facade (the closest precedent; mirror its shape except
  where echo's error model dictates otherwise)
- `gin/middleware.go`, `gin/errors.go`, `gin/options.go` — the package
  shape and option-mapping pattern to follow

---

## Decisions

### 1. Adopt `otelecho` as upstream and ship a thin T2 facade

Package layout mirrors `gin/`:

```text
echo/
├── middleware.go       // Middleware: otelecho.Middleware wired to SDK providers + default OnError
├── errors.go           // recordEchoError: the OnError implementation + echo error classification
├── options.go          // curated Option subset mapped onto otelecho options
├── doc.go              // Tier: T2 annotation + policy reference (required by ADR 0008 §7.2)
└── *_test.go
```

`doc.go` must carry the `// Tier: T2 facade over
go.opentelemetry.io/contrib/instrumentation/github.com/labstack/echo/otelecho`
line so the ADR 0008 §7.2 CI gate passes. The gate's integration-dir
include-list gains `echo/` in the same PR.

### 2. Public API: a single `echo.MiddlewareFunc`, error recording folded into `otelecho`

Because `otelecho` exposes `WithOnError`, the facade does **not** add a
second recorder handler. It returns one middleware:

```go
package echo

import (
    "github.com/labstack/echo/v4"
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/trace"
)

// Middleware returns the canonical echo middleware for o11y tracing,
// configured with the SDK's providers and a default OnError handler that
// records the returned error (and any hidden *echo.HTTPError.Internal
// cause) onto the active server span.
//
//   e.Use(o11yecho.Middleware(serviceName, tp, mp, prop))
//
// Unlike o11ygin.Middleware, this returns a single echo.MiddlewareFunc
// (not a slice): echo's returned-error flow is observable through
// otelecho's native WithOnError hook, so no separate ErrorRecorder
// handler is needed.
func Middleware(
    service string,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) echo.MiddlewareFunc
```

The facade always supplies `WithTracerProvider`, `WithMeterProvider`,
and `WithPropagators` so `otelecho` never falls back to process-wide
globals (ADR 0003). A default `WithOnError` is installed unless the
caller overrides it (Decision 2a). Caller `opts` are appended last.

The difference from ADR 0010's `[]gin.HandlerFunc` return is
intentional and documented in godoc: gin needed a self-written
`ErrorRecorder` appended to the chain because `otelgin` has no error
hook; echo gets the same observability from the upstream `WithOnError`,
so a single middleware is the simpler, more echo-idiomatic shape.

#### 2a. Default `OnError` semantics (echo's analog of ADR 0010 §2)

Pseudocode (concrete implementation in the follow-up PR):

```go
func recordEchoError(c echo.Context, err error) {
    span := trace.SpanFromContext(c.Request().Context())
    if span.SpanContext().IsValid() {
        attrs := []attribute.KeyValue{
            attribute.String("echo.error.kind", echoErrorKind(err)),
        }
        var he *echo.HTTPError
        if errors.As(err, &he) {
            attrs = append(attrs, attribute.Int("echo.http_error.code", he.Code))
            // he.Internal is the cause echo hides from the client. It is
            // the primary signal the agnostic http/ facade cannot see.
            if he.Internal != nil {
                span.RecordError(he.Internal,
                    trace.WithAttributes(attribute.String("echo.error.source", "internal")))
            }
        }
        span.RecordError(err, trace.WithAttributes(attrs...))
    }

    // Commit the response so the span observes the real status code
    // before it ends; see Decision 4. Guard against double-write.
    if !c.Response().Committed {
        c.Error(err) // routes through Echo.HTTPErrorHandler exactly once
    }
    if c.Response().Status >= 500 {
        span.SetStatus(codes.Error, err.Error())
    }
}
```

Error classification (`echo.error.kind`):

| Returned value | `echo.error.kind` | Extra |
|---|---|---|
| `*echo.HTTPError` | `http_error` | `echo.http_error.code`; `.Internal` recorded as a second event tagged `echo.error.source=internal` when present |
| any other `error` | `generic` | recorded as-is |

Rationale for naming: this mirrors ADR 0010's choice of a
framework-specific dotted key (`gin.error.type`) rather than reusing
OTel's `error.type` (which is reserved for transport/protocol class
names). `echo.http_error.code` is deliberately **span-only**, never a
metric label — it would duplicate `http.response.status_code` on the
metric and risk cardinality. The genuinely new signal echo gives us is
`*echo.HTTPError.Internal`: the underlying cause that the client never
sees because echo replaces it with a sanitized message. Surfacing it as
a span event is the main value-add of this facade over the agnostic
`http/` one.

### 3. Span-status ownership and the `c.Error` commit

`otelecho` sets HTTP-derived span status during its own unwind from the
response status code. The default `OnError` above sets `codes.Error`
for 5xx as a belt-and-suspenders measure for the case where the facade's
hook runs before `otelecho` reads the (now-committed) status. The
follow-up PR's tests must pin which layer wins so the behavior is not
left to upstream-version drift (the test matrix in Decision 6 asserts
the end-state span status per scenario).

### 4. Status-commit timing — the load-bearing echo difference

When a handler returns an error and has not written a response, echo's
response status is `200` until `Echo.HTTPErrorHandler` runs. That
handler is invoked by `Echo.ServeHTTP` **after** the middleware chain,
i.e. after `otelecho`'s span has already ended. Without intervention the
span and the `http.server.request.duration` sample would record `200`
for a request the client saw as `500`.

Decision: the default `OnError` calls `c.Error(err)` (guarded by
`!c.Response().Committed`) so the centralized error handler runs, and
the response is committed, **inside** the middleware/span scope. This is
exactly the pattern `otelecho`'s documentation prescribes ("depends on
calling `c.Error()`").

Two caveats the follow-up PR documents in godoc + README:

- **Custom `HTTPErrorHandler`.** Services that set
  `e.HTTPErrorHandler` keep their handler; `c.Error` routes through
  whatever handler is installed. The only requirement is that the custom
  handler commits a response (the echo default does). A handler that
  intentionally leaves the response uncommitted will still record `200`
  — documented as a known footgun, not worked around.
- **Double-commit.** If user middleware already called `c.Error`, the
  `Committed` guard makes the facade a no-op on the second call, so the
  response is written exactly once.

### 5. Configurability gap: no `WithSpanNameFormatter` upstream

`otelecho` v0.68.0 exposes no span-name formatter. This is a real
divergence from `otelgin` and the reason §2 item 4 is marked partial.
It is **not** a T2 disqualifier because span names default to
`c.Path()` — echo's matched route template — which is bounded and
correct for both readability and `http.route` cardinality (Decision 7).

If a service needs custom span names:

1. Preferred: contribute `WithSpanNameFormatter` upstream (it parallels
   the `otelgin` option and is low-risk).
2. Interim: rename in a tiny post-span wrapper. This is explicitly
   **out of scope** for the initial facade; the facade does not expose a
   `WithSpanNameFormatter` that it cannot honor.

The facade's `Option` set therefore omits span-name formatting until
upstream gains it, rather than shipping a no-op or a fragile shim.

### 6. Recovery interaction and middleware ordering

Echo's `middleware.Recover()` is the analog of `gin.Recovery()` and the
ordering concern from ADR 0010 §5 recurs. Canonical order — register the
o11y middleware **first** (outermost) so it opens the span before
`Recover` and observes the committed status on unwind:

```go
e.Use(o11yecho.Middleware("svc", tp, mp, prop)) // outermost: span open
e.Use(middleware.Recover())                      // inner: panic recover
// ... routes
```

#### Middleware-ordering / error test matrix (ship in the implementation PR)

Each row asserts span status, recorded error events (including the
`echo.http_error.code` attribute and any `Internal` cause event), and
the `http.server.request.duration` `status_code` sample.

| # | Scenario | Recover present | Handler outcome | Expected span status | Expected error event(s) | Expected metric `status_code` |
|---|---|---|---|---|---|---|
| 1 | Happy path: `return c.JSON(200, ...)` | yes | 200 | Unset | none | 200 |
| 2 | `return c.JSON(400, ...)` (no error returned) | yes | 400 | Unset (4xx not Error) | none | 400 |
| 3 | `return echo.NewHTTPError(400, "bad")` | yes | 400 | per Decision 3 | one `http_error` event, `echo.http_error.code=400` | 400 |
| 4 | `return echo.NewHTTPError(500, "boom")` | yes | 500 | Error | one `http_error` event, code=500 | 500 |
| 5 | `return echo.NewHTTPError(502).SetInternal(cause)` | yes | 502 | Error | `http_error` event + second event tagged `echo.error.source=internal` carrying `cause` | 502 |
| 6 | `return errors.New("raw")` (non-HTTPError) | yes | 500 (echo default handler) | Error | one `generic` event | 500 |
| 7 | Handler panics, `middleware.Recover()` inner | yes | 500 | Error (from committed 500) | none (Recover does not return it through OnError) | 500 |
| 8 | Custom `HTTPErrorHandler` that maps to 503 | yes | `return errors.New(...)` → 503 | Error | one `generic` event | 503 |
| 9 | Inverted order: `Recover()` outermost, o11y inner, handler panics | yes | 500 (recovered by outer Recover) | **span incomplete**: opened by otelecho, ended by its defer, but post-`next` status/metric not recorded because the panic propagated past otelecho before it read the status | none | **not recorded** |

Row 5 is the load-bearing test: it proves the facade surfaces the hidden
`Internal` cause that the agnostic `http/` facade cannot. Row 9
documents the wrong-order failure mode as a regression guard, exactly as
ADR 0010 §5 row 10 does for gin; the README's echo section references it
to discourage inversion.

### 7. Cardinality control is inherited (same as ADR 0010 §3–§4)

This package emits no instruments of its own. `otelecho` registers
`http.server.request.duration` (and the body-size histograms, see
Decision 8) into the SDK's `MeterProvider`, which already carries the
ADR 0009 `metric.View` allowlist and the export-boundary route cap.
`http.route` is sourced from `c.Path()` — echo's finite route table —
so it is bounded by construction. Defense-in-depth from ADR 0009 §2
still applies for pathological cases (catch-all routes writing arbitrary
paths). Callers needing extra dimensions use
`o11y.WithExtraHTTPServerAttributeKeys(...)` plus
`o11yecho.WithMetricAttributesFn(...)`, with cardinality their
responsibility — identical contract to gin.

### 8. Body-size histograms — already symmetric with gin, no echo-specific work

`otelecho` v0.68.0 additionally emits `http.server.request.body.size`
and `http.server.response.body.size`. **So does `otelgin` v0.68.0** —
both call the same internal semconv `RecordMetrics`
(`otelgin` passes `c.Request.ContentLength` / `c.Writer.Size()`,
`otelecho` passes `request.ContentLength` / `c.Response().Size`). There
is therefore **no gin↔echo asymmetry** to correct; an earlier draft of
this ADR wrongly assumed gin did not emit them.

Current SDK state (verified in `internal/metrics/metrics.go:147-169`):
the only views registered target `http.server.request.duration` and
`http.client.request.duration` by instrument name. Nothing drops or
trims the body-size histograms, so today the gin facade (and the
`otelhttp` facade) already export them with their default attribute set.
Echo will behave identically out of the box — parity is automatic and
this facade needs **no** body-size-specific code.

The open question is therefore **not** echo-specific: should the SDK
trim or drop `http.server.*.body.size` for *all* HTTP instrumentation?
The OTel HTTP semconv marks these histograms **Opt-In** (not
recommended by default) because each adds buckets × attribute
cardinality on top of the RED signals. If the team wants them dropped or
allowlist-trimmed, that is a cross-cutting change to the ADR 0009
metrics pipeline (one view per body-size instrument, applied to gin,
echo, and `otelhttp` at once), not something this echo ADR should decide
alone. This ADR's position: **leave body-size handling exactly as gin
has it today**, and if a payload-size policy is wanted, raise it as an
ADR 0009 amendment covering every HTTP facade uniformly.

### 9. Curated `Option` set

Mapped onto `otelecho` options, names kept consistent with `gin/`:

- `WithFilter(func(*http.Request) bool)` → `otelecho.WithSkipper`.
  **Semantics are inverted on each side**: the facade's filter returns
  `false` to skip (matching `o11ygin.WithFilter`), while echo's
  `middleware.Skipper` returns `true` to skip and receives an
  `echo.Context`. The facade adapts both the boolean polarity and the
  argument type (`c.Request()`). This inversion must have a dedicated
  unit test.
- `WithMetricAttributesFn(func(*http.Request) []attribute.KeyValue)` →
  `otelecho.WithMetricAttributeFn` (pluralized facade name mirrors
  `o11ygin` / `o11yhttp`).
- `WithOnError(func(echo.Context, error))` → overrides the SDK default
  from Decision 2a for callers who want full control. When supplied, the
  caller owns the `c.Error`-commit responsibility from Decision 4.
- Optionally `WithEchoMetricAttributesFn(func(echo.Context) ...)` for
  echo-context-derived labels; defer unless a consumer needs it.

No span-name option (Decision 5). No provider/propagator override
options — the facade owns those (ADR 0003), same as gin.

### 10. Compliance updates (deferred to the implementation PR)

Per ADR 0008 §4, the implementation PR updates **both** tables; this
ADR-only PR does **not** touch them so the registry never claims a
dependency the module does not yet have:

- ADR 0003 §"Approved integrations" gains:

  | Library | Version | Verified | Behavior | Notes |
  |---|---|---|---|---|
  | `go.opentelemetry.io/contrib/instrumentation/github.com/labstack/echo/otelecho` | (pinned, target v0.68.0) | ✅ | Reads globals as fallback only; safe when `WithTracerProvider`, `WithMeterProvider`, `WithPropagators` are supplied. | See ADR 0017 |
  | `github.com/labstack/echo/v4` | (pinned) | ✅ | Pure HTTP framework; no OTel provider globals. | See ADR 0017 |

- ADR 0008 §5's application table gains an `echo/` row marked
  **T2 facade over `otelecho`**, parallel to the existing `gin/` row.

---

## Implementation plan (informative)

The follow-up (code) PR, after this ADR is accepted:

1. Add `echo/` (`middleware.go`, `errors.go`, `options.go`, `doc.go`,
   tests covering Decision 6's matrix + the `WithFilter` inversion).
2. Add `github.com/labstack/echo/v4` and the `otelecho` contrib package
   to `go.mod`, pinned to the v0.68.0 train; run the ADR 0008 §7 gate
   and add `echo/` to its include-list.
3. Update ADR 0003 / ADR 0008 tables (Decision 10).
4. Add `examples/echo/main.go` mirroring `examples/gin/main.go`
   (`/ok`, `/fail` returning `echo.NewHTTPError(...).SetInternal(...)`,
   `/panic`).
5. Update README (a "Using with echo" section beside "Using with gin",
   referencing the ordering footgun row) and AGENTS.md.
6. Resolve Decision 8 (body-size histograms) before merge.

This package depends on the `metric.View` infrastructure from ADR 0009
but not on the `http/` facade itself. It is independent of any other
in-flight integration and can land on its own.

---

## Consequences

**Positive**

- Echo services get the same first-party treatment as gin: bounded
  `http.route`, provider-wired no-globals instrumentation, and visibility
  into the framework's error flow — including the `*echo.HTTPError.Internal`
  cause the client never sees, which neither the agnostic `http/` facade
  nor a plain `otelecho` wiring surfaces.
- Simpler than gin: no self-written recorder handler. The facade is a
  thin wrapper around `otelecho` + a default `WithOnError`, comfortably
  inside ADR 0008's ~100-LOC T2 budget.
- Echo and gin facades stay observably interchangeable (same option
  names, same metric naming — including identical body-size behavior,
  see Decision 8), matching the cross-framework consistency discipline in
  ADR 0008.

**Negative / Trade-offs**

- `go.mod` gains `github.com/labstack/echo/v4` and the `otelecho`
  contrib package as direct dependencies. Mitigated by Go's lazy module
  loading: consumers who do not import `o11y/echo` do not link echo into
  their binary (same trade-off accepted for gin in ADR 0010 and NATS in
  ADR 0004).
- No upstream span-name formatter (Decision 5); custom span names need
  an upstream contribution. Tracked, not blocking.
- The `c.Error`-commit timing (Decision 4) couples the facade to echo's
  centralized-error-handler model. A future echo major that changes when
  `HTTPErrorHandler` runs would require revisiting Decision 4 — the
  test matrix (Decision 6) is the regression guard.
- We track upstream `otelecho` releases for semconv alignment, the same
  recurring audit cost ADR 0008 accepts for every T2 facade.

---

## Resolved questions

- **Separate recorder vs native hook.** Resolved in favor of
  `otelecho.WithOnError`. Echo exposes the returned error natively;
  re-deriving gin's `ErrorRecorder` would be redundant and would force
  the slice-return shape for no benefit.
- **Return type.** Single `echo.MiddlewareFunc`, not a slice — a
  deliberate, documented divergence from ADR 0010 driven by Decision 2.
- **`echo.http_error.code` as a metric label.** Rejected — span-only, to
  avoid duplicating `http.response.status_code` and inflating
  cardinality (same reasoning as ADR 0010 §2 for `gin.error.type`).

## Open questions (for the implementation PR)

- **Body-size histograms** (Decision 8): no echo-specific question — gin
  and echo both emit them today and the SDK trims neither. If a
  payload-size policy is desired, it belongs in an ADR 0009 amendment
  covering all HTTP facades uniformly, not here.
- **`WithEchoMetricAttributesFn`** (Decision 9): ship now or defer until
  a consumer needs echo-context-derived labels. Lean defer.
- **Convenience overload `MiddlewareFromSDK(obs *o11y.SDK, ...)`.**
  Deferred for the same reason as ADR 0010: the `echo/` package must not
  import the root `github.com/flywindy/o11y` package until the
  four-argument provider form proves too much friction in practice.
