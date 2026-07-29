# AGENTS.md — o11y Golang SDK

> This is the canonical AI context file for the o11y project.
> `CLAUDE.md` and `GEMINI.md` are symlinks pointing to this file.

---

## Project Overview

A lightweight Go SDK providing standardized observability for Go services.
It integrates OpenTelemetry (OTel) tracing with structured logging (`slog`)
so that every log entry is automatically enriched with `traceId` and `spanId`.

**Module path**: `github.com/flywindy/o11y`

---

## Tech Stack

| Layer | Choice | Notes |
|---|---|---|
| Language | Go 1.25+ | Use standard library where possible |
| Tracing | OpenTelemetry Go SDK (OTLP/HTTP) | Not gRPC — keep it simple for local dev |
| Logging | `log/slog` + `otelslog` bridge | Dual output: OTLP/HTTP → Loki (full OTel Log Data Model) and JSON stdout; `OtelSlogHandler` injects traceId / spanId on stdout path |
| Metrics | Prometheus pull (`:2112`) or OTLP push | Pull: k8s pods scraped by Prometheus; Push: `WithMetricsOTLPEndpoint` for local dev / serverless |
| Web framework | gin v1 | `gin/` wraps `otelgin` with SDK providers and typed `c.Errors` recording |
| Messaging | NATS | High-performance pub/sub |
| Database | MongoDB | NoSQL persistence |
| Cache | Redis / Valkey | Redis-protocol cache and data store instrumentation |
| Tracing backend | Grafana Tempo | |
| Log backend | Grafana Loki | |
| Metrics backend | Prometheus | Scrapes k8s pods on `:2112`; also accepts remote write from OTel Collector |
| Visualization | Grafana | Unified traces, logs, and metrics; exemplars link histograms → Tempo traces |
| Collector | OTel Collector | Centralized telemetry pipeline for traces, logs, and OTLP metrics |
| Local cluster | kind (Kubernetes in Docker) | Port 4318 mapped for OTLP/HTTP (traces, logs, and metrics push) |

---

## Package Layout

| Path | Purpose |
|---|---|
| `/` | Core SDK initialization, options, resources, and provider lifecycle |
| `http/` | T2 facade over `otelhttp` for server and client HTTP instrumentation |
| `gin/` | T2 facade over `otelgin` plus typed gin error recording |
| `nats/` | T2 facade over NATS Core and JetStream instrumentation |
| `mongo/` | T2 facade over MongoDB driver instrumentation plus SDK-owned pool metrics |
| `redis/` | SDK-owned Redis/Valkey instrumentation over go-redis/v9 |
| `internal/` | SDK-owned logging, tracing, metrics, and test utilities |
| `examples/` | Runnable examples for supported integrations; `examples/README.md` documents how to run each |
| `docs/guide.md` | Developer Guide — structured logging, sampling, profiling, and per-integration sub-package usage |

---

## Required SDK Init Options

Every service **must** provide all four options; `Init` returns an error if any are missing or invalid:

| Option | semconv key | Notes |
|--------|------------|-------|
| `WithServiceName("my-svc")` | `service.name` | Unique service identifier |
| `WithServiceVersion("1.2.3")` | `service.version` | Required for canary/rollback tracking |
| `WithServiceNamespace("platform")` | `service.namespace` | Owning team/product; maps to k8s namespace |
| `WithEnvironment("production")` | `deployment.environment.name` | Canonical values only (see below) |

**Canonical environment values** — aliases are auto-normalized, unknown values are rejected:

| Input | Canonical |
|-------|-----------|
| `production`, `prod` | `production` |
| `staging`, `stage`, `stg` | `staging` |
| `development`, `develop`, `dev` | `development` |
| `testing`, `test` | `testing` |

---

## Core Principles — Never Violate These

