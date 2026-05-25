# o11y Golang SDK

A lightweight Go SDK for standardized observability, integrating OpenTelemetry (OTel) tracing with structured logging (`slog`) for automatic trace correlation.

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
        o11y.WithServiceVersion("1.0.0"),          // required
        o11y.WithEnvironment("production"),        // required; see canonical values below
        o11y.WithServiceNamespace("platform"),     // required; maps to k8s namespace / team
        o11y.WithOTLPEndpoint("http://localhost:4318"),
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

_Metrics:_

| Option | Default | Description |
|--------|---------|-------------|
| `WithMetricsOTLPEndpoint(url)` | `""` | Switch metrics to OTLP push (serverless); when unset, Prometheus pull on `:2112` is used |
| `WithMetricsAddr(addr)` | `:2112` | Prometheus `/metrics` scrape address |
| `WithRuntimeMetrics(bool)` | `true` | Collect Go runtime metrics (goroutines, GC, memory) |
| `WithHistogramBuckets([]float64)` | SLO defaults | Override HTTP latency histogram boundaries; see `DefaultLatencyBuckets()` |
| `WithDisableDefaultViews()` | off | Disable SDK-managed HTTP metric label allowlists and bucket views |
| `WithMaxUniqueRoutes(n)` | `1000` | Cap exported distinct `http.route` values and derive the SDK aggregation cardinality budget |
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

### Structured Logging with Trace Correlation

Use `obs.Logger` instead of the global `slog` package. Every log record is written to two destinations automatically:

- **OTLP → Loki**: Full OTel Log Data Model. `service.name` and `deployment.environment` live in the OTel Resource (not per-record attributes). `traceId`, `spanId`, and `trace_flags` are extracted from the context by the `otelslog` bridge.
- **stdout (JSON)**: Human-readable output for local development. Includes `service.name`, `environment`, `traceId`, and `spanId` as flat JSON fields.
- When `WithLogEnabled(false)` is set, only the stdout destination is active.

```go
// Without a span — no trace fields in either destination
obs.Logger.Info("service started")

// With an active span — trace context included automatically
ctx, span := obs.Tracer("my-tracer").Start(ctx, "my-operation")
defer span.End()

obs.Logger.InfoContext(ctx, "processing request", slog.String("user_id", "42"))
// stdout: {"time":"...","level":"INFO","msg":"processing request","service.name":"my-service","traceId":"4bf92f...","spanId":"00f067...","user_id":"42"}
// Loki:   OTel Log Record — Body="processing request", TraceId=4bf92f..., SpanId=00f067..., Attributes={user_id: "42"}, Resource={service.name: "my-service", ...}
```

### Logging Guidelines

The dual-output logger forwards every record to two backends. Treat both as
shared, queryable infrastructure — anything you log is searchable by every
engineer with cluster access.

- **Never log secrets**: API tokens, session cookies, signed URLs, full
  `Authorization` headers, internal IPs, JWTs, raw OAuth state, encryption
  keys. Redact before passing to `slog`. The `WithOTLPHeaders` option
  intentionally does not log header values for the same reason.
- **Hash or truncate user identifiers**: prefer `slog.String("user_id", hash(uid))`
  over the raw email/phone. `traceId` already lets you correlate a single user's
  request across logs without storing PII.
- **Never log raw request bodies**: a malicious client can plant `\n{"level":"INFO",...}`
  inside a body field and inject a synthetic log line into your stdout JSON
  pipeline. If you must record body shape, log only the field count or a
  schema hash.
- **Use `*Context` variants**: `Logger.InfoContext(ctx, ...)` (not `Info(...)`)
  so that `traceId` and `spanId` are populated. A log without trace correlation
  is operationally a needle in a haystack.
- **Pre-validate attribute keys**: `slog.String(userInput, ...)` lets the
  attacker control the log's field name. Use a fixed key and put untrusted
  data in the value.
- **Watch attribute size**: `slog.Any` happily serializes arbitrary structs.
  A 10 MB struct logged at 1 kHz overruns both stdout and the OTLP exporter's
  batch queue. Cap or summarise large payloads before logging.

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

### Continuous Profiling

Profiling is opt-in and **doubly gated**: it requires both a non-empty
`WithProfilingEndpoint` AND `WithProfilingEnabled(true)` (or
`O11Y_PROFILING_ENABLED=true`). Either alone is insufficient. In the provided
Kubernetes stack, applications should send profiles to Alloy:

```go
obs, err := o11y.Init(ctx,
    o11y.WithServiceName("orders-api"),
    o11y.WithServiceVersion("1.0.0"),
    o11y.WithEnvironment("production"),
    o11y.WithServiceNamespace("platform"),
    o11y.WithOTLPEndpoint("http://otel-collector.infra.svc.cluster.local:4318"),
    o11y.WithProfilingEndpoint("http://alloy.infra.svc.cluster.local:4040"),
    o11y.WithProfilingEnabled(true),
)
```

