# ADR 0009 — Resty Integration

**Status**: Proposed
**Date**: 2026-05-08

---

## Context

The SDK has no client-side HTTP instrumentation today. Services
calling other HTTP APIs lose the trace tree at the egress boundary:
the inbound server span exists, the next inbound span on the
downstream service exists, but no client span connects them, and no
`traceparent` header is injected unless the caller hand-writes the
propagator call.

`github.com/go-resty/resty/v2` is the dominant HTTP client in the
target codebases. Its hook system (`OnBeforeRequest`,
`OnAfterResponse`, `OnError`, `OnRetry`) is rich enough to build a
complete client instrumentation without monkey-patching the
`http.RoundTripper` chain.

Two off-the-shelf options exist:

- `github.com/go-resty/resty/v2`'s own
  `client.SetTransport(otelhttp.NewTransport(...))` — works at the
  transport layer, but loses resty-level information (request
  template, retry attempt index, structured error from `OnError`).
- Community packages such as `otelresty` — vary in maintenance
  status; none enforce the cardinality discipline ADR 0008 establishes
  for server-side metrics.

This ADR records the decision to ship a first-party `resty/` wrapper
that mirrors the architectural choices in ADR 0008.

Relevant existing files / context:

- ADR 0008 — Gin Integration (parallel decision, server-side)
- ADR 0003 — Global State Policy
- `nats/conn.go` — precedent for `Connect`-style wrappers that take
  `tp` / `prop`
- `o11y.go` — `*SDK.MeterProvider()`, `TracerProvider()`, `Propagator`

---

## Decisions

### 1. Build a first-party wrapper, do not depend on `otelresty` / `otelhttp`-only

`otelhttp.NewTransport` is rejected as the **primary** mechanism (it
remains usable by callers who prefer it; we just don't ship it as our
opinionated default) because:

- Resty-level signal is lost. `OnRetry` attempt index, original URL
  template before path-param substitution, and structured `error.type`
  from `OnError` are not visible at the `RoundTripper` boundary.
- `otelhttp` records `http.client.request.duration` with the
  request URL's host as `server.address`, which is correct, but also
  records `http.url` / `url.full` as a span attribute that includes
  the resolved path. If we want to control what flows into metrics
  vs. spans, we need a layer above the transport.

Community `otelresty` packages are rejected for the same reasons as
`otelgin` in ADR 0008: metric naming control, ADR 0003 audit burden,
and cardinality discipline.

### 2. Wrapper location: `resty/` under module root, single `go.mod`

Mirrors `gin/`, `nats/`, `http/`. Same trade-off as ADR 0008 §2:
users who do not import `github.com/flywindy/o11y/resty` carry resty
in their `go.mod` graph but not their compiled binary (Go 1.17+ lazy
loading).

### 3. Public API

```go
package resty

import (
    "github.com/go-resty/resty/v2"
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/trace"
)

// Wrap installs tracing + metrics hooks on an existing client. The
// caller retains control over timeouts, retries, base URL, and
// transport. Returns the same client for chaining.
func Wrap(
    rc *resty.Client,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) *resty.Client

// NewClient is sugar for Wrap(resty.New(), ...). Provided because most
// services build a client at startup with no special transport.
func NewClient(
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) *resty.Client
```

Shape rationale:

- `Wrap` over `NewClient` is the canonical entry point. Resty users
  almost always tune retry / timeout / middleware first; forcing them
  to thread those settings through our constructor would couple
  surface area unnecessarily.
- Same three-provider arity as `nats.Connect` and `gin.Middleware`.
- Idempotency: calling `Wrap` twice on the same client must not
  register duplicate hooks. Implementation detail: the wrapper stamps
  a sentinel via `rc.SetContext` / a custom client-level value to
  detect prior wrapping, and returns early on the second call. The
  sentinel mechanism is documented in the godoc.

`Option` initially exposes:

- `WithRouteFromContext(key any)` — see §5.
- `WithSpanNameFormatter(func(*resty.Request) string)` — override the
  default span name.
- `WithMetricRouteEnabled(bool)` — explicit opt-in to record `http.route`
  on the client metric. Default **off** (see §5).

### 4. Span model

One **client span per attempt**:

- Started in `OnBeforeRequest`. Span kind `SpanKindClient`.
- Span name: default `"{METHOD}"` (e.g. `"GET"`) when no route is
  available; `"{METHOD} {route}"` when a route template is supplied
  via `WithRouteFromContext`. Following OTel semconv guidance that
  client span names are low-cardinality.
- Attributes set on start: `http.request.method`, `server.address`,
  `server.port`, `url.full` (post-substitution; trace-only, not
  metric).
- Closed in `OnAfterResponse` (success and non-2xx alike) or `OnError`
  (transport / DNS / context cancellation). Final attributes:
  `http.response.status_code` (response only), `error.type` (error
  only — `errors.As` chain checked for `*url.Error`, `net.Error`,
  `context.DeadlineExceeded`; falls back to `reflect.TypeOf(err).String()`).
