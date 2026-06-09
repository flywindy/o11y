# o11y SDK Examples

Runnable programs demonstrating each pillar and integration of the o11y SDK.
For SDK setup and the `Init` options reference, see the
[README](../README.md); for the deeper integration guide, see
[`docs/guide.md`](../docs/guide.md).

All `go run` commands below are run from the **repository root**.

## Prerequisites

Before running any example, port-forward the required services from the `kind` cluster:

```bash
kubectl port-forward -n infra svc/otel-collector 4318:4318  # OTel traces and logs
kubectl port-forward -n infra svc/nats           4222:4222  # NATS connection
kubectl port-forward -n infra svc/redis          6379:6379  # Redis connection
kubectl port-forward -n infra svc/grafana        3000:3000  # Grafana UI
kubectl port-forward -n infra svc/prometheus     9090:9090  # Prometheus UI
kubectl port-forward -n infra svc/alloy          4040:4040  # Pyroscope ingest for local app profiling
```

## Basic (spans + logs)

```bash
go run examples/basic/main.go
```

## NATS Core (two terminals)

```bash
# Terminal 1 — start subscriber first
go run examples/nats-core/subscriber/main.go

# Terminal 2 — publisher sends a message every 3 seconds
go run examples/nats-core/publisher/main.go
```

## JetStream (two terminals; requires JetStream-enabled NATS server)

```bash
# Terminal 1 — publisher creates the stream and publishes
go run examples/jetstream/publisher/main.go

# Terminal 2 — subscriber attaches a durable consumer and processes messages
go run examples/jetstream/subscriber/main.go
```

## Metrics (otelhttp facade + OTLP push)

```bash
go run examples/metrics/main.go
```

The example starts an HTTP server on `:8080` and generates synthetic traffic every 500 ms. Metrics flow via OTLP/HTTP to the OTel Collector, which forwards them to Prometheus via remote write. It uses the same `localhost:4318` NodePort as traces and logs, so no extra metrics scrape port is needed for this example. Histogram buckets include exemplars linking each measurement to its trace.

Open Grafana at `http://localhost:3000` and navigate to:
- **Explore → Tempo** — producer and consumer spans linked across services
- **Explore → Loki** — structured log entries with correlated `traceId` and `spanId`
- **Explore → Prometheus** — `http_server_request_duration_seconds`; click an exemplar dot to jump to the linked trace in Tempo
- **Dashboards → Observability → Metrics Correlation** — HTTP latency metrics with a data link that opens the matching `metrics-example` logs in Loki

## Gin

```bash
go run examples/gin/main.go
curl http://localhost:8080/ok
curl http://localhost:8080/fail
```

The example registers `o11ygin.Middleware(...)` before `gin.Recovery()` and
demonstrates typed `gin.error.type` span events from `c.AbortWithError`.

## Resty

```bash
go run examples/resty/main.go
```

The example starts a local downstream HTTP server on `:8081`, calls it through
the o11y Resty wrapper every two seconds, and intentionally returns periodic
503 responses so retry attempts appear as sibling client spans with
`http.request.resend_count`.

## Redis

Port-forward Redis to `localhost:6379`, then run the example:

```bash
kubectl port-forward -n infra svc/redis 6379:6379
go run examples/redis/main.go
```

The example emits Redis command spans plus `db.client.operation.duration` and
connection-pool metrics through OTLP metrics push. It runs `PING`, `SET`,
`GET`, a cache-miss `GET`, and a pipeline every two seconds.

## MinIO

Port-forward MinIO to `localhost:9000`, then run the example:

```bash
kubectl port-forward -n infra svc/minio 9000:9000
go run examples/minio/main.go
```

The example loops every three seconds through `PutObject` → `StatObject`
→ `GetObject` → `RemoveObject` with `WithHTTPChildSpans(true)`, so each
logical operation produces a `SpanKindClient` span with HTTP child
spans for the underlying round-trips and one
`minio.client.operation.duration` sample per operation. Credentials
default to `minioadmin`/`minioadmin`; override via `MINIO_ENDPOINT`,
`MINIO_ACCESS_KEY`, and `MINIO_SECRET_KEY`.

## Profiling

```bash
go run examples/profiling/main.go
```

The example starts a sampled root span every two seconds and burns CPU long
enough for Pyroscope to capture useful samples. It sends profiles to
`PYROSCOPE_ENDPOINT` (default `http://localhost:4040`) and traces/logs/OTLP
metrics to `OTLP_ENDPOINT` (default `http://localhost:4318`). Keep the Alloy
and Grafana port-forwards from the setup block running while the example runs.

## MongoDB

Run a local MongoDB instance or port-forward one to `localhost:27017`, then run
the example:

```bash
export MONGODB_URI=mongodb://localhost:27017
go run examples/mongodb/main.go
```

The example sends traces, logs, MongoDB operation-duration metrics, and MongoDB
connection-pool metrics to the OTel Collector at `http://localhost:4318`, so
keep the Collector port-forward open while it runs.

## NATS over WebSocket (browser)

See [`nats-ws-browser/README.md`](nats-ws-browser/README.md) for the
browser-based end-to-end trace propagation example (Vite frontend + Go backend).
