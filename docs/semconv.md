# OpenTelemetry Semantic Conventions - v1.39.0 Catalog

This document is the single source of truth for which OTel attributes and
instruments the o11y SDK emits. Every PR that introduces a new attribute
key or instrument name MUST update this catalog in the same commit.

---

## Version Pin

- **Active version**: `v1.39.0`
- **Go package**: `go.opentelemetry.io/otel/semconv/v1.39.0`
- **Upstream spec**: <https://opentelemetry.io/docs/specs/semconv/>

### Upgrade Rule

Mixing semconv versions in SDK-owned code is forbidden. A version bump is
a single atomic change:

1. Replace every SDK-owned `semconv/v1.X.Y` import with the new version.
2. Re-map any renamed keys.
3. Update this document with the new version and any key deltas.
4. Run `go vet ./...` and `go test ./...` after the change.

Third-party instrumentation may import the same pinned semconv version, or
may emit documented string keys that match the pinned version after source
inspection. Any exception must be recorded in the integration ADR.

### Enforcement Rules

1. **No string literals for SDK-owned semconv keys.** Always reference the
   constant from the pinned semconv package where the package exposes one.
2. **New attribute? Update this catalog.** The PR reviewer checks that this
   file lists any new key introduced in the diff.
3. **Deviations require explicit justification** in the "Deviations"
   section below. The default answer is "no deviation".

---

## Resource Attributes

Emitted once per process via `o11y.Init` / `buildResource` and attached to
every signal so that service identity is identical across backends.

| Key | Type | Source | Required |
|---|---|---|---|
| `service.name` | string | `WithServiceName` | yes |
| `service.version` | string | `WithServiceVersion` | yes |
| `service.namespace` | string | `WithServiceNamespace` | yes |
| `deployment.environment.name` | string | `WithEnvironment` (canonicalized: `production` / `staging` / `development` / `testing`) | yes |
| `host.*` | various | `resource.WithHost()` | detected |
| `process.*` | various | `resource.WithProcess()` | detected |
| (env-provided) | various | `resource.WithFromEnv()` / `OTEL_RESOURCE_ATTRIBUTES` | optional |

---

## HTTP Server (package `github.com/flywindy/o11y/http`)

### Instruments

| Name | Kind | Unit | Description |
|---|---|---|---|
| `http.server.request.duration` | Float64Histogram | `s` | Duration of HTTP server requests. `_count` doubles as traffic + error counter; no separate counter emitted. |

Histogram boundaries pinned via an OTel View to
`[0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]` seconds.

### Attributes

| Key | Type | Notes |
|---|---|---|
| `http.request.method` | string | e.g. `GET`, `POST`. |
| `http.route` | string | Normalized route template (e.g. `/users/:id`), never the raw URL path. Export cardinality is capped via `WithMaxUniqueRoutes` (default 1000); ordinary overflow collapses to `"other"`, while SDK aggregation overflow uses `otel.metric.overflow=true`. |
| `http.response.status_code` | int | Must be `attribute.Int`, not `attribute.String`. |
| `otel.metric.overflow` | bool | Emitted by the OTel SDK when the aggregation cardinality limit is reached. This is an SDK safety valve, not a semconv HTTP label. |

---

## HTTP Client (packages `github.com/flywindy/o11y/http` and `github.com/flywindy/o11y/resty`)

### Instruments

| Name | Kind | Unit | Description |
|---|---|---|---|
| `http.client.request.duration` | Float64Histogram | `s` | Duration of outbound HTTP client requests. Emitted by the `http` otelhttp facade and by the SDK-owned `resty` wrapper. |

Histogram boundaries use the SDK's configured latency buckets.

### Attributes

