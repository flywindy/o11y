# o11y SDK Examples

Runnable programs demonstrating the o11y SDK. They are organized the same way as
the [Developer Guide](../docs/guide.md):

1. **[The Four Pillars](#the-four-pillars)** — Tracing, Logging, Metrics, and
   Profiling. Start here.
2. **[Integrations](#integrations)** — per-library examples grouped by
   [OpenTelemetry semantic-convention](../docs/semconv.md) domain (HTTP,
   Databases, Messaging, Object Storage). Run the ones for the libraries you
   care about.

For SDK setup and the `Init` options reference, see the [README](../README.md).

All `go run` commands below are run from the **repository root**.

## Prerequisites

Before running any example, start the kind cluster and deploy the monitor stack:

```bash
kind create cluster --config kind-config.yaml
kubectl apply -k k8s/infrastructure/base/monitor
```

Then port-forward the monitor services that are not already exposed via kind's extraPortMappings:

```bash
# 4318 (otel-collector) and 4040 (alloy/pyroscope) are already bound to host
# ports via kind-config.yaml — no port-forward needed for those.
kubectl port-forward -n infra svc/grafana    3000:3000  # Grafana UI
kubectl port-forward -n infra svc/prometheus 9090:9090  # Prometheus UI
```

Datastore services (NATS, Redis, MongoDB, MinIO, Elasticsearch, Cassandra) are **not deployed by default**. Each example section below shows the `kubectl apply -k` command to bring up the required component before port-forwarding it.

---

# The Four Pillars

## Basic — Tracing + Logging

```bash
go run examples/basic/main.go
```

Demonstrates the trace and log pillars together: spans created from
`obs.Tracer(...)` and structured logs from `obs.Logger` that are automatically
correlated with `traceId` / `spanId`.

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

## Profiling

```bash
go run examples/profiling/main.go
```

The example starts a sampled root span every two seconds and burns CPU long
enough for Pyroscope to capture useful samples. It sends profiles to
`PYROSCOPE_ENDPOINT` (default `http://localhost:4040`) and traces/logs/OTLP
metrics to `OTLP_ENDPOINT` (default `http://localhost:4318`). Keep the Alloy
and Grafana port-forwards from the setup block running while the example runs.

---

# Integrations

## HTTP

### Gin

```bash
go run examples/gin/main.go
curl http://localhost:8080/ok
curl http://localhost:8080/fail
```

The example registers `o11ygin.Middleware(...)` before `gin.Recovery()` and
demonstrates typed `gin.error.type` span events from `c.AbortWithError`.

### Resty

```bash
go run examples/resty/main.go
```

The example starts a local downstream HTTP server on `:8081`, calls it through
the o11y Resty wrapper every two seconds, and intentionally returns periodic
503 responses so retry attempts appear as sibling client spans with
`http.request.resend_count`.

## Databases

### MongoDB

```bash
kubectl apply -k k8s/infrastructure/base/components/mongodb
kubectl port-forward -n infra svc/mongodb 27017:27017
export MONGODB_URI=mongodb://localhost:27017
go run examples/mongodb/main.go
```

The example sends traces, logs, MongoDB operation-duration metrics, and MongoDB
connection-pool metrics to the OTel Collector at `http://localhost:4318`, so
keep the Collector port-forward open while it runs.

### Background work (gin + MongoDB + obsctx)

Demonstrates the safe context pattern for work that outlives a request. With MongoDB deployed and port-forwarded (see the MongoDB section above) and the Collector running:

```bash
export MONGODB_URI=mongodb://localhost:27017
go run examples/background/main.go
curl http://localhost:8080/things/abc
```

The handler reads with the request context, responds, and then runs a
post-response MongoDB write via `obsctx.Go` — keeping the request's trace while
not being canceled when the request ends. See
[ADR 0024](../docs/adr/0024-context-lifecycle-for-background-work.md).

### Redis

```bash
kubectl apply -k k8s/infrastructure/base/components/redis
kubectl port-forward -n infra svc/redis 6379:6379
go run examples/redis/main.go
```

The example emits Redis command spans plus `db.client.operation.duration` and
connection-pool metrics through OTLP metrics push. It runs `PING`, `SET`,
`GET`, a cache-miss `GET`, and a pipeline every two seconds, and a background
liveness probe issues an unparented `PING` every five seconds to model
health-check traffic.

Two environment variables exercise the command-noise filtering options:

- `O11Y_REDIS_IGNORE_COMMANDS` — comma-separated command verbs to drop entirely
  (span and duration sample), e.g. `O11Y_REDIS_IGNORE_COMMANDS=ping,info`. This
  drops every matching command regardless of context, including the request-bound
  `PING` in the main loop.
- `O11Y_REDIS_REQUIRE_PARENT_SPAN=true` — drop commands issued without an active
  parent span. The background liveness `PING` disappears (it runs on a bare
  context) while the request-bound `PING` inside the `redis-cycle` span stays.

Compare the two against the background probe to see why `WithIgnoredCommands` is
the precise way to silence one command while `WithRequireParentSpan` drops all
non-request-bound work. (`O11Y_REDIS_COMMAND_TEXT=true` additionally records
`db.query.text`.)

### Elasticsearch

```bash
kubectl apply -k k8s/infrastructure/base/components/elasticsearch
kubectl port-forward -n infra svc/elasticsearch 9200:9200
go run examples/elasticsearch/main.go
```

The example loops every three seconds through `Index` → `Search`, so each
request appears as a `SpanKindClient` span carrying the upstream's `db.system`
/ `db.operation` / `db.elasticsearch.path_parts.*` attributes (legacy semconv
keys, see [`docs/semconv.md`](../docs/semconv.md)). It builds the client with
`WithSearchBody(true)`, so the search query body is recorded as `db.statement`.
The integration is trace-only (ADR 0020 §6). Override the endpoint and
credentials via `ELASTICSEARCH_URL`, `ELASTICSEARCH_USERNAME`, and
`ELASTICSEARCH_PASSWORD`.

### Cassandra

```bash
kubectl apply -k k8s/infrastructure/base/components/cassandra
kubectl port-forward -n infra svc/cassandra 9042:9042
go run examples/cassandra/main.go
```

The example builds an instrumented `*gocql.Session` with
`cassandra.NewSession`, then runs an `INSERT`, a `SELECT`, and a batch (via
`cassandra.ExecuteBatch`) every two seconds. It emits per-attempt CLIENT spans,
`db.client.operation.duration`, and the `cassandra.query.attempts` counter
through OTLP. Set `O11Y_CASSANDRA_QUERY_TEXT=1` to record CQL statement text on
spans.

## Messaging

### NATS Core (two terminals)

```bash
kubectl apply -k k8s/infrastructure/base/components/nats
kubectl port-forward -n infra svc/nats 4222:4222

# Terminal 1 — start subscriber first
go run examples/nats-core/subscriber/main.go

# Terminal 2 — publisher sends a message every 3 seconds
go run examples/nats-core/publisher/main.go
```

### NATS Core request/reply (two terminals)

Requires NATS — apply and port-forward it as shown in the NATS Core section above if not already running.

`otel-nats` gates trace propagation behind two env vars; export them in **both**
terminals or no `traceparent` is injected and you will see replies without
NATS trace correlation:

```bash
export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true
export OTEL_NATS_TRACING_ENABLED=true

# Terminal 1 — start responder first
go run examples/nats-core/responder/main.go

# Terminal 2 — requester sends a request every 3 seconds and logs the reply
go run examples/nats-core/requester/main.go
```

The responder replies with `conn.Respond`, which routes the reply through the
traced publish path so the reply message carries the responder's trace context
(unlike raw `msg.Respond`). The requester uses `conn.Request`; the upstream
otel-nats v0.6.0 layer records a `receive {inbox}` span for the reply — named
for the reply inbox, parented under the responder's trace and linked back to
its reply-send span (the reply-span recording moved from this SDK's facade to
upstream in the v0.6.0 upgrade; see ADR 0022's 2026-07-09 amendment).

This example demonstrates the full round trip: in Tempo you should see the
request publish, responder processing, the responder's traced reply publish,
and the requester-side `receive {inbox}` span carrying a link back to that
reply publish — so the handler → requester leg is no longer a dead end.

### JetStream (two or three terminals; requires JetStream-enabled NATS server)

Requires NATS — apply and port-forward it as shown in the NATS Core section above if not already running.

As with the core examples, export both tracing gates in each terminal or no
`traceparent` is injected:

```bash
export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true
export OTEL_NATS_TRACING_ENABLED=true

# Terminal 1 — publisher creates the stream and publishes
go run examples/jetstream/publisher/main.go

# Terminal 2 — subscriber attaches a durable consumer and processes messages
go run examples/jetstream/subscriber/main.go

# Terminal 3 (optional) — fetch-worker pulls the same stream in batches via its
# own durable consumer; run it alongside or instead of Terminal 2
go run examples/jetstream/fetch-worker/main.go
```

The subscriber's `Consume` handler receives the native `jetstream.Msg` plus a
`ctx` carrying the consumer span (linked to the publisher's trace), and config
types come from `github.com/nats-io/nats.go/jetstream` — no upstream
instrumentation import.

The fetch-worker demonstrates the batch-pull path (`Consumer.Fetch`) instead of
the push-style `Consume` the subscriber uses — the pattern a bulk-processing
worker (e.g. syncing a batch of events to a search index per round trip) needs
instead of one callback invocation per message. Each `FetchedMessage` on the
returned `MessageBatch` channel pairs the native `jetstream.Msg` with the same
kind of consumer-span `ctx` `Consume`/`Messages` deliver — range the channel to
completion (as this example does) so the batch's forwarding goroutine isn't
left blocked waiting for a reader that never comes back.

### NATS over WebSocket (browser)

See [`nats-ws-browser/README.md`](nats-ws-browser/README.md) for the
browser-based end-to-end trace propagation example (Vite frontend + Go backend).

## Object Storage

### MinIO

```bash
kubectl apply -k k8s/infrastructure/base/components/minio
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
