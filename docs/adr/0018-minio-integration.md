# ADR 0018 — MinIO / S3 Object Storage Integration

**Status**: Proposed
**Date**: 2026-05-27

**Applies** ADR 0008 (sourcing policy); **inherits the metric.View
cardinality controls established by** ADR 0009 (the
`http.client.request.duration` allowlist applies to any HTTP client
histogram produced under this package's optional transport layer);
**reuses** the `otelhttp` approved-integration entry from ADR 0009.

---

## Context

The SDK has no object-storage instrumentation today. Services that
upload and download files through `github.com/minio/minio-go/v7`
(MinIO, and any S3-compatible backend) get no span for the storage
operation: the inbound server span exists, an adjacent `redis` or
`mongo` span exists, but the object-storage round-trip is invisible.

The motivating incident: a file-upload API span measured **~16 s
wall-clock**, while the CPU profile for the same window totalled only
**~2 s**. The `redis` span on that trace was present and fast. The
remaining ~14 s was off-CPU wait (network I/O to the object store)
that is invisible to both the CPU profiler (ADR 0012 default
`ProfileTypes` are CPU + alloc/inuse — no block/mutex profile) **and**
to the trace, because nothing instruments the MinIO call. The time
appeared only as an unattributed gap inside the parent span.

This ADR applies ADR 0008 to MinIO. Per ADR 0008 §2 we evaluate
candidate libraries before defaulting to T3 (self-written).

### ADR 0008 §2 evaluation

**Candidate A: a maintained OTel instrumentation library for the
data-plane client (`minio-go/v7`).**

| Item | Result |
|---|---|
| ADR 0003 compliance | — |
| Maintenance signal | ❌ **No such library exists.** `minio-go/v7` ships no first-party OTel instrumentation, and `opentelemetry-go-contrib` has no `otelminio`. The only MinIO package that carries OpenTelemetry code is `minio/madmin-go` (`opentelemetry.go`), which instruments the **admin** API (server status, healing, config), not the data-plane `PutObject` / `GetObject` path that the upload service uses. |
| Semconv alignment | — |
| Configurability | — |
| Framework signal access | — |

No candidate exists to evaluate; the maintenance-signal item fails by
absence. T2 facade adoption over a MinIO-specific library is not
viable.

**Candidate B: pure `otelhttp.NewTransport` via `minio.Options.Transport`.**

`minio-go/v7` exposes exactly one observability seam — the
`Transport http.RoundTripper` field on `minio.Options`. It has **no**
hook / middleware / event-listener API analogous to go-redis's `Hook`
interface (ADR 0013) or resty's `OnBeforeRequest` (ADR 0011), so the
only library-native interception point is the HTTP transport.

| Item | Result |
|---|---|
| ADR 0003 compliance | ✅ (the SDK's `http.NewTransport`, ADR 0009) |
| Maintenance signal | ✅ OTel contrib. |
| Semconv alignment | ✅ v1.39.0 HTTP client semconv. |
| Configurability | ✅ |
| Framework signal access | ❌ **The operative fail.** The HTTP boundary cannot express a *logical* object-storage operation. See below. |

Why the §5 fail is decisive — a transport-only solution gives HTTP
spans, not object-storage spans:

1. **No logical operation identity.** A single `PutObject` of a large
   file becomes a multipart upload: an `InitiateMultipartUpload`
   POST, N `UploadPart` PUTs, and a `CompleteMultipartUpload` POST.
   The transport sees N+2 unrelated HTTP requests, never one
   "PutObject took 16 s" span. Distinguishing `GetObject` from
   `StatObject` (HEAD) from `ListObjects` from the HTTP verb +
   URL alone is brittle and incomplete.
2. **No object-storage attributes.** Bucket, object key, object size,
   storage-system identity, and the structured S3 error code
   (`NoSuchKey`, `AccessDenied`, …) are not derivable at the
   `RoundTripper` boundary without re-parsing S3 REST semantics from
   raw requests — i.e. reimplementing the client.
3. **The original goal was per-operation attribution.** The incident
   needs "this span = one PutObject, here is where its 16 s went",
   which is precisely the signal the HTTP layer cannot synthesize.

Four passes, one decisive fail.

### Conclusion

The decision under ADR 0008 is **T3 (self-written) for the logical
operation layer**, with **optional reuse of the ADR 0009 `otelhttp`
transport as a nested HTTP layer** underneath it.

Unlike resty (ADR 0011), the hybrid composition is sound here, and the
lifecycle objection that ruled out resty's hybrid does **not** apply:

- In resty, `otelhttp` opened and `defer`-ended its span *inside*
  `RoundTrip`, before resty's `OnAfterResponse` hook could annotate
  it — so the resty signals had no live span to attach to.
- Here, **the SDK owns the outer logical span across the entire
  method call** (`PutObject` entry → return). The `otelhttp` transport
  spans are genuine child round-trips that open and close *within*
  that window. There is no annotation-after-end problem: the logical
  span is annotated by our wrapper directly, and the HTTP children are
  independent, correctly-nested spans (one per multipart part). The
  two layers compose without contending for the same span.

This satisfies ADR 0008: the §2 checklist fails for every candidate
data-plane library (enumerated above), and self-writing the logical
layer is the smallest answer that delivers per-operation attribution.

Relevant existing files / context:

- ADR 0008 — sourcing policy (T2 default, T3 exception gate)
- ADR 0009 — `otelhttp` facade and the `http.client.request.duration`
  metric.View cardinality allowlist (applies automatically to the
  optional transport layer here)
- ADR 0011 — resty integration (the T3 precedent; contrasted above)
- ADR 0012 — profiling (explains why the CPU profile did not see the
  off-CPU storage wait)
- ADR 0013 — redis integration (the hook-based T3 shape MinIO cannot
  use, since `minio-go` exposes no hook API)

---

## Decisions

### 1. Architecture: a method-wrapping client owns the logical span

`minio-go` has no hook API, so the wrapper cannot be a hook (redis) or
a middleware (resty). It is a **client wrapper** that embeds
`*minio.Client` (the shape `mongo/` uses, ADR 0005) and overrides the
data-plane methods to bracket each call in a span.

```text
svc.PutObject(ctx, bucket, key, r, size, opts)
   │
   ├─ tracer.Start(ctx, "PutObject {bucket}", SpanKindClient)
   │    set minio.* attributes (bucket, key, size, operation, system)
   │    record start time
   │
   ▼  delegate to embedded *minio.Client.PutObject(spanCtx, ...)
   │       │
   │       └─ (optional) o11y http.NewTransport child spans:
   │             InitiateMultipartUpload / UploadPart×N / Complete
   │
   ├─ on return: set status_code / minio.error.kind, record histogram
   └─ span.End()
```

Two layers, both optional-composable:

- **Logical layer (always on, T3):** one span per high-level
  operation, with object-storage attributes. This is the layer that
  answers the incident.
- **HTTP layer (opt-in via constructor):** `http.NewTransport`
  (ADR 0009) injected into `minio.Options.Transport`, producing child
  spans for the actual round-trips — including each multipart part, so
  an operator can see *which* part of a 16 s upload stalled.

### 2. Public API

```go
package minio

import (
    miniogo "github.com/minio/minio-go/v7"
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/trace"
)

// Client is a tracing-aware MinIO / S3 client. It embeds the upstream
// *minio.Client so the full driver API stays available; the data-plane
// methods listed in §5 are shadowed by instrumented overrides.
type Client struct {
    *miniogo.Client
}

// New constructs an instrumented client. The transport seam is set at
// construction (minio-go only accepts Options.Transport at New time),
// so New is the only entry point that can enable the optional HTTP
// child-span layer (§1).
//
// tp/mp/prop are required and are passed explicitly; the wrapper never
// reads or writes OTel globals (ADR 0003).
func New(
    endpoint string,
    minioOpts *miniogo.Options,
    tp trace.TracerProvider,
    mp metric.MeterProvider,
    prop propagation.TextMapPropagator,
    opts ...Option,
) (*Client, error)
```

```go
type Option func(*config)

// WithHTTPChildSpans injects o11y http.NewTransport into the client's
// transport so each underlying HTTP round-trip (incl. multipart parts)
// becomes a child span of the logical operation span. Default: off.
// When on, the transport is configured with a no-op propagator (§7).
//
// To avoid silently changing minio-go's data-plane HTTP semantics,
// the base RoundTripper that o11y wraps is selected as:
//
//   - minioOpts.Transport, if the caller already set it; the
//     wrapper does not replace caller-supplied transports.
//   - otherwise miniogo.DefaultTransport(minioOpts.Secure) — minio-go's
//     own default transport (response decompression disabled, S3-tuned
//     timeouts and connection pool), NOT http.DefaultTransport. The
//     SDK's http.NewTransport substitutes http.DefaultTransport when
//     handed a nil base, which would re-enable compression and shift
//     pool/timeout behavior; this wrapper passes minio-go's default
//     explicitly to keep enabling child spans behavior-neutral.
func WithHTTPChildSpans(enabled bool) Option

// WithSpanNameFormatter overrides the default "{Operation} {bucket}"
// span name.
func WithSpanNameFormatter(func(op, bucket, key string) string) Option

// WithObjectKeyAttribute controls whether the (potentially high-
// cardinality) object key is set on the span. Default: true (spans
// tolerate high cardinality; metrics never carry the key — §3).
func WithObjectKeyAttribute(enabled bool) Option

// WithAWSS3CompatAttributes additionally emits the experimental
// aws.s3.* attribute keys (aws.s3.bucket, aws.s3.key) alongside the
// default object_store.* attributes (§4), for compatibility with
// dashboards and processors keyed on the OTel AWS-S3 semantic
// conventions. The two sets are dual-emitted — generic stays on
// when this is enabled. Default: false.
func WithAWSS3CompatAttributes(enabled bool) Option
```

There is intentionally **no** `Wrap(existing *minio.Client)`. The
transport seam is only settable at `minio.New` time, so wrapping a
pre-built client could offer the logical layer but never the HTTP
child-span layer; exposing a half-capable entry point invites the
"why are my part spans missing" support load. Callers construct
through `minio.New`.

### 3. Metrics

One histogram is created at `New` time:

```go
hist, _ := mp.Meter("github.com/flywindy/o11y/minio").
    Float64Histogram(
        "minio.client.operation.duration",
        metric.WithUnit("s"),
        metric.WithDescription("Duration of MinIO/S3 logical operations."),
    )
```

A package-specific metric name (not `http.client.request.duration`) is
used deliberately: the logical operation and the underlying HTTP
round-trips are different units of work, and reusing the HTTP metric
name would double-count whenever `WithHTTPChildSpans` is on (the
transport already emits `http.client.request.duration`, governed by
the ADR 0009 allowlist).

Labels — a closed, bounded set, matching the span attribute schema
in §4:

- `object_store.operation.name` (enum: `PutObject`, `GetObject`, …) —
  bounded.
- `object_store.bucket.name` — bounded by the service's bucket set.
  Acceptable.
- `server.address`, `server.port` — bounded by the set of MinIO
  endpoints the service connects to (typically 1–3, constant per
  `*Client` instance). Included to mirror the HTTP client metric
  label set in ADR 0009 §2 Layer A, so dashboards keyed on
  `server.address` aggregate consistently across the HTTP and
  object-store layers and so multi-endpoint deployments can break
  out per-backend latency.
- `error.type` — bounded. Encodes both the failure class and (by
  its absence on success) the success/failure axis itself; this
  matches the OTel HTTP client metric pattern. **No `otel.status_code`
  label** is set: the OTel registry defines that attribute's values
  as `OK`/`ERROR` only, with the attribute absent for `UNSET`, so a
  literal `"Unset"` value would be non-conformant with semconv
  v1.39.0 and would not be interpreted as a span status by
  downstream processors.

Explicitly **not** a label:

- `object_store.object.key` — unbounded. Span attribute only (§4),
  never a metric dimension. A package-owned `metric.View` drops any
  key that escapes the closed set, mirroring ADR 0009 §2 Layer A
  defense-in-depth.

OTel Go `View`s are fixed when the SDK `MeterProvider` is built and
cannot be retro-fitted at instrument creation. Following the
established repo pattern (see `redis/views.go`, `mongo/views.go`,
and the `ExtraViews` composition at `o11y.go:234`), this package
exports:

```go
// MetricViews returns the metric.Views that bound the cardinality
// of minio.client.operation.duration and pin its histogram buckets.
// Composed by o11y.Init via the ExtraViews seam; client code does
// not call this directly.
func MetricViews(histogramBuckets []float64) []sdkmetric.View
```

`o11y.Init` appends `o11yminio.MetricViews(cfg.histogramBuckets)`
to its `ExtraViews` list alongside the existing redis/mongo entries.
Services that build their own `MeterProvider` outside `o11y.Init` are
expected to register the same views via `sdkmetric.WithView(...)` at
construction; otherwise the allowlist is not in effect and
high-cardinality attributes would export unfiltered.

When `WithAWSS3CompatAttributes` is enabled, the AWS-S3 alias keys
(§4) appear on **spans only**, not on metric labels — the metric
schema is single-sourced on `object_store.*` regardless of compat
mode, so cardinality is constant.

### 4. Span model

- One span per logical operation, kind `SpanKindClient`, tracer name
  `"github.com/flywindy/o11y/minio"`, schema URL pinned to the SDK
  semconv version (v1.39.0, ADR 0006).
- Default span name: `"{Operation} {bucket}"` (e.g. `PutObject media`).
  Operation and bucket are low/bounded cardinality; the **object key is
  never in the span name** (it is an attribute).
- Attributes set before delegation — **default schema:
  package-local `object_store.*` namespace** (vendor-neutral).
  OTel has no stable, vendor-neutral object-store convention; only
  the experimental AWS-S3 page exists, and that page is tightly
  framed as AWS-SDK / `rpc.system=aws-api`, which is not the wire
  framing here (minio-go is not the AWS SDK). The schema below is
  package-local but shaped to mirror current OTel naming patterns
  — see References — so a future migration to a blessed
  convention is a key rename rather than a re-modeling:

  | Attribute | Value | Required | Mirrors |
  |---|---|---|---|
  | `object_store.system.name` | `"s3"` | always | `db.system.name` — the **client-perceived** dialect, parallel to setting `db.system.name="postgresql"` when pgx connects to CockroachDB. The actual backend is told by `server.address`, not by this attribute. |
  | `object_store.operation.name` | `"PutObject"`, `"GetObject"`, … | always | `db.operation.name`, `messaging.operation.name`, `gen_ai.operation.name` (all use the `.operation.name` form) |
  | `object_store.bucket.name` | bucket | always | `messaging.destination.name`, `db.collection.name` (named container the operation targets) |
  | `object_store.object.key` | object key | controlled by `WithObjectKeyAttribute` (default on) | retains the S3 term-of-art "key" rather than overloading "name"; high cardinality, span-only |
  | `object_store.object.size` | bytes (int) | only when a real byte count is known | **Uploads:** `PutObject`'s caller-supplied size is set only when `size >= 0`. minio-go uses `size = -1` to mean "stream of unknown length, read until EOF"; in that case the attribute is **omitted** rather than recorded as `-1`, which would be misleading. `FPutObject` reads the size from the returned `UploadInfo.Size` after the call succeeds (always known). **Downloads:** the wrapper has no observable response — `FGetObject` returns only `error`, and `GetObject` is lazy (§5) — so download size is not populated in v1. A caller that needs download-size visibility issues a `StatObject` first. |
  | `server.address`, `server.port` | endpoint host/port | always | stable HTTP/network semconv — carries the truthful backend identity (e.g. `minio.internal:9000`), so no `cloud.provider` is set |

  Naming follows the OTel naming guide: dots as namespace
  separators, snake_case within a segment (e.g.
  `object_store.bucket.name`, not `objectstore.bucketName`).

  **Multipart attributes are intentionally not part of v1.** The
  upstream `minio-go` UploadInfo result does not surface `UploadID`
  or part count, and the optional HTTP child-span layer uses generic
  `otelhttp` (ADR 0009), which has no S3-URL parser to attach
  `object_store.multipart.*` to per-part spans. The information is
  still visible on the HTTP children's URL/query bytes
  (`?partNumber=N&uploadId=…`) in v1; promoting it to structured
  attributes requires either a thin minio-aware `RoundTripper`
  layered above `otelhttp` or for `minio-go` to expose multipart
  state in its public API. Tracked under Open questions.

- Attributes set before delegation — **opt-in `aws.s3.*` compat
  layer** (`WithAWSS3CompatAttributes`, §2). When enabled, the
  following experimental AWS-S3 keys are **dual-emitted** alongside
  the generic ones (generic stays on; compat does not replace it):

  | AWS-S3 alias (opt-in) | Aliases the generic |
  |---|---|
  | `aws.s3.bucket` | `object_store.bucket.name` |
  | `aws.s3.key` | `object_store.object.key` |

  `aws.s3.upload_id` / `aws.s3.part_number` are intentionally
  omitted from the v1 compat layer for the same reason their generic
  counterparts are: there is no v1 emission point for them.

  We deliberately do **not** adopt `rpc.system=aws-api` or the
  `Service.Operation` span-name rule from the AWS-SDK convention:
  those assert AWS-SDK framing that is false here. The compat
  flag is scoped to attribute-key aliasing only.

  `cloud.provider=aws` is **never** set — the backend is not AWS.
- Attributes set after delegation:
  - `otel.status` — Error on failure, Unset on success.
  - `error.type` — Go type / S3 code on failure (§8).
  - `minio.error.kind` — closed SRE-facing classification (§8).
- **No trace-context propagation to the object store** (§7): the
  storage backend is a leaf dependency that will not continue the
  trace, so the logical span injects no `traceparent`.

### 5. Instrumented method surface (incremental)

The first PR wraps the synchronous data-plane methods that dominate
the upload/download path. Other `*minio.Client` methods remain
available unmodified through the embedded pointer and may be wrapped in
follow-ups as needs appear.

| Method | Span captures | Notes |
|---|---|---|
| `PutObject` | Full upload incl. multipart | Synchronous; size known up front. |
| `FPutObject` | Full file upload | Synchronous. |
| `FGetObject` | Full download to file | Synchronous — span covers the whole transfer. |
| `StatObject` | HEAD round-trip | Synchronous. |
| `RemoveObject` | DELETE round-trip | Synchronous. |
| `CopyObject` | Server-side copy | Synchronous. |
| `ListObjects` | Full stream — span ends when channel closes | Channel-returning; lifecycle and caller contract in §6. |

**`GetObject` is a deliberate exception.** `minio-go`'s `GetObject`
is *lazy*: it returns an `*minio.Object` and performs no network I/O
until the first `Read`/`Stat`/`Seek`. `minio-go` additionally
**stashes the `ctx` passed to `GetObject` on the returned `*Object`**
and reuses it for every subsequent transfer call — not whatever
context is active at Read time. Bracketing the `GetObject` call
would close the span before any bytes move and mismeasure download
latency as ~0. The decision:

- `GetObject` gets a **thin span around the call itself** (it can
  still fail fast, e.g. arg validation), and the `ctx` actually
  passed to `minio-go.GetObject` is **the caller's original `ctx`,
  not the `ctx` carrying our thin span**. Trojan-horsing our span
  context into the stashed context would parent every lazy Read's
  HTTP span to a span that has already been `End()`-ed by the time
  `GetObject` returned — a valid parent reference per the OTel
  data model, but a misleading and impossible-looking trace shape.
- The returned `*Object`'s transfer is **not** wrapped in the
  logical layer in v1.
- Consequence: when `WithHTTPChildSpans` is on, the lazy Read's
  HTTP span is parented to whatever caller span was active **at
  `GetObject` time** (not at Read time — that distinction is not
  ours to make, it is fixed by `minio-go`'s stashed context).
  Read latency is visible on the HTTP span; no `object_store.*`
  attributes appear on it because no logical wrapper span
  participates in the stashed context.
- For *measured* downloads with logical-span coverage, callers use
  `FGetObject` (synchronous). Wrapping the returned `*Object` to
  emit a transfer span — which would also restore the
  parent-at-Read-time story — is tracked under Open questions
  ("`*Object` reader instrumentation").

### 6. Channel-returning APIs (`ListObjects` in v1)

`ListObjects` returns a channel and streams results lazily, so a
single start/end bracket cannot bound the whole operation. v1 records
a span that ends when the wrapper's goroutine observes channel close
(for the common drain-to-completion pattern) and sets
`minio.error.kind` if any streamed element carries an error.

The wrapper inherits — and cannot weaken — minio-go's existing
contract for channel-returning APIs: **callers must either drain the
channel or cancel the request context**. Abandoning the channel
without cancelling is already a goroutine leak in minio-go itself
(its producer blocks on send to the un-drained channel), and our
wrapper goroutine and the open span have the same fate. A GC-driven
teardown is not viable here because the live producer goroutine
keeps the channel reachable. We do not attempt to paper over a
contract the underlying library cannot enforce; godoc on the wrapped
method will reproduce minio-go's drain-or-cancel requirement.

**Scope:** v1 wraps `ListObjects` only. `RemoveObjects` (bulk delete)
has the same channel-in / channel-out shape and the same contract,
but is **not** wrapped in v1 — calls flow through the embedded
`*minio.Client` and produce no logical span. Wrapping it (and any
other channel API minio-go adds) reuses this section's lifecycle
verbatim and is tracked alongside the open question on streaming-span
semantics.

The streaming-span shape is the least certain part of this ADR and
is flagged as an open question.

### 7. Trace-context propagation is disabled toward the store

When `WithHTTPChildSpans` is enabled, the injected `http.NewTransport`
is constructed with an **empty propagator**
(`propagation.NewCompositeTextMapPropagator()`), not the SDK's W3C
propagator. Rationale:

- A MinIO/S3 server does not continue the client's trace, so injecting
  `traceparent` yields no downstream linkage.
- S3 SigV4 signs a specific `SignedHeaders` set. Adding unsigned
  headers is tolerated by S3-compatible servers, but emitting trace
  headers that travel toward a storage backend is needless surface;
  suppressing injection keeps the request bytes minimal and avoids any
  interaction with presign/signature edge cases.

Child-span *parenting* still works: it flows through the Go
`context.Context` passed into the minio method, not through wire
headers.

### 8. `minio.error.kind` taxonomy

`error.type` (Go type name, or the S3 error code from
`minio.ToErrorResponse(err).Code`) is the programmer view.
`minio.error.kind` is a closed, SRE-facing classification set on the
span (not on metrics beyond the bounded set in §3). **First match
wins.**

| # | Value | Trigger | Detection |
|---|---|---|---|
| 1 | `client_canceled` | Caller canceled the context, no deadline | `errors.Is(err, context.Canceled)` and **not** `DeadlineExceeded` |
| 2 | `client_timeout` | Caller deadline expired mid-transfer | `errors.Is(err, context.DeadlineExceeded)` |
| 3 | `not_found` | Object or bucket missing | `ToErrorResponse(err).Code` in `NoSuchKey` / `NoSuchBucket` |
| 4 | `access_denied` | AuthN/AuthZ rejected | code `AccessDenied` / `InvalidAccessKeyId` / `SignatureDoesNotMatch` |
| 5 | `precondition` | ETag / If-Match style failure | code `PreconditionFailed` |
| 6 | `throttled` | Server slow-down / rate limit | code `SlowDown` / HTTP 503 from the store |
| 7 | `transport` | DNS / TCP / TLS / RST before a structured S3 response | `errors.As` to `*net.OpError` / TLS error with no `ErrorResponse.Code` |
| 8 | `server_error` | Structured 5xx with an S3 code | `ErrorResponse.StatusCode >= 500` and a non-empty code |
| 9 | `unknown` | none of the above | default |

The mapping is unit-tested table-driven with fixture errors built from
real underlying types so `errors.Is`/`As` behave as in production.
Logging is left to the caller; the wrapper only sets span/metric
signals.

### 9. Golden trace tests

The implementation PR ships golden-trace tests (the ADR 0011 §9
pattern: in-memory `tracetest.SpanRecorder`, committed expected JSON
with ids/timestamps blanked, `UPDATE_GOLDEN=1` regenerator). Backend
under test is a containerized MinIO in CI, with a lightweight
`httptest`-based S3 double for the error-classification rows that do
not need a real server.

| # | Scenario | Expected span tree |
|---|---|---|
| 1 | Small `PutObject` success | 1 logical span, `otel.status=Unset`, `object_store.object.size` set, no `error.type` / `minio.error.kind` |
| 2 | Large `PutObject` (multipart) with `WithHTTPChildSpans` | 1 logical span → child HTTP spans: Initiate, UploadPart×N, Complete |
| 3 | `FGetObject` success | 1 logical span, `otel.status=Unset`, no `object_store.object.size` (FGetObject returns only `error`; §4) |
| 4 | `StatObject` on missing key | 1 logical span, Error, `error.type=NoSuchKey`, `minio.error.kind=not_found` |
| 5 | `PutObject` with bad credentials | 1 logical span, Error, `minio.error.kind=access_denied` |
| 6 | `FPutObject` with caller timeout | 1 logical span, Error, `minio.error.kind=client_timeout` |
| 7 | `RemoveObject` success | 1 logical span, no error |
| 8 | Connection refused (server down) | 1 logical span, Error, `minio.error.kind=transport` |

### 10. Compliance with ADR 0003

`minio-go/v7` does not import OpenTelemetry or mutate OTel globals.
The wrapper takes `tp`/`mp`/`prop` explicitly and never reads or
writes any `go.opentelemetry.io/otel` global. The optional HTTP layer
reuses `http.NewTransport` (already approved, ADR 0009).

`minio-go/v7` is not yet a dependency of this repo (`go.mod` has no
`minio-go` line at the time of this ADR). Per ADR 0003 — "When a new
library is added or an existing one bumped, update this table in the
same PR as the version change" — the registry row lands in the
**implementing PR** that adds `minio-go/v7` to `go.mod`. The row
content is pre-specified here so reviewers can verify the
ADR 0003 §"For every new instrumentation integration" checklist
ahead of that PR:

| Library | Version | Verified | Behavior | Notes |
|---|---|---|---|---|
| `github.com/minio/minio-go/v7` | (pinned by implementing PR) | ✅ | Pure S3 client; does not import OpenTelemetry or mutate provider globals. The SDK-owned `minio` wrapper wires providers explicitly and only uses the public `Options.Transport` seam. | See ADR 0018 |

---

## Example

End-to-end illustration of how §2 (API), §3 (metrics), and §4 (span
schema) compose. Attribute values are realistic; trace ids and exact
durations are elided.

### Construction

```go
import (
    "context"

    "github.com/flywindy/o11y"
    o11yminio "github.com/flywindy/o11y/minio"
    miniogo "github.com/minio/minio-go/v7"
    "github.com/minio/minio-go/v7/pkg/credentials"
)

sdk, err := o11y.Init(ctx,
    o11y.WithServiceName("uploader"),
    o11y.WithServiceVersion("1.4.2"),
    o11y.WithServiceNamespace("media"),
    o11y.WithEnvironment("production"),
)
if err != nil { /* ... */ }
defer sdk.Shutdown(context.Background())

client, err := o11yminio.New(
    "minio.internal:9000",
    &miniogo.Options{
        Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
        Secure: false,
    },
    sdk.TracerProvider(),
    sdk.MeterProvider(),
    sdk.Propagator,
    o11yminio.WithHTTPChildSpans(true),
)
```

### Successful multipart upload (default schema)

```go
_, err := client.PutObject(ctx, "media",
    "videos/2026/clip.mp4",
    file, 40*miniogo.MiB,
    miniogo.PutObjectOptions{ContentType: "video/mp4"})
```

Resulting trace (the parent HTTP-server span from gin/echo is omitted
for brevity; the logical span is its direct child via `ctx`):

```text
PutObject media                                                client
│  object_store.system.name      = "s3"
│  object_store.operation.name   = "PutObject"
│  object_store.bucket.name      = "media"
│  object_store.object.key       = "videos/2026/clip.mp4"
│  object_store.object.size      = 41943040
│  server.address                = "minio.internal"
│  server.port                   = 9000
│  otel.status                   = Unset
├── HTTP POST minio.internal/media/...?uploads                  client
│      http.request.method           = "POST"
│      http.response.status_code     = 200
│      (InitiateMultipartUpload)
├── HTTP PUT  minio.internal/media/...?partNumber=1&uploadId=…  client
│      http.request.method           = "PUT"
│      http.response.status_code     = 200
├── HTTP PUT  ...?partNumber=2&uploadId=…                       client
└── HTTP POST minio.internal/media/...?uploadId=…               client
       http.request.method = "POST"      (CompleteMultipartUpload)
```

And one metric observation:

```text
minio.client.operation.duration   = 14.2s         histogram
   object_store.operation.name = "PutObject"
   object_store.bucket.name    = "media"
   server.address              = "minio.internal"
   server.port                 = 9000
   # error.type and otel.status_code both absent on success
```

The 14 s of off-CPU storage time from the motivating incident is
now attributed to a named span and decomposed by HTTP child into
"InitiateMultipart / per-part PUTs / Complete" — exactly the
breakdown the CPU profile alone could not provide.

### Same call with `WithAWSS3CompatAttributes(true)`

The compat flag dual-emits the experimental `aws.s3.*` keys
alongside the generic ones; the generic schema is unchanged.

```text
PutObject media                                                client
│  object_store.system.name      = "s3"
│  object_store.operation.name   = "PutObject"
│  object_store.bucket.name      = "media"
│  aws.s3.bucket                 = "media"                  ← alias
│  object_store.object.key       = "videos/2026/clip.mp4"
│  aws.s3.key                    = "videos/2026/clip.mp4"   ← alias
│  object_store.object.size      = 41943040
│  ...
├── HTTP PUT ...?partNumber=1&uploadId=…                       client
│      (HTTP client attributes only; multipart structured
│       attributes are out of scope for v1 — see §4)
...
```

Metric labels are **unchanged** — `aws.s3.*` aliases appear on spans
only (§3), so cardinality is identical between the two modes.

### Failure: missing key on `StatObject`

```go
_, err := client.StatObject(ctx, "media",
    "videos/missing.mp4",
    miniogo.StatObjectOptions{})
// err: *miniogo.ErrorResponse{Code: "NoSuchKey", ...}
```

```text
StatObject media                                               client  ERROR
   object_store.system.name      = "s3"
   object_store.operation.name   = "StatObject"
   object_store.bucket.name      = "media"
   object_store.object.key       = "videos/missing.mp4"
   server.address                = "minio.internal"
   server.port                   = 9000
   error.type                    = "NoSuchKey"
   minio.error.kind              = "not_found"
   otel.status                   = Error
```

Histogram observation:

```text
minio.client.operation.duration   = 0.023s        histogram
   object_store.operation.name = "StatObject"
   object_store.bucket.name    = "media"
   server.address              = "minio.internal"
   server.port                 = 9000
   error.type                  = "NoSuchKey"
   # otel.status_code is intentionally not set — error.type's
   # presence already marks the failure (§3, semconv v1.39.0)
```

The split between `error.type` (programmer view: the S3 wire code)
and `minio.error.kind` (SRE view: the closed taxonomy in §8) is the
intent described in §4 / §8 — both keys coexist.

---

## Consequences

**Positive**

- The motivating incident is directly addressed: a file upload now
  produces a `PutObject` span with wall-clock duration, sitting beside
  the `redis` span on the same trace. With `WithHTTPChildSpans`, the
  ~14 s off-CPU gap resolves into per-part HTTP child spans.
- Per-operation object-storage attributes (bucket, key, size, error
  kind) that the HTTP layer cannot synthesize.
- Services do not name storage spans themselves; naming and attributes
  are consistent across all consumers (the user's explicit goal).
- Metric cardinality bounded by default; object key never enters
  metrics.
- The hybrid is clean here (outer logical span owns the lifecycle),
  unlike the resty case it is contrasted against.

**Negative / Trade-offs**

- T3 means the SDK owns this code and must track `minio-go` API
  changes; the §2 evaluation is re-run on each `minio-go` major bump
  and at least annually, adopting a maintained `otelminio` (T2) if one
  emerges.
- `GetObject`'s laziness (§5) makes streaming-download latency a
  second-class signal in v1; `FGetObject` or `WithHTTPChildSpans` is
  the measured path.
- Channel-returning APIs (§6) have a less certain span shape.
- The method surface is wrapped incrementally, so an un-wrapped method
  silently produces no logical span (the embedded client still works).

---

## Open questions

- **Semconv evolution.** §4 commits to a package-local
  `object_store.*` schema modeled on current OTel naming
  (`db.system.name` / `*.operation.name` / `*.bucket.name` patterns).
  If OTel later mints a vendor-neutral object-store convention, the
  migration is expected to be a key rename rather than a re-modeling;
  the `WithAWSS3CompatAttributes` flag is reusable as the
  dual-emit mechanism during that transition (per the
  `OTEL_SEMCONV_STABILITY_OPT_IN` precedent used by the database
  migration).
- **Configurable system identity.** `object_store.system.name`
  is hardcoded to `"s3"` in v1 (the dialect minio-go speaks).
  An option to override it (e.g. for instrumentation embedded in a
  multi-protocol wrapper) is deferred until a concrete need
  appears.
- **Multipart attribute enrichment.** v1 emits no
  `object_store.multipart.*` / `aws.s3.upload_id` /
  `aws.s3.part_number` (§4 default schema). Two paths to add them in
  v2: (a) a thin minio-aware `RoundTripper` layered above `otelhttp`
  that parses the S3 query string and annotates the live HTTP child
  span before `otelhttp`'s span ends; (b) an upstream change in
  `minio-go` to surface `UploadID` and part count on `UploadInfo`,
  enabling enrichment on the outer logical span. Path (a) is the
  more likely route.
- **`*Object` reader instrumentation.** Whether to wrap the
  `GetObject`-returned reader to emit a transfer span, making streaming
  downloads first-class (§5).
- **Streaming-span semantics** for `ListObjects` / `RemoveObjects`
  (§6): end-on-close vs. first-page vs. per-page events.
- **Idempotency / double construction.** `New` always builds a fresh
  client, so there is no resty-style `Wrap` idempotency concern; if a
  future `Wrap` is added, it inherits ADR 0011 §6's open question.

---

## References

OTel semantic conventions consulted when shaping §4. All linked pages
are at status **Development** (experimental) as of this ADR:

- [Naming | OpenTelemetry](https://opentelemetry.io/docs/specs/semconv/general/naming/)
  — namespace separator + snake_case-within-segment rule (justifies
  `object_store.bucket.name` over `objectstore.bucketName`).
- [Database semantic convention stability migration guide | OpenTelemetry](https://opentelemetry.io/docs/specs/semconv/non-normative/db-migration/)
  — `db.system` → `db.system.name` rename and the
  `OTEL_SEMCONV_STABILITY_OPT_IN` dual-emit precedent.
- [Semantic conventions for database client spans | OpenTelemetry](https://opentelemetry.io/docs/specs/semconv/db/database-spans/)
  — `db.system.name` as the **client-perceived** dialect identifier
  (PostgreSQL client → CockroachDB note); the model for setting
  `object_store.system.name="s3"` regardless of the actual backend.
- [Semantic conventions for AWS S3 client spans | OpenTelemetry](https://opentelemetry.io/docs/specs/semconv/object-stores/s3/)
  — the only existing object-store convention; source of the
  `aws.s3.*` keys exposed via `WithAWSS3CompatAttributes`. AWS-SDK
  framing (`rpc.system=aws-api`, `Service.Operation` span name)
  deliberately not adopted (§4).
- [Semantic conventions for AWS SDK client spans | OpenTelemetry](https://opentelemetry.io/docs/specs/semconv/cloud-providers/aws-sdk/)
  — the framing the S3 page sits inside; cited as the explicit
  reason we adopt attribute keys but not the RPC framing.
- [Semantic conventions for GenAI agent and framework spans | OpenTelemetry](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-agent-spans/)
  — the most recent `.operation.name` precedent, alongside
  `messaging.operation.name`.
- [Semantic conventions for object stores | OpenTelemetry](https://opentelemetry.io/docs/specs/semconv/object-stores/)
  — top-level object-store group; confirms no vendor-neutral
  namespace exists yet.