| Key | Type | Notes |
|---|---|---|
| `http.request.method` | string | e.g. `GET`, `POST`. |
| `http.route` | string | Resty only, opt-in through `resty.WithRouteFromContext` and `resty.WithMetricRouteEnabled(true)`. Must be a caller-supplied route template, never the raw URL path. Export cardinality is capped through `WithMaxUniqueRoutes`. |
| `http.response.status_code` | int | Response-path metric label and span attribute. |
| `http.request.resend_count` | int | Resty spans only; emitted on retry attempts after the first attempt. |
| `url.full` | string | Resty spans only; full outbound URL. Do not put secrets in URLs. |
| `error.type` | string | Error-path metric label and span attribute. |
| `resty.error.kind` | string | Resty spans only; SDK-owned closed enum: `client_canceled`, `client_timeout`, `server_timeout`, `tls`, `transport`, `protocol`, `unknown`. |
| `resty.retry.exhausted` | bool | Resty spans only; set on the last failed attempt when resty's retry budget is exhausted. |
| `server.address` | string | Upstream host. |
| `server.port` | int | Upstream port. |

### Explicitly NOT Emitted by Default

| Key | Reason |
|---|---|
| Raw URL path as `http.route` | High cardinality. Resty route metrics require explicit caller-provided templates. |

---

## Go Runtime (package `go.opentelemetry.io/contrib/instrumentation/runtime`)

Enabled by default via `WithRuntimeMetrics(true)`. The emitted metric set is
defined by the contrib package and covers the Saturation golden signal
(goroutines, GC pauses, heap allocations, scheduler latency).

This catalog does not duplicate the upstream metric list because the contrib
package is the authoritative source and changes across contrib versions.

---

## Messaging - NATS (package `github.com/flywindy/o11y/nats`)

Spans are emitted by
`github.com/Marz32onE/instrumentation-go/otel-nats` v0.2.11. The upstream
package imports `go.opentelemetry.io/otel/semconv/v1.39.0`; the o11y wrapper
adds no attributes of its own.

### Core and JetStream Attributes

| Key | Type | Notes |
|---|---|---|
| `messaging.system` | string | Constant `"nats"`. |
| `messaging.destination.name` | string | NATS subject (e.g. `events.created`). |
| `messaging.operation.type` | string | `send`, `receive`, or `process`. |
| `messaging.operation.name` | string | `publish`, `receive`, or `process`. |
| `messaging.message.body.size` | int | Emitted when payload is non-empty. |
| `messaging.message.conversation_id` | string | Request/reply inbox, when present. |
| `messaging.consumer.group.name` | string | Queue group, when present. |
| `server.address` | string | NATS endpoint host. |
| `server.port` | int | NATS endpoint port, omitted for default port 4222. |

### Upstream NATS Trace-Event Attributes

`otel-nats` also emits NATS-server infrastructure trace-event spans when the
optional `Nats-Trace-Dest` flow is enabled. These use intentionally
NATS-specific keys such as `nats.server.name`, `nats.event.type`, and
`nats.subject`; they are listed as a documented deviation because OTel
semconv has no direct stable equivalent for the NATS server trace-event
payload.

### Known Cardinality Risks

- `messaging.destination.name` per raw subject can explode if applications
  publish to unbounded subject spaces (e.g. `events.user.<userID>`). Use
  bounded subject templates or hash the dynamic portion before publishing.

---

## Database - MongoDB

Spans are emitted by
`github.com/Marz32onE/instrumentation-go/otel-mongo/v2`, wrapped by
`github.com/flywindy/o11y/mongo`. The upstream module is pinned to the
`otel-mongo/v2/v0.2.11` tag commit through a Go pseudo-version. Operation
duration metrics are emitted by the official contrib `otelmongo`
CommandMonitor in metrics-only mode. See ADR 0005 and ADR 0014.

### Instruments

| Name | Kind | Unit | Attributes |
|---|---|---|---|
| `db.client.operation.duration` | Float64Histogram | `s` | `db.system.name`, `db.operation.name`, `network.peer.address`, `network.peer.port`, `error.type` |

### Expected Attributes