- Span status: `Error` for transport errors **or** status code
  `>= 400` (per OTel semconv: client side treats 4xx as Error because
  the client made an invalid request from its own perspective).
- `prop.Inject(ctx, propagation.HeaderCarrier(req.Header))` runs
  **after** span start in `OnBeforeRequest` so the downstream service
  receives a `traceparent` pointing at the attempt span, not its
  parent.

### 5. Metrics

Records into `http.client.request.duration` (OTel semconv stable
HTTP client metric).

**Default labels**: `http.request.method`, `server.address`,
`http.response.status_code`, `error.type`. **No path or route by
default.** Rationale:

- Client-side path cardinality is dictated by the API being called,
  not by code we control. A microservice that calls a downstream REST
  API with `/users/{uuid}` paths would emit one series per uuid
  unless we strip path entirely.
- OTel semconv explicitly notes `url.template` is optional on client
  metrics and recommends omitting it when not statically known.

**Opt-in path label** via `WithMetricRouteEnabled(true)` paired with
`WithRouteFromContext(key)`:

```go
client := resty.NewClient(tp, mp, prop,
    resty.WithMetricRouteEnabled(true),
    resty.WithRouteFromContext(routeKey{}),
)

ctx := context.WithValue(parent, routeKey{}, "/users/{id}")
client.R().SetContext(ctx).Get("/users/" + id)
```

This puts the cardinality decision in the caller's hands. Without the
option, no path-shaped label can leak into Prometheus.

### 6. Retry semantics

Resty retries are observed via the `OnBeforeRequest` /
`OnAfterResponse` / `OnRetry` cycle. Each invocation of
`OnBeforeRequest` corresponds to one network attempt.

The wrapper produces:

- One **child** client span per attempt, parented to whatever span is
  in `req.Context()` at attempt time (typically the caller's span).
- Attribute `http.request.resend_count` on attempts 2..N (per OTel
  semconv: omitted on the first attempt).
- One metric sample per attempt. The dashboard can sum or filter by
  `resend_count` to distinguish first-try latency from retry latency.

Retries are **not** wrapped in an outer "logical" span. Adding one
would require an additional hook to fire before the first attempt,
which resty does not expose; emulating it via `OnBeforeRequest` with
state would be fragile across resty versions.

### 7. Compliance with ADR 0003 (Global State Policy)

`github.com/go-resty/resty/v2` itself does not import OpenTelemetry.
The wrapper threads `tp`, `mp`, `prop` explicitly. No global is
touched.

The ADR 0003 approved-integrations table is updated in the same PR
that introduces this wrapper:

| Library | Version | Verified | Behavior | Notes |
|---|---|---|---|---|
| `github.com/go-resty/resty/v2` | (pinned at adoption) | ✅ | Pure HTTP client; does not import OTel. | See ADR 0009 |

### 8. Resty v3 readiness

resty v3 is in pre-release and rearranges several hook signatures.
This ADR commits to v2 only. A v3 port will be its own ADR amendment.
The wrapper's surface (`Wrap`, `NewClient`) is shaped to admit a v3
counterpart living under a sibling `resty/v3/` directory if needed,
without forcing a breaking change on v2 users.

---

## Implementation plan (informative; subject to its own PR review)

Per ADR 0008 §"Implementation plan", this is PR #3 in the sequence:

1. PR #1 — `internal/httpinstr/` refactor + `http/` server span (ADR 0008).
2. PR #2 — `gin/` package (ADR 0008).
3. **PR #3 — `resty/` package (this ADR).**

PR #3 depends on PR #1 only for shared attribute-builder helpers
(`internal/httpinstr/attrs.go`); it does not depend on PR #2.

---

## Consequences

**Positive**

- End-to-end trace continuity from gin server → resty client →
  downstream service, with `traceparent` injected automatically and
  `slog.InfoContext` in resty hooks carrying `traceId` / `spanId`
  attached to the client span.
- Client metric cardinality is bounded by default; path-shaped labels
  are opt-in.
- Retries are observable as distinct attempts without an out-of-band
  log scrape.

**Negative / Trade-offs**

- `github.com/go-resty/resty/v2` joins the SDK's direct dependencies.
- Idempotency of `Wrap` relies on a sentinel; future resty changes to
  client-level value semantics could weaken the guarantee.
- No outer "logical request" span across retries (see §6). Users who
  need that signal must create one in caller code.
- v3 port is deferred (§8); when v3 lands as stable, this wrapper will
  need a coordinated update or a parallel module path.

**Open questions** (to resolve before status flips to Accepted)

- Should the wrapper expose a helper to fetch the active client span
  from `req.Context()` for handlers that want to attach business
  attributes (e.g. tenant id) to the egress span? Lean: **yes**,
  but ship in PR #3 only if a concrete consumer exists.
- Should `OnError` distinguish "context canceled by caller" from
  "transport timeout" in `error.type`, or collapse them? Lean: keep
  them distinct using `errors.Is(err, context.Canceled)` /
  `errors.Is(err, context.DeadlineExceeded)`; document the matrix.
