# ADR 0008 — Gin Integration

**Status**: Proposed
**Date**: 2026-05-08

---

## Context

The SDK currently ships a framework-agnostic HTTP server middleware at
`github.com/flywindy/o11y/http`. It emits a single histogram
(`http.server.request.duration`) with a cardinality cap on `http.route`
and is usable with any `http.Handler` — gin, chi, echo, stdlib, etc.

What it does **not** do today:

- Create a server-side OTel span for each request.
- Extract trace context from inbound headers.
- Surface gin's matched route template (`*gin.Context.FullPath()`)
  unless the caller writes a custom `WithPathNormalizer` that bridges
  gin's context into a `*http.Request`.

In practice that means a service using `gin` together with this SDK
either:

1. Pulls in `go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin`
   for tracing **and** registers `o11y/http` for metrics — duplicating
   middleware and producing two parallel metric pipelines with
   inconsistent attribute keys; or
2. Uses only `o11y/http`, in which case the trace tree starts at the
   first manually-created span (typically inside the handler), with no
   server entry span carrying `traceparent` from upstream.

Neither outcome is acceptable. This ADR records the decision to ship a
first-party `gin/` subpackage and the design constraints around it.

Relevant existing files:

- `http/middleware.go` — generic server middleware (metrics only)
- `nats/conn.go` — precedent for "wrapper subpackage in single go.mod"
- `o11y.go` — SDK init returns `*SDK` exposing `TracerProvider()`,
  `MeterProvider()`, and `Propagator`

---

## Decisions

### 1. Build a first-party wrapper, do not depend on `otelgin`

`go.opentelemetry.io/contrib/.../otelgin` is rejected as a dependency
for this integration. Reasons:

- **Cardinality control.** `otelgin` records metrics without a hard cap
  on `http.route`. A 404-handling service exposing arbitrary paths can
  drive Prometheus into an OOM. The o11y SDK's `pathLimiter` (see
  `http/middleware.go:291-320`) is a non-negotiable invariant; a
  third-party middleware that does not enforce it cannot be the
  primary entry point.
- **Attribute consistency.** `otelgin` and `o11y/http` would emit
  different attribute keys for the same conceptual data (e.g. status
  code on the metric vs. on the span), making dashboards and alerts
  awkward to write.
- **Metric name collision.** Both libraries target
  `http.server.request.duration`. Registering both produces a
  meter conflict at runtime.
- **Global-state risk.** `otelgin` historically reads
  `otel.GetTracerProvider()` as fallback. ADR 0003 forbids any path
  that could lead to global mutation; auditing one external library
  per gin upgrade is friction we can avoid by owning the wrapper.

Resty (ADR 0009) and gin share the same reasoning, so the decision is
applied consistently across both.

### 2. Wrapper location: `gin/` under module root, single `go.mod`

Mirrors `nats/` and `http/`. No nested module. Implications:

- Users who do not import `github.com/flywindy/o11y/gin` still pull
  `github.com/gin-gonic/gin` as a transitive `go.mod` entry. This is
  the same trade-off accepted for `nats.go` in ADR 0004.
- A future move to nested modules is reversible. Doing it preemptively
  for one integration would split the release process before we have
  evidence the dependency weight matters.

### 3. Public API

```go
package gin

import (
    "context"

    "github.com/gin-gonic/gin"
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/trace"
)

func Middleware(
    ctx context.Context,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) gin.HandlerFunc
```

Shape rationale:

- Three-provider arity (`tp`, `mp`, `prop`) matches `nats.Connect` and
  keeps the wrapper independent of `*o11y.SDK` — the wrapper does not
  import the root package, so circular imports are impossible.
- `ctx` is used **only** for instrument creation logging (mirrors
  `http.New`). It is not retained across requests.
- Histogram creation failure is logged via `slog` and the middleware
  degrades to a no-op pass-through, matching `http.New`'s behavior.

`Option` initially exposes:

- `WithMaxUniquePaths(int)` — cardinality cap (default
  `DefaultMaxUniquePaths` from `http/`).
- `WithSpanNameFormatter(func(*gin.Context) string)` — override the
  default `"{METHOD} {route}"`.
- `WithPropagator(propagation.TextMapPropagator)` — only for callers
  who want a different propagator from `prop`; rare, but cheaper to
  add now than to extend the signature later.

### 4. Route resolution

`http.route` is resolved as follows, in order:

1. `c.FullPath()` if non-empty (gin populates this once the route is
   matched).
2. The literal `"NoRoute"` for 404 handlers.
3. The literal `"NoMethod"` for 405 handlers.
4. The literal `"unmatched"` as final fallback (defensive; should not
   occur in practice).

The result is then passed through the cardinality limiter shared with
`http/`. This keeps Prometheus series stable even if a misconfigured
service exposes unbounded paths via a catch-all `c.Request.URL.Path`
fallback in the future.

### 5. Histogram identity

