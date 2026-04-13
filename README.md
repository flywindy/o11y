# o11y Golang SDK

A lightweight Go SDK for standardized observability, integrating OpenTelemetry (OTel) tracing with structured logging (`slog`) for automatic trace correlation.

## Architecture & Tech Stack

This project provides a "Context-First" observability layer for Go applications, ensuring that every log entry is automatically enriched with `trace_id` and `span_id`.

- **Language**: Go 1.25+
- **Tracing**: OpenTelemetry Go SDK (OTLP/HTTP)
- **Logging**: Go `slog` with a custom OTel correlation handler
- **Infrastructure**:
  - **NATS**: High-performance messaging
  - **MongoDB**: NoSQL database for persistence
  - **Tempo**: Distributed tracing backend
  - **Loki**: Log aggregation system
  - **Grafana**: Unified visualization for traces, logs, and metrics
  - **OTel Collector**: Centralized pipeline — all telemetry (traces and logs) flows through it
  - **Alloy**: Log collection agent (DaemonSet), forwards logs to OTel Collector via OTLP

### Telemetry Flow

```
Traces: App ──OTLP/HTTP──► OTel Collector ──► Tempo
Logs:   App stdout ──► Alloy ──OTLP/HTTP──► OTel Collector ──► Loki
```

## Prerequisites

Before running the infrastructure, ensure you have the following installed:

- [Docker](https://www.docker.com/get-started)
- [Go 1.25+](https://go.dev/doc/install)
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/) (Kubernetes in Docker)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)

## Getting Started with `kind`

### 1. Create the Cluster

```bash
kind create cluster --config kind-config.yaml
```

This configures a control-plane node with an extra port mapping for the OTel Collector (port `4318`).

### 2. Deploy Infrastructure

Apply the infrastructure components using Kustomize:

```bash
# Standard deployment (public images)
kubectl apply -k k8s/infrastructure/base

# OR: Private registry deployment (replace with your registry host)
# Note: Update internal-registry.example.com in the overlay's kustomization.yaml to your host
kubectl apply -k k8s/infrastructure/overlays/private-registry
```

Wait for all pods to reach the `Running` state.

### 3. Access Grafana

```bash
kubectl port-forward svc/grafana 3000:3000 -n infra
```

Open `http://localhost:3000` (default credentials: `admin` / `admin`).

## Using the SDK

### Initialization

`Init` accepts functional options and returns an `*SDK` instance. No global OTel state is mutated.

```go
import (
    "context"
    "log/slog"
    "time"

    "github.com/flywindy/o11y"
)

func main() {
    ctx := context.Background()

    obs, err := o11y.Init(ctx,
        o11y.WithServiceName("my-service"),        // required
        o11y.WithServiceVersion("1.0.0"),
        o11y.WithEnvironment("production"),
        o11y.WithOTLPEndpoint("http://localhost:4318"),
        o11y.WithLogLevel(slog.LevelInfo),
    )
    if err != nil {
        slog.ErrorContext(ctx, "failed to initialize o11y SDK", slog.Any("error", err))
        return
    }

    // Flush in-flight spans on exit (always use a timeout).
    defer func() {
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        if err := obs.Shutdown(shutdownCtx); err != nil {
            obs.Logger.ErrorContext(shutdownCtx, "SDK shutdown error", slog.Any("error", err))
        }
    }()
}
```

**Available options:**

| Option | Default | Description |
|--------|---------|-------------|
| `WithServiceName(name)` | — (required) | OTel `service.name` resource attribute |
| `WithServiceVersion(ver)` | `""` | OTel `service.version` resource attribute |
| `WithEnvironment(env)` | `""` | OTel `deployment.environment` resource attribute |
| `WithOTLPEndpoint(url)` | `http://localhost:4318` | OTLP/HTTP collector endpoint |
| `WithLogLevel(level)` | `slog.LevelInfo` | Minimum log level |

### Structured Logging with Trace Correlation

Use `obs.Logger` instead of the global `slog` package. When a span is active in `ctx`, every log record automatically includes `trace_id` and `span_id` as JSON fields.

```go
// Without a span — no trace fields injected
obs.Logger.Info("service started")

// With an active span — trace_id and span_id are injected automatically
ctx, span := obs.Tracer("my-tracer").Start(ctx, "my-operation")
defer span.End()

obs.Logger.InfoContext(ctx, "processing request", slog.String("user_id", "42"))
// Output: {"time":"...","level":"INFO","msg":"processing request","trace_id":"4bf92f...","span_id":"00f067...","user_id":"42"}
```

### Creating Spans

Use `obs.Tracer(name)` to obtain a named tracer. No global OTel tracer provider is required.