For local development, port-forward Alloy and use
`WithProfilingEndpoint("http://localhost:4040")`. Use
`WithProfilingAuthHeaders` when the Pyroscope endpoint requires auth or
tenant routing headers. Header values are copied defensively and are not
logged.

When profiling is enabled, the SDK wraps its tracer provider with the Grafana
span profiling bridge. Root spans receive a `pyroscope.profile.id` attribute,
and Pyroscope samples are labeled so Grafana can open CPU profiles from Tempo.
The link is statistical: short spans, especially below the CPU sampling
interval, can legitimately show an empty profile.

Important caveats:

- Trace-to-profile navigation is CPU-profile-only. Service-level profiles also
  include allocation and in-use memory profiles.
- By default, only local root spans are labeled by the bridge.
- Go `pprof` labels apply to the current goroutine. Work started in a new
  goroutine is captured in the service-level profile, but it is not linked to
  the span unless the application propagates pprof labels explicitly.

### Distributed Tracing over NATS

Use `obs.Propagator` together with the `nats` sub-package to propagate trace context across NATS messages.

```go
import (
    o11ynats "github.com/flywindy/o11y/nats"
    gonats "github.com/nats-io/nats.go"
)

conn, err := o11ynats.Connect(ctx, natsURL, obs.TracerProvider(), obs.Propagator)

// Publisher: trace context is injected into message headers automatically.
if err := conn.Publish(ctx, "orders.created", payload); err != nil {
    obs.Logger.ErrorContext(ctx, "publish failed", slog.Any("error", err))
}

// Subscriber: ctx in the handler already carries the publisher's trace.
// Subscribe returns (*nats.Subscription, error) — capture both: the error
// surfaces invalid input (empty subject, nil handler, cancelled ctx) and the
// Subscription handle is what you call Unsubscribe()/Drain() on at shutdown.
sub, err := conn.Subscribe(ctx, "orders.created", func(ctx context.Context, msg *gonats.Msg) {
    ctx, span := obs.Tracer("consumer").Start(ctx, "orders.created")
    defer span.End()
    obs.Logger.InfoContext(ctx, "order received") // traceId and spanId injected automatically
})
if err != nil {
    obs.Logger.ErrorContext(ctx, "subscribe failed", slog.Any("error", err))
    return
}
defer func() { _ = sub.Drain() }() // gracefully drain on shutdown
```

### MongoDB

Use the `mongo` sub-package to wire MongoDB tracing to the SDK-owned
TracerProvider and Propagator. Do not import the upstream `otel-mongo/v2`
package directly from application code.

```go
import o11ymongo "github.com/flywindy/o11y/mongo"

client, err := o11ymongo.Connect(ctx, mongoURI, obs.TracerProvider(), obs.Propagator)
if err != nil {
    obs.Logger.ErrorContext(ctx, "MongoDB connect failed", slog.Any("error", err))
    return
}
defer func() {
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = client.Disconnect(shutdownCtx)
}()

collection := client.Database("app").Collection("orders")
_, err = collection.InsertOne(ctx, bson.M{"_id": "order-123", "status": "created"})
```

MongoDB command spans are gated by the upstream instrumentation flags:

```bash
export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true
export OTEL_MONGO_TRACING_ENABLED=true
```

Document trace propagation is disabled by default because it writes an
`_oteltrace` field into persisted documents. Enable it only for asynchronous
patterns such as change streams or outbox processors that need to restore trace
context from MongoDB documents:

```go
client, err := o11ymongo.Connect(ctx, mongoURI, obs.TracerProvider(), obs.Propagator,
    o11ymongo.WithDocumentTracePropagation(true),
)
```

### Redis / Valkey

Use the `redis` sub-package to instrument `github.com/redis/go-redis/v9`
clients. The wrapper supports Redis and Valkey servers through go-redis and
does not call `redisotel`; spans and metrics are emitted directly with the
SDK's pinned semantic conventions.

```go
import (
    o11yredis "github.com/flywindy/o11y/redis"
    "github.com/redis/go-redis/v9"
)

rdb := redis.NewClient(&redis.Options{
    Addr: "localhost:6379",
    DB:   0,
})
defer rdb.Close()

_, err := o11yredis.Wrap(
    rdb,
    obs.TracerProvider(),
    obs.MeterProvider(),
    o11yredis.WithPoolName("sessions-cache"),
)
if err != nil {
    obs.Logger.ErrorContext(ctx, "Redis instrumentation failed", slog.Any("error", err))
}

if err := rdb.Set(ctx, "health", "ok", 0).Err(); err != nil {
    obs.Logger.ErrorContext(ctx, "Redis set failed", slog.Any("error", err))
}
```

