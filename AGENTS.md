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
| Messaging | NATS | High-performance pub/sub |
| Database | MongoDB | NoSQL persistence |
| Tracing backend | Grafana Tempo | |
| Log backend | Grafana Loki | |
| Metrics backend | Prometheus | Scrapes k8s pods on `:2112`; also accepts remote write from OTel Collector |
| Visualization | Grafana | Unified traces, logs, and metrics; exemplars link histograms → Tempo traces |
| Collector | OTel Collector | Centralized telemetry pipeline for traces, logs, and OTLP metrics |
| Local cluster | kind (Kubernetes in Docker) | Port 4318 mapped for OTLP/HTTP (traces, logs, and metrics push) |

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
5. **Semconv v1.27.0**: All instrument names, attribute keys, and attribute types must conform to OTel Semantic Conventions v1.27.0. Do not mix versions. *(See [`docs/semconv.md`](docs/semconv.md) for the complete catalog of attributes emitted by this SDK.)*

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

# Deploy observability stack via Kustomize (handles ordering and dependencies)
kubectl apply -k k8s/infrastructure/base
# OR: private registry deployment (update internal-registry.example.com first)
kubectl apply -k k8s/infrastructure/overlays/private-registry

# Verify all pods are Running
kubectl get pods -n infra

# Run the basic example (cluster must be up)
go run examples/basic/main.go

# Run the NATS Core examples (two terminals; cluster must be up with NATS running)
go run examples/nats-core/subscriber/main.go
go run examples/nats-core/publisher/main.go

# Run the JetStream examples (two terminals; NATS must have JetStream enabled)
# Start publisher first — it creates the JetStream stream; then start the subscriber
go run examples/jetstream/publisher/main.go   # creates the stream and publishes
go run examples/jetstream/subscriber/main.go  # attaches durable consumer and processes

# Run the metrics example (pushes via OTLP → OTel Collector → Prometheus; cluster must be up)
go run examples/metrics/main.go

# Port-forward Grafana (default credentials: admin/admin)
kubectl port-forward -n infra svc/grafana 3000:3000

# Port-forward Prometheus
kubectl port-forward -n infra svc/prometheus 9090:9090
```

---

## Kubernetes Infrastructure Verification

When modifying files under `k8s/infrastructure/**`, use the repo-local `verify-kubernetes-manifests` skill at `.agents/skills/verify-kubernetes-manifests`.

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
| NATS integration | `github.com/Marz32onE/instrumentation-go/otel-nats` — verified at v0.2.1 not to mutate globals; wrapped by the `nats/` package | Covers NATS Core + all JetStream consumer patterns with OTel semconv v1.27.0. See [ADR 0004](docs/adr/0004-nats-integration.md) |
| MongoDB integration | Native `event.CommandMonitor` on the official `go.mongodb.org/mongo-driver/v2`; `Marz32onE/instrumentation-go/otel-mongo` deliberately not used | Upstream emits attribute keys via hand-rolled string literals (post-v1.30 DB-stable rename, e.g. `db.system.name`) and does not import any `semconv/vX.Y.Z` Go package, breaking alignment with our v1.27.0 pin. Document injection is also coupled to command-span emission with no independent off-switch. ~150 LOC monitor preserves semconv consistency. See [ADR 0005](docs/adr/0005-mongodb-integration.md) |
| Semconv version policy | Pin v1.27.0; upgrade only when concrete triggers fire | Single pin per process avoids cognitive cost and dashboard breakage. Upgrade triggers and process documented to keep version moves deliberate. See [ADR 0006](docs/adr/0006-semconv-upgrade-strategy.md) |

---

## NATS & JetStream Usage

All NATS connections must go through `github.com/flywindy/o11y/nats` so that the SDK's `TracerProvider` and `Propagator` are wired in without touching global OTel state.

### NATS Core

```go
conn, err := o11ynats.Connect(ctx, natsURL, sdk.TracerProvider(), sdk.Propagator)

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
- ❌ Call `otel.SetTracerProvider` or `otel.SetTextMapPropagator` anywhere in SDK code — the SDK must not mutate OTel globals. Application `main()` may still choose to do so. See [ADR 0003](docs/adr/0003-global-state-policy.md)
- ❌ Use OTLP/gRPC unless explicitly asked
- ❌ Import `github.com/sirupsen/logrus` or `go.uber.org/zap` — we use stdlib `slog`
- ❌ Commit without running `go fmt` and `go mod tidy`
- ❌ Add Kubernetes manifests that send traces or logs directly to backends (Tempo, Loki) — traces and logs must go through the OTel Collector; Prometheus scraping `:2112` directly is intentional and correct
- ❌ Call `otelnats.Connect` or `otelnats.ConnectWithOptions` directly — always go through `o11ynats.Connect` so the SDK providers are wired correctly
- ❌ Import `github.com/Marz32onE/instrumentation-go/otel-mongo` (any submodule) — its hand-rolled attribute keys (post-v1.30 DB-stable rename, e.g. `db.system.name`) drift from our pinned semconv v1.27.0, and its `_oteltrace` document injection cannot be disabled independently of command spans. MongoDB instrumentation uses the official driver's `event.CommandMonitor` via the forthcoming `mongo/` package. See [ADR 0005](docs/adr/0005-mongodb-integration.md). (Note: v0.2.10 fixed the original global-state issue at our request — global state is no longer the blocker, but the two issues above remain.)
- ❌ Use `msg.Respond(data)` inside a Subscribe handler when trace context must be preserved in the reply — use `conn.Publish(ctx, msg.Reply, data)` instead
- ❌ Use `WithTeam` — it no longer exists; use `WithServiceNamespace` instead
- ❌ Use non-canonical environment strings in config files or docs (code accepts aliases like `"prod"` but canonical values are preferred)
- ❌ Mix OTel semconv versions — always import `go.opentelemetry.io/otel/semconv/v1.27.0`
- ❌ Use high-cardinality values (user IDs, request IDs, trace IDs) as metric label values
