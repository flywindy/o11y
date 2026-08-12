# o11y Golang SDK

A lightweight Go SDK for standardized observability, integrating OpenTelemetry (OTel) tracing with structured logging (`slog`) for automatic trace correlation.

## Documentation

- **README** (this file) — project overview, infrastructure setup, SDK initialization, and the full `Init` options reference.
- **[Developer Guide](docs/guide.md)** — the four pillars (Tracing, Logging, Metrics, Profiling) in depth, plus per-integration sub-packages grouped by semantic-convention domain (HTTP, Databases, Messaging, Object Storage).
- **[Examples](examples/README.md)** — runnable programs for each pillar and integration.
- **[Architecture Decision Records](docs/adr/)** — the "why" behind key design choices.
- **[Semantic Conventions](docs/semconv.md)** — pinned OTel semconv reference.

## Architecture & Tech Stack

> Architecture Decision Records (ADRs) explaining key design choices are in [`docs/adr/`](docs/adr/).

This project provides a "Context-First" observability layer for Go applications, ensuring that every log entry is automatically enriched with `traceId` and `spanId`.

- **Language**: Go 1.25+
- **Tracing**: OpenTelemetry Go SDK (OTLP/HTTP)
- **Logging**: Go `slog` with dual output — OTLP/HTTP via `otelslog` bridge (→ Loki) and JSON stdout (→ Alloy)
- **Metrics**: Prometheus pull (default `:2112`) or OTLP push (`WithMetricsOTLPEndpoint`)
- **Profiling**: Opt-in continuous profiling via Pyroscope (`WithProfilingEndpoint`)
- **Infrastructure**:
  - **NATS**: High-performance messaging
  - **MongoDB**: NoSQL database for persistence
  - **Redis / Valkey**: Cache and Redis-protocol data stores
  - **Cassandra**: Wide-column store via `gocql` (SDK-owned observers, [ADR 0019](docs/adr/0019-cassandra-integration.md))
  - **Tempo**: Distributed tracing backend
  - **Loki**: Log aggregation system
  - **Pyroscope**: Continuous profiling backend
  - **Prometheus**: Metrics storage and scraping
  - **Grafana**: Unified visualization for traces, logs, metrics, and profiles
  - **OTel Collector**: Centralized pipeline — all telemetry (traces and logs) flows through it
  - **Alloy**: Log collection agent and Pyroscope ingest proxy

Profiles are the one signal that bypasses the OTel Collector: applications
push Pyroscope-format profiles to Alloy, which forwards them to Pyroscope.

### Telemetry Flow

```
Traces:    App ──OTLP/HTTP──► OTel Collector ──► Tempo
Logs:      App ──OTLP/HTTP──► OTel Collector ──► Loki   (primary: full OTel Log Data Model)
           App stdout ──► Alloy ──OTLP/HTTP──► OTel Collector ──► Loki  (secondary: k8s pods via Alloy)
Metrics:   App :2112/metrics ◄──scrape── Prometheus ──► Grafana  (pull model)
Profiles:  App ──Pyroscope ingest──► Alloy ──► Pyroscope ──► Grafana
```

Both log paths are active simultaneously. When running `go run` locally (outside the cluster),
only the OTLP path reaches Loki; Alloy scrapes pods exclusively inside kind.
Profiles flow through Alloy's Pyroscope receiver to the Pyroscope backend; they
do not go through the OTel Collector because Pyroscope ingest is not OTLP.
Prometheus scraping also only works inside the cluster; locally, scrape `:2112/metrics` directly.

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
# Standard deployment (public images) — monitor stack only
kubectl apply -k k8s/infrastructure/base/monitor
# Add datastores as needed for the examples you want to run, e.g.:
# kubectl apply -k k8s/infrastructure/base/components/nats

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