| Key | Type | Notes |
|---|---|---|
| `db.system.name` | string | Constant `"mongodb"`. |
| `db.namespace` | string | Database name. |
| `db.collection.name` | string | Collection name. |
| `db.operation.name` | string | Operation name (`insert`, `find`, `update`, ...). |
| `db.operation.batch.size` | int | Batch size for multi-document operations, when applicable. |
| `db.response.status_code` | string | MongoDB error code, when available. |
| `error.type` | string | MongoDB error code or `_OTHER`, when an operation fails. |
| `server.address` | string | MongoDB host. |
| `server.port` | int | MongoDB port, omitted for default port 27017 by upstream. |

### Explicitly NOT Emitted by Default

| Key | Reason |
|---|---|
| `db.query.text` | Query documents routinely contain PII and secrets. Future opt-in must carry an explicit warning. |
| Response document contents | Same rationale. |

### Document Trace Propagation

`otel-mongo/v2` supports `_oteltrace` document injection through
`WithTracePropagationEnabled(bool)` and `OTEL_MONGO_PROPAGATION_ENABLED`.
The o11y wrapper defaults this off unless an application explicitly opts in
through `mongo.WithDocumentTracePropagation(true)`.

---

## Database - Redis / Valkey

Spans and metrics are emitted by the SDK-owned
`github.com/flywindy/o11y/redis` wrapper for
`github.com/redis/go-redis/v9`. The wrapper does not call
`redisotel.InstrumentTracing` or `redisotel.InstrumentMetrics`; see ADR 0013.

### Span Attributes

| Key | Type | Notes |
|---|---|---|
| `db.system.name` | string | Constant `"redis"` for Redis and Valkey backends. |
| `db.operation.name` | string | Uppercase command name such as `GET`, or `pipeline` for batches. |
| `db.namespace` | string | Selected Redis DB number, when known. |
| `db.operation.batch.size` | int | Pipeline user-command count when the effective count is at least 2. |
| `db.query.text` | string | Only when `redis.WithCommandTextEnabled(true)` is used; truncated to 1 KiB. |
| `error.type` | string | Emitted for errors except `redis.Nil`, which is treated as a cache miss success. |
| `redis.error.kind` | string | SDK-owned closed enum: `pool_timeout`, `client_timeout`, `client_canceled`. |
| `server.address` | string | Redis endpoint host, or go-redis sentinel placeholder when that is all the client exposes. |
| `server.port` | int | Redis endpoint port when the address parses as `host:port`. |

### Instruments

| Name | Kind | Unit | Attributes |
|---|---|---|---|
| `db.client.operation.duration` | Float64Histogram | `s` | `db.system.name`, `db.operation.name`, `server.address`, `server.port`, `error.type` |
| `db.client.connection.count` | Int64ObservableUpDownCounter | `{connection}` | `db.system.name`, `db.client.connection.pool.name`, `db.client.connection.state`, `server.address`, `server.port` |
| `db.client.connection.idle.max` | Int64ObservableUpDownCounter | `{connection}` | `db.system.name`, `db.client.connection.pool.name`, `server.address`, `server.port` |
| `db.client.connection.idle.min` | Int64ObservableUpDownCounter | `{connection}` | `db.system.name`, `db.client.connection.pool.name`, `server.address`, `server.port` |
| `db.client.connection.max` | Int64ObservableUpDownCounter | `{connection}` | `db.system.name`, `db.client.connection.pool.name`, `server.address`, `server.port` |
| `db.client.connection.timeouts` | Int64ObservableCounter | `{timeout}` | `db.system.name`, `db.client.connection.pool.name`, `server.address`, `server.port` |
| `db.client.connection.create_time` | Float64Histogram | `s` | `db.system.name`, `db.client.connection.pool.name`, `server.address`, `server.port` |

### Explicitly NOT Emitted by Default

| Key | Reason |
|---|---|
| `db.connection_string` | May contain credentials in Redis URLs. |
| `db.system` | Legacy semconv key replaced by `db.system.name`. |
| `db.statement` | Legacy semconv key replaced by opt-in `db.query.text`. |
| `code.function`, `code.filepath`, `code.lineno` | Would describe instrumentation internals rather than application call sites. |

