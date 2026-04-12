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

# OR: Private registry deployment (internal-registry.example.com)
kubectl apply -k k8s/infrastructure/overlays/private-registry
```

Wait for all pods to reach the `Running` state.

### 4. Access Grafana

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

### Distributed Tracing over NATS

Use `obs.Propagator` together with the `nats` sub-package to propagate trace context across NATS messages.

```go
import (
    o11ynats "github.com/flywindy/o11y/nats"
    gonats "github.com/nats-io/nats.go"
)

// Publisher: inject the current span context into the message
msg := &gonats.Msg{Subject: "orders.created"}
o11ynats.Inject(ctx, obs.Propagator, msg)
nc.PublishMsg(msg)

// Subscriber: extract the span context from the incoming message
func handler(msg *gonats.Msg) {
    ctx := o11ynats.Extract(context.Background(), obs.Propagator, msg)
    // ctx now carries the upstream trace — create a child span to continue it
    ctx, span := obs.Tracer("consumer").Start(ctx, "orders.created")
    defer span.End()
}
```

## Running the Example

A complete example is provided in `examples/basic/main.go`. It demonstrates initialization, span creation, child spans, and structured logging.

1. Ensure the `kind` cluster and infrastructure are running (see above).
2. Run the example:
   ```bash
   go run examples/basic/main.go
   ```
3. Open Grafana at `http://localhost:3000` to see correlated traces and logs.

## Core Principles

1. **Context-First**: Always propagate `context.Context` — trace information flows through context only.
2. **Zero Global State**: No `init()` side effects, no global logger or tracer provider variables.
3. **Correlation**: `slog` output always includes `trace_id` and `span_id` as JSON fields when a span is active.
4. **Performance**: Non-blocking middleware and minimal allocations in the hot path.
5. **Errors**: Use `slog.ErrorContext(ctx, ...)` with structured attributes; never `panic` for recoverable errors.

## AI Collaboration

This project uses `AGENTS.md` to store AI-assisted development context and project-specific rules. `CLAUDE.md` and `GEMINI.md` are symlinks pointing to that file. If using an AI assistant, refer to `AGENTS.md` for project patterns.
