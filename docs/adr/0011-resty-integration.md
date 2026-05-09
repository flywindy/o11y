# ADR 0011 — Resty Integration

**Status**: Proposed
**Date**: 2026-05-08

**Applies** ADR 0008 (sourcing policy); **inherits the metric.View
cardinality controls established by** ADR 0009 (the
`http.client.request.duration` allowlist applies to histograms
emitted by this package automatically).

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

The decision under ADR 0008 is **pure T3**: write the resty wrapper
from OTel primitives, with **no otelhttp dependency** in this
package. The earlier draft of this ADR proposed a hybrid (otelhttp
as base transport + resty hooks for annotations), but a careful read
of `otelhttp.Transport.RoundTrip` shows the hybrid cannot work:

- `otelhttp` opens its per-attempt span at the entry of `RoundTrip`
  and ends it via `defer span.End()` before `RoundTrip` returns.
- Resty's `OnAfterResponse` / `OnError` hooks fire **after**
  `Client.execute` receives the result of `RoundTrip` — i.e. after
  the span has already been ended.
- The OTel SDK contract is that `SetAttributes` / `RecordError` /
  `SetStatus` / `AddEvent` are no-ops on an Ended span. So
  resty-level signals (`resty.error.kind`, `http.request.resend_count`
  for the just-completed attempt, route-from-context, retry-exhausted
  marker) cannot reach the otelhttp-created span via hooks.