---

## User Identity Helpers (package `github.com/flywindy/o11y`)

ADR 0016 exposes explicit helpers and opt-in baggage materialization for the
acting user's login name. Explicit helpers are call-site controlled.
`ContextWithUser` propagates over W3C Baggage; `WithUserBaggage` materializes
the whitelisted baggage value onto this service's spans and SDK log records.

### Attributes

| Key | Type | Source | Notes |
|---|---|---|---|
| `user.name` | string | `SetUser(ctx, name)` span attribute; `UserName(name)` slog attribute; `ContextWithUser(ctx, name)` + `WithUserBaggage()` baggage materialization | Login/username of the acting user. Personal data / PII; allowed on traces and logs only, never as a metric label. |

---

## Object Storage - MinIO / S3 (package `github.com/flywindy/o11y/minio`)

Spans and metrics are emitted by the SDK-owned `minio` wrapper for
`github.com/minio/minio-go/v7`. The wrapper does not import any OTel
contrib instrumentation; per-operation spans and the duration histogram
come directly from this package. See ADR 0018 for the design and the
ADR 0008 §2 evaluation that justifies the T3 sourcing.

OTel object-store conventions are at status **Development** (only the
AWS S3 page exists, under the AWS-SDK framing), so the schema below is
package-local under the `object_store.*` namespace and shaped to mirror
current OTel naming (`db.system.name` client-perceived-dialect model,
`*.operation.name` form, snake_case-within-segment). When OTel mints a
vendor-neutral object-store convention, the migration is expected to be
a key rename, and the existing `WithAWSS3CompatAttributes` option
provides the dual-emit mechanism.

### Span Attributes

| Key | Type | Notes |
|---|---|---|
| `object_store.system.name` | string | Constant `"s3"` — the client-perceived dialect (parallel to `db.system.name="postgresql"` when pgx connects to CockroachDB). The actual backend identity comes from `server.address`, not from this key. |
| `object_store.operation.name` | string | Logical operation: `PutObject`, `FPutObject`, `FGetObject`, `GetObject`, `StatObject`, `RemoveObject`, `CopyObject`, `ListObjects`. |
| `object_store.bucket.name` | string | Bucket the operation targets. |
| `object_store.object.key` | string | Object key. Controlled by `minio.WithObjectKeyAttribute` (default on). Span attribute only — never a metric label. |
| `object_store.object.size` | int | Bytes. Set only when a real byte count is known: PutObject from caller-supplied size `>= 0`, or from `UploadInfo.Size` after a successful unknown-length upload (`-1` is never recorded as bytes); FPutObject from `UploadInfo.Size`; StatObject from `ObjectInfo.Size`. Downloads do not populate this in v1. |
| `error.type` | string | Go type name (e.g. `context.DeadlineExceeded`, `*net.OpError`) or the S3 wire code from `minio.ToErrorResponse(err).Code` (e.g. `NoSuchKey`). |
| `minio.error.kind` | string | SDK-owned closed enum: `client_canceled`, `client_timeout`, `not_found`, `access_denied`, `precondition`, `throttled`, `transport`, `server_error`, `unknown`. Span-only — never a metric label. |
| `aws.s3.bucket` | string | Opt-in via `minio.WithAWSS3CompatAttributes(true)`; dual-emitted alongside `object_store.bucket.name`. Sourced from `semconv.AWSS3BucketKey`. |
| `aws.s3.key` | string | Opt-in alias of `object_store.object.key`. Sourced from `semconv.AWSS3KeyKey`. |
| `server.address` | string | MinIO/S3 endpoint host from the caller-supplied endpoint. |
| `server.port` | int | Endpoint port; defaulted from `Options.Secure` (443/80) when the endpoint omits a port. |

### Instruments

| Name | Kind | Unit | Attributes |
|---|---|---|---|
| `minio.client.operation.duration` | Float64Histogram | `s` | `object_store.operation.name`, `object_store.bucket.name`, `server.address`, `server.port`, `error.type` |