This section covers initialization, the full options reference, and feature
toggles. For the four pillars in depth (Tracing, Logging, Metrics, Profiling)
and the per-integration sub-packages (HTTP, Databases, Messaging, Object
Storage), see the **[Developer Guide](docs/guide.md)**.

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
        o11y.WithServiceVersion("1.0.0"),          // required
        o11y.WithEnvironment("production"),        // required; see canonical values below
        o11y.WithServiceNamespace("platform"),     // required; maps to k8s namespace / team
        o11y.WithOTLPEndpoint("http://localhost:4318"),
        // Optional: reduce head sampling on high-throughput producers.
        // o11y.WithSamplingRatio(0.001),
        // Optional: enable continuous profiling. In-cluster, prefer
        // "http://alloy.infra.svc.cluster.local:4040". Profiling requires
        // both the endpoint and WithProfilingEnabled(true).
        // o11y.WithProfilingEndpoint("http://localhost:4040"),
        // o11y.WithProfilingEnabled(true),
        o11y.WithLogLevel(slog.LevelInfo),
    )
    if err != nil {
        slog.ErrorContext(ctx, "failed to initialize o11y SDK", slog.Any("error", err))
        return
    }

    // Flush in-flight spans and metrics on exit (always use a timeout).
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

_Required — Init returns an error if any of these are missing:_

| Option | Description |
|--------|-------------|
| `WithServiceName(name)` | OTel `service.name` resource attribute |
| `WithServiceVersion(ver)` | OTel `service.version`; used for canary/rollback tracking |
| `WithEnvironment(env)` | OTel `deployment.environment.name`; accepted: `production`, `staging`, `development`, `testing` (aliases like `prod`/`stg` are normalized) |
| `WithServiceNamespace(ns)` | OTel `service.namespace`; identifies the owning team/product, maps to k8s namespace |

_OTLP (shared by traces and logs):_

| Option | Default | Description |
|--------|---------|-------------|
| `WithOTLPEndpoint(url)` | `http://localhost:4318` | OTLP/HTTP collector endpoint for traces and logs |
| `WithOTLPHeaders(map[string]string)` | `nil` | Headers attached to every OTLP/HTTP request (auth tokens, multi-tenant routing) |

_Tracing:_

| Option | Default | Description |
|--------|---------|-------------|
| `WithSamplingRatio(ratio)` | unset → OTel default/env | Configure SDK-side head sampling as `ParentBased(TraceIDRatioBased(ratio))`; `ratio` must be in `[0.0, 1.0]` |
| `WithTraceSampler(sampler)` | unset → OTel default/env | Escape hatch for a custom `sdktrace.Sampler`; a non-nil sampler overrides `OTEL_TRACES_SAMPLER` for this SDK instance |
| `WithBaggageAttributes(keys ...string)` | `nil` | Materialize up to 8 application-defined W3C baggage keys onto spans and SDK log records. Keys must not collide with semconv, Resource, SDK, or slog fields |
| `WithUserBaggage()` | off | Materialize the PII-bearing semconv `user.name` baggage member; use `ContextWithUser` to set it after authentication |

