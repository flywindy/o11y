# ADR 0001: Log Format Strategy — stdout vs. OTLP

**Status**: Accepted  
**Date**: 2026-04-17

---

## Context

In Kubernetes, stdout is scraped by Fluentd and forwarded to Elasticsearch (ES). The same
application also exports logs via OTLP/HTTP to the OTel Collector, which routes them to Loki.
This means every log record lands in two backends through independent pipelines.

A natural question arises: should the stdout JSON format be made identical to the OTLP payload
sent to the Collector, so that ES and Loki receive a consistent schema?

---

## Why Identical Formats Are Not Feasible

### 1. The OTel Log Data Model is a logical model, not an output format

The OTel specification explicitly states it *"defines a **logical model** for a log record
(irrespective of the physical format and encoding of the record)."* It is designed as a
translation layer between logging systems, not as a format that applications should emit
directly to stdout.  
Reference: [Logs Data Model](https://opentelemetry.io/docs/specs/otel/logs/data-model/)

### 2. OTel recommends a bridge, not rewriting application log output

> *"OpenTelemetry is designed to work with the **logs you already produce** …  
> you don't need to rewrite your logging code."*

The prescribed path is to use a log appender or bridge (this SDK's `otelslog` bridge) and let
the Collector handle all format transformation.  
Reference: [Logs | OpenTelemetry](https://opentelemetry.io/docs/concepts/signals/logs/)

### 3. The official OTLP file format is not human-readable

The OTel File Exporter — the closest thing to an "OTLP stdout" format — outputs dense,
single-line JSON blobs explicitly designed for automated processing in Kubernetes and FaaS
contexts, not for human inspection.  
Reference: [OTLP File Exporter](https://opentelemetry.io/docs/specs/otel/protocol/file-exporter/)

### 4. `Resource` and `Attributes` cannot be flattened without semantic loss

OTLP separates `Resource` (static per-process metadata such as `service.name`, sent once per
batch) from per-record `Attributes` (event-specific context). stdout has no concept of a
Resource; flattening both into a single JSON object loses the semantic distinction and breaks
round-trip fidelity. This trade-off was debated by Elastic engineers in the OTel specification
itself and has no clean resolution.  
Reference: [Discussion #1621](https://github.com/open-telemetry/opentelemetry-specification/discussions/1621)

---

## Options Considered

### Option A — Collector fan-out

Route ES through the same OTLP pipeline instead of using stdout as the ES ingestion path:

```
App → OTLP/HTTP → Collector ─┬─→ Loki  (lokiexporter)
                              └─→ ES    (elasticsearchexporter, otel mode)
```

Both backends would receive the same OTel Log Data Model, with schema consistency managed
centrally by the Collector config. The `elasticsearchexporter`'s default `otel` mode stores
data using original OTLP field names, closely matching what Loki receives.

**When to consider Option A**: This approach becomes worth adopting when all of the following
conditions are met:

- The project is greenfield or existing ES index mappings can be migrated without disrupting
  active consumers.
- The logging pipeline is fully owned and operated within the same team, making Collector
  configuration changes straightforward to coordinate.
- stdout is acceptable as a local development tool only, with no expectation of structured
  log consumption from outside the cluster.

---

### Option B — Align stdout field naming (adopted)

Keep the current dual-output architecture. Rename `trace_id` / `span_id` in the stdout handler
to the OTel-idiomatic camelCase (`traceId` / `spanId`) so that trace correlation queries use
the same field names across ES and Loki.

**Code change**: two field name strings in `internal/log/handler.go`.

**Format after Option B:**

stdout → ES (via Fluentd, flat JSON):
```json
{
  "time": "2026-04-16T10:00:00.000Z",
  "level": "INFO",
  "msg": "order received",
  "service.name": "order-service",
  "environment": "production",
  "traceId": "4bf92f3577b34da6a3ce929d0e0e4736",
  "spanId": "00f067aa0ba902b7"
}
```

Loki (via OTLP, OTel Log Data Model):
```
Stream labels:       { service_name="order-service", deployment_environment="production" }
Log line:            order received
Structured metadata: { traceID="4bf92f...", spanID="00f067...", severity="INFO" }
```

**Remaining intentional differences between ES and Loki:**

| Field | ES (stdout via Fluentd) | Loki (OTLP via Collector) |
|---|---|---|
| Message | `msg` | body (log line) |
| Severity | `level` | `severityText` |
| `service.name` | flat top-level field | stream label |
| App attributes | flat top-level fields | structured metadata |
| Trace / span ID | `traceId` / `spanId` ✅ | `traceID` / `spanID` ✅ |

These remaining differences cannot be eliminated without fully reimplementing the `slog.Handler`
interface to emit OTLP-compatible JSON — which contradicts OTel's own guidance (see reason 2
above).

---

## Decision

**Option B is adopted.**

Reasons:

1. **Preserves existing log reading habits.** The flat JSON stdout format is familiar to
   operators and developers who already consume logs via ES or terminal output. Switching to
   an OTLP-native structure would change field names, nesting, and key conventions across the
   board, requiring updates to dashboards, alert rules, and runbooks on all adopting services.

2. **Minimal blast radius for services already in production.** Services integrated with this
   SDK emit a stable stdout schema today. A field-level rename is scoped to two keys
   (`trace_id` → `traceId`, `span_id` → `spanId`) and does not affect the OTLP pipeline,
   Loki queries, or any other part of the system.

3. **Trace correlation is the highest-priority consistency requirement.** Operators querying
   across ES and Loki primarily join on trace and span IDs. Aligning these two field names
   resolves the most impactful cross-system inconsistency without requiring infrastructure
   changes.

4. **Consistent with OTel guidance.** OTel explicitly recommends keeping application log
   output unchanged and using a bridge for OTLP export. Option B follows this guidance;
   Option A would too, but with a higher coordination cost under current constraints.