1. **Context-First**: Every function must accept and propagate `context.Context`. Trace information flows through context only. *(Follows the Go stdlib `context` idiom established in Go 1.7.)*
2. **Zero Global State**: Encapsulate OTel providers in structs. No package-level `init()` with side effects. No global logger variables. *(Rooted in Go 2020+ library idioms — newer stdlib APIs such as `log/slog`, `rand/v2`, and `http.Client` all moved away from package-level globals. See [ADR 0003](docs/adr/0003-global-state-policy.md) for the full rationale and the third-party integration policy.)*
3. **Correlation**: `slog` output must always include `traceId` and `spanId` as JSON fields when a span is active. *(See [ADR 0001](docs/adr/0001-log-format-strategy.md) for the stdout ↔ OTLP field naming decision.)*
4. **Errors**: Use `slog.ErrorContext(ctx, ...)` with structured attributes. Never use `panic` for recoverable errors.
5. **Semconv v1.39.0**: All instrument names, attribute keys, and attribute types must conform to OTel Semantic Conventions v1.39.0. Do not mix SDK-owned semconv imports. *(See [`docs/semconv.md`](docs/semconv.md) for the complete catalog of attributes emitted by this SDK.)* **Before writing, reviewing, or defending any attribute-key string — in code, ADRs, or `docs/semconv.md` — use the [`verify-semconv-attributes`](.agents/skills/verify-semconv-attributes/SKILL.md) skill.** Keys are renamed and moved between namespaces across semconv versions (e.g. `db.cassandra.*` → `cassandra.*`, `db.elasticsearch.node.name` → `elasticsearch.node.name`); resolve every key against the pinned package source on disk, never from memory or a web summary.

---

## Common Commands

```bash
# Format & tidy (run before every commit)
go fmt ./...
go mod tidy

# Lint
go vet ./...

# Test
go test ./...
go test -race ./...          # Always run with race detector

# Start local kind cluster
kind create cluster --config kind-config.yaml

# Deploy monitor stack only (Tempo, Loki, Pyroscope, Alloy, OTel Collector, Prometheus, Grafana)
kubectl apply -k k8s/infrastructure/base/monitor

# Deploy monitor stack + a specific datastore component (repeat for each needed)
kubectl apply -k k8s/infrastructure/base/monitor
kubectl apply -k k8s/infrastructure/base/components/nats          # examples: nats-core, jetstream, nats-ws-browser
kubectl apply -k k8s/infrastructure/base/components/mongodb       # examples: mongodb
kubectl apply -k k8s/infrastructure/base/components/redis         # examples: redis
kubectl apply -k k8s/infrastructure/base/components/minio         # examples: minio
kubectl apply -k k8s/infrastructure/base/components/elasticsearch # examples: elasticsearch
kubectl apply -k k8s/infrastructure/base/components/cassandra     # examples: cassandra

# Private registry deployment: edit overlays/private-registry/kustomization.yaml
# to uncomment the resources you need, then:
kubectl apply -k k8s/infrastructure/overlays/private-registry

# Verify all pods are Running
kubectl get pods -n infra

# Run the basic example (cluster must be up)
go run examples/basic/main.go

# Run the NATS Core examples (two terminals; cluster must be up with NATS running)
go run examples/nats-core/subscriber/main.go
go run examples/nats-core/publisher/main.go

# Run the NATS Core request/reply examples (two terminals; responder replies via conn.Respond)
# No env vars needed: o11ynats.Connect enables tracing via otelnats.WithTracingEnabled(true).
# For a hard disabled-observability/native-cost mode, drive NATS tracing from
# the SDK's resolved toggle:
# o11ynats.ConnectWithOptions(..., o11ynats.WithTracingEnabled(obs.Toggles.Trace)).
go run examples/nats-core/responder/main.go
go run examples/nats-core/requester/main.go

# Run the JetStream examples (NATS must have JetStream enabled)
# Start publisher first — it creates the JetStream stream; then start the subscriber
# and/or the fetch-worker (own durable consumer, so both can run at once)
go run examples/jetstream/publisher/main.go     # creates the stream and publishes
go run examples/jetstream/subscriber/main.go    # push-style Consume: durable consumer, processes as messages arrive
go run examples/jetstream/fetch-worker/main.go  # batch-pull: Consumer.Fetch in bounded batches

# Run the metrics example (pushes via OTLP → OTel Collector → Prometheus; cluster must be up)
go run examples/metrics/main.go

# Run the MongoDB example (cluster must be up with OTel Collector; MongoDB must be reachable)
go run examples/mongodb/main.go

# Run the Redis example (cluster must be up with Redis port-forwarded to localhost:6379)
kubectl port-forward -n infra svc/redis 6379:6379
go run examples/redis/main.go

# Port-forward Grafana (default credentials: admin/admin)
kubectl port-forward -n infra svc/grafana 3000:3000

# Port-forward Prometheus
kubectl port-forward -n infra svc/prometheus 9090:9090
```