`db.query.text` is off by default because Redis commands often contain key
names or values that may be sensitive. Enable it only when that data is safe
for your trace backend:

```go
_, err := o11yredis.Wrap(rdb, obs.TracerProvider(), obs.MeterProvider(),
    o11yredis.WithCommandTextEnabled(true),
)
```

### Prometheus Metrics

By default the SDK exposes a `/metrics` endpoint on `:2112` for Prometheus to scrape. Every series carries `service_namespace`, `service_name`, `service_version`, and `deployment_environment_name` as constant labels.

```bash
curl http://localhost:2112/metrics   # inspect raw output
```

HTTP handler instrumentation is provided by the `github.com/flywindy/o11y/http` package:

```go
import o11yhttp "github.com/flywindy/o11y/http"

mux := http.NewServeMux()
mux.HandleFunc("GET /api/orders/{id}", handleOrder)

// Wrap the mux. The SDK passes TracerProvider, MeterProvider, and Propagator
// explicitly, so otelhttp never reads OpenTelemetry globals.
handler := o11yhttp.NewServerHandler(
    mux,
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
)
```

For Go 1.22+ `http.ServeMux`, route patterns become bounded span names such
as `GET /api/orders/{id}` and bounded `http.route` metric labels. For routers
such as chi or echo, use their route pattern as an otelhttp label or span-name
formatter at the router edge; keep raw URL paths out of metric labels.
`WithMaxUniqueRoutes` rewrites excess exported server routes to
`http_route="other"` while the OTel SDK's own cardinality limit protects
in-process aggregators from attacker-controlled attribute sets. If the separate
SDK guard trips, metrics are preserved under `otel_metric_overflow="true"`
with route detail intentionally dropped.

### Using with gin

Use the `gin` sub-package to wire gin's OTel middleware to the SDK-owned
TracerProvider, MeterProvider, and Propagator. Register the returned chain
before `gin.Recovery()` so panics recovered by gin still produce complete HTTP
status attributes and metrics.

```go
import (
    "errors"
    "net/http"

    o11ygin "github.com/flywindy/o11y/gin"
    "github.com/gin-gonic/gin"
)

router := gin.New()
router.Use(o11ygin.Middleware(
    "orders-api",
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
)...)
router.Use(gin.Recovery())

router.GET("/orders/:id", func(c *gin.Context) {
    c.JSON(http.StatusOK, gin.H{"status": "ok"})
})
router.GET("/fail", func(c *gin.Context) {
    err := errors.New("simulated failure")
    c.AbortWithError(http.StatusInternalServerError, err).SetType(gin.ErrorTypePublic)
})
```

`ErrorRecorder` adds typed `gin.error.type` span events for errors pushed via
`c.Error` / `c.AbortWithError`. The metric label set remains governed by the
SDK's HTTP metric views and does not include gin error types.

**Exemplars** are enabled automatically (OTel SDK default trace-based filter). When Prometheus is deployed with `--enable-feature=exemplar-storage` (included in `k8s/infrastructure/base/prometheus.yaml`), Grafana can navigate from a histogram bucket directly to the correlated trace in Tempo. The measurement context must contain an active sampled span; exemplar trace IDs are stored as exemplar metadata (`trace_id` / `span_id`), not as metric labels, so they do not create high-cardinality time series.

**Kubernetes pods** must opt in to scraping with the annotation:

```yaml
metadata:
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "2112"   # optional; 2112 is the default
```

## Running the Examples

Before running any example, port-forward the required services from the `kind` cluster:

```bash
kubectl port-forward -n infra svc/otel-collector 4318:4318  # OTel traces and logs
kubectl port-forward -n infra svc/nats           4222:4222  # NATS connection
kubectl port-forward -n infra svc/redis          6379:6379  # Redis connection
kubectl port-forward -n infra svc/grafana        3000:3000  # Grafana UI
kubectl port-forward -n infra svc/prometheus     9090:9090  # Prometheus UI
kubectl port-forward -n infra svc/alloy          4040:4040  # Pyroscope ingest for local app profiling
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

### Metrics (otelhttp facade + OTLP push)

```bash
go run examples/metrics/main.go
```

The example starts an HTTP server on `:8080` and generates synthetic traffic every 500 ms. Metrics flow via OTLP/HTTP to the OTel Collector, which forwards them to Prometheus via remote write. It uses the same `localhost:4318` NodePort as traces and logs, so no extra metrics scrape port is needed for this example. Histogram buckets include exemplars linking each measurement to its trace.

Open Grafana at `http://localhost:3000` and navigate to:
- **Explore → Tempo** — producer and consumer spans linked across services
- **Explore → Loki** — structured log entries with correlated `traceId` and `spanId`
- **Explore → Prometheus** — `http_server_request_duration_seconds`; click an exemplar dot to jump to the linked trace in Tempo
- **Dashboards → Observability → Metrics Correlation** — HTTP latency metrics with a data link that opens the matching `metrics-example` logs in Loki

