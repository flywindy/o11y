# ADR 0011 — Resty Integration

**Status**: Proposed
**Date**: 2026-05-08

**Applies** ADR 0008 (sourcing policy); **builds on** ADR 0009
(`otelhttp` transport facade and metric.View cardinality control).

---

## Context

The SDK has no client-side HTTP instrumentation today. Internal
services using `github.com/go-resty/resty/v2` lose the trace tree at
egress: the inbound server span exists, the next inbound span on the
downstream service exists, but no client span connects them, and
`traceparent` is not injected unless the caller hand-writes the
propagator call.

This ADR applies ADR 0008 to resty. Per ADR 0008 §2, we evaluate
candidate libraries before defaulting to T3 (self-written).

### ADR 0008 §2 evaluation

**Candidate A: community `otelresty`-style packages**

Surveyed: `github.com/jkratz55/otelresty`, `github.com/AmineG7/otelresty`,
similar. Outcomes against the §2 checklist:

| Item | Result |
|---|---|
| ADR 0003 compliance | Mixed; some set globals during `New`. |
| Maintenance signal | ❌ Most have no commits in 12+ months; no stable v1 line. |
| Semconv alignment | ❌ Several emit pre-stable attribute keys (`http.method` instead of `http.request.method`). |
| Configurability | Mixed. |
| Framework signal access | ⚠️ Few expose `OnRetry` attempt index on the span. |

No candidate passes all five items. T2 facade adoption is not viable.

**Candidate B: pure `otelhttp.NewTransport` via `client.SetTransport`**

| Item | Result |
|---|---|
| ADR 0003 compliance | ✅ |
| Maintenance signal | ✅ OTel contrib. |
| Semconv alignment | ✅ v1.39.0 with current pin. |
| Configurability | ✅ |
| Framework signal access | ❌ Resty-specific signals (`OnRetry` attempt index, structured `OnError` error chain, route-from-context for metric labels) are invisible at the `RoundTripper` boundary. |

Four passes, one fail. The §5 fail is the operative one: a transport-
only solution loses retry observability, which is one of the primary
reasons services choose resty over `net/http`.

### Conclusion

The decision under ADR 0008 is a **justified T3** integration. We
write the resty wrapper ourselves, using `otelhttp.NewTransport` as
an internal building block where it carries weight (the actual span
creation and `traceparent` injection happen there) and adding resty-
aware glue on top via resty's hook system.

This is not a contradiction with ADR 0008. The policy permits T3
when the §2 checklist fails for every candidate library; it requires
the ADR to enumerate the failures (done above) and the upstream pieces
that are nonetheless reused (next section).

Relevant existing files / context:

- ADR 0008 — sourcing policy
- ADR 0009 — `otelhttp` facade and metric.View cardinality
- ADR 0010 — gin integration (parallel decision, server-side)

---

## Decisions

### 1. Architecture: `otelhttp.NewTransport` for span/metric, resty hooks for resty-only signals

The wrapper composes two layers, each owning a distinct concern:

| Layer | Source | Owns |
|---|---|---|
| Transport | `otelhttp.NewTransport` (T2 building block) | Client span creation, status code, `server.address`, `server.port`, base metric `http.client.request.duration`, `traceparent` injection |
| Hooks | Self-written, this package | Per-attempt span attribute `http.request.resend_count`, `OnError` error classification, optional `http.route` from context, sentinel for `Wrap` idempotency |

The hooks **do not start their own span**. They annotate the span
that `otelhttp.NewTransport` is about to create or has just created
via `c.Request.Context()` reads. This avoids duplicate spans and
inherits `otelhttp`'s correctness for the network-level signals.

Where resty's hook fires before `otelhttp`'s span exists
(`OnBeforeRequest`), the wrapper sets the attempt index into the
request `context.Context` via a private key; a small custom
`http.RoundTripper` interposed between resty and `otelhttp.NewTransport`
reads the key and calls `trace.SpanFromContext` after `otelhttp`
creates the span, then sets `http.request.resend_count`.

