# o11y SDK Developer Guide

This guide is organized in two parts:

1. **[The Four Pillars](#the-four-pillars)** — Tracing, Logging, Metrics, and
   Profiling. These apply to every service, regardless of which libraries it
   uses.
2. **[Integrations](#integrations)** — per-library sub-packages, grouped by
   [OpenTelemetry semantic-convention](semconv.md) domain (HTTP, Databases,
   Messaging, Object Storage). Read only the sections for the libraries your
   service actually uses.

For project setup, the `Init` options reference, and feature toggles, see the
[README](../README.md). For running the bundled examples, see
[`examples/README.md`](../examples/README.md).

## Contents

- [The Four Pillars](#the-four-pillars)
  - [Tracing](#tracing)
    - [User Identity Attributes](#user-identity-attributes)
    - [Application-Defined Baggage Attributes](#application-defined-baggage-attributes)
    - [Trace Sampling](#trace-sampling)
  - [Logging](#logging)
    - [Logging Guidelines](#logging-guidelines)
  - [Metrics](#metrics)
  - [Profiling](#profiling)
- [Integrations](#integrations)
  - [HTTP](#http)
    - [net/http server & client](#nethttp-server--client)
    - [gin](#gin)
    - [Resty HTTP client](#resty-http-client)
  - [Databases](#databases)
    - [MongoDB](#mongodb)
    - [Redis / Valkey](#redis--valkey)
    - [Elasticsearch](#elasticsearch)
    - [Cassandra](#cassandra)
  - [Messaging](#messaging)
    - [NATS](#nats)
  - [Object Storage](#object-storage)
    - [MinIO / S3](#minio--s3)

---

# The Four Pillars

## Tracing

For obtaining a tracer and creating spans, see
[Creating Spans](../README.md#creating-spans) in the README. The subsections
below cover the advanced tracing topics: attaching user identity to spans, and
controlling span volume with sampling.

### User Identity Attributes

Use the explicit user helpers when a service has authenticated the acting user
and needs the login name on a specific span or log record:

```go
ctx, span := obs.Tracer("my-service").Start(ctx, "handle-request")
defer span.End()

o11y.SetUser(ctx, "a.einstein")
obs.Logger.InfoContext(ctx, "processing request", o11y.UserName("a.einstein"))
```

`SetUser` writes `user.name` to the current span only. `UserName` returns a
`slog.Attr` for the log record where it is supplied. These helpers are explicit
and in-process: they do not use OTel Baggage, do not add per-service provider
wiring, and do not propagate usernames across HTTP/NATS boundaries. Empty
usernames are treated as unauthenticated and do not emit `user.name`.

Use opt-in baggage propagation when the product needs to identify the user once
at the source and have downstream services materialize the same `user.name`
without per-call helper usage:

```go
obs, err := o11y.Init(
    ctx,
    o11y.WithServiceName("edge"),
    o11y.WithServiceVersion("1.2.3"),
    o11y.WithServiceNamespace("platform"),
    o11y.WithEnvironment("production"),
    o11y.WithUserBaggage(),
)
if err != nil {
    return err
}

ctx, err = o11y.ContextWithUser(ctx, "a.einstein")
if err != nil {
    return err
}

ctx, span := obs.Tracer("edge").Start(ctx, "handle-request")
defer span.End()

obs.Logger.InfoContext(ctx, "processing request")
```

`ContextWithUser` is the source-side opt-in that puts `user.name` into W3C
Baggage. `WithUserBaggage` is the per-service opt-in that copies whitelisted
baggage onto that service's spans and SDK log records. Empty usernames leave the
context unchanged. Enable it only after authenticating the user, ignore or
overwrite untrusted inbound baggage at the edge, strip baggage before calls to
external third parties, and never promote `user.name` into metric labels.

### Application-Defined Baggage Attributes

ADR 0025 generalizes the same mechanism for identifiers owned by an
application. Define one shared key registry per product, use an application
namespace, and configure the same list in every participating service:

```go
const KeyOrderID = "app.order.id"

obs, err := o11y.Init(ctx,
    // ...required options...
    o11y.WithBaggageAttributes(KeyOrderID),
)

ctx, err = o11y.ContextWithBaggageValue(ctx, KeyOrderID, orderID)
if err != nil {
    obs.Logger.WarnContext(ctx, "set baggage attribute failed",
        slog.String("key", KeyOrderID), slog.Any("error", err))
    // Telemetry is not load-bearing: continue with the original context.
}
```

The setter puts a trusted value on the wire; the option controls whether this
service copies that key onto spans and SDK log records. Calls to the option
accumulate and de-duplicate. At most eight application keys are materialized;
`user.name` remains a separate PII opt-in and does not consume that budget.
Values are strings, limited to 256 bytes, and never become metric labels.

At a public boundary, do not trust inbound baggage. Prefer a TraceContext-only
propagator so the framework's entry span is never stamped with forged baggage,
or clear all baggage before rebuilding only authenticated values:

```go
ctx = baggage.ContextWithoutBaggage(ctx)
ctx, err = o11y.ContextWithBaggageValue(ctx, KeyOrderID, authorizedOrderID)
```

`ContextWithoutBaggageValues` is for internal boundaries where other trusted
members should survive. Removal must happen before setting because an empty or
rejected value deliberately leaves the original context unchanged. For an
entry span that started before the value was established, set the same trusted
attribute explicitly after the baggage setter succeeds. Strip baggage before
third-party egress, including deleting any pre-existing outbound `baggage`
header.

Explicit log attributes win over baggage at the same output path, including
attributes inside an empty-key group (which `slog` inlines). A named group is a
different path: after `logger.WithGroup("audit")`, a baggage key is emitted as
`audit.<key>` and does not collide with the same key previously attached at the
top level.

HTTP integrations already preserve baggage through their configured
propagator. The traced NATS facade restores it on Core and JetStream consumer
contexts. `nats.WithTracingEnabled(false)` intentionally disables all NATS
propagation, including baggage.

### Trace Sampling

The SDK default remains OpenTelemetry's `ParentBased(AlwaysSample)`, so local
development and normal services get 100% trace visibility and full
log/metric/profile correlation unless they opt into a lower rate. This is
intentional: exemplars and profile-to-trace links require an active sampled
span, so aggressive head sampling reduces those links at roughly the same
rate.

Use **head sampling** when a producing service needs protection from span
allocation, BatchSpanProcessor queue pressure, and GC overhead. Message
workers, MongoDB change-stream watchers, JetStream publishers, and other
high-throughput producers should set a per-service rate at the most upstream
trace origin so the `ParentBased` sampled flag propagates to downstream
consumers automatically. Typical hot-path rates are `0.01` to `0.001`;
normal or low-traffic services should usually stay at `1.0`.

```go
obs, err := o11y.Init(ctx,
    o11y.WithServiceName("orders-watcher"),
    o11y.WithServiceVersion("1.0.0"),
    o11y.WithEnvironment("production"),
    o11y.WithServiceNamespace("platform"),
    o11y.WithOTLPEndpoint("http://otel-collector.infra.svc.cluster.local:4318"),
    o11y.WithSamplingRatio(0.001), // 0.1% head sampling for a hot producer
)
```

Typed sampling options and OpenTelemetry environment variables coexist:

- If `WithSamplingRatio` or non-nil `WithTraceSampler` is set, that typed
  sampler wins for this SDK instance.
- If no typed sampler is set, the OTel Go SDK still honors
  `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG`. For example:

  ```bash
  OTEL_TRACES_SAMPLER=parentbased_traceidratio
  OTEL_TRACES_SAMPLER_ARG=0.001
  ```

- If neither typed options nor env vars are set, the default is 100% sampling.

`OTEL_BSP_MAX_QUEUE_SIZE`, `OTEL_BSP_MAX_EXPORT_BATCH_SIZE`,
`OTEL_BSP_SCHEDULE_DELAY`, and `OTEL_BSP_EXPORT_TIMEOUT` can also tune the OTel
BatchSpanProcessor queue, batch size, and timeout without code changes. These
settings bound exporter behavior, but they do not avoid creating recording
spans; use head sampling when the application itself is under CPU or memory
pressure.

**Head sampling vs. tail sampling:** SDK head sampling protects the service
creating spans. Collector tail sampling protects backend storage and can retain
all errors or slow traces intelligently, but it runs after the application has
already paid span creation cost. Use tail sampling as deployment configuration
for backend cost control, not as a substitute for SDK head sampling on hot
producers.

Worked example: in a MongoDB change-stream watcher → JetStream → websocket
worker pipeline, setting `WithSamplingRatio(0.001)` on the watcher causes most
root spans to be non-recording. The unsampled `traceparent` then flows through
JetStream, and downstream workers using `ParentBased` also avoid recording
those traces, thinning the whole pipeline from the source.

## Logging

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

## Metrics

By default the SDK exposes a `/metrics` endpoint on `:2112` for Prometheus to scrape. Every series carries `service_namespace`, `service_name`, `service_version`, and `deployment_environment_name` as constant labels.

```bash
curl http://localhost:2112/metrics   # inspect raw output
```

Set `WithMetricsAddr(":9090")` to move the listener, or `WithMetricsOTLPEndpoint(url)`
to switch from the Prometheus pull model to OTLP push (useful for serverless
deployments that cannot be scraped). See the
[options reference](../README.md#using-the-sdk) for the full metrics option set.

**Exemplars** are enabled automatically (OTel SDK default trace-based filter). When Prometheus is deployed with `--enable-feature=exemplar-storage` (included in [`k8s/infrastructure/base/prometheus.yaml`](../k8s/infrastructure/base/prometheus.yaml)), Grafana can navigate from a histogram bucket directly to the correlated trace in Tempo. The measurement context must contain an active sampled span; exemplar trace IDs are stored as exemplar metadata (`trace_id` / `span_id`), not as metric labels, so they do not create high-cardinality time series.

**Kubernetes pods** must opt in to scraping with the annotation:

```yaml
metadata:
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "2112"   # optional; 2112 is the default
```

HTTP request metrics (`http.server.request.duration`, `http.client.request.duration`)
are emitted by the HTTP integrations; see [HTTP](#http) for route-cardinality
controls.

## Profiling

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

---

# Integrations

The sub-packages below are grouped by their OpenTelemetry semantic-convention
domain. Each is a thin facade that wires the upstream instrumentation to the
SDK-owned TracerProvider, MeterProvider, and Propagator — application code
should use these facades rather than importing the upstream instrumentation
directly. Read only the sections for the libraries your service uses.

## HTTP

Spans and metrics in this group follow the OTel
[HTTP semantic conventions](semconv.md) (`http.*`,
`http.server.request.duration`, `http.client.request.duration`).

### net/http server & client

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

For outbound calls over the standard library client, wrap any
`http.RoundTripper` with `o11yhttp.NewTransport`. It emits one client span per
request, records `http.client.request.duration`, and injects `traceparent` so
the trace continues into the downstream service:

```go
client := &http.Client{
    Transport: o11yhttp.NewTransport(
        http.DefaultTransport,
        obs.TracerProvider(),
        obs.MeterProvider(),
        obs.Propagator,
    ),
}
```

### gin

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
SDK's HTTP metric views and does not include gin error types. gin's
`http.server.request.duration` histogram participates in the same exemplar and
route-cardinality behavior described under [Metrics](#metrics).

**Excluding infra endpoints from tracing**

`WithSkipPaths` excludes common Kubernetes probe and metrics-scrape paths from
tracing. By default it exact-matches the paths returned by `DefaultSkipPaths()`
(`/health`, `/healthz`, `/livez`, `/readyz`, `/metrics`, `/ping`, `/ready`,
`/live`). Use `WithSkipPathPrefixes` to also skip sub-path conventions such as
`/health/probe` or `/health/live`:

```go
// exact default list only
router.Use(o11ygin.Middleware(
    "orders-api",
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
    o11ygin.WithSkipPaths(),
)...)

// exact list + prefix opt-in for /health/* sub-paths
router.Use(o11ygin.Middleware(
    "orders-api",
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
    o11ygin.WithSkipPaths(o11ygin.WithSkipPathPrefixes("/health/", "/internal/")),
)...)
```

### Resty HTTP client

Use the `resty` sub-package to instrument `github.com/go-resty/resty/v2`
clients. The wrapper creates one client span per attempt, injects
`traceparent` on every attempt, and records `http.client.request.duration`.

```go
import (
    "context"
    "net/http"

    o11yresty "github.com/flywindy/o11y/resty"
    restyclient "github.com/go-resty/resty/v2"
)

type routeKey struct{}

client := o11yresty.NewClient(
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
    o11yresty.WithRouteFromContext(routeKey{}),
    o11yresty.WithMetricRouteEnabled(true),
)
client.SetRetryCount(1).AddRetryCondition(func(resp *restyclient.Response, _ error) bool {
    return resp != nil && resp.StatusCode() == http.StatusServiceUnavailable
})

ctx := context.WithValue(parentCtx, routeKey{}, "/api/orders/{id}")
resp, err := client.R().SetContext(ctx).Get("http://orders:8080/api/orders/123")
```

Route metrics are opt-in. Use bounded route templates such as
`/api/orders/{id}`; never put raw URL paths, user IDs, or trace IDs into
`http.route`.

## Databases

Spans and metrics in this group follow the OTel
[database semantic conventions](semconv.md) (`db.*`,
`db.client.operation.duration`, `db.client.connection.*`). Spans are always-on
and governed by the SDK's sampler, just like HTTP, gin, and NATS spans.

### MongoDB

Use the `mongo` sub-package to wire MongoDB tracing, operation-duration
metrics, and connection-pool metrics to the SDK-owned TracerProvider,
MeterProvider, and Propagator. Do not import upstream MongoDB instrumentation
packages directly from application code.

```go
import o11ymongo "github.com/flywindy/o11y/mongo"

client, err := o11ymongo.Connect(ctx, mongoURI,
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
    o11ymongo.WithPoolName("orders-mongo"),
)
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

MongoDB command spans and `db.client.operation.duration` metrics are emitted by
the official contrib `otelmongo` CommandMonitor. The SDK also owns the
`db.client.connection.*` pool metrics from ADR 0014, including connection
counts, configured min/max size, pending requests, check-out timeouts, and
connection create time.

For services that already build `*options.ClientOptions` themselves (for
`SetAuth`, pool sizing, read/write concerns, or deployment-specific settings),
instrument those options before calling the driver:

```go
opts := options.Client().
    ApplyURI(mongoURI).
    SetAuth(options.Credential{Username: user, Password: pass})

cleanup, err := o11ymongo.Instrument(opts,
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
    o11ymongo.WithPoolName("orders-mongo"),
)
if err != nil {
    return err
}
defer func() { _ = cleanup(context.Background()) }()

client, err := mongo.Connect(opts)
```

Here, `mongo.Connect` is the MongoDB driver's `go.mongodb.org/mongo-driver/v2/mongo.Connect`.
`Instrument` composes with existing `CommandMonitor` and `PoolMonitor` callbacks
instead of replacing them.
Calling `opts.SetMonitor(...)` after `Instrument` replaces the composed monitor
and drops o11y instrumentation.
Calling `opts.SetPoolMonitor(...)` after `Instrument` similarly drops o11y's
pool metrics. The cleanup function disables SDK-owned pool event handling;
defer it for app-built options near `client.Disconnect`, after the final
metrics flush if the service needs a last zero-value pool snapshot.
`ConnectionPoolClosed` events zero the closed pool and remove its state while
leaving the tracker ready for a later pool recreation on the same client.

Document trace propagation into persisted business documents is intentionally
not supported. For async MongoDB-sourced workflows, model trace context on an
outbox/event envelope and propagate it through message headers; see ADR 0021.

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

The wrapper always drops telemetry for Pub/Sub commands and for the
connection-lifecycle commands the client issues itself (`AUTH`, `HELLO`,
`SELECT`, `READONLY`, and the auto-issued `CLIENT SETINFO` / `SETNAME`). These
are connection management rather than application data work; `READONLY` is also
a public command, so filtering it is intentional control-plane suppression (a
deliberate `ClusterClient.ReadOnly` call is dropped too — it cannot be told
apart from the auto-issued one by name). Two opt-in options trim further noise:

```go
_, err := o11yredis.Wrap(rdb, obs.TracerProvider(), obs.MeterProvider(),
    // Drop named commands by verb (case-insensitive). Useful for
    // health-check PINGs or periodic INFO polls you never want traced.
    o11yredis.WithIgnoredCommands("ping", "info"),
    // Drop any command issued without an active parent span — i.e. work
    // outside a traced request, such as background liveness probes,
    // pool keepalive, or topology refreshes. Off by default.
    o11yredis.WithRequireParentSpan(true),
)
```

Both options suppress the span *and* the `db.client.operation.duration`
sample. They target different cases: `WithIgnoredCommands` is exact and
independent of where the command runs (it drops every `PING`, request-bound or
not), while `WithRequireParentSpan` is a blanket "only trace request-bound
work" policy that cannot pick out a single command. `WithRequireParentSpan`
defaults to off because it would otherwise silently drop legitimate unparented
background work (scheduled jobs, warmup). To stop seeing one specific noisy
command, reach for `WithIgnoredCommands` first.

To map what you see in a trace back to this model, here is how common commands
are handled by default:

| Command you see | What it is | Default | How to trim |
| --- | --- | --- | --- |
| `SET`, `GET`, `HGETALL`, `INCR`, `DEL`, … | Application data operations | Instrumented | Leave as-is; these are the work you want to see |
| `EVAL`, `EVALSHA` | Lua scripts run by the app | Instrumented | Leave as-is (or `WithIgnoredCommands` if a specific script is noise) |
| `PING` | Liveness / health probe | Instrumented | `WithRequireParentSpan(true)` for background probes, or `WithIgnoredCommands("ping")` to drop all |
| `INFO`, `COMMAND` | Periodic polls / introspection | Instrumented | `WithIgnoredCommands("info", "command")` if unwanted |
| `CLIENT LIST`, `CLIENT KILL` | Deliberate admin commands | Instrumented | `WithIgnoredCommands` if unwanted |
| `AUTH`, `HELLO`, `SELECT`, `READONLY`, `CLIENT SETINFO` / `SETNAME` | Connection setup / control-plane | Always dropped | n/a (built-in) |
| `SUBSCRIBE`, `PUBLISH`, … | Pub/Sub | Always dropped | n/a (built-in) |

A command issued under a parent span (e.g. inside a traced HTTP handler) keeps
its span regardless of `WithRequireParentSpan`; that option only affects
commands with no active parent span.

### Elasticsearch

Use the `elasticsearch` sub-package to wire the official
`github.com/elastic/go-elasticsearch/v8` client's first-party OpenTelemetry
instrumentation to the SDK-owned `TracerProvider`. `NewClient` returns the
standard `*elasticsearch.Client`, so the low-level API is unchanged; the
instrumentation is wired into the transport before the client is built.

```go
import (
    o11ies "github.com/flywindy/o11y/elasticsearch"
    elastic "github.com/elastic/go-elasticsearch/v8"
)

client, err := o11ies.NewClient(
    elastic.Config{Addresses: []string{"http://localhost:9200"}},
    obs.TracerProvider(),
)
if err != nil {
    obs.Logger.ErrorContext(ctx, "Elasticsearch client failed", slog.Any("error", err))
}

// Pass the request context so the ES span joins the active trace.
res, err := client.Search(
    client.Search.WithContext(ctx),
    client.Search.WithIndex("my-index"),
)
if err != nil {
    obs.Logger.ErrorContext(ctx, "Elasticsearch search failed", slog.Any("error", err))
}
defer res.Body.Close()
```

The integration is **trace-only** (no `MeterProvider` or propagator parameter):
the upstream instrumentation accepts only a `TracerProvider`, and it does not
propagate trace context toward Elasticsearch. `tp` is required and rejected when
nil — the facade never falls back to the global `TracerProvider`. Spans are
named per the cross-package convention, e.g. `elasticsearch.search my-index`,
and an ES HTTP error response (4xx/5xx, or a 3xx redirect) is reflected as span
status Error with `http.response.status_code`.

Search query **bodies** are not captured by default, because they can be large
and may carry user-supplied terms (PII). Opt in only when that data is safe for
your trace backend:

```go
client, err := o11ies.NewClient(cfg, obs.TracerProvider(), o11ies.WithSearchBody(true))
```

`WithSearchBody` governs only the request *body* (`db.statement`). The span's
`url.full` is always emitted by the upstream and includes the URL query string,
so a query-string search (`client.Search.WithQuery("...")`) records its terms
regardless of this option — use the body DSL for sensitive search terms.

Two upstream caveats to know:

- **Use the client's API methods**, e.g. `client.Search(...)` (and the typed
  client's `.Do(ctx)`). Constructing a low-level request struct and calling it
  directly — `esapi.SearchRequest{...}.Do(ctx, client)` — leaves its
  instrumentation field nil and emits no span.
- For the **typed client** (`NewTypedClient`), terminate requests with
  `.Do(ctx)`, not `.Perform(ctx)`: upstream typed `Perform` starts the span on a
  shadowed context, so path parts, attributes, and error status are not recorded.

### Cassandra

Use the `cassandra` sub-package to instrument `github.com/gocql/gocql`. Spans
and metrics are emitted directly by SDK-owned observers (the `otelgocql`
contrib package was removed upstream); no OTel globals are touched.

Because gocql attaches observers through the `*gocql.ClusterConfig` and cannot
attach them to a live session, the SDK builds the session: pass your fully
configured cluster (contact points, consistency, auth, timeouts) to
`NewSession`, which wires the observers and returns a normal `*gocql.Session`.
`tp` and `mp` are required and rejected when nil.

```go
import (
    o11ycassandra "github.com/flywindy/o11y/cassandra"
    "github.com/gocql/gocql"
)

cluster := gocql.NewCluster("localhost:9042")
cluster.Consistency = gocql.LocalQuorum

session, err := o11ycassandra.NewSession(
    cluster,
    obs.TracerProvider(),
    obs.MeterProvider(),
    o11ycassandra.WithPoolName("chat-cluster"),
)
if err != nil {
    obs.Logger.ErrorContext(ctx, "Cassandra session failed", slog.Any("error", err))
    return
}
defer session.Close()

// Pass the request context so the query span joins the active trace.
var name string
if err := session.Query(`SELECT name FROM rooms WHERE id = ?`, id).
    WithContext(ctx).Scan(&name); err != nil {
    obs.Logger.ErrorContext(ctx, "Cassandra query failed", slog.Any("error", err))
}
```

Each query observation produces one CLIENT span — gocql fires the observer once
per attempt and per page, so retries and paged reads appear as sibling spans
carrying their attempt index and coordinator host. Span names follow the
cross-package convention, e.g. `cassandra.SELECT rooms`.

**Batches must go through the `cassandra` batch seams** to be traced. The gocql
v1.7.0 batch-observer payload cannot identify a logical batch, so the SDK owns
the seams instead; calling `session.ExecuteBatch(batch)` directly emits no batch
span. Use `cassandra.ExecuteBatch` for normal batches and
`cassandra.ExecuteBatchCAS` / `cassandra.MapExecuteBatchCAS` for conditional
(lightweight-transaction) batches — the CAS forms return the driver's `applied`
flag and result iterator unchanged. All three bind the context onto the batch, so
the context governs the driver call, not just telemetry:

```go
batch := session.NewBatch(gocql.LoggedBatch)
batch.Query(`INSERT INTO rooms (id, name) VALUES (?, ?)`, id, name)
if err := o11ycassandra.ExecuteBatch(ctx, session, batch); err != nil {
    obs.Logger.ErrorContext(ctx, "Cassandra batch failed", slog.Any("error", err))
}
```

**Do not call `(*gocql.Query).Observer`** on queries issued through an
instrumented session: gocql gives each query a single observer slot, so setting
your own replaces the SDK's and silently drops that query's span and metrics. To
run your own query/connect observer alongside the SDK's, set it on the
`*gocql.ClusterConfig` (`QueryObserver` / `ConnectObserver`) before `NewSession`,
which composes the two. This does not apply to batches: their telemetry comes
from the `ExecuteBatch*` seams above, not the driver's batch observer, so a
`(*gocql.Batch).Observer` you set runs independently and does not affect the SDK
batch spans.

`db.query.text` (the CQL statement) is off by default because statements can be
high-cardinality and reveal schema topology; bound values are never captured.
Enable it with `o11ycassandra.WithQueryText(true)` when that is safe for your
trace backend.

`server.address` / `server.port` identify the node that actually served the
operation (the coordinator chosen by token-aware routing), falling back to the
contact point when the driver reports no host. The coordinator's id and
datacenter (`cassandra.coordinator.id` / `.dc`, plus `network.peer.*`) are
additional and off by default; enable them with
`o11ycassandra.WithHostAttributes(true)`.

`db.collection.name` (the addressed table) is recorded **both on spans and as a
metric label** on `db.client.operation.duration` and `cassandra.query.attempts`.
The metric label matters because metrics are not sampled: per-table latency and
error rate stay exact where the same breakdown derived from traces would be
skewed by sampling (tail sampling especially, since it preferentially keeps
errors and slow requests). It is also the dimension that joins these series to
the server-side `cassandra-exporter`, whose table-level metrics are labeled by
`keyspace`/`table`:

```promql
# Which table has the worst read latency, client-side?
histogram_quantile(0.99, sum by (db_collection_name, le) (
  rate(db_client_operation_duration_seconds_bucket{db_operation_name="SELECT"}[5m])))

# Which table has the worst client-side error rate?
sum by (db_collection_name) (rate(db_client_operation_duration_seconds_count{error_type!=""}[5m]))
  / sum by (db_collection_name) (rate(db_client_operation_duration_seconds_count[5m]))

# Per-table round-trip rate, including retries, speculative executions and
# paging. Compare against a table's expected request rate: an excess is
# client-side amplification the server-side exporter cannot show you.
topk(5, sum by (db_collection_name) (rate(cassandra_query_attempts_total[5m])))
```

> **`cassandra.query.attempts` counts round trips, not retries.** gocql fires its
> query observer once per attempt *and* once per page, and the SDK records one
> `db.client.operation.duration` sample and one attempt on every such callback.
> The two therefore advance together, so
> `rate(cassandra_query_attempts_total) / rate(db_client_operation_duration_seconds_count)`
> is identically `1` for query traffic and is **not** a retry ratio. A true
> per-table retry *rate* needs a counter restricted to non-zero attempt indexes,
> which the current instrument set does not provide; until then the attempt index
> is available per span as `cassandra.query.attempt`, and the absolute
> per-table attempt rate above is the metric-side signal.

The label is omitted when no single table can be resolved — an unparsed
statement, or a batch spanning several tables — which matches the semantic
convention's "performed on a single collection" condition. Pass
`o11ycassandra.WithCollectionMetricLabel(false)` to keep the table off metric
labels (spans are unaffected). Distinct table values are capped at
`o11y.DefaultMaxUniqueCollections` (200) and collapse to `"other"` past that;
since a Cassandra schema is DDL-fixed, an `"other"` bucket means a statement
shape confused the SDK's CQL tokenizer and is worth reporting rather than
raising the cap with `o11y.WithMaxUniqueCollections(n)`.

The overflow bucket is the literal label value `"other"`, which is also a legal
table name. A table actually named `other` consumes a cap slot like any other
table (so the cap's guarantee holds), but once the cap is reached its samples
merge into the overflow bucket and the two become indistinguishable. If you have
such a table and need it broken out, keep the cap above your table count.

## Messaging

Spans and propagation in this group follow the OTel
[messaging semantic conventions](semconv.md) (`messaging.*`). Trace context is
carried in message headers, so a consumer's handler context continues the
producer's trace.

### NATS

Use `obs.Propagator` together with the `nats` sub-package to propagate trace context across NATS messages.

> **Tracing is on by default, but the default is no longer the last word.**
> `o11ynats.Connect` supplies an enabled *connection-local default*
> (`otelnats.WithTracingEnabled(true)`). Since `otel-nats` v0.8.0 that default
> sits at the bottom of the precedence ladder described below, so
> `OTEL_NATS_TRACING_ENABLED` and a feature-flag relay can both override it, and
> the process-wide `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` master switch is
> ANDed above the whole ladder — a falsy value there turns off NATS tracing,
> header propagation and baggage restoration regardless of everything else.
> Audit both variables in your deployment configuration before upgrading.
>
> When the SDK's trace pillar is disabled, `obs.TracerProvider()` is a no-op
> provider: NATS
> spans are non-recording and carry no span context, so no active trace is
> propagated while the pillar is off — cross-service trace correlation is
> inactive (not broken) and resumes as soon as the trace pillar is enabled.
> If your service-level "observability off" mode must also avoid tracing
> wrapper cost and use the native NATS path, call `ConnectWithOptions` with
> `o11ynats.WithTracingEnabled(obs.Toggles.Trace)` so NATS follows the SDK's
> resolved trace toggle — subject to the same precedence: that option selects
> the native path only when no relay is configured and `OTEL_NATS_TRACING_ENABLED`
> is unset. Do not re-parse env vars or infer state by inspecting
> the TracerProvider type.

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

To use the native NATS path while observability is off, use the option-based
constructor and pass the SDK's resolved trace toggle as the connection-local
default:

```go
natsOpts := []gonats.Option{gonats.Name("orders-worker")}
conn, err := o11ynats.ConnectWithOptions(
    ctx,
    natsURL,
    obs.TracerProvider(),
    obs.Propagator,
    o11ynats.WithTracingEnabled(obs.Toggles.Trace),
    o11ynats.WithNATSOptions(natsOpts...),
)
```

In `otel-nats` v0.8.0 the upstream precedence is **relay > environment >
option > default**. Therefore the direct/native path is guaranteed by the
option only when no relay is configured and `OTEL_NATS_TRACING_ENABLED` is
unset. When a relay is available, flags are evaluated on every operation even
while tracing is currently disabled; enabling that control plane is a separate
deployment decision from this dependency upgrade.

The process-wide master switch `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` is
applied *after* that ladder and ANDed with its result. A falsy value disables
NATS tracing, trace propagation and baggage restoration whatever the relay, the
environment variable or the option resolved to. Both variables are a strict
tri-state since v0.8.0: only `1`/`true`/`yes`/`on` and `0`/`false`/`no`/`off`
are accepted, and any other value — including the empty string an unexpanded
`${VAR}` produces — fails the connection with an error instead of being
ignored. See
[README's feature-toggle section](../README.md#feature-toggles-progressive-rollout)
and [ADR 0004](adr/0004-nats-integration.md) for the same ladder stated
alongside the option reference and the integration decision record.

To make the *consumer* span itself searchable on domain identifiers the SDK
has no way to know — a room ID, a site ID, a request ID pulled from the
payload — set them on the span the handler was handed, via the standard OTel
API. `conn.Subscribe`'s consumer span (and the JetStream consume spans below)
are created and named by `otel-nats` and cannot take a caller-supplied name or
be forked for extra baked-in attributes (ADR 0022 amendment, 2026-07-01), but
any span accepts more attributes after the fact:

```go
sub, err := conn.Subscribe(ctx, "orders.created", func(ctx context.Context, msg *gonats.Msg) {
    trace.SpanFromContext(ctx).SetAttributes(
        attribute.String("app.order_id", orderID),
    )
    // ...
})
```

For **request/reply**, reply with `conn.Respond` from inside the handler so the
reply message carries trace context. Do not use `msg.Respond` — it routes
through the raw NATS connection, skips header injection, and breaks reply-side
trace propagation.

```go
// Responder: Respond validates the reply subject (non-nil msg, non-empty
// msg.Reply) and routes through the same traced publish path as conn.Publish.
_, err = conn.Subscribe(ctx, "orders.get", func(ctx context.Context, msg *gonats.Msg) {
    if err := conn.Respond(ctx, msg, []byte("ok")); err != nil {
        obs.Logger.ErrorContext(ctx, "respond failed", slog.Any("error", err))
    }
})

// Requester: conn.Request injects the active trace into the request headers.
// Since otel-nats v0.6.0 the upstream layer also records the requester-side
// reply-receive span itself: a "receive {inbox}" CLIENT span (v0.7.0 corrected
// the kind from CONSUMER) that, when the reply carries a trace context (the
// responder used conn.Respond), is parented under — and linked to — the
// responder's reply-send context.
_, err = conn.Request(ctx, "orders.get", []byte("42"), 2*time.Second)
```

`Respond` guarantees the reply carries trace context, and upstream `Request`
completes the requester-side half of the round trip: in Tempo you should see
request publish, responder processing, responder reply publish, and a
requester-side "receive {inbox}" span in the responder's trace, linked to the
reply publish. If the reply carries no trace context — an untraced responder,
or one that used the raw `msg.Respond` instead of `conn.Respond` — the
receive span is still recorded (in the requester's own trace) but carries no
link.

Migration note (pre-1.0 API change, otel-nats v0.6.0 upgrade): `Request`'s
optional variadic `attrs` parameter was removed — the reply span is now
created inside otel-nats, which has no caller-attribute injection point.
Attach domain identifiers (request/correlation IDs, room/site IDs) to your
own ambient span instead.

**Cross-trace topology (not a single trace):** under the v0.6.0 upstream
path the two sides of a request/reply do **not** share one trace. The
requester's "send" span parents to `ctx`; the reply "receive" span (and the
responder's own processing) parent under the responder's trace, tied back to
the request only by a **span link**. In Tempo you follow the link between the
two traces — there is no single trace containing both sides. (The pre-v0.6.0
facade parented the receive span in the requester's trace; that is what
changed.)

What opening an ambient span at the call site *does* buy you: it gives the
requester "send" span an application parent instead of a disconnected root
trace, so a background worker's request operations group under its own trace
rather than scattering as orphan roots. It does not, and cannot, pull the
responder side into that trace.

```go
ctx, span := obs.Tracer("worker").Start(ctx, "request orders.get")
defer span.End()
reply, err := conn.Request(ctx, "orders.get", []byte("42"), 2*time.Second)
```

When auditing a service, check any request/reply call made from a background
loop, cron job, or startup path rather than from within a handler or consumer
callback — those are the call sites most likely to be passing a bare `ctx`,
where the "send" span would otherwise be an orphan root.

#### JetStream

`conn.JetStream()` returns an o11y-owned JetStream handle. Configuration types
(`StreamConfig`, `ConsumerConfig`, `AckExplicitPolicy`, publish/consume options)
come from `github.com/nats-io/nats.go/jetstream` directly; callers never import
the upstream instrumentation package. Consume callbacks and the `Messages`
pull iterator deliver the native `jetstream.Msg` plus a `ctx` carrying the
consumer span — the same `(ctx, msg)` shape as core `Subscribe`. (Single-message
`Consumer.Next` is a different case — deferred, not wrapped; see below.)

```go
import "github.com/nats-io/nats.go/jetstream"

js, err := conn.JetStream()

// Producer: the active trace is injected into the message headers. Pass
// jetstream.WithMsgID(id) to drive server-side deduplication.
_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
    Name: "EVENTS", Subjects: []string{"events.created"},
})
_, err = js.Publish(ctx, "events.created", payload)

// Consumer: handler ctx carries the consumer span linked to the producer.
cons, err := js.CreateOrUpdateConsumer(ctx, "EVENTS", jetstream.ConsumerConfig{
    Durable: "worker", FilterSubject: "events.created", AckPolicy: jetstream.AckExplicitPolicy,
})
cc, err := cons.Consume(ctx, func(ctx context.Context, msg jetstream.Msg) {
    obs.Logger.InfoContext(ctx, "event", slog.String("subject", msg.Subject()))
    _ = msg.Ack()
})
if err != nil {
    return // cc is nil when Consume returns an error
}
defer cc.Stop()
```

`Consume` and `Messages` take a `ctx` like the core `Subscribe`: it is a
registration-time guard (an already-cancelled `ctx` is rejected), **not** a
trace carrier and **not** a way to cancel a running loop — per-message trace
context arrives on the handler's `ctx`, and you stop via `ConsumeContext.Stop` /
`MessagesContext.Stop`/`Drain`.

The pull-iterator form (`cons.Messages(ctx, …)` → `iter.Next()`) delivers the
same `(ctx, msg)` per message. Since the otel-nats v0.6.0 upgrade, `Consume`'s
`ConsumeContext` exposes the full native teardown surface — `Stop()`
(immediate, discards buffered messages), `Drain()` (drain-and-wait), and
`Closed()` (a channel to await full teardown) — so `Consume` no longer forces
a choice between callback ergonomics and graceful shutdown.

The batch pull forms — `cons.Fetch(ctx, batch, …)`, `cons.FetchBytes(ctx,
maxBytes, …)`, `cons.FetchNoWait(ctx, batch)` — return a `MessageBatch` whose
`Messages()` channel yields `FetchedMessage{Ctx, Msg}` pairs, mirroring the
same `(ctx, msg)` shape delivered per message rather than per callback/iterator
call:

```go
batch, err := cons.Fetch(ctx, 10) // up to 10 messages
if err != nil {
    return err
}
for m := range batch.Messages() {
    obs.Logger.InfoContext(m.Ctx, "event", slog.String("subject", m.Msg.Subject()))
    _ = m.Msg.Ack()
}
if err := batch.Error(); err != nil {
    // terminal batch error, checked after the channel closes
}
```

`FetchBytes` stops once `maxBytes` is reached *or* its wait expires — pass
`jetstream.FetchMaxWait(d)` to bound how long an under-budget batch stays open.
`FetchNoWait` returns only currently-available messages and never blocks
waiting for new ones.

Unlike `Consume`/`Messages`, `ctx` on `Fetch`/`FetchBytes` is more than a
registration-time guard: it is also passed into the native pull request, so
cancelling or timing out `ctx` after the call returns ends the in-flight
fetch early and closes the batch channel — useful for bounding a fetch by the
caller's own deadline without setting `FetchMaxWait`. If `opts` already
includes an explicit `jetstream.FetchMaxWait`, that takes precedence (the two
are mutually exclusive in the native API) and `ctx` reverts to a
registration-time guard only, same as before. `FetchNoWait` takes no
`FetchOpt`, so it stays a registration-time guard regardless. Passing your
own `jetstream.FetchContext` in `opts` is unsupported and not detected —
unlike `FetchMaxWait`, the native API has no double-set guard for it, so it
silently overrides this `ctx` parameter's cancellation wiring with no error.
Pass the `ctx` you want to govern cancellation as `Fetch`'s/`FetchBytes`' own
`ctx` argument, never as a `FetchContext` opt.

A background goroutine forwards messages onto the channel, buffered to the
request's own message-count bound where one exists. For `Fetch`/`FetchNoWait`,
`batch` is that bound, so it's safe to abandon `Messages()` before reading
everything — the goroutine drains the whole batch into the buffer and exits
on its own either way. `FetchBytes` has no message-count bound (only a byte
budget), so its buffer is a best-effort fixed size. When abandoning a batch
early, call `batch.Stop()` (added with the otel-nats v0.6.0 upgrade): it
discards undelivered messages and releases both this facade's forwarding
goroutine and the upstream one. Draining fully remains equally supported —
`examples/jetstream/fetch-worker` shows the full drain pattern.

There is a tracing consequence: `m.Ctx`'s receive span is already ended by the
time your loop body runs. Since otel-nats v0.7.0 upstream ends every batch
message's receive span *at handover* (just before putting it on the channel),
`IsRecording()` is deterministically false at delivery for every message —
unlike the `Subscribe` pattern above, `trace.SpanFromContext(m.Ctx).
SetAttributes(...)` is a no-op here. (Before v0.7.0 upstream ended message N's
span only when it read message N+1, so the no-op was racy rather than
guaranteed; v0.7.0 makes it consistent, and the receive-span durations shorter,
measuring receive-to-handover only.) Two things still work:
`obs.Logger.*Context(m.Ctx, ...)` log correlation (reads only the immutable
trace/span IDs, not the span's live state), and starting your own child span
via `tracer.Start(m.Ctx, ...)` for per-message work — the pattern
`examples/jetstream/fetch-worker` actually uses.

Single-message `Consumer.Next` is wrapped since the otel-nats v0.6.0 upgrade
(upstream fixed it to return the local receive-span context). Not yet wrapped
(use a later facade addition or, if needed sooner, the upstream package
directly): push consumers and ordered consumers.

`Consume`/`Messages`/`Fetch`/`FetchBytes`/`FetchNoWait` extract per-message
trace context entirely inside the upstream `oteljetstream` package. Since
otel-nats v0.7.0 its `HeaderCarrier` matches the verbatim key first and then
falls back to MIME-canonical and case-folded lookups, a message whose trace
header was written under a canonicalized key ("Traceparent" rather than the
literal lowercase "traceparent" this SDK's own `Publish` writes) — by any
canonicalizing producer — is still linked to its producer's trace when consumed
through any of these five methods. (Earlier versions had no such fallback; that
was the ADR 0022 2026-07-03 documented limitation, now resolved upstream.)
The **legacy** `nats.JetStreamContext` API (`js.PullSubscribe()` +
`sub.FetchBatch()`) is a different, un-instrumented API; for it, propagate trace
context manually with `nats.Inject` / `nats.Extract`.

## Object Storage

Spans in this group use the package-local `object_store.*` attribute schema,
with optional dual-emit of the experimental OTel `aws.s3.*` keys for dashboards
keyed on the AWS-S3 semantic conventions.

### MinIO / S3

Use the `minio` sub-package to instrument `github.com/minio/minio-go/v7`
clients (and any S3-compatible backend). The wrapper produces one
`SpanKindClient` span per high-level operation
(`PutObject`/`GetObject`/`StatObject`/…) with the package-local
`object_store.*` attribute schema and records
`minio.client.operation.duration`. See ADR 0018 for the full design.

```go
import (
    o11yminio "github.com/flywindy/o11y/minio"
    miniogo "github.com/minio/minio-go/v7"
    "github.com/minio/minio-go/v7/pkg/credentials"
)

client, err := o11yminio.New(
    "minio.internal:9000",
    &miniogo.Options{
        Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
        Secure: false,
    },
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
    o11yminio.WithHTTPChildSpans(true),
)
```

Options:

| Option | Default | Effect |
|---|---|---|
| `WithHTTPChildSpans(bool)` | `false` | Wrap minio-go's own `DefaultTransport(Secure)` (not `http.DefaultTransport`) with `o11yhttp.NewTransport`, so every HTTP round-trip (incl. multipart `UploadPart`s) becomes a child span. The transport uses an empty propagator — no `traceparent` flows toward the store. |
| `WithObjectKeyAttribute(bool)` | `true` | Whether to record `object_store.object.key` on spans. Metric labels never carry the key regardless. |
| `WithAWSS3CompatAttributes(bool)` | `false` | Dual-emit the experimental `aws.s3.*` keys (`aws.s3.bucket`, `aws.s3.key`) alongside the default `object_store.*` attributes for dashboards keyed on the OTel AWS-S3 semantic conventions. |
| `WithSpanNameFormatter(...)` | `nil` | Override the default `"s3.{Operation} {bucket}"` span name. A non-empty return is used verbatim (the `s3.` prefix is part of the default only). |

`GetObject` is wrapped as a thin span only; the caller's original ctx
is what minio-go stashes on the returned `*Object`, so any lazy `Read`
HTTP child span parents to whatever caller span was active at
`GetObject` time, not to the (already-ended) wrapper span. For
measured downloads use `FGetObject` or read through `WithHTTPChildSpans`
HTTP children.

`ListObjects` ends its span when the channel closes; the
drain-or-cancel contract is inherited from minio-go itself — abandoning
the channel without cancelling the context leaks both the upstream
producer goroutine and the open span.
