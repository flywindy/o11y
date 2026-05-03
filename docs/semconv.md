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
| `http.route` | string | Normalized route template (e.g. `/users/:id`), never the raw URL path. Cardinality-capped via `WithPathNormalizer` + `WithMaxUniquePaths` (default 1000); overflow collapses to `"other"`. |
| `http.response.status_code` | int | Must be `attribute.Int`, not `attribute.String`. |

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

**Status**: integration pending; `otel-mongo/v2` v0.2.11 is now an adoption
candidate after the SDK semconv pin moved to v1.39.0. See ADR 0005.

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
The o11y wrapper must default this off unless an application explicitly opts in.

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

Any new deviation must list:

1. The non-standard key and type.
2. The reason no standard alternative works.
3. The mitigation plan or upstream spec issue link.
