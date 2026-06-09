# o11y SDK Developer Guide

This guide covers the deeper, task-driven parts of the o11y SDK: structured
logging, trace sampling, continuous profiling, and the per-integration
sub-packages (NATS, MongoDB, Redis/Valkey, HTTP, Resty, MinIO, gin).

For project setup, the `Init` options reference, and feature toggles, see the
[README](../README.md). For running the bundled examples, see
[`examples/README.md`](../examples/README.md).

## Contents

- [Structured Logging with Trace Correlation](#structured-logging-with-trace-correlation)
- [User Identity Attributes](#user-identity-attributes)
- [Logging Guidelines](#logging-guidelines)
- [Trace Sampling](#trace-sampling)
- [Continuous Profiling](#continuous-profiling)
- [Distributed Tracing over NATS](#distributed-tracing-over-nats)
- [MongoDB](#mongodb)
- [Redis / Valkey](#redis--valkey)
- [Prometheus Metrics & HTTP](#prometheus-metrics--http)
- [Resty HTTP Client](#resty-http-client)
- [MinIO / S3 Object Storage](#minio--s3-object-storage)
- [Using with gin](#using-with-gin)

## Structured Logging with Trace Correlation

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

## User Identity Attributes

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

## Logging Guidelines

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

## Trace Sampling

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

## Continuous Profiling

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

## Distributed Tracing over NATS

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

## MongoDB

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
connection create time. Spans are always-on and are governed by the SDK's
sampler, just like HTTP, gin, NATS, and Redis spans.

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

## Redis / Valkey

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

## Prometheus Metrics & HTTP

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

## Resty HTTP Client

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

## MinIO / S3 Object Storage

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
| `WithSpanNameFormatter(...)` | `nil` | Override the default `"{Operation} {bucket}"` span name. |

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

## Using with gin

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