```go
tracer := obs.Tracer("my-service")

ctx, span := tracer.Start(ctx, "parent-operation")
defer span.End()

// Child span — inherits the trace from ctx
ctx, child := tracer.Start(ctx, "child-operation")
defer child.End()

obs.Logger.InfoContext(ctx, "child work done")
```

If you need to wire the SDK's provider into the global OTel state (e.g. for third-party libraries that call `otel.Tracer()`):

```go
import "go.opentelemetry.io/otel"

otel.SetTracerProvider(obs.TracerProvider())
otel.SetTextMapPropagator(obs.Propagator)
```

### Distributed Tracing over NATS Core

Use `o11ynats.Connect` to obtain a tracing-aware connection. The `TracerProvider` and `Propagator` from the SDK are wired in automatically — no global OTel state is touched.

```go
import (
    o11ynats "github.com/flywindy/o11y/nats"
    "github.com/nats-io/nats.go"
)

conn, err := o11ynats.Connect(ctx, nats.DefaultURL, obs.TracerProvider(), obs.Propagator)

// Publisher: trace context is injected into message headers automatically.
conn.Publish(ctx, "orders.created", payload)

// Subscriber: ctx in the handler already carries the publisher's trace.
conn.Subscribe("orders.created", func(ctx context.Context, msg *nats.Msg) {
    ctx, span := obs.Tracer("consumer").Start(ctx, "process-order")
    defer span.End()
    obs.Logger.InfoContext(ctx, "order received") // trace_id and span_id injected automatically
})
```

> **Request-Reply note**: to reply while preserving trace context, use `conn.Publish(ctx, msg.Reply, data)` instead of `msg.Respond(data)`. `msg.Respond` bypasses header injection and breaks the distributed trace.

### Distributed Tracing over JetStream

```go
import "github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream"

js, err := conn.JetStream()

// Create or update stream (idempotent — safe to call on every startup).
js.CreateOrUpdateStream(ctx, oteljetstream.StreamConfig{
    Name: "ORDERS", Subjects: []string{"orders.>"},
})

// Publisher: trace context injected into JetStream message headers.
ack, err := js.Publish(ctx, "orders.created", payload)

// Subscriber: durable consumer with Consume (push-style pull delivery).
stream, _ := js.Stream(ctx, "ORDERS")
consumer, _ := stream.CreateOrUpdateConsumer(ctx, oteljetstream.ConsumerConfig{
    Durable: "orders-processor", AckPolicy: oteljetstream.AckExplicitPolicy,
})
cc, _ := consumer.Consume(func(m oteljetstream.Msg) {
    ctx, span := obs.Tracer("consumer").Start(m.Context(), "process-order")
    defer span.End()
    m.Ack()
})
defer cc.Stop()
```

## Running the Examples

Before running any example, port-forward the required services from the `kind` cluster:

```bash
kubectl port-forward -n infra svc/otel-collector 4318:4318  # OTel traces
kubectl port-forward -n infra svc/nats          4222:4222   # NATS connection
kubectl port-forward -n infra svc/grafana       3000:3000   # Grafana UI
```

### Basic (spans + logs)

```bash
go run examples/basic/main.go
```

### NATS Core (two terminals)

```bash
# Terminal 1 — start subscriber first
go run examples/nats-core/subscriber/main.go

# Terminal 2 — publisher sends a message every 3 seconds
go run examples/nats-core/publisher/main.go
```

### JetStream (two terminals; requires JetStream-enabled NATS server)

```bash
# Terminal 1 — publisher creates the stream and publishes
go run examples/jetstream/publisher/main.go

# Terminal 2 — subscriber attaches a durable consumer and processes messages
go run examples/jetstream/subscriber/main.go
```

Open Grafana at `http://localhost:3000` and navigate to **Explore → Tempo** to see producer and consumer spans linked across services. Navigate to **Explore → Loki** to see structured log entries with correlated `trace_id` and `span_id` fields.

## Core Principles

1. **Context-First**: Always propagate `context.Context` — trace information flows through context only.
2. **Zero Global State**: No `init()` side effects, no global logger or tracer provider variables.
3. **Correlation**: `slog` output always includes `trace_id` and `span_id` as JSON fields when a span is active.
4. **Performance**: Non-blocking middleware and minimal allocations in the hot path.
5. **Errors**: Use `slog.ErrorContext(ctx, ...)` with structured attributes; never `panic` for recoverable errors.

## AI Collaboration

This project uses `AGENTS.md` to store AI-assisted development context and project-specific rules. `CLAUDE.md` and `GEMINI.md` are symlinks pointing to that file. If using an AI assistant, refer to `AGENTS.md` for project patterns.