Cardinality is bounded by the `MetricViews()` allowlist installed via
`o11y.Init`'s `ExtraViews` (mirrors the redis/mongo pattern). Services
that build their own MeterProvider must register the same views via
`sdkmetric.WithView(...)` at construction.

### Explicitly NOT Emitted

| Key | Reason |
|---|---|
| `cloud.provider` | The backend is not AWS; setting `aws` would be a lie. The endpoint host carries truthful backend identity. |
| `rpc.system=aws-api` / `Service.Operation` span name | The AWS-SDK RPC framing from the experimental AWS-S3 semconv is rejected — minio-go is not the AWS SDK. The compat option (§4) is scoped to attribute-key aliasing only. |
| `otel.status_code` metric label | Semconv defines values `OK`/`ERROR` only, with the attribute absent when status is `UNSET`; success/failure is encoded by the presence of `error.type` instead. |
| `object_store.multipart.upload_id` / `object_store.multipart.part_number` | Not emitted in v1: `minio-go`'s `UploadInfo` does not surface them and the optional HTTP child-span layer uses generic `otelhttp` with no S3-URL parser. Tracked under ADR 0018 Open Questions. |

---

## Logs

All log records pass through the `otelslog` bridge, which applies OTel Log
Data Model attributes automatically.

### Per-Record Attributes Injected by the SDK

| Key | Source | Notes |
|---|---|---|
| `traceId` (stdout JSON) / `trace_id` (OTLP) | Active span from ctx | Via `OtelSlogHandler` on stdout path; via `otelslog` bridge on OTLP path. See ADR 0001. |
| `spanId` (stdout JSON) / `span_id` (OTLP) | Active span from ctx | Same mechanism as above. |
| `service.name` | Stdout JSON top-level field; OTLP Resource attribute | ADR 0001 Option B. |
| `environment` | Stdout JSON top-level field; OTLP Resource attribute `deployment.environment.name` | Legacy stdout name retained for backward compatibility. |

---

## Deviations / Exceptions

| Key family | Source | Reason |
|---|---|---|
| `nats.*` | `otel-nats` trace-event spans | NATS server trace-event payload fields have no direct stable OTel semconv equivalent. They are isolated to the optional infrastructure trace-event flow. |
| MongoDB operation metric `network.peer.*` labels | contrib `otelmongo` CommandMonitor | The maintained contrib instrumentation emits `network.peer.address` / `network.peer.port` for `db.client.operation.duration`; the SDK keeps those labels and filters out `network.transport` rather than forking the T2 dependency. See ADR 0014. |
| `redis.error.kind` | `redis` wrapper | Redis-specific bounded error class that distinguishes pool exhaustion from caller cancellation/deadline without using it as a metric label. |
| `resty.error.kind`, `resty.retry.exhausted` | `resty` wrapper | Resty-specific bounded failure class and retry-budget marker. Standard `error.type` remains present; these keys preserve operator-facing retry and transport semantics without adding metric cardinality. |
| `object_store.*` namespace (`system.name`, `operation.name`, `bucket.name`, `object.key`, `object.size`) | `minio` wrapper | OTel object-store semconv is at status Development and only the AWS-S3 page exists, framed as AWS-SDK / `rpc.system=aws-api`. The SDK-owned `object_store.*` namespace is package-local but shaped to mirror current OTel naming patterns; future migration to a blessed convention is expected to be a key rename. See ADR 0018 §4 and References. |
| `minio.error.kind`, `minio.client.operation.duration` | `minio` wrapper | MinIO-specific bounded SRE classification (span-only) and per-operation duration histogram. No stable OTel object-store metric exists, so the instrument name stays package-local; standard `error.type` is co-emitted on spans and as the metric failure label. |

Any new deviation must list:

1. The non-standard key and type.
2. The reason no standard alternative works.
3. The mitigation plan or upstream spec issue link.