Application baggage is a two-sided opt-in: call
`ContextWithBaggageValue(ctx, key, value)` where a trusted value is known, and
configure `WithBaggageAttributes(key)` in every service that should surface it.
At public ingress, exclude baggage from extraction or clear all inbound baggage
before rebuilding authenticated values. Never use these keys as metric labels.
See [Application-Defined Baggage Attributes](docs/guide.md#application-defined-baggage-attributes).

_Metrics:_

| Option | Default | Description |
|--------|---------|-------------|
| `WithMetricsOTLPEndpoint(url)` | `""` | Switch metrics to OTLP push (serverless); when unset, Prometheus pull on `:2112` is used |
| `WithMetricsAddr(addr)` | `:2112` | Prometheus `/metrics` scrape address |
| `WithRuntimeMetrics(bool)` | `true` | Collect Go runtime metrics (goroutines, GC, memory) |
| `WithHistogramBuckets([]float64)` | SLO defaults | Override HTTP latency histogram boundaries; see `DefaultLatencyBuckets()` |
| `WithDisableDefaultViews()` | off | Disable SDK-managed HTTP metric label allowlists and bucket views |
| `WithMaxUniqueRoutes(n)` | `1000` | Cap exported distinct `http.route` values and derive the SDK aggregation cardinality budget |
| `WithMaxUniqueCollections(n)` | `200` | Cap exported distinct `db.collection.name` values on the Cassandra client metrics, collapsing the overflow to `"other"`. A Cassandra schema is DDL-fixed, so reaching this cap signals a statement shape the SDK's CQL tokenizer mis-read rather than schema growth. To drop the label entirely instead, pass `cassandra.WithCollectionMetricLabel(false)` to `cassandra.NewSession` |
| `WithExtraHTTPServerAttributeKeys(keys ...string)` | `nil` | Promote caller-controlled attribute keys (e.g. `app_name`, `bot_name`) onto the SDK-managed `http.server.request.duration` series. Pair with `o11ygin.WithMetricAttributesFn` / `otelhttp` equivalent to inject the values per request. Cardinality is the caller's responsibility — use enumerable, bounded keyspaces |
| `WithExemplars(bool)` | `true` | Enable OpenMetrics negotiation on `/metrics` so per-bucket exemplars carry `trace_id` / `span_id` to Grafana / Tempo. Set `false` only as a temporary mitigation when migrating dashboards that hardcode integer histogram boundaries (`le="1"` → `le="1.0"` under OpenMetrics) |

_Logging:_

| Option | Default | Description |
|--------|---------|-------------|
| `WithLogLevel(level)` | `slog.LevelInfo` | Minimum log level |

_Profiling (opt-in via endpoint):_

| Option | Default | Description |
|--------|---------|-------------|
| `WithProfilingEndpoint(url)` | `""` | Pyroscope-compatible ingest endpoint; empty means profiling never starts |
| `WithProfilingAuthHeaders(map[string]string)` | `nil` | Headers attached to every profile push (Grafana Cloud Profiles auth, `X-Scope-OrgID`, etc.) |

_Per-pillar feature toggles (progressive rollout):_

| Option | Default | Description |
|--------|---------|-------------|
| `WithTraceEnabled(bool)` | `true` | When `false`, use a no-op TracerProvider; W3C headers are still parsed and forwarded |
| `WithMetricsEnabled(bool)` | `true` | When `false`, use a no-op MeterProvider; no Prometheus server is started |
| `WithLogEnabled(bool)` | `true` | When `false`, write logs to stdout only; no OTLP log provider is started |
| `WithProfilingEnabled(bool)` | `false` | Opt-in. When `true` **and** `WithProfilingEndpoint` is set, the SDK starts the Pyroscope profiler and installs the trace-to-profile bridge |

> **Migration note (pre-1.0 API change)** — `DefaultLatencyBuckets` is now a
> function (`o11y.DefaultLatencyBuckets()` returning a fresh copy) rather
> than a package-level slice variable, so callers cannot accidentally mutate
> the package defaults. `DefaultMetricsAddr` is now a `const` (was `var`);
> `SDK.TracerProvider()` now returns `trace.TracerProvider`, and
> `SDK.MeterProvider()` now returns `metric.MeterProvider`. Use
> `WithMetricsAddr(":9090")` to override the metrics listener. See
> `CHANGELOG.md` for the migration recipe.

### Feature Toggles (Progressive Rollout)

Each observability pillar — Trace, Metrics, Log, Profiling — can be controlled independently so teams can adopt the new SDK incrementally without breaking existing dashboards or pipelines.

```go
obs, err := o11y.Init(ctx,
    // ... required options ...
    o11y.WithTraceEnabled(false),     // keep existing trace logic, skip new SDK
    o11y.WithMetricsEnabled(false),   // keep ginprom /metrics; no new Prometheus server
    o11y.WithLogEnabled(false),       // stdout only; no OTLP log export
    o11y.WithProfilingEndpoint("http://alloy.infra.svc.cluster.local:4040"),
    o11y.WithProfilingEnabled(true),  // opt-in: profiling defaults to off
)
```

**When disabled, each pillar is gracefully stubbed out:**

| Pillar | Disabled behaviour |
|--------|-------------------|
| **Trace** | No-op `TracerProvider`; W3C `traceparent`/`tracestate` headers are still parsed and forwarded to downstream services |
| **Metrics** | No-op `MeterProvider`; the Prometheus HTTP server is not started; existing `ginprom` dashboards are unaffected |
| **Log** | `obs.Logger` writes to stdout only (JSON); no OTLP collector connection is attempted |
| **Profiling** | No Pyroscope profiler is started; the trace-to-profile `pyroscope.profile.id` span attribute is not added. Trace/log/metric pillars are untouched |

`WithTraceEnabled(false)` returns a no-op `TracerProvider`; integration code
that is already installed may still execute a no-op wrapper path. NATS callers
can supply the SDK's resolved toggle as their connection-local tracing default:

```go
conn, err := o11ynats.ConnectWithOptions(
    ctx,
    natsURL,
    obs.TracerProvider(),
    obs.Propagator,
    o11ynats.WithTracingEnabled(obs.Toggles.Trace),
    o11ynats.WithNATSOptions(natsOpts...),
)
```

Since `otel-nats` v0.8.0, the NATS-specific precedence is **relay > upstream
environment > connection option > upstream default**. The option above selects
the direct/native path only when no feature-flag relay and no overriding
`OTEL_NATS_TRACING_ENABLED` value are present. A relay-capable process keeps the
instrumented path available and evaluates flags per operation even while the
effective flag is false. This SDK does not configure a relay; adopting one is a
separate application/deployment decision.

Above that whole ladder sits an upstream **master switch**,
`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED`, which is ANDed with the result. It
defaults to enabled, so leaving it unset changes nothing — but setting it to a
falsy value turns off NATS tracing (and with it header propagation and baggage
restoration) no matter what the option, the module variable or the relay say.
Both upstream variables are strict tri-state since v0.8.0: only `1`/`true`/`yes`/`on`
and `0`/`false`/`no`/`off` are accepted, and **any other value — including the
empty string an unexpanded `${VAR}` produces — fails the connection with an
error rather than being ignored.** Audit deployment configuration for these two
names before upgrading.

**Environment-variable control** (useful for staged rollouts without deploys):

| Env var | Default |
|---------|---------|
| `O11Y_TRACE_ENABLED` | `true` |
| `O11Y_METRICS_ENABLED` | `true` |
| `O11Y_LOG_ENABLED` | `true` |
| `O11Y_PROFILING_ENABLED` | `false` |

Accepted values: `1`/`t`/`true`/`TRUE` (truthy) and `0`/`f`/`false`/`FALSE` (falsy). Any other value (e.g. `"yes"`, `"on"`) emits a startup `WARN` log and falls back to the SDK default.

Precedence: **code option > env var > SDK default**.  
An explicit `WithTraceEnabled(true)` always wins over `O11Y_TRACE_ENABLED=false`.

Profiling is opt-in and **doubly gated**: it requires both `WithProfilingEnabled(true)` (or `O11Y_PROFILING_ENABLED=true`) **and** a non-empty `WithProfilingEndpoint`. Either condition alone is insufficient, so misconfiguration cannot accidentally start the profiler. The SDK emits a startup `WARN` when only one of the two is set so the misconfig is noticed. `sdk.Toggles.Profiling` reports whether the SDK actually started a profiler.

**Runtime introspection** — `sdk.Toggles` reports what is active:

```go
if !obs.Toggles.Metrics {
    obs.Logger.Warn("metrics pillar disabled; Prometheus server not started")
}
if !obs.Toggles.Profiling {
    obs.Logger.Warn("profiling inactive; toggle off or no Pyroscope endpoint")
}
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

For everything else — structured logging with trace correlation, user identity
attributes, trace sampling, continuous profiling, and the NATS / MongoDB /
Redis / Elasticsearch / HTTP / Resty / MinIO / gin sub-packages — see the
**[Developer Guide](docs/guide.md)**.

### Background work & context lifecycle

Work that outlives the request — background goroutines, post-response writes,
fire-and-forget — must **not** use the request context (`c.Request.Context()` or
`*gin.Context`). It is canceled the moment the handler returns, which aborts the
work mid-flight (`context canceled`, and for databases `connection reset by
peer`). The failure is timing-dependent, so it tends to pass locally and only
surface under production latency/concurrency.

Use [`obsctx`](obsctx) to keep the trace context but drop the cancelation:

```go
import "github.com/flywindy/o11y/obsctx"

// in a gin handler, after responding:
obsctx.Go(c.Request.Context(), 5*time.Second, func(ctx context.Context) {
    _ = repo.WriteAudit(ctx, event) // stays in the same trace; not canceled with the request
})
```

See [`examples/background`](examples/background) and
[ADR 0024](docs/adr/0024-context-lifecycle-for-background-work.md).

## Examples

Runnable programs live in [`examples/`](examples/), organized like the
[Developer Guide](docs/guide.md): the four pillars first (basic spans + logs,
metrics, profiling), then integrations grouped by semantic-convention domain
(HTTP: gin, Resty; Databases: MongoDB, Redis; Messaging: NATS Core, JetStream,
browser WebSocket; Object Storage: MinIO). See
**[`examples/README.md`](examples/README.md)** for the prerequisites
(port-forwards) and the `go run` command for each one.

## Core Principles

1. **Context-First**: Always propagate `context.Context` — trace information flows through context only.
2. **Zero Global State**: No `init()` side effects, no global logger or tracer provider variables. See [ADR 0003](docs/adr/0003-global-state-policy.md).
3. **Correlation**: Every log record includes `traceId` and `spanId` when a span is active — as JSON fields on stdout and as OTel Log Data Model fields in Loki. See [ADR 0001](docs/adr/0001-log-format-strategy.md).
4. **Errors**: Use `slog.ErrorContext(ctx, ...)` with structured attributes; never `panic` for recoverable errors.
5. **Semconv v1.39.0**: All instrument names, attribute keys, and types conform to OTel Semantic Conventions v1.39.0. See [`docs/semconv.md`](docs/semconv.md).

## Acknowledgements

- [`github.com/akira-core/instrumentation-go/otel-nats`](https://github.com/akira-core/instrumentation-go) — provides the underlying NATS Core + JetStream tracing semantics used by the `nats/` wrapper (module path renamed from `Marz32onE` to `akira-core` upstream in v0.6.0). Verified at v0.9.1 not to mutate global OTel providers/propagators and to import semconv v1.39.0. When explicitly configured through `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT`, v0.8.0 may install a named OpenFeature provider; o11y does not set that variable. See [ADR 0004](docs/adr/0004-nats-integration.md) for the integration decision and audit discipline.
- [`go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo) provides MongoDB command spans and `db.client.operation.duration` metrics through the `mongo` facade; the SDK owns MongoDB connection-pool metrics. See [ADR 0014](docs/adr/0014-mongodb-metrics.md) and [ADR 0021](docs/adr/0021-mongodb-instrumentation-mechanism.md).
- [`github.com/redis/go-redis/v9`](https://pkg.go.dev/github.com/redis/go-redis/v9) — is the Redis/Valkey client wrapped by the SDK-owned `redis/` instrumentation. The wrapper does not call `redisotel`; see [ADR 0013](docs/adr/0013-redis-valkey-integration.md).
- [`github.com/elastic/go-elasticsearch/v8`](https://pkg.go.dev/github.com/elastic/go-elasticsearch/v8) — ships first-party OpenTelemetry tracing in its shared transport; the `elasticsearch/` facade wires the SDK `TracerProvider` into it (trace-only, search body opt-in) without touching OTel globals. See [ADR 0020](docs/adr/0020-elasticsearch-integration.md).
- [`github.com/gocql/gocql`](https://pkg.go.dev/github.com/gocql/gocql) — is the Cassandra driver instrumented by the SDK-owned `cassandra/` observers. The `otelgocql` contrib package was removed upstream, so the SDK owns the observer code; see [ADR 0019](docs/adr/0019-cassandra-integration.md).
- [`go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp) — provides the underlying HTTP server/client instrumentation used by the `http/` facade. See [ADR 0009](docs/adr/0009-replace-http-with-otelhttp.md).
- [`github.com/grafana/pyroscope-go`](https://github.com/grafana/pyroscope-go) and [`github.com/grafana/otel-profiling-go`](https://github.com/grafana/otel-profiling-go) provide the Pyroscope profiler and trace-to-profile bridge. See [ADR 0012](docs/adr/0012-profiling-integration.md).
- [`go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin) — provides the underlying gin instrumentation used by the `gin/` facade. See [ADR 0010](docs/adr/0010-gin-integration.md).
- [`github.com/go-resty/resty/v2`](https://pkg.go.dev/github.com/go-resty/resty/v2) is the HTTP client wrapped by the SDK-owned `resty` instrumentation. The wrapper does not use `otelhttp` for Resty; it owns span lifecycle in Resty hooks so retry attempts stay observable. See [ADR 0011](docs/adr/0011-resty-integration.md).

## AI Collaboration

This project uses `AGENTS.md` to store AI-assisted development context and project-specific rules. `CLAUDE.md` and `GEMINI.md` are symlinks pointing to that file. If using an AI assistant, refer to `AGENTS.md` for project patterns.