---

## Kubernetes Infrastructure Verification

When modifying files under `k8s/infrastructure/**`, use the repo-local `verify-kubernetes-manifests` skill at `.agents/skills/verify-kubernetes-manifests`.

### kustomization overlay image sync

Whenever a new `image:` is added to any file under `k8s/infrastructure/base/`, also add the corresponding entry to `k8s/infrastructure/overlays/private-registry/kustomization.yaml`.

Verify coverage by comparing:

```bash
find k8s/infrastructure/base/monitor k8s/infrastructure/base/components \
  -name "*.yaml" -type f -print0 \
  | xargs -0 grep -h "image:" \
  | awk '{print $2}' | sed 's/:[^/]*$//' | sort -u
```

against the `images:` block in the overlay — every image must have a matching `newName` entry.

### kind-config.yaml port mappings

Whenever a new `NodePort` Service is added to `k8s/infrastructure/base/`, add a corresponding `extraPortMappings` entry in `kind-config.yaml` so the port is reachable from the host.

Current NodePort → hostPort mappings:

| NodePort | hostPort | Service |
|---|---|---|
| 30000 | 4318 | otel-collector (OTLP HTTP) |
| 30001 | 4223 | NATS |
| 30002 | 4040 | Pyroscope |

For changes that affect live infrastructure behavior, verify against the kind cluster with `kubectl` when access is available:

- Inspect the live resource before or after the change with `kubectl get ... -o yaml`
- Apply through the relevant Kustomize entry point, usually `kubectl apply -k k8s/infrastructure/base`
- Restart workloads that require config reloads, such as Grafana datasource provisioning
- Wait for rollout completion with `kubectl rollout status`
- Verify behavior through the relevant in-cluster service API when practical

---

## Code Standards

- All code, comments, and documentation must be in **English**
- Every exported symbol must have a **godoc comment**
- Use **named return values** only when they aid clarity
- Prefer `errors.New` / `fmt.Errorf` with `%w` for wrapping
- JSON log output is the default format (structured, machine-parseable)
- Do not introduce new external dependencies without discussion
- Keep user-facing docs in sync with the code whenever public-facing API, usage patterns, or examples change — README is the first point of contact for SDK users. Init options and feature toggles live in `README.md`; per-integration usage lives in `docs/guide.md`; example run instructions live in `examples/README.md`

---

## Test Standards

- Every public function must have a unit test
- Use testify/mock or gomock for dependencies
- Table-driven tests preferred

---

## Git Workflow

- Use **Conventional Commits**: `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`
- Run `go fmt ./...` and `go mod tidy` before every commit
- Keep commits small and focused — one logical change per commit
- PR titles must follow Conventional Commits format as well

---

## Architecture Decisions (ADR Summary)

Full ADR documents live in [`docs/adr/`](docs/adr/).