Workarounds (custom base RoundTripper that annotates from inside
`otelhttp`'s RoundTrip; emitting an outer "logical" span in addition
to otelhttp's per-attempt spans; double-spanning) all either fail to
deliver retry-aware signals or violate the ~100 LOC budget for a
single integration. Owning span lifecycle in the resty hooks is
strictly simpler and matches what resty hooks were designed for.

This is not a contradiction with ADR 0008. The policy permits T3
when the §2 checklist fails for every candidate library; it requires
the ADR to enumerate the failures (done above) and to justify that
self-writing is the smallest viable answer (the lifecycle analysis
above).

Relevant existing files / context:

- ADR 0008 — sourcing policy
- ADR 0009 — `otelhttp` facade and metric.View cardinality (the
  metric.View defined there covers `http.client.request.duration`
  regardless of which library produces the histogram, so this
  package's metric inherits the cardinality view automatically)
- ADR 0010 — gin integration (parallel decision, server-side)

---

## Decisions

### 1. Architecture: resty hooks own span and metric lifecycle

The wrapper installs three resty hooks. No custom `RoundTripper` and
no `otelhttp` involvement; resty's default transport handles the
network and the hooks own everything observability-related.

| Hook | Owns |
|---|---|
| `OnBeforeRequest` | Start client span via `tp.Tracer(...).Start`, set client attributes (`http.request.method`, `server.address`, `server.port`, `url.full`, `http.request.resend_count` on attempts ≥ 2, `http.route` if `WithRouteFromContext` resolves), inject `traceparent` into `req.Header` via `prop.Inject`, attach the span to `req.Context()` so handler-side hooks and the resty Request itself see it, capture `time.Now()` for the histogram. |
| `OnAfterResponse` | Set `http.response.status_code`; set span status (Error for ≥ 4xx per OTel client semconv, Unset otherwise); for status 408/504 also set `resty.error.kind=server_timeout`; record the histogram sample; end the span. |
| `OnError` | Classify the error into `resty.error.kind` (§8); set `error.type` to the underlying Go type name; set span status Error; record the histogram sample with `error.type` label; end the span. If the error is the last attempt under retry policy, additionally tag `resty.error.kind=retry_exhausted` (§8 row 7). |

```text
resty.Client.R().Get(...)
   │
   ├─ OnBeforeRequest ──► tracer.Start(ctx, "{METHOD}"...)
   │                       prop.Inject(req.Header)
   │                       req.SetContext(ctxWithSpan)
   │                       record start time
   │
   ▼
resty's default transport ──► http.DefaultTransport.RoundTrip
   │                            (no instrumentation here)
   ▼
   response or error
   │
   ├─ OnAfterResponse ──► span.SetAttributes(status_code, ...)
   │                       span.SetStatus(...)
   │                       histogram.Record(elapsed, attrs)
   │                       span.End()
   │
   └─ OnError ──────────► classify error → resty.error.kind
                           span.RecordError(err)
                           span.SetStatus(Error)
                           histogram.Record(elapsed, attrs)
                           span.End()
```

We control the span open/close window so both `OnAfterResponse` and
`OnError` operate on a live span. The lifecycle problem that ruled
out the otelhttp-hybrid design (Conclusion above) does not exist
here.

The hooks are **idempotent under retry**: each attempt enters
`OnBeforeRequest` fresh and produces its own span. Retries become
sibling spans under whatever caller-supplied parent span is in
`req.Context()` when the request is initiated.

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

The wrapper creates one histogram instrument at `Wrap` time:

```go
hist, _ := mp.Meter("github.com/flywindy/o11y/resty").
    Float64Histogram(
        "http.client.request.duration",
        metric.WithUnit("s"),
        metric.WithDescription("Duration of outbound HTTP client requests."),
    )
```

Histogram name and unit match OTel semconv stable HTTP client metric.
The metric.View registered by `o11y.Init` (ADR 0009 §2 Layer A
client-side allowlist) applies automatically to this histogram
because it is keyed by name, not by source library.

Default labels recorded by the wrapper: `http.request.method`,
`server.address`, `server.port`, `http.response.status_code` (for
response paths) or `error.type` (for error paths). The ADR 0009
client-side allowlist permits exactly this set; anything beyond it
is dropped at the View.

Cardinality-sensitive concerns:

- **Path / route** — not recorded by default. See opt-in below.
- **Server address / port** — bounded by the set of upstream hosts
  the service calls. Acceptable.
- **Status code / method / error.type** — bounded enums. Acceptable.

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

- One span per attempt, kind `SpanKindClient`. Created in
  `OnBeforeRequest`, ended in `OnAfterResponse` or `OnError`. Tracer
  name: `"github.com/flywindy/o11y/resty"`.
- Default span name: `"{METHOD}"` (low cardinality per OTel semconv
  guidance for client spans). With `WithSpanNameFormatter` or
  `WithRouteFromContext` set, name becomes `"{METHOD} {route}"`.
- Attributes set in `OnBeforeRequest`: `http.request.method`,
  `server.address`, `server.port`, `url.full`,
  `http.request.resend_count` (only on attempts ≥ 2; OTel semconv
  omits it on the first attempt), `http.route` (if
  `WithRouteFromContext` resolves a non-empty value).
- Attributes set in `OnAfterResponse`: `http.response.status_code`.
  Plus `resty.error.kind=server_timeout` for status 408 / 504.
- Attributes set in `OnError`: `error.type` (Go type name from
  `errors.As` / `reflect`), `resty.error.kind` (§8 taxonomy).
  `error.type` is also recorded as a label on the histogram sample
  for the failed attempt.
- Span status: Error for transport errors (set in `OnError`) **or**
  HTTP status `>= 400` (set in `OnAfterResponse`, per OTel semconv
  client-side convention that 4xx is Error because the request was
  malformed from the client's perspective). Unset otherwise.
- `traceparent` injection: `prop.Inject(ctx, propagation.HeaderCarrier(req.Header))`
  in `OnBeforeRequest` after the span is started, so the downstream
  service receives a `traceparent` pointing at this attempt's span.

### 5. Retry semantics

Each resty retry produces a fresh `OnBeforeRequest` →
`OnAfterResponse`/`OnError` cycle, which means a fresh wrapper-owned
span per attempt. The wrapper reads `req.Attempt` (resty v2's
attempt counter on `*resty.Request`) inside `OnBeforeRequest` and
sets `http.request.resend_count` on attempts ≥ 2.

There is no "outer logical span" across retries. Resty does not
expose a hook that fires once before the first attempt and once
after the last; emulating one with state would be fragile across
resty minor versions. Callers who need a logical-request span
create one themselves before calling `client.R().Do()`.

### 6. `Wrap` idempotency

`Wrap` must be safe to call twice on the same `*resty.Client`. The
exact mechanism is an implementation detail of the PR (resty v2 does
not expose a general-purpose value store on `Client`, so the
sentinel cannot be a simple key/value); candidate mechanisms include
(a) checking whether the client's transport is already an instance
of our intermediate `RoundTripper` type, (b) maintaining a
package-level `sync.Map` keyed by `*resty.Client` with weak-reference
semantics, or (c) stamping a marker hook in the client's
`OnBeforeRequest` chain and detecting it on second call.

The implementation PR picks one and documents the choice in godoc.
This ADR commits only to the **contract** (idempotent `Wrap`), not to
the mechanism.

### 7. Compliance with ADR 0003

`go-resty/resty/v2` itself does not import OpenTelemetry. The
wrapper takes `tp`, `mp`, and `prop` as explicit parameters and
never reads or writes any `go.opentelemetry.io/otel` global. There
is no third-party OTel instrumentation library in this package's
dependency graph after the pivot away from `otelhttp.NewTransport`,
so the ADR 0003 audit surface for this integration is bounded to
our own code.

The ADR 0003 §"Approved integrations" table is updated in the same PR:

| Library | Version | Verified | Behavior | Notes |
|---|---|---|---|---|
| `github.com/go-resty/resty/v2` | (pinned) | ✅ | Pure HTTP client; no OTel coupling. | See ADR 0011 |

### 8. `resty.error.kind` taxonomy

`error.type` (set by this wrapper from Go error class names —
`*net.OpError`, `*url.Error`, `context.DeadlineExceeded`,
`context.Canceled`) is correct for protocol/transport classification
but hides whether the failure came from resty's retry policy, the
user's timeout, or the network itself — which is what an SRE
actually wants to filter dashboards by.

This package adds a **span-only** attribute `resty.error.kind`
(intentionally not on metrics; see §3 cardinality logic) with a
fixed, closed enumeration. The enum is set from the wrapper's
`OnError` (and, for `server_timeout`, `OnAfterResponse`) hook based
on the structured error returned by resty/`http`/`net`/`context`.

**Detection runs in the order listed below; first match wins.** The
ordering matters because some Go error sentinels have an
`Is`-relationship: `context.DeadlineExceeded` is a kind of "context
cancellation" but is more specific than `context.Canceled`, so it
must be checked first.

| # | Value | Trigger | Detection |
|---|---|---|---|
| 1 | `client_canceled` | The user canceled the context (deliberate abort, no deadline involved) | `errors.Is(err, context.Canceled)` **and not** `errors.Is(err, context.DeadlineExceeded)` |
| 2 | `client_timeout` | The user's `context.WithTimeout` / `SetTimeout` deadline expired before the response was complete | `errors.Is(err, context.DeadlineExceeded)` |
| 3 | `server_timeout` | The downstream server returned 408 or 504 | inspected post-response in `OnAfterResponse`; sets the kind on the span before `OnError` would fire |
| 4 | `tls` | TLS handshake or certificate verification failure | `errors.As` to `*tls.CertificateVerificationError`, `tls.RecordHeaderError`, or a `*net.OpError` with `Op == "remote error"` and a TLS-shaped inner error |
| 5 | `transport` | DNS resolution failure, TCP connect refused, RST mid-stream (non-TLS) | `errors.As` to `*net.OpError` (after the `tls` row excluded the TLS sub-cases) |
| 6 | `protocol` | HTTP/2 stream error, malformed response, framing error | `errors.As` to `http2.StreamError`, or other `http.*` parse errors |
| 7 | `retry_exhausted` | Resty's retry policy gave up: the request reached its configured retry limit and the last attempt still failed | per-attempt span keeps the kind from rows 1–6 above; the **last** attempt's span also gets `resty.error.kind=retry_exhausted` (additional attribute, not overwriting). If a caller-supplied parent span exists in `req.Context()`, the same `retry_exhausted` event is recorded on that parent span via `span.AddEvent`. Detected in `OnError` by inspecting resty's request attempt count against its retry budget. |
| 8 | `unknown` | Default when no row above matches | otherwise |

The mapping function is unit-tested with table-driven cases for each
row using the relevant fixture errors (`fixtures.DialRefused()`,
`fixtures.TLSBadCert()`, `fixtures.ContextCanceled()`,
`fixtures.ContextDeadline()`, etc.), constructed with the real
underlying types so `errors.As`/`Is` behave as in production.

**Logging is left to the caller.** The wrapper sets the attribute on
the span only; it does not call `slog` itself. Services that want
"log-on-error" either install their own resty `OnError` hook (it
runs alongside ours, no conflict) or read the active span's
attributes from inside the handler. This keeps the wrapper focused
on the trace/metric pipeline and lets callers decide their own
log volume policy.

`resty.error.kind` is **not** an `error.type` replacement; both
attributes coexist on the span. `error.type` answers "what Go type
was the error" (programmer view); `resty.error.kind` answers "what
class of failure does an operator see" (SRE view). Both are set by
this package.

### 9. Golden trace tests for retry / timeout

Retry and timeout flows are the highest-value resty behaviors and the
most likely to regress when resty's hook ordering shifts between
versions or when our error classification logic drifts. The
implementation PR ships a **golden-trace** test suite that asserts
the recorded span tree byte-for-byte against committed expected JSON
(with timestamp and span-id fields blanked out).

Test fixtures use an in-process `httptest.Server` that can be
configured per scenario; the trace is captured via an in-memory
`tracetest.SpanRecorder` exporter wired to the SDK's TracerProvider.

| # | Scenario | Server behavior | Resty config | Expected span tree shape |
|---|---|---|---|---|
| 1 | Single success | 200 OK | no retries, no timeout | Caller → 1 client span (`status_code=200`, no `resend_count`, no `resty.error.kind`) |
| 2 | Single transport error, no retry | connection refused | no retries | Caller → 1 client span (status `Error`, `error.type=*net.OpError`, `resty.error.kind=transport`) |
| 3 | Retry succeeds on attempt 2 | 503, 200 | `SetRetryCount(3)` | Caller → 2 sibling client spans: span#1 (`status_code=503`, `resend_count` absent), span#2 (`status_code=200`, `resend_count=1`) |
| 4 | Retry exhausted | 503 × 4 | `SetRetryCount(3)` | Caller → 4 sibling client spans, attempt indices 0..3, last span has `resty.error.kind=retry_exhausted` |
| 5 | Client timeout mid-attempt | server hangs | `SetTimeout(50ms)`, no retries | Caller → 1 client span, status `Error`, `error.type=context.DeadlineExceeded`, `resty.error.kind=client_timeout` |
| 6 | Client timeout across multiple attempts | server delays response by `2 * timeout` per attempt (deterministic) | `SetTimeout(50ms)`, `SetRetryCount(3)` | Caller → ≥ 1 client span; **the last span** has `resty.error.kind=client_timeout` and `error.type=context.DeadlineExceeded`; `retry_exhausted` is **not** set because the failure mode was the user-level deadline. Assertions are restricted to the last span and the kind attribute — span count is not asserted, since it depends on scheduler timing. |
| 7 | Caller-canceled mid-flight | server hangs | no timeout, but caller cancels at 100 ms | Caller → 1 client span, `error.type=context.Canceled`, `resty.error.kind=client_canceled`, `Error` status |
| 8 | Server returns 504 (not retried by default) | 504 | no retries | Caller → 1 client span, `status_code=504`, `Error` status, `resty.error.kind=server_timeout` |
| 9 | TLS handshake failure | `httptest.NewTLSServer` with mismatched CA | resty pointed at the server, no `SetTLSClientConfig` skip | Caller → 1 client span, `error.type=*tls.CertificateVerificationError`, `resty.error.kind=tls` |
| 10 | Trace propagation across attempts | 503, 200 | `SetRetryCount(1)` | Both attempt requests' inbound `traceparent` headers (captured server-side) reference the **same trace id** as the caller span, but **different parent-span ids** matching their respective attempt spans |

Rows 1–9 assert one trace tree each. Row 10 additionally asserts the
propagator behavior across retries by inspecting the headers the
test server received.

The expected JSON files live at
`resty/testdata/golden/<scenario>.json`. The test harness includes
an `UPDATE_GOLDEN=1` env-gated regenerator (mirrors a common Go
testing idiom) so intentional changes to span shape are explicit
diffs in the PR.

### 10. Resty v3 readiness

resty v3 is in pre-release at the time of this ADR. Hook signatures
shift between v2 and v3. This ADR commits to v2 only. A v3 port lands
under a sibling `resty/v3/` directory if and when v3 stabilizes; v2
users are not forced to migrate.

---

## Consequences

**Positive**

- End-to-end trace continuity: gin server (ADR 0010) → resty client
  (this ADR) → downstream service. `traceparent` injection happens
  in `OnBeforeRequest` after the span is started, so downstream
  services see a parent that is alive and recordable.
- Client metric cardinality is bounded by default (allowlist view
  from ADR 0009 §2); route labels are opt-in.
- Retries are observable as distinct sibling spans with
  `http.request.resend_count`, so dashboards can separate first-try
  latency from retry latency.
- Span lifecycle matches resty's hook lifecycle, so every hook fires
  on a live span. No no-op `SetAttributes` after end.
- One package, no `otelhttp` import for resty users. Smaller
  dependency surface.

**Negative / Trade-offs**

- T3 means we own the resty-specific code permanently, including
  network-layer behaviors that `otelhttp` would have given us for
  free (e.g. handling redirect chains as separate spans, baggage
  semantics across retries). For resty's surface this is bounded
  because resty's redirect / retry behavior is itself bounded by
  resty's options, but it is real maintenance.
- The §2 evaluation must be re-run on every resty major bump and at
  least annually for community-lib re-evaluation; the ADR 0009 path
  (T2 facade) becomes available if a maintained `otelresty` emerges.
- `Wrap` idempotency mechanism is an implementation choice (§6),
  not pinned by this ADR.
- No outer logical span across retries (§5). Callers needing that
  signal create it themselves.
- `WithRouteFromContext` requires the caller to construct a context
  per request. Higher-friction than gin's automatic `c.FullPath()`,
  but unavoidable on the client side where path templates are not
  available without caller intent.

---

## Open questions

- **Wrap idempotency mechanism.** Three candidates listed in §6:
  transport-chain check, package-level `sync.Map` with weak refs,
  or a marker hook in `OnBeforeRequest`. The implementation PR picks
  one based on a small benchmark and documents the choice in godoc.
  If `sync.Map` wins, it must use `runtime.SetFinalizer` or
  Go 1.24's experimental weak references to avoid leaking entries
  for collected clients.
- **Per-host cap configuration.** Currently `server.address` cardinality
  is implicitly bounded by the number of distinct upstream hosts. If
  a future use case calls dynamic hosts (URL shorteners, multi-tenant
  egress), we may need per-host cap config in the metric.View. Lean:
  defer until a concrete case appears.