### Gin

```bash
go run examples/gin/main.go
curl http://localhost:8080/ok
curl http://localhost:8080/fail
```

The example registers `o11ygin.Middleware(...)` before `gin.Recovery()` and
demonstrates typed `gin.error.type` span events from `c.AbortWithError`.

### Redis

Port-forward Redis to `localhost:6379`, then run the example:

```bash
kubectl port-forward -n infra svc/redis 6379:6379
go run examples/redis/main.go
```

The example emits Redis command spans plus `db.client.operation.duration` and
connection-pool metrics through OTLP metrics push. It runs `PING`, `SET`,
`GET`, a cache-miss `GET`, and a pipeline every two seconds.

### Profiling

```bash
go run examples/profiling/main.go
```

The example starts a sampled root span every two seconds and burns CPU long
enough for Pyroscope to capture useful samples. It sends profiles to
`PYROSCOPE_ENDPOINT` (default `http://localhost:4040`) and traces/logs/OTLP
metrics to `OTLP_ENDPOINT` (default `http://localhost:4318`). Keep the Alloy
and Grafana port-forwards from the setup block running while the example runs.

### MongoDB

Run a local MongoDB instance or port-forward one to `localhost:27017`, then
enable the upstream tracing gates before running the example:

```bash
export MONGODB_URI=mongodb://localhost:27017
export OTEL_INSTRUMENTATION_GO_TRACING_ENABLED=true
export OTEL_MONGO_TRACING_ENABLED=true
go run examples/mongodb/main.go
```

To demonstrate `_oteltrace` document propagation, opt in explicitly:

```bash
export O11Y_MONGO_DOCUMENT_TRACE_PROPAGATION=true
go run examples/mongodb/main.go
```

## Core Principles

1. **Context-First**: Always propagate `context.Context` — trace information flows through context only.
2. **Zero Global State**: No `init()` side effects, no global logger or tracer provider variables. See [ADR 0003](docs/adr/0003-global-state-policy.md).
3. **Correlation**: Every log record includes `traceId` and `spanId` when a span is active — as JSON fields on stdout and as OTel Log Data Model fields in Loki. See [ADR 0001](docs/adr/0001-log-format-strategy.md).
4. **Errors**: Use `slog.ErrorContext(ctx, ...)` with structured attributes; never `panic` for recoverable errors.
5. **Semconv v1.39.0**: All instrument names, attribute keys, and types conform to OTel Semantic Conventions v1.39.0. See [`docs/semconv.md`](docs/semconv.md).

## Acknowledgements

- [`github.com/Marz32onE/instrumentation-go/otel-nats`](https://github.com/Marz32onE/instrumentation-go) — provides the underlying NATS Core + JetStream tracing semantics used by the `nats/` wrapper. Verified at v0.2.11 not to mutate OTel globals and to import semconv v1.39.0. See [ADR 0004](docs/adr/0004-nats-integration.md) for the integration decision and audit discipline.
- [`github.com/Marz32onE/instrumentation-go/otel-mongo/v2`](https://github.com/Marz32onE/instrumentation-go) — provides the underlying MongoDB driver v2 tracing semantics used by the `mongo/` wrapper. Verified at the `otel-mongo/v2/v0.2.11` tag not to mutate OTel globals, with `_oteltrace` document propagation disabled by default through the o11y wrapper. See [ADR 0005](docs/adr/0005-mongodb-integration.md).
- [`github.com/redis/go-redis/v9`](https://pkg.go.dev/github.com/redis/go-redis/v9) — is the Redis/Valkey client wrapped by the SDK-owned `redis/` instrumentation. The wrapper does not call `redisotel`; see [ADR 0013](docs/adr/0013-redis-valkey-integration.md).
- [`go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp) — provides the underlying HTTP server/client instrumentation used by the `http/` facade. See [ADR 0009](docs/adr/0009-replace-http-with-otelhttp.md).
- [`github.com/grafana/pyroscope-go`](https://github.com/grafana/pyroscope-go) and [`github.com/grafana/otel-profiling-go`](https://github.com/grafana/otel-profiling-go) provide the Pyroscope profiler and trace-to-profile bridge. See [ADR 0012](docs/adr/0012-profiling-integration.md).
- [`go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin) — provides the underlying gin instrumentation used by the `gin/` facade. See [ADR 0010](docs/adr/0010-gin-integration.md).

## AI Collaboration

This project uses `AGENTS.md` to store AI-assisted development context and project-specific rules. `CLAUDE.md` and `GEMINI.md` are symlinks pointing to that file. If using an AI assistant, refer to `AGENTS.md` for project patterns.
