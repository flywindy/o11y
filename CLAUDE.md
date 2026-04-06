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

# Deploy observability stack (order matters)
kubectl apply -f k8s/infrastructure/nats.yaml
kubectl apply -f k8s/infrastructure/mongodb.yaml
kubectl apply -f k8s/infrastructure/tempo.yaml
kubectl apply -f k8s/infrastructure/loki.yaml
kubectl apply -f k8s/infrastructure/otel-collector.yaml
kubectl apply -f k8s/infrastructure/grafana.yaml

# Verify all pods are Running
kubectl get pods

# Run the basic example (cluster must be up)
go run examples/basic/main.go

# Port-forward Grafana (default credentials: admin/admin)
kubectl port-forward svc/grafana 3000:3000
```

---

## Code Standards

- All code, comments, and documentation must be in **English**
- Every exported symbol must have a **godoc comment**
- Use **named return values** only when they aid clarity
- Prefer `errors.New` / `fmt.Errorf` with `%w` for wrapping
- JSON log output is the default format (structured, machine-parseable)
- Do not introduce new external dependencies without discussion

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

---

## Do NOT

- ❌ Add `init()` functions with side effects in any package
- ❌ Use `panic` for error handling — use `slog.ErrorContext` instead
- ❌ Use a global `*slog.Logger` variable — pass logger via context or struct
- ❌ Use OTLP/gRPC unless explicitly asked
- ❌ Import `github.com/sirupsen/logrus` or `go.uber.org/zap` — we use stdlib `slog`
- ❌ Commit without running `go fmt` and `go mod tidy`
- ❌ Add Kubernetes manifests that skip the OTel Collector (all telemetry must go through it)