| Decision | Choice | Reason |
|---|---|---|
| Transport | OTLP/HTTP (not gRPC) | Simpler firewall / proxy rules for local dev |
| Logger | `log/slog` (stdlib) | No external dep; native structured logging since Go 1.21 |
| Tracing backend | Tempo | OSS, Grafana-native, cost-effective |
| Log backend | Loki | OSS, integrates with Grafana and Tempo for trace-to-log correlation |
| Local infra | kind | Reproducible Kubernetes without cloud cost |
| Log format strategy | Option B — align stdout `traceId`/`spanId` field names | Preserves existing log reading habits; minimal blast radius. See [ADR 0001](docs/adr/0001-log-format-strategy.md) |
| Metrics strategy | Prometheus pull (default `:2112`) + OTLP push opt-in (`WithMetricsOTLPEndpoint`) | Prometheus pull requires zero Collector config; OTLP push covers serverless. Exemplars enabled by default (OTel SDK `SampledFilter`). See [ADR 0002](docs/adr/0002-metrics-strategy.md) |
| Global state policy | SDK packages must not mutate OTel globals; third-party instrumentation libraries are verified per-version before adoption | See [ADR 0003](docs/adr/0003-global-state-policy.md) |
| NATS integration | `github.com/akira-core/instrumentation-go/otel-nats` — verified at v0.6.0 not to mutate global providers/propagators (module path renamed from `Marz32onE` upstream); wrapped by the `nats/` package | Covers NATS Core + all JetStream consumer patterns with OTel semconv v1.39.0. See [ADR 0004](docs/adr/0004-nats-integration.md) |
| MongoDB integration | `go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo` — wrapped by the `mongo/` package | Wires SDK providers explicitly through a driver `CommandMonitor`, emits command spans and operation metrics, adds SDK-owned connection-pool metrics through `PoolMonitor`, and does not inject `_oteltrace` into persisted documents. See [ADR 0014](docs/adr/0014-mongodb-metrics.md) and [ADR 0021](docs/adr/0021-mongodb-instrumentation-mechanism.md) |
| Semconv version policy | Pin v1.39.0; upgrade only when concrete triggers fire | Single SDK-owned pin avoids cognitive cost and dashboard breakage. Upgrade triggers and process documented to keep version moves deliberate. See [ADR 0006](docs/adr/0006-semconv-upgrade-strategy.md) |
| HTTP integration | `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` — wrapped by the `http/` package | Provides `NewServerHandler` and `NewTransport` with SDK providers and propagator wired explicitly. See [ADR 0009](docs/adr/0009-replace-http-with-otelhttp.md) |
| Gin integration | `go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin` — wrapped by the `gin/` package | Provides canonical middleware ordering and typed `gin.error.type` events for `c.Errors`. See [ADR 0010](docs/adr/0010-gin-integration.md) |
| Redis / Valkey integration | SDK-owned bounded T3 wrapper over `github.com/redis/go-redis/v9` | Avoids legacy `redisotel` semconv and sensitive defaults while emitting SDK-controlled spans, metrics, and views. See [ADR 0013](docs/adr/0013-redis-valkey-integration.md) |

---

## NATS & JetStream Usage

All NATS connections must go through `github.com/flywindy/o11y/nats` so that the SDK's `TracerProvider` and `Propagator` are wired in without touching global OTel state.

### NATS Core

```go
conn, err := o11ynats.Connect(ctx, natsURL, sdk.TracerProvider(), sdk.Propagator)

// Publish — trace context is injected into message headers automatically.
conn.Publish(ctx, "o11y.events", payload)

// Subscribe — ctx in the handler already carries the publisher's trace.
conn.Subscribe(ctx, "o11y.events", func(ctx context.Context, msg *nats.Msg) {
    _, span := tracer.Start(ctx, "process-event")
    defer span.End()
    slog.InfoContext(ctx, "received", slog.String("payload", string(msg.Data)))
})
```

To make the consumer span itself searchable on domain identifiers the SDK has
no way to know (a room ID, a site ID, a request ID from the payload), set them
on the span the handler was handed — `otel-nats` owns that span's name and
base attributes and cannot be forked for this (ADR 0022), but its span
accepts extra attributes like any other:

```go
conn.Subscribe(ctx, "o11y.events", func(ctx context.Context, msg *nats.Msg) {
    trace.SpanFromContext(ctx).SetAttributes(
        attribute.String("app.room_id", roomID),
    )
    // ...
})
```

### JetStream