```
resty.Client
   │
   ├─ OnBeforeRequest hook ──► writes attempt index into req.Context
   │
   ▼
custom RoundTripper (this package) ──► reads attempt index, prepares ctx
   │
   ▼
otelhttp.NewTransport ──► creates client span, injects traceparent,
   │                       records http.client.request.duration
   ▼
http.DefaultTransport (or user-supplied base)
```

### 2. Public API

```go
package resty

import (
    "github.com/go-resty/resty/v2"
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/trace"
)

// Wrap installs tracing + metrics on an existing client. The caller
// retains control over timeouts, retries, base URL, and any other
// resty configuration. Returns the same client for chaining.
//
// Wrap is idempotent: calling it twice on the same client is a no-op
// after the first call.
func Wrap(
    rc *resty.Client,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) *resty.Client

// NewClient is sugar for Wrap(resty.New(), ...).
func NewClient(
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) *resty.Client
```

`Option` initially exposes:

- `WithRouteFromContext(key any)` — when set, the wrapper reads
  `req.Context().Value(key)` as a string and sets it as a route
  attribute on the span (for trace search) and, when paired with
  `WithMetricRouteEnabled(true)`, on the metric.
- `WithMetricRouteEnabled(bool)` — explicit opt-in; default `false`
  to keep client metric cardinality bounded by default (see §3).
- `WithSpanNameFormatter(func(*resty.Request) string)` — overrides the
  default `"{METHOD}"` / `"{METHOD} {route}"` span name.

### 3. Metrics

`otelhttp.NewTransport` already records `http.client.request.duration`.
Default labels emitted by `otelhttp`: `http.request.method`,
`server.address`, `http.response.status_code`, `error.type`, plus
`network.protocol.version`.

Cardinality-sensitive concerns:

- **Path / route** — by default not emitted as a metric label. The
  metric.View from ADR 0009 covers `http.client.request.duration`
  with the same allowlist filter and trims any high-cardinality
  attributes that future `otelhttp` versions might add.
- **Server address** — bounded by the set of upstream hosts the
  service calls. Acceptable.
- **Status code / method** — bounded by the HTTP spec. Acceptable.

Path label is opt-in via `WithMetricRouteEnabled` paired with
`WithRouteFromContext`:

```go
type routeKey struct{}

client := o11yresty.NewClient(tp, mp, prop,
    o11yresty.WithMetricRouteEnabled(true),
    o11yresty.WithRouteFromContext(routeKey{}),
)

ctx := context.WithValue(parent, routeKey{}, "/users/{id}")
client.R().SetContext(ctx).Get("/users/" + id)
```

The caller controls cardinality: only string templates the caller
explicitly attaches enter the metric.

### 4. Span model

- One span per attempt, kind `SpanKindClient`. Created by
  `otelhttp.NewTransport`, **not** by this package's hooks.
- Default span name: `"{METHOD}"` (low cardinality per OTel semconv
  for client spans). With `WithSpanNameFormatter` or
  `WithRouteFromContext` set, name becomes `"{METHOD} {route}"`.
- Attributes set by `otelhttp`: `http.request.method`,
  `server.address`, `server.port`, `http.response.status_code`,
  `network.protocol.version`. Set by this package via the
  intermediate RoundTripper: `http.request.resend_count` (attempts
  ≥ 2), `http.route` (if `WithRouteFromContext` resolves).
