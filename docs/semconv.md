# OpenTelemetry Semantic Conventions — v1.27.0 Catalog

This document is the single source of truth for which OTel attributes and
instruments the o11y SDK emits. Every PR that introduces a new attribute
key or instrument name MUST update this catalog in the same commit.

---

## Version pin

- **Active version**: `v1.27.0`
- **Go package**: `go.opentelemetry.io/otel/semconv/v1.27.0`
- **Upstream spec**: <https://opentelemetry.io/docs/specs/semconv/>

### Upgrade rule

Mixing semconv versions is forbidden. A version bump is a single atomic
change:

1. Replace every `semconv/v1.27.0` import with the new version.
2. Re-map any renamed keys (e.g. `deployment.environment` in v1.26.0
   became `deployment.environment.name` in v1.27.0).
3. Update this document with the new version and any key deltas.
4. Run `go vet ./...` and `go test ./...` after the change.

### Enforcement rules

1. **No string literals for attribute keys.** Always reference the
   constant from the pinned semconv package (e.g.
   `semconv.ServiceNameKey.String(name)`). String literals bypass
   compile-time version checking.
2. **New attribute? Update this catalog.** The PR reviewer checks that
   this file lists any new key introduced in the diff.
3. **Deviations require explicit justification** in the "Deviations"
   section below. The default answer is "no deviation".

---

## Resource attributes

Emitted once per process via `o11y.Init` → `buildResource` and attached
to every signal (traces, metrics, logs) so that service identity is
byte-for-byte identical across backends.

| Key | Type | Source | Required |
|---|---|---|---|
| `service.name` | string | `WithServiceName` | ✅ |
| `service.version` | string | `WithServiceVersion` | ✅ |
| `service.namespace` | string | `WithServiceNamespace` | ✅ |
| `deployment.environment.name` | string | `WithEnvironment` (canonicalized: `production` / `staging` / `development` / `testing`) | ✅ |
| `host.*` | various | `resource.WithHost()` | detected |
| `process.*` | various | `resource.WithProcess()` | detected |
| (env-provided) | various | `resource.WithFromEnv()` → `OTEL_RESOURCE_ATTRIBUTES` | optional |

---

## HTTP server (package `github.com/flywindy/o11y/http`)

### Instruments

| Name | Kind | Unit | Description |
|---|---|---|---|
| `http.server.request.duration` | Float64Histogram | `s` | Duration of HTTP server requests. `_count` doubles as traffic + error counter; no separate counter emitted. |

Histogram boundaries pinned via an OTel View to
`[0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]` seconds
(see ADR 0002 §9).

### Attributes

| Key | Type | Notes |
|---|---|---|
| `http.request.method` | string | e.g. `GET`, `POST`. |
| `http.route` | string | Normalized route template (e.g. `/users/:id`), **never** the raw URL path. Cardinality-capped via `WithPathNormalizer` + `WithMaxUniquePaths` (default 1000) → overflow collapses to literal `"other"`. |
| `http.response.status_code` | **int** | Must be `attribute.Int`, not `attribute.String`, per semconv v1.27.0. |

---

## Go runtime (package `go.opentelemetry.io/contrib/instrumentation/runtime`)

Enabled by default via `WithRuntimeMetrics(true)`. The emitted metric
set is defined by the contrib package and covers the Saturation golden
signal (goroutines, GC pauses, heap allocations, scheduler latency).

This catalog **does not duplicate** the upstream metric list because
the contrib package is the authoritative source and changes across
contrib versions. Refer to:

- Go package docs: <https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/runtime>
- Spec: <https://opentelemetry.io/docs/specs/semconv/runtime/go-metrics/>

When the contrib package version is bumped in `go.mod`, the reviewer
confirms that no metric name collides with one defined by the SDK
itself and that cardinality stays bounded.

---

## Messaging — NATS (package `github.com/flywindy/o11y/nats`)

Spans are emitted by the upstream
`github.com/Marz32onE/instrumentation-go/otel-nats` library. The
wrapper adds no attributes of its own.

### Attributes (produced by upstream, pinned to semconv v1.27.0)

| Key | Type | Notes |
|---|---|---|
| `messaging.system` | string | Constant `"nats"` |
| `messaging.destination.name` | string | NATS subject (e.g. `events.created`) |
| `messaging.operation` | string | `publish` / `receive` / `process` |
| `messaging.message.id` | string | Per-message identifier, when available |
| `server.address` | string | NATS endpoint host |
| `server.port` | int | NATS endpoint port |

JetStream-specific attributes (stream name, consumer name, delivery
count, ack policy) follow the same upstream semconv mapping. The
full list is reviewed whenever the upstream library is bumped; see
ADR 0004's "Audit discipline for upstream bumps".

### Known cardinality risks

- `messaging.destination.name` per raw subject can explode if
  applications publish to unbounded subject spaces (e.g.
  `events.user.<userID>`). Use subject templates or hash the dynamic
  portion before publishing.

---

## Database — MongoDB (package `github.com/flywindy/o11y/mongo`, per ADR 0005)

**Status**: catalog entry reserved; implementation pending per ADR 0005.

### Spans

Span kind: `trace.SpanKindClient`.
Span name: `{db.operation.name} {db.namespace}.{db.collection.name}` —
e.g. `find orders.events`. Falls back to `{db.operation.name}` when
the collection cannot be determined.

### Attributes

| Key | Type | Notes |
|---|---|---|
| `db.system` | string | Constant `"mongodb"` |
| `db.namespace` | string | Database name |
| `db.collection.name` | string | Collection name (best-effort; see ADR 0005 §7) |
| `db.operation.name` | string | Command name (`insert`, `find`, `update`, …) |
| `server.address` | string | MongoDB host |
| `server.port` | int | MongoDB port |
| `network.peer.address` | string | Mirror of `server.address`; required by semconv client-span rules |
| `network.peer.port` | int | Mirror of `server.port` |

### Explicitly NOT emitted (privacy / security)

| Key | Reason |
|---|---|
| `db.query.text` / `db.statement` | Query documents routinely contain PII and secrets. Future opt-in (`WithCapturedQueryText(true)`) must carry an explicit warning. |
| Response document contents | Same rationale. |

### Skipped operations by default

No spans emitted for `getMore`, `killCursors`, `ping`, `hello`,
`isMaster`. Rationale and override knob in ADR 0005 §6.

---

## Logs

All log records pass through the `otelslog` bridge, which applies OTel
Log Data Model attributes automatically.

### Per-record attributes injected by the SDK

| Key | Source | Notes |
|---|---|---|
| `traceId` (stdout JSON) / `trace_id` (OTLP) | Active span from ctx | Via `OtelSlogHandler` on stdout path; via `otelslog` bridge on OTLP path. See ADR 0001. |
| `spanId` (stdout JSON) / `span_id` (OTLP) | Active span from ctx | Same mechanism as above. |
| `service.name` | Stdout JSON top-level field (explicit); OTLP: Resource attribute | ADR 0001 §Option B |
| `environment` | Stdout JSON top-level field (explicit); OTLP: Resource attribute `deployment.environment.name` | Legacy stdout name retained for backward compatibility |

### Log severity

- stdout: `"level"` (slog default)
- OTLP: `severityNumber` + `severityText` (OTel Log Data Model)

See ADR 0001 for the full table of intentional stdout↔OTLP differences.

---

## Deviations / exceptions

None at this time.

If a deviation is required (e.g. a backend constraint forces a
non-semconv key), it must be listed here with:

1. The non-standard key and type.
2. The reason no standard alternative works.
3. The mitigation plan (deprecation path or upstream spec issue link).