`conn.JetStream()` returns an o11y-owned facade (ADR 0022 Phase 2); configuration types come straight from `github.com/nats-io/nats.go/jetstream`, so callers never import the upstream `oteljetstream` package.

```go
js, _ := conn.JetStream()

// Idempotent stream creation — safe to call on every startup.
js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
    Name: "EVENTS", Subjects: []string{"events.>"},
})

// Publish — trace context injected into JetStream message headers.
js.Publish(ctx, "events.created", payload)

// Durable pull consumer with Consume (push-style delivery). Handler ctx
// carries the consumer span linked to the producer's trace.
stream, _ := js.Stream(ctx, "EVENTS")
consumer, _ := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
    Durable: "events-processor", AckPolicy: jetstream.AckExplicitPolicy,
})
cc, _ := consumer.Consume(ctx, func(ctx context.Context, msg jetstream.Msg) {
    _, span := tracer.Start(ctx, "process-event")
    defer span.End()
    msg.Ack()
})
defer cc.Stop()

// Fetch / FetchBytes / FetchNoWait deliver a MessageBatch: each FetchedMessage
// on the channel pairs the native jetstream.Msg with its consumer-span ctx.
batch, _ := consumer.Fetch(ctx, 10)
for m := range batch.Messages() {
    _, span := tracer.Start(m.Ctx, "process-event")
    m.Msg.Ack()
    span.End()
}
```

Not yet wrapped: single-message `Consumer.Next` (use `Messages(ctx, jetstream.PullMaxMessages(1))` instead — see ADR 0022 amendment), `PushConsumer`, and ordered consumers.

### Request-Reply note

When replying to a message inside a `Subscribe` handler, do **not** use `msg.Respond(data)` if you need the reply to carry trace context. `msg.Respond` routes through the raw NATS connection and skips header injection. Use `conn.Respond(ctx, msg, data)` (or `conn.Publish(ctx, msg.Reply, data)`) instead — `Respond` validates the reply subject and routes through the traced publish path.

On the requester side, `conn.Request(ctx, subject, data, timeout)` closes the round trip. Since the otel-nats v0.6.0 upgrade the reply "receive" span is recorded by the upstream layer (not the facade): a `receive {inbox}` span named for the reply inbox, parented under the responder's trace and linked back to the request when the reply carries a trace context (i.e. the responder replied via `conn.Respond`). When the reply carries no trace context (untraced responder, or one that used raw `msg.Respond`) the span is still recorded, with no link. Note the topology is two linked traces, not one: the request "send" span lives in the requester's trace and the reply "receive" span in the responder's — follow the span link in Tempo to cross between them. The pre-v0.6.0 variadic `attrs` parameter was removed (upstream offers no caller-attribute hook on that span); attach domain identifiers to your own ambient span instead. For a request that must carry headers, use `conn.RequestMsg(ctx, msg, timeout)`, the ctx-first shadow of the embedded ctx-less `RequestMsg`.

---

## MongoDB Usage

All MongoDB clients must go through `github.com/flywindy/o11y/mongo` so that
the SDK's `TracerProvider` and `MeterProvider` are wired into official
`otelmongo` instrumentation and SDK-owned pool metrics without reading global
OpenTelemetry state.
Command spans are always-on and sampler-governed; there are no Mongo-specific
env gates.

The MongoDB package must not write `_oteltrace` into persisted business
documents. For asynchronous workflows, propagate trace context through an
outbox or event envelope instead of mutating MongoDB documents.

```go
client, err := o11ymongo.Connect(
    ctx,
    mongoURI,
    sdk.TracerProvider(),
    sdk.MeterProvider(),
    sdk.Propagator,
    o11ymongo.WithPoolName("app-mongo"),
)
```

Applications that build their own `*options.ClientOptions` must call
`o11ymongo.Instrument(...)` before the MongoDB driver's `mongo.Connect(...)`
and defer the returned cleanup function near `client.Disconnect` so SDK-owned
pool event handling stops after the final metrics flush.

---

## Redis / Valkey Usage

