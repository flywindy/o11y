# AGENTS.md — o11y Golang SDK

> This is the canonical AI context file for the o11y project.
> `CLAUDE.md` and `GEMINI.md` are symlinks pointing to this file.

---

## Project Overview

A lightweight Go SDK providing standardized observability for Go services.
It integrates OpenTelemetry (OTel) tracing with structured logging (`slog`)
so that every log entry is automatically enriched with `trace_id` and `span_id`.

**Module path**: `github.com/flywindy/o11y`

---

## Tech Stack

| Layer | Choice | Notes |
|---|---|---|
| Language | Go 1.23+ | Use standard library where possible |
| Tracing | OpenTelemetry Go SDK (OTLP/HTTP) | Not gRPC — keep it simple for local dev |
| Logging | `log/slog` | Custom handler injects trace_id / span_id |
| Messaging | NATS | High-performance pub/sub |
| Database | MongoDB | NoSQL persistence |
| Tracing backend | Grafana Tempo | |
| Log backend | Grafana Loki | |
| Visualization | Grafana | Unified traces + logs dashboard |
| Collector | OTel Collector | Centralized telemetry pipeline |
| Local cluster | kind (Kubernetes in Docker) | Port 4318 mapped for OTLP/HTTP |

---

## Core Principles — Never Violate These

1. **Context-First**: Every function must accept and propagate `context.Context`. Trace information flows through context only.
2. **Zero Global State**: Encapsulate OTel providers in structs. No package-level `init()` with side effects. No global logger variables.
3. **Correlation**: `slog` output must always include `trace_id` and `span_id` as JSON fields when a span is active.
4. **Performance**: Middleware and handlers must be non-blocking. Minimize allocations in the hot path.
5. **Errors**: Use `slog.ErrorContext(ctx, ...)` with structured attributes. Never use `panic` for recoverable errors.

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

# Deploy observability stack (order matters — namespace must come first)
kubectl apply -f k8s/infrastructure/namespace.yaml
kubectl apply -f k8s/infrastructure/nats.yaml
kubectl apply -f k8s/infrastructure/mongodb.yaml
kubectl apply -f k8s/infrastructure/tempo.yaml
kubectl apply -f k8s/infrastructure/loki.yaml
kubectl apply -f k8s/infrastructure/alloy.yaml
kubectl apply -f k8s/infrastructure/otel-collector.yaml
kubectl apply -f k8s/infrastructure/grafana.yaml

# Verify all pods are Running
kubectl get pods -n infra

# Run the basic example (cluster must be up)
go run examples/basic/main.go

# Run the NATS Core examples (two terminals; cluster must be up with NATS running)
go run examples/nats-core/subscriber/main.go
go run examples/nats-core/publisher/main.go

# Run the JetStream examples (two terminals; NATS must have JetStream enabled)
# Start subscriber first — it expects the stream to already exist via the publisher
go run examples/jetstream/publisher/main.go   # creates the stream and publishes
go run examples/jetstream/subscriber/main.go  # attaches durable consumer and processes

# Port-forward Grafana (default credentials: admin/admin)
kubectl port-forward -n infra svc/grafana 3000:3000
```

---

## Code Standards

- All code, comments, and documentation must be in **English**
- Every exported symbol must have a **godoc comment**
- Use **named return values** only when they aid clarity
- Prefer `errors.New` / `fmt.Errorf` with `%w` for wrapping
- JSON log output is the default format (structured, machine-parseable)
- Do not introduce new external dependencies without discussion
- Update `README.md` whenever public-facing API, usage patterns, or examples change — README is the first point of contact for SDK users and must stay in sync with actual code

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

| Decision | Choice | Reason |
|---|---|---|
| Transport | OTLP/HTTP (not gRPC) | Simpler firewall / proxy rules for local dev |
| Logger | `log/slog` (stdlib) | No external dep; native structured logging since Go 1.21 |
| Tracing backend | Tempo | OSS, Grafana-native, cost-effective |
| Log backend | Loki | OSS, integrates with Grafana and Tempo for trace-to-log correlation |
| Local infra | kind | Reproducible Kubernetes without cloud cost |
| NATS instrumentation | `instrumentation-go/otel-nats` | Company-internal library; covers NATS Core + all JetStream consumer patterns with OTel semconv v1.27.0 |

---

## NATS & JetStream Usage

All NATS connections must go through `github.com/flywindy/o11y/nats` so that the SDK's `TracerProvider` and `Propagator` are wired in without touching global OTel state.

### NATS Core

```go
conn, err := o11ynats.Connect(natsURL, sdk.TracerProvider(), sdk.Propagator)

// Publish — trace context is injected into message headers automatically.
conn.Publish(ctx, "o11y.events", payload)

// Subscribe — ctx in the handler already carries the publisher's trace.
conn.Subscribe("o11y.events", func(ctx context.Context, msg *nats.Msg) {
    _, span := tracer.Start(ctx, "process-event")
    defer span.End()
    slog.InfoContext(ctx, "received", slog.String("payload", string(msg.Data)))
})
```

### JetStream

```go
js, err := conn.JetStream()

// Idempotent stream creation — safe to call on every startup.
js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
    Name: "EVENTS", Subjects: []string{"events.>"},
})

// Publish — trace context injected into JetStream message headers.
js.Publish(ctx, "events.created", payload)

// Durable pull consumer with Consume (push-style delivery).
stream, _ := js.Stream(ctx, "EVENTS")
consumer, _ := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
    Durable: "events-processor", AckPolicy: oteljetstream.AckExplicitPolicy,
})
cc, _ := consumer.Consume(func(m oteljetstream.Msg) {
    ctx, span := tracer.Start(m.Context(), "process-event")
    defer span.End()
    m.Ack()
})
defer cc.Stop()
```

### Request-Reply note

When replying to a message inside a `Subscribe` handler, do **not** use `msg.Respond(data)` if you need the reply to carry trace context. `msg.Respond` routes through the raw NATS connection and skips header injection. Use `conn.Publish(ctx, msg.Reply, data)` instead.

---

## Do NOT

- ❌ Add `init()` functions with side effects in any package
- ❌ Use `panic` for error handling — use `slog.ErrorContext` instead
- ❌ Use a global `*slog.Logger` variable — pass logger via context or struct
- ❌ Use OTLP/gRPC unless explicitly asked
- ❌ Import `github.com/sirupsen/logrus` or `go.uber.org/zap` — we use stdlib `slog`
- ❌ Commit without running `go fmt` and `go mod tidy`
- ❌ Add Kubernetes manifests that skip the OTel Collector (all telemetry must go through it)
- ❌ Call `otelnats.Connect` or `otelnats.ConnectWithOptions` directly — always go through `o11ynats.Connect` so the SDK providers are wired correctly
- ❌ Use `msg.Respond(data)` inside a Subscribe handler when trace context must be preserved in the reply — use `conn.Publish(ctx, msg.Reply, data)` instead