The gin middleware records into the **same** histogram name as
`http/`: `http.server.request.duration`. Attribute set is identical
(`http.request.method`, `http.route`, `http.response.status_code`).
Rationale:

- A gateway that runs both `gin` and stdlib mux on different ports
  produces a single metric, not two parallel families.
- Dashboards built for `http/` continue to work without modification.

The gin middleware **must** therefore not be registered on a meter that
already has the `http/` middleware registered against it for the same
server, or duplicate samples would be recorded. This is documented in
the package godoc.

### 6. Span semantics

- Span kind: `SpanKindServer`.
- Span name: `"{METHOD} {route}"` (e.g. `"GET /users/:id"`); when
  `route == ""`, falls back to `"HTTP {METHOD}"` to comply with OTel
  semconv guidance against unbounded span names.
- Inbound `traceparent` / `tracestate` / `baggage` are extracted via
  `prop.Extract` from `c.Request.Header`.
- The handler-side context (`c.Request.Context()`) is replaced so that
  `slog.InfoContext(c.Request.Context(), ...)` carries `traceId` /
  `spanId` automatically (consistent with the rest of the SDK).
- Status code: `c.Writer.Status()` after `c.Next()`. Span status is
  `Error` when the code is `>= 500` **or** when `c.Errors` is
  non-empty; otherwise `Unset` (per OTel semconv: 4xx is not an error
  on the server side).

### 7. Panic interaction with gin's `Recovery`

gin's `Recovery()` middleware swallows panics and writes a 500. The
order of registration matters:

```go
r.Use(o11ygin.Middleware(...))   // outer
r.Use(gin.Recovery())            // inner
```

In this order:

- A panic inside the handler is recovered by `gin.Recovery()`, which
  writes 500 to the response writer.
- Control returns to `o11ygin.Middleware`'s `defer`, which observes
  `c.Writer.Status() == 500` and records the metric / closes the span
  with `Error`.
- The outer middleware therefore does **not** call `recover()` itself.
  Documented in the package godoc.

If the user reverses the order or omits `gin.Recovery()`, the gin
engine's own default recovery will return 500. The middleware is
robust to both cases because it inspects `c.Writer.Status()` after
`c.Next()` regardless of how the response was written.

### 8. Compliance with ADR 0003 (Global State Policy)

`gin-gonic/gin` itself does not read or write any
`go.opentelemetry.io/otel` global. The wrapper threads `tp`, `mp`,
`prop` through explicitly. No global is touched.

The ADR 0003 approved-integrations table is updated in the same PR
that introduces this wrapper:

| Library | Version | Verified | Behavior | Notes |
|---|---|---|---|---|
| `github.com/gin-gonic/gin` | (pinned at adoption) | ✅ | Pure HTTP framework; does not import OTel. | See ADR 0008 |

---

## Implementation plan (informative; subject to its own PR review)

This ADR commits only to the design. The implementation lands in
separate PRs:

1. **PR #1 — Refactor `http/`.** Lift `pathLimiter`, the response
   writer wrappers, and attribute-builder helpers into
   `internal/httpinstr`. Add server-span creation to `http/middleware.go`
   via new options (`WithTracerProvider`, `WithPropagator`). No public
   API break.
2. **PR #2 — `gin/` package.** Implementation, tests, example under
   `examples/gin/`, README and AGENTS.md updates.
3. **PR #3 — `resty/` package.** Per ADR 0009.

PR #2 depends on PR #1 because `internal/httpinstr` is the shared
limiter source.

---

## Consequences

**Positive**

- Single-line gin instrumentation that produces consistent metrics +
  traces + slog correlation with the rest of the SDK.
- Unbounded-cardinality protection extends to gin services.
- No new third-party OTel instrumentation library to audit on every
  upgrade.

**Negative / Trade-offs**

- The SDK's `go.mod` gains `github.com/gin-gonic/gin` as a direct
  dependency. Users who never import `o11y/gin` still resolve gin in
  their dependency graph (mitigated by Go's lazy module loading from
  Go 1.17+ — gin's transitives are not compiled into binaries that
  don't import the package).
- Maintenance burden: gin v1 → v2 (when it lands) will require a
  middleware port on our side rather than a library bump.
- `c.FullPath()` returns `""` for routes registered via gin's
  no-route / no-method handlers; the literals chosen in §4 are a
  policy decision that may need revisiting if user feedback shows
  they obscure useful signal.

**Open questions** (to resolve before status flips to Accepted)

- Should `Middleware` accept a `*o11y.SDK` convenience overload (e.g.
  `gin.MiddlewareFromSDK(ctx, obs, opts...)`)? Pro: ergonomics. Con:
  introduces a back-edge from `gin/` to the root package. Lean: **no**,
  consistent with `nats.Connect`.
- Should the cardinality limiter be **per-middleware-instance** (current
  `http/` behavior) or **shared** across all middleware instances on
  the same meter? Lean: per-instance, keep behavior identical to
  `http/`.