- Span status: set by `otelhttp` for transport errors and ≥ 4xx (per
  OTel semconv: client treats 4xx as Error because the request was
  malformed from the client's perspective).
- `error.type`: set by `otelhttp` for transport errors based on Go
  standard error types (`*net.OpError`, `*url.Error`,
  `context.DeadlineExceeded`, `context.Canceled`). Resty's
  `OnError`-only signals (e.g. retry budget exhausted) are added to
  the **last** attempt's span by a hook.

### 5. Retry semantics

Each resty retry produces a fresh `OnBeforeRequest` → RoundTrip cycle,
which means a fresh `otelhttp` span per attempt. Attempt index is
written into `req.Context()` via a private key in `OnBeforeRequest`;
the intermediate RoundTripper reads it post-span-creation and sets
`http.request.resend_count` on attempts ≥ 2.

There is no "outer logical span" across retries. Resty does not
expose a hook that fires once before the first attempt and once after
the last; emulating one with state would be fragile across resty
minor versions. Callers who need a logical-request span create one
themselves before calling `client.R().Do()`.

### 6. `Wrap` idempotency

The wrapper stamps a sentinel onto the resty client (private struct
key in the client's value store) on first call and short-circuits
subsequent calls. This protects against double-installation when a
service calls `Wrap` from multiple init paths.

### 7. Compliance with ADR 0003

`go-resty/resty/v2` itself does not import OpenTelemetry.
`otelhttp.NewTransport` reads globals as fallback only; the wrapper
always supplies `WithTracerProvider`, `WithMeterProvider`,
`WithPropagators`. No global is touched.

The ADR 0003 §"Approved integrations" table is updated in the same PR:

| Library | Version | Verified | Behavior | Notes |
|---|---|---|---|---|
| `github.com/go-resty/resty/v2` | (pinned) | ✅ | Pure HTTP client; no OTel coupling. | See ADR 0011 |

### 8. Resty v3 readiness

resty v3 is in pre-release at the time of this ADR. Hook signatures
shift between v2 and v3. This ADR commits to v2 only. A v3 port lands
under a sibling `resty/v3/` directory if and when v3 stabilizes; v2
users are not forced to migrate.

---

## Consequences

**Positive**

- End-to-end trace continuity: gin server (ADR 0010) → resty client
  (this ADR) → downstream service. `traceparent` injection is
  automatic via `otelhttp`.
- Client metric cardinality is bounded by default; route labels are
  opt-in.
- Retries are observable as distinct attempts with
  `http.request.resend_count`, so dashboards can separate first-try
  latency from retry latency.
- Network-layer correctness (DNS, connection reuse, redirects,
  HTTP/2 stream errors) is inherited from `otelhttp`, not
  re-implemented.

**Negative / Trade-offs**

- T3 means we own the resty-specific code permanently. The §2
  evaluation must be re-run on every resty major bump and at least
  annually for community-lib re-evaluation; the ADR 0009 path
  (T2 facade) becomes available if a maintained `otelresty` emerges.
- `Wrap` idempotency relies on a sentinel. If a future resty release
  changes the client value store semantics, the sentinel may need to
  move to a `sync.Map` keyed by client pointer.
- No outer logical span across retries (§5). Callers needing that
  signal create it themselves.
- `WithRouteFromContext` requires the caller to construct a context
  per request. Higher-friction than gin's automatic `c.FullPath()`,
  but unavoidable on the client side where path templates are not
  available without caller intent.

---

## Open questions

- **Wrap idempotency mechanism.** Resty v2's client value store is
  fine for now. If we move to a process-global `sync.Map` keyed by
  `*resty.Client`, we need to ensure entries are evicted when clients
  are GC'd (use `runtime.SetFinalizer` or a weak map). Lean: keep
  client-local sentinel for v2; revisit for v3.
- **`error.type` granularity.** `otelhttp` sets it from Go error
  types. Should the resty hook overwrite with resty's own classification
  (e.g. distinguish "retry budget exhausted" from "single-attempt
  transport error")? Lean: do not overwrite; add `resty.error.kind`
  as a span-only attribute when resty has classification information.
- **Per-host cap configuration.** Currently `server.address` cardinality
  is implicitly bounded by the number of distinct upstream hosts. If
  a future use case calls dynamic hosts (URL shorteners, multi-tenant
  egress), we may need per-host cap config in the metric.View. Lean:
  defer until a concrete case appears.
