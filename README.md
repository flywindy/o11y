# o11y Golang SDK

A lightweight Go SDK for standardized observability, integrating OpenTelemetry (OTel) tracing with structured logging (`slog`) for automatic trace correlation.

## Architecture & Tech Stack

This project provides a "Context-First" observability layer for Go applications, ensuring that every log entry is automatically enriched with `trace_id` and `span_id`.

- **Language**: Go 1.23+
- **Tracing**: OpenTelemetry Go SDK (OTLP/HTTP)
- **Logging**: Go `slog` with a custom OTel correlation handler.
- **Infrastructure**:
  - **NATS**: High-performance messaging.
  - **MongoDB**: NoSQL database for persistence.
  - **Tempo**: Distributed tracing backend.
  - **Loki**: Log aggregation system.
  - **Grafana**: Unified visualization for traces, logs, and metrics.
  - **OTel Collector**: Centralized pipeline for receiving and exporting telemetry.

## Prerequisites

Before running the infrastructure, ensure you have the following installed:

- [Docker](https://www.docker.com/get-started)
- [Go 1.23+](https://go.dev/doc/install)
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/) (Kubernetes in Docker)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)

## Getting Started with `kind`

We use `kind` to spin up a local Kubernetes cluster for the observability stack.

### 1. Create the Cluster

```bash
kind create cluster --config kind-config.yaml
```

This configures a control-plane node with an extra port mapping for the OTel Collector (Port `4318`).

### 2. Deploy Infrastructure

Apply the Kubernetes manifests in the following order:

```bash
# Infrastructure components
kubectl apply -f k8s/infrastructure/nats.yaml
kubectl apply -f k8s/infrastructure/mongodb.yaml
kubectl apply -f k8s/infrastructure/tempo.yaml
kubectl apply -f k8s/infrastructure/otel-collector.yaml
kubectl apply -f k8s/infrastructure/grafana.yaml
```

### 3. Verify Deployment

```bash
kubectl get pods
```

Wait for all pods to be in the `Running` state.

## Using the SDK

The SDK simplifies the initialization of OpenTelemetry and correlates it with `slog`.

### Initialization

```go
import (
    "context"
    "github.com/flywindy/o11y/pkg/o11y"
)

func main() {
    ctx := context.Background()
    cfg := o11y.Config{
        ServiceName:  "my-service",
        Environment:  "production",
        OTLPEndpoint: "http://localhost:4318",
    }

    shutdown, err := o11y.Init(ctx, cfg)
    if err != nil {
        panic(err)
    }
    defer shutdown(ctx)
}
```

### Trace-Aware Logging

Once initialized, use `slog.InfoContext(ctx, ...)` to automatically include trace information in your logs:

```go
slog.InfoContext(ctx, "processing request", slog.String("user_id", "123"))
```

## Running the Example

A basic example is provided in `examples/basic/main.go`. To run it:

1. Ensure the `kind` cluster and infrastructure are running.
2. Run the example:
   ```bash
   go run examples/basic/main.go
   ```
3. Access **Grafana** (usually at `http://localhost:3000` via port-forwarding or NodePort) to view the traces and logs.

## Core Principles

1. **Context-First**: Always propagate `context.Context`.
2. **Zero Global State**: No package-level `init()` side effects.
3. **Correlation**: `slog` output includes `trace_id` and `span_id` automatically.
4. **Performance**: Non-blocking middleware and minimal overhead.

## 🤖 AI Collaboration
This project uses `GEMINI.md` to store AI-assisted development context and specific infrastructure rules. If using an AI assistant, please refer to that file for project-specific patterns.