All Redis and Valkey clients must use `github.com/flywindy/o11y/redis`
so spans and metrics are emitted with SDK-owned semconv v1.39.0 attributes.
The SDK wraps an existing `github.com/redis/go-redis/v9` client and does not
call `redisotel`.

```go
rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
client, err := o11yredis.Wrap(
    rdb,
    sdk.TracerProvider(),
    sdk.MeterProvider(),
    o11yredis.WithPoolName("cache"),
)
```

Redis command text is disabled by default. Only enable
`o11yredis.WithCommandTextEnabled(true)` when the application explicitly
accepts the key and value exposure risk.

The wrapper always skips Pub/Sub and connection-lifecycle commands (`AUTH`,
`HELLO`, `SELECT`, `READONLY`, auto-issued `CLIENT SETINFO` / `SETNAME`). To trim
further noise such as health-check PINGs, use `o11yredis.WithIgnoredCommands("ping")`
(exact, drops every invocation by verb) or `o11yredis.WithRequireParentSpan(true)`
(drops commands issued without an active parent span; off by default).

---

## Do NOT

- ❌ Add `init()` functions with side effects in any package
- ❌ Use `panic` for error handling — use `slog.ErrorContext` instead
- ❌ Use a global `*slog.Logger` variable — pass logger via context or struct
- ❌ Thread a request-scoped context (`c.Request.Context()`, `r.Context()`, or `*gin.Context`) into a goroutine, `defer`, or post-response write that outlives the request — it is canceled when the handler returns and aborts the work (`context canceled` / DB `connection reset by peer`). Use `obsctx.Detach` / `obsctx.DetachWithTimeout` / `obsctx.Go` (keeps the trace, drops cancelation) and `c.Copy()` for the gin side. See [ADR 0024](docs/adr/0024-context-lifecycle-for-background-work.md)
- ❌ Call `otel.SetTracerProvider` or `otel.SetTextMapPropagator` anywhere in SDK code — the SDK must not mutate OTel globals. Application `main()` may still choose to do so. See [ADR 0003](docs/adr/0003-global-state-policy.md)
- ❌ Use OTLP/gRPC unless explicitly asked
- ❌ Import `github.com/sirupsen/logrus` or `go.uber.org/zap` — we use stdlib `slog`
- ❌ Commit without running `go fmt` and `go mod tidy`
- ❌ Add Kubernetes manifests that send traces or logs directly to backends (Tempo, Loki) — traces and logs must go through the OTel Collector; Prometheus scraping `:2112` directly is intentional and correct
- ❌ Call `otelnats.Connect` or `otelnats.ConnectWithOptions` directly — always go through `o11ynats.Connect` or `o11ynats.ConnectWithOptions` so the SDK providers are wired correctly
- ❌ Import `go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo` directly from services — always go through `o11ymongo.Connect` or `o11ymongo.Instrument` so SDK providers and monitor composition are wired consistently
- ❌ Import `go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin` directly from services — always go through `o11ygin.Middleware` so SDK providers, propagator, and typed gin error events are wired consistently
- ❌ Import `github.com/redis/go-redis/extra/redisotel/v9` directly from services — always go through `o11yredis.Wrap` so SDK-owned semconv v1.39.0 attributes, metrics, and sensitive defaults stay consistent
- ❌ Write MongoDB `_oteltrace` into persisted business documents through this SDK — use outbox/event-envelope propagation for asynchronous workflows instead
- ❌ Use `msg.Respond(data)` inside a Subscribe handler when trace context must be preserved in the reply — use `conn.Respond(ctx, msg, data)` instead
- ❌ Use `WithTeam` — it no longer exists; use `WithServiceNamespace` instead
- ❌ Use non-canonical environment strings in config files or docs (code accepts aliases like `"prod"` but canonical values are preferred)
- ❌ Mix SDK-owned OTel semconv imports — always import `go.opentelemetry.io/otel/semconv/v1.39.0` in this repository's own code
- ❌ Use high-cardinality values (user IDs, request IDs, trace IDs) as metric label values
