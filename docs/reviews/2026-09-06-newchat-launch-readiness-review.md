# o11y × newchat launch-readiness review (2026-09-06)

A full pass over the o11y SDK (`main` at v0.12.0, commit `159f408`) and over how
the chat platform `github.com/hmchangw/chat` ("newchat", ~40 Go services,
pinned to o11y **v0.11.0**) integrates it, ranked as a senior engineer / SRE
would before the platform's production launch.

> **Status**: findings only; nothing here is implemented yet. Every finding is
> marked CONFIRMED (verified against source, a scratch test, or the pinned
> upstream module) or PLAUSIBLE (inference not exercised at runtime).

Method:

- Three independent review passes: o11y core (`o11y.go`, `options.go`,
  `internal/*`, `obsctx`), o11y integration packages (`nats`, `gin`, `http`,
  `resty`, `mongo`, `redis`, `cassandra`, `elasticsearch`, `minio`), and the
  newchat side (every `main.go`, `pkg/obs`, `pkg/*util`, `pkg/natsmetrics`,
  deploy manifests, and newchat's own `docs/specs/o11y/*` which state what it
  needs from the SDK).
- Upstream behaviour checked on disk in the module cache: otel-nats v0.9.1,
  otel-flags v0.2.0, otelgin/otelhttp v0.68.0, otel/sdk v1.44.0, sdk/log
  v0.19.0, otelslog v0.18.0, exporters/prometheus v0.65.0, go-redis v9.9.0,
  gocql v1.7.0, pyroscope-go v1.3.0.
- Already-verified items from
  [`2026-08-15-verification-and-remediation-plan.md`](./2026-08-15-verification-and-remediation-plan.md)
  are referenced by their IDs (B2, C1, C2, C3, N1, N2) and not re-derived.
- `go build ./... && go vet ./... && go test ./...` are clean on both trees.

IDs: **SDK-n** is work in this repository; **APP-n** is work in newchat (listed
because the SDK either caused it, can prevent it, or must document it).

---

## 1. Executive summary

newchat's integration is unusually disciplined (explicit providers everywhere,
bounded metric labels, a registry guard test, correct shutdown ordering in all
30 services that use the SDK). The launch risks are concentrated in a handful of
places:

1. **Observability is off by default and the production manifests do not turn
   it on** (APP-1). `pkg/obs` defaults `O11Y_ENABLED=false`, and no
   `deploy/**` manifest sets `O11Y_ENABLED`, `SERVICE_VERSION`, `DEPLOY_ENV`, or
   a sampler; 18 compose files also lack `OTEL_SERVICE_NAME`.
2. **Seven business metrics are never exported** (APP-2 / SDK-3), including
   `atrest_kek_renewal_failures_total`, whose help text says "treat as a hard
   alert". They register on client_golang's default registry; the SDK's
   Prometheus exporter serves a private one and offers no bridge.
3. **One mislabeled datapoint makes `/metrics` return HTTP 500 until the pod
   restarts** (SDK-1). This already happened once in newchat (2026-08-19).
4. **Any sampling ratio below 100 % fragments every cross-NATS-hop trace**
   (SDK-2). otel-nats starts each consumer span as a new root with a link, so
   `ParentBased(TraceIDRatioBased(r))` re-rolls at every hop. This is newchat's
   single explicit feature request to o11y, and it is implementable entirely in
   this repository with a link-aware sampler.
5. **During a collector outage every pod writes plain-text lines to stderr
   every 1–5 s and nothing counts the dropped spans/logs** (SDK-4, SDK-5).
6. **`resource.WithProcess()` ships `process.command_args` and `process.owner`
   to Prometheus (`target_info`), Tempo and Loki** (SDK-6).
7. **The in-process cardinality guard is set to 1,024,000 attribute sets per
   stream** (SDK-7), so an accidental `chat.room.id` label is not caught until
   the pod is out of memory.
8. **Upgrading newchat to v0.12.0 (the release that adds the ES metrics) is a
   compile break** (`elasticsearch.NewClient` now takes a `MeterProvider`); the
   checklist is in §5.

---

## 2. Priority table

| ID | Sev | Owner | Title | Status |
|---|---|---|---|---|
| APP-1 | P0 | newchat/ops | Production manifests leave observability off and identity unset | CONFIRMED |
| APP-2 | P0 | newchat | 7 `promauto` metrics (incl. a "hard alert") are never exported | CONFIRMED |
| SDK-1 | P0 | o11y | `/metrics` returns HTTP 500 permanently after one reserved-label collision | CONFIRMED |
| SDK-2 | P1 | o11y | Link-consistent sampling across NATS hops (`WithLinkConsistentSampling`) | CONFIRMED |
| SDK-3 | P1 | o11y | Prometheus default-registry bridge (`WithPrometheusGatherers`) + docs | CONFIRMED |
| SDK-4 | P1 | o11y | Route OTel internal errors through the SDK logger; helper + docs | CONFIRMED |
| SDK-5 | P1 | o11y | SDK self-telemetry: dropped spans/logs, export failures, `sdk.Health()` | CONFIRMED |
| SDK-6 | P1 | o11y | `resource.WithProcess()` leaks `process.command_args`/`process.owner` | CONFIRMED |
| SDK-7 | P1 | o11y | In-process cardinality limit is effectively disabled (1,024,000) | CONFIRMED |
| SDK-8 | P1 | o11y | Soften the v0.12.0 `elasticsearch.NewClient` break; upgrade guide | CONFIRMED |
| APP-3 | P1 | newchat | Upgrade to o11y v0.12.0 (ES metrics) — see §5 | CONFIRMED |
| APP-4 | P1 | newchat | 16 services use `natsutil.Connect` without NATS metrics (federation lane blind) | CONFIRMED |
| APP-5 | P1 | newchat | 10 raw `session.ExecuteBatch` sites bypass the Cassandra seam | CONFIRMED |
| APP-6 | P1 | newchat | `pkg/obs` must call `otel.SetErrorHandler` / `otel.SetLogger` | CONFIRMED |
| SDK-9 | P2 | o11y | Profiling closer ignores the shutdown ctx (can hang ~30 s) | CONFIRMED |
| SDK-10 | P2 | o11y | gin: panics are metric-less and status-less when Recovery runs first | CONFIRMED |
| SDK-11 | P2 | o11y | NATS: document that the consumer span ends when the callback returns | CONFIRMED |
| APP-7 | P2 | newchat | `natsrouter` dispatches to a goroutine, so `process` spans are ~0 s | CONFIRMED |
| SDK-12 | P2 | o11y | NATS: no panic recovery in `Subscribe`/`Consume`; JetStream poison loop | CONFIRMED |
| SDK-13 | P2 | o11y | redis `ClusterClient`: `CLUSTER SLOTS` reload on every scrape; `Wrap` has no deadline | CONFIRMED |
| SDK-14 | P2 | o11y | `WithTraceSampler(nil)` silently discards an earlier `WithSamplingRatio` | CONFIRMED |
| SDK-15 | P2 | o11y | `WithEnvironment` is case/whitespace-sensitive → boot failure | CONFIRMED |
| SDK-16 | P2 | o11y | Public-ingress propagator and inbound-baggage stripping helpers | CONFIRMED |
| SDK-17 | P2 | o11y | Collector-outage runbook, `WithOTLPCompression`, queue-size options | CONFIRMED |
| SDK-18 | P2 | o11y | SDK-owned NATS client metrics (adopt newchat's `pkg/natsmetrics` vocabulary) | CONFIRMED |
| APP-8 | P2 | newchat | `restyutil`/`pkg/oidc` bypass `o11y/resty` and `o11y/http` | CONFIRMED |
| APP-9 | P2 | newchat | No `obsctx` use; many `slog` calls without ctx on request paths | CONFIRMED |
| APP-10 | P2 | newchat/ops | Deployment checklist (otel-nats env overrides, JetStream filters, scrape target, proxy) | PLAUSIBLE |
| SDK-19..29 | P3 | o11y | Backlog (views by unit, body-size views, Cassandra seam, Mongo lifecycle, dynamic log level, `o11ytest`, docs…) | mixed |
| APP-11 | P3 | newchat | 7 cron/one-shot services have no observability at all | CONFIRMED |

---

## 3. Findings

### P0 — before newchat goes live

#### APP-1 · Production manifests leave observability off and identity unset

- `newchat/pkg/obs/obs.go:66-76` — `Enabled bool env:"O11Y_ENABLED" envDefault:"false"`;
  `options()` (`:130-140`) then forces all four pillars off.
- No file under `*/deploy/**` (`docker-compose.yml`, `azure-pipelines.yml`)
  sets `O11Y_ENABLED`, `SERVICE_VERSION`, `DEPLOY_ENV`, `OTEL_TRACES_SAMPLER`.
  Only `docker-local/compose.services.yaml:43` and `tools/dev/dev.sh` set
  `O11Y_ENABLED=true`.
- 18 deploy compose files have no `OTEL_SERVICE_NAME` (all `teams-*`,
  `user-presence-service/sync`, three `data-migration/*`,
  `message-worker/deploy/docker-compose.approle.yml`, …); several of those do
  call `obs.Init` and would report `service.name=unknown-service`.
- Consequence: the launch deploy, as reviewable today, has no traces, no
  `/metrics` listener, no NATS trace headers, `deployment.environment=development`
  and `service.version=dev`.
- Fix (newchat/ops): set `O11Y_ENABLED=true`, `OTEL_SERVICE_NAME`,
  `SERVICE_VERSION=<build tag>`, `DEPLOY_ENV=production`,
  `OTEL_TRACES_SAMPLER=parentbased_traceidratio` + `_ARG` in every production
  manifest. Fix (o11y, small): warn at `Init` when the environment is
  `production` but `service.name` looks like a placeholder or the OTLP endpoint
  is `localhost`; ship a k8s `Deployment` env-block example under `examples/`.

#### APP-2 · Seven `promauto` metrics are never exported

- `newchat/pkg/atrest/metrics.go:22-51` (`atrest_dek_cache_hits_total`,
  `_misses_total`, `atrest_dek_creations_total`, `atrest_kek_wrap_total`,
  `atrest_kek_unwrap_total`, **`atrest_kek_renewal_failures_total`** — help:
  "treat as a hard alert") and `bot-message-worker/metrics.go:9`
  (`bot_msg_worker_permanent_error_total`) use `promauto` on client_golang's
  default registry.
- The SDK's Prometheus path is a private registry
  (`internal/metrics/metrics.go:333`, `prometheus.NewRegistry()`); no newchat
  service mounts `promhttp` (only `tools/loadgen`, `tools/clientsim`).
- Consequence: a Vault token-renewal failure (encryption stops when the token
  expires) is invisible. The metrics are incremented on hot paths in six
  services.
- Fix (newchat): port to the OTel meter as `search-service/metrics.go` already
  did; extend `pkg/obs/instrument_registry_test.go` to fail on any
  `promauto`/`prometheus.New*` outside `tools/`. Fix (o11y): SDK-3.

#### SDK-1 · `/metrics` returns HTTP 500 permanently after one reserved-label collision

- `internal/metrics/metrics.go:338-346` promotes `service.name`,
  `service.namespace`, `service.version`, `deployment.environment.name` to
  constant labels; `:398-400` uses `promhttp.HandlerFor` with the default
  `HTTPErrorOnError`.
- otelprom appends the constant labels after the datapoint's labels with no
  de-dup (`exporters/prometheus@v0.65.0/exporter.go:268-269, 493-505`). Any
  instrument recorded with an attribute that sanitises to one of those four
  names fails `NewConstHistogram`/`NewConstMetric`, and the whole scrape
  returns `500 duplicate label names in constant and variable labels`.
  Reproduced with a scratch test. Because aggregation is cumulative the bad
  series lives until restart.
- newchat hit exactly this on 2026-08-19 (`docs/specs/o11y/o11y-metrics-inventory.md`
  §2.1) and now carries the rule in comments in `pkg/natsmetrics/metrics.go:251-256`.
- Consequence: one `Record(..., attribute.String("service.name", …))` anywhere
  in 40 services blanks that pod's dashboards and alerts (including runtime
  and HTTP metrics) until redeploy.
- Fix: (a) `promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError,
  ErrorLog: <slog adapter>}` so a scrape degrades to "that family missing";
  (b) a global view `AttributeFilter` that drops the four reserved keys (they
  are already listed in `reservedHTTPServerPromLabels`, `options.go:507-518`)
  so the collision cannot form; (c) a unit test that records with
  `service.name` and asserts a 200.

### P1 — o11y before launch; newchat before production scale

#### SDK-2 · Link-consistent sampling across NATS hops

- Upstream `otelnats/conn_traced.go:245-272` (`wrapMsgHandler`),
  `oteljetstream/consumer_traced.go:80,184,232`, `oteljetstream/consumer.go:285`:
  every consumer span is `tracer.Start(context.Background(), …, WithLinks(origin))`.
  o11y builds `ParentBased(TraceIDRatioBased(r))` (`o11y.go:519`), which sees
  no parent and re-rolls the ratio on the new trace ID at every hop.
- Scratch test `TestConsumerHopIsSampledIndependently`: producer AlwaysSample,
  consumer `ParentBased(TraceIDRatioBased(0))`, publish→subscribe through the
  facade → `producer sampled=true, consumer spans recorded=0`.
- `docs/guide.md:225-229` ("the unsampled traceparent then flows through
  JetStream, and downstream workers using ParentBased also avoid recording
  those traces") and ADR 0015's cascade claim are **false** for the NATS
  facade in both directions.
- newchat's own spec (`docs/specs/o11y/o11y-upstream-sampling-requirement.md`)
  asks for exactly this and runs 100 % sampling until it exists
  (~10–20 spans per message, `o11y-performance-and-sampling.md`).
- Consequence: at ratio r across k hops a complete flow survives with
  probability r^k (0.1 over the 5–6-trace message flow ≈ 1e-5..1e-6); tail
  sampling cannot repair it (different trace IDs).
- Fix (this repository, no upstream change): `sdktrace.SamplingParameters`
  carries `Links`, so a sampler can decide from them. Ship
  `o11y.LinkBasedSampler(base sdktrace.Sampler)` — for a root span with links,
  `RecordAndSample` iff any link is sampled, `Drop` if links exist and none is
  sampled, otherwise delegate to `base` — and `o11y.WithLinkConsistentSampling()`
  that composes it with the ratio/typed sampler (default off, ADR amendment to
  0015). Scratch test `TestLinkAwareSamplerRemedy` shows `consumer spans
  recorded=1` in the same setup. Because child spans use `ParentBased`, an
  unsampled consumer span drops its DB/HTTP children too, so the whole flow is
  kept or dropped as a unit. Also fix `docs/guide.md` and ADR 0015.

#### SDK-3 · Prometheus default-registry bridge and documentation

- ADR 0002 §2 chooses the private registry deliberately but offers no opt-in
  bridge; README/guide never say that `promauto` metrics will not appear.
- Fix: `o11y.WithPrometheusGatherers(g ...prometheus.Gatherer)` that wraps
  `prometheus.Gatherers{reg, g...}` before `metricscap.NewGatherer`
  (`internal/metrics/metrics.go:383-386`), documented as a migration aid with
  the constant-label caveat; plus a loud README note. Keeps the zero-global
  default.

#### SDK-4 · Route OTel internal errors through the SDK logger

- OTel's global error handler and logger default to `stdr` on `os.Stderr`
  (`otel@v1.44.0/internal/global/internal_logging.go:21`). During a collector
  outage each pod prints
  `2026/09/06 06:07:40 traces export: Post ".../v1/traces": dial tcp ...: connection refused`
  every 5 s (traces) and every 1 s (logs) — non-JSON, bypassing the pipeline
  and any parser. Neither o11y (ADR 0003 forbids touching globals) nor
  newchat's `pkg/obs` calls `otel.SetErrorHandler`/`otel.SetLogger`
  (verified: no matches in newchat).
- Fix: ship `o11y.OTelErrorHandler(logger *slog.Logger) otel.ErrorHandler` and
  `o11y.OTelLogr(logger) logr.Logger` (rate-limited, structured, counting into
  SDK-5's failure counter), and document them as a required wrapper step next
  to `otel.SetTracerProvider`. newchat: APP-6.

#### SDK-5 · SDK self-telemetry

- Verified defaults: BatchSpanProcessor queue 2048, drop-newest, export
  timeout 30 s, connection-refused **not** retried (`otlptracehttp` treats
  only `Temporary()` errors as retryable; ECONNREFUSED is not) — a 10 s
  collector restart loses essentially all spans from that window silently.
  sdk/log BatchProcessor queue 2048, **drop-oldest**, 1 s interval; stdout
  keeps flowing (50k `Info` against a stuck collector: 2.3 µs/record).
- Nothing exposes the drop counts or the last export error; newchat's
  follow-ups F4 ask for "the BatchSpanProcessor dropped-span counter" and
  `sdk.Toggles` is advertised "for health-check endpoints" but there is no
  helper.
- Fix: wrap the exporters to count `o11y_export_failures_total{signal}`,
  `o11y_spans_dropped_total`, `o11y_log_records_dropped_total` (drops are
  observable by wrapping the processor/queue or by diffing `ForceFlush`
  errors), and `sdk.Health() (Status)` returning toggles, last export error
  and time, for a `/readyz` informational check.

#### SDK-6 · `resource.WithProcess()` leaks `process.command_args` and `process.owner`

- `o11y.go:454-457` and `internal/metrics/metrics.go:517-519`. `WithProcess()`
  adds pid, executable path, **`process.command_args`**, **`process.owner`**,
  runtime. otelprom's `target_info` carries the whole resource unfiltered;
  every span and OTLP log record carries it too. Reproduced scrape line:
  `target_info{…process_command_args="[…,\"--db-password=hunter2\"]",process_owner="root",…}`.
- Consequence: any credential passed as a CLI flag lands in Prometheus TSDB,
  Tempo and Loki. newchat configures via env so the exposure is low today,
  but it is one flag away.
- Fix: replace with `WithProcessPID()`, `WithProcessExecutableName()`,
  `WithProcessRuntimeName()`, `WithProcessRuntimeVersion()`; add
  `WithResourceAttributes(...)` / `WithResourceDetectors(...)` for callers who
  want more. Add `resource.WithTelemetrySDK()` at the same time (SDK-23).

#### SDK-7 · In-process cardinality limit is effectively disabled

- `internal/metrics/metrics.go:464-489` derives the SDK cardinality limit as
  `MaxUniqueRoutes × 16 × 64` = **1,024,000** at defaults, versus OTel's
  default 2,000. Reproduced: 3,000 distinct `chat.room.id` values on a counter
  → 3,000 exported series, no `otel.metric.overflow`.
- The export-boundary caps (`metricscap`) protect only `http.route` and
  `db.collection.name`; application instruments have no guard at all.
- Consequence: on a chat platform the temptation to label by room/site/user is
  real; one such instrument can hold ~1 M attribute sets per pod (hundreds of
  MB) and ship them all on every scrape.
- Fix: keep the export caps but set the SDK limit to
  `max(2000, 4 × MaxUniqueRoutes)` (or similar), add `WithCardinalityLimit(n)`
  for conscious overrides, and consider a generic
  `WithMaxUniqueAttributeValues(instrument, key, n)` export cap for app metrics.

#### SDK-8 · Soften the v0.12.0 `elasticsearch.NewClient` break

- CHANGELOG [0.12.0] documents the break; newchat's `pkg/searchengine/factory.go:79`
  calls `o11yes.NewClient(*esCfg, obs.TracerProvider())` and its
  `Observability` interface (`observability.go:12-14`) lacks `MeterProvider()`.
- Fix: keep the new signature, but add a "Migrating from v0.11.0" section to
  `docs/guide.md` with the two-line diff, and consider `NewClientWithTracer`
  (deprecated) for one release. newchat: APP-3 (§5).

#### APP-4 · 16 services use `natsutil.Connect` without NATS metrics

- `natsutil.ConnectWithMetrics` is used by 14 services; plain `Connect` by 16,
  including **outbox-worker** and **inbox-worker** (the federation lane),
  push-notification-service, roomlist-worker, search-sync-worker,
  bot-message-worker, hr-sync-worker, admin-service, botplatform-service and
  all `data-migration/*`. `pkg/natsmetrics` is imported by 14 `main.go`s.
- newchat's own reviews say it: `availability-performance-2026-09-01.md` §4.2
  "The federation path is entirely unmonitored"; contract §11 alerts
  (`NATSTerminalDelivery`, consumer-loop-dead) cannot fire for those lanes.
- Fix (newchat): wire `ConnectWithMetrics` + `natsmetrics.Publisher/Consumer`
  in at least outbox-worker, inbox-worker, push-notification-service,
  search-sync-worker, roomlist-worker, bot-message-worker. Fix (o11y): SDK-18
  makes this automatic.

#### APP-5 · 10 raw `session.ExecuteBatch` sites bypass the Cassandra seam

- `history-service/internal/cassrepo/reactions.go:41,47,64,70`, `pin.go:81,101`;
  `bot-message-worker/store_cassandra.go:93,134,183,238`. Only message-worker
  uses `o11ycassandra.ExecuteBatch`. gocql's observers do not see batches
  (ADR 0019), so those writes emit no span, no `db.client.operation.duration`,
  no `cassandra.query.attempts`.
- Fix (newchat): route through `o11ycassandra.ExecuteBatch`. Fix (o11y, P3
  SDK-21): a wrapped session type whose `ExecuteBatch` is instrumented so the
  seam cannot be bypassed; a prominent note in `cassandra/doc.go`.

#### APP-6 · `pkg/obs` must install the OTel error handler and logger

- Pair of SDK-4. Until the helper exists, `pkg/obs.initSDK` can call
  `otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error){ slog.Warn(...) }))`
  and `otel.SetLogger(logr.FromSlogHandler(sdk.Logger.Handler()))` right after
  `o11y.Init`.

### P2 — soon after launch

#### SDK-9 · Profiling closer ignores the shutdown ctx

- `internal/profiling/profiling.go:85-98` calls `profiler.Stop()`;
  pyroscope-go's `Stop` waits for the flush and the upload goroutines (HTTP
  timeout 30 s, `api.go:70`, `remote.go:119-126`). The SDK runs it before the
  trace flush (`o11y.go:427-430`).
- Consequence: with Pyroscope/Alloy down, `Shutdown` can hang ~30 s past the
  caller's budget → SIGKILL and the trace flush never runs. newchat has
  profiling off today, so P2.
- Fix: run `Stop()` in a goroutine and `select` on `ctx.Done()`; move the
  profiler closer after `tpShutdown`.

#### SDK-10 · gin: panics are metric-less and status-less when Recovery runs first

- `gin/middleware.go:22-33` returns `[otelgin.Middleware, ErrorRecorder]` with
  no recovery; otelgin records status and the duration metric only after
  `c.Next()` returns. With the `gin.Default()` shape (Recovery outermost) a
  panicking request yields HTTP 500, **zero** `http.server.request.duration`
  samples and a span with `status=Unset` (scratch test
  `TestRecoveryBeforeMiddleware_LosesMetricAndStatus`).
- newchat uses `gin.New()` and places the o11y middleware before
  `gin.Recovery()` in all ten HTTP services, so it is not affected today.
- Fix: make the facade order-independent — a first handler that `recover()`s,
  stamps `span.RecordError`/`SetStatus(Error)`/`http.response.status_code=500`,
  and re-panics so gin's Recovery still runs; or a `WithRecovery()` option and
  a "never `gin.Default()`" note.

#### SDK-11 / APP-7 · NATS consumer span ends when the callback returns

- `otelnats/conn_traced.go:268-271`: `defer span.End()` around the handler
  callback. newchat's `pkg/natsrouter/router.go:1033-1071` spawns a goroutine
  per message and returns immediately, so every `process` span is ~µs long and
  all children (Mongo, Valkey, `Respond`) start after the parent has ended.
  Tempo timelines show children outside the parent; `process` latency panels
  are meaningless (newchat's `rpc.server.call.duration` metric is correct, so
  the metric side is fine).
- Fix (newchat): start a child span inside the goroutine (or run the handler
  synchronously). Fix (o11y): document the contract in `nats/doc.go`; consider
  an upstream option for handler-owned span end.

#### SDK-12 · NATS: no panic recovery in `Subscribe`/`Consume`

- No `recover()` in otel-nats or the facade; nats.go recovers only in
  `netchan.go`. A JetStream `Consume` handler panic kills the worker, the
  message is redelivered, the worker dies again (poison loop). newchat's
  consume loops carry their own guard (`MarkHandlerPanic`), the router has
  Recovery middleware.
- Fix: `nats.WithRecover(func(ctx, msg, r any))` (or default
  recover-record-`Nak`-and-continue for JetStream) that also stamps
  `error.type` on the consumer span.

#### SDK-13 · redis `ClusterClient`: `CLUSTER SLOTS` per scrape; `Wrap` has no deadline

- `redis/metrics.go:132-142` observes pool stats via `client.ForEachShard`,
  which is `ReloadOrGet` → unconditional `Reload` in go-redis v9.9.0
  (`osscluster.go:877-884, 914-920, 1160-1166`). newchat uses `ClusterClient`
  in ~15 services: one `CLUSTER SLOTS` round trip and a cluster-state swap per
  pod per scrape. `redis/client.go:308` installs hooks through `ForEachShard`
  with `context.Background()` — network I/O with no deadline at `Wrap` time.
- Fix: keep a shard registry from the `OnNewNode` hook and read `PoolStats()`
  from it; give `Wrap` a ctx or `WithSetupTimeout`.

#### SDK-14 · `WithTraceSampler(nil)` discards an earlier `WithSamplingRatio`

- `options.go:176-182` sets `samplingRatioSet = false` even for nil. Verified:
  `[WithSamplingRatio(0), WithTraceSampler(nil)]` → root spans sampled. The
  docstring says nil "leaves sampling unset". newchat never passes nil, but a
  wrapper that always appends `WithTraceSampler(samplerOrNil)` would run at
  100 % silently.
- Fix: `if sampler == nil { return }`.

#### SDK-15 · `WithEnvironment` is case/whitespace-sensitive

- `o11y.go:637-649`: `"Production"`, `"PROD"`, `" production"` → `Init` error →
  CrashLoopBackOff. Fix: lower-case and trim before the alias lookup.

#### SDK-16 · Public-ingress propagator and inbound-baggage stripping

- The SDK propagator always includes `Baggage{}` (`internal/trace/trace.go:65-68`)
  and nothing in `gin/`, `http/`, `nats/` strips inbound baggage. A client can
  send `baggage: user.name=admin,chat.room.id=x` and it lands on the entry span
  and every log line. newchat built `obs.PublicIngressPropagator()`
  (TraceContext-only) and `ContextWithPublicIdentity` (NATS-side clear) itself.
- Fix: adopt them into the SDK — `o11y.PublicIngressPropagator()` and a
  `gin`/`http` option (`WithInboundBaggage(Strip)`) plus
  `o11y.ContextWithoutBaggage`, with the README's "inbound baggage is
  untrusted" note pointing at them.

#### SDK-17 · Collector-outage runbook and exporter options

- Behaviour verified in SDK-5; only partly documented (`OTEL_BSP_*` in
  guide:212; `OTEL_BLRP_*`, `OTEL_METRIC_EXPORT_*`,
  `OTEL_EXPORTER_OTLP_TIMEOUT` default 10 s, `OTEL_EXPORTER_OTLP_COMPRESSION`
  default none are not).
- Fix: a "when the collector is down" section in `docs/guide.md`;
  `WithOTLPCompression(gzip)` (worth enabling fleet-wide),
  `WithSpanQueueSize`/`WithLogQueueSize`; recommend collector as
  DaemonSet/sidecar with `memory_limiter`.

#### SDK-18 · SDK-owned NATS client metrics

- The `nats` facade emits spans only (ADR 0004 deferral). newchat
  re-implemented seven metric families (`chat.nats.consumer.*`,
  `chat.nats.publish.failures`, `chat.nats.terminal.failures`,
  `rpc.client/server.call.duration`) plus connection-state gauges and a
  slow-consumer counter, and still has to opt every service in twice (APP-4).
- Fix: adopt that vocabulary as SDK-owned instruments on `o11ynats.Conn`
  (connection state, async errors/slow consumer, publish failures by bounded
  cause, consumer disposition and processing duration, request/reply
  durations), scope-filtered views with the SDK buckets, so coverage is
  automatic for every connection.

#### APP-8 · `restyutil` and `pkg/oidc` bypass `o11y/resty` and `o11y/http`

- `pkg/restyutil/restyutil.go:70` and `pkg/oidc/oidc.go:78` wrap
  `otelhttp.NewTransport` on the OTel globals. Because `pkg/obs` does set the
  globals, telemetry flows and the SDK's `http.client.request.duration` view
  applies (it matches by instrument name). What is lost: `o11y/resty`'s
  per-attempt spans, `resty.retry.exhausted`, `server.address` on relative
  URLs (all fixed in v0.12.0), and `WithRouteFromContext` route templating.
- Fix (newchat): `o11yresty.Wrap(client, tp, mp, prop)` in `restyutil.New`,
  `o11yhttp.NewTransport(base, tp, mp, prop)` in oidc. Fix (o11y, docs): a
  "migrating from a bare otelhttp transport" note listing exactly this.

#### APP-9 · No `obsctx` use; `slog` without ctx on request paths

- `grep -rn obsctx newchat` → 0 files. `history-service/internal/service/warmback.go:147`
  builds `context.Background()` for detached work (an `obsctx.Detach`
  candidate); the fleet review §8 lists bot-room-service deferred work on the
  request ctx (ADR 0024's failure mode).
- Uncorrelated `slog` calls on request paths: room-service 20/30,
  notification-worker 9/10, history-service 9/27; fleet-wide 700 non-ctx vs
  290 ctx calls.
- Fix (newchat): adopt `obsctx.Go/DetachWithTimeout`; `sloglint` context-only
  in handler packages. Fix (o11y, docs): link ADR 0024 and
  `examples/background` from the README quick-start; publish a recommended
  `sloglint` config.

#### APP-10 · Deployment checklist (PLAUSIBLE — outside the repo)

- otel-nats resolves `relay > OTEL_NATS_TRACING_ENABLED > option`; an
  `OTEL_NATS_TRACING_ENABLED=true` leftover silently overrides
  `WithTracingEnabled(sdk.Toggles.Trace)`, and `OTEL_NATS_TRACING_ENABLED=""`
  is an error at `Connect` (crash loop). A configured
  `OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT` adds ~4 OpenFeature evaluations per
  message. Audit production manifests for `OTEL_INSTRUMENTATION_GO_*` and
  `OTEL_NATS_TRACING_ENABLED` (o11y: log at `Connect` when the effective switch
  differs from the option).
- Every JetStream consumer must set a single `FilterSubject`, or the concrete
  subject (`room.<id>.msg`) becomes the span name (upstream-owned).
- Prometheus must scrape `:2112` in the real deploy target (no k8s manifests
  in the repo; Dockerfiles have no `EXPOSE`).
- Pods with `HTTPS_PROXY` need `NO_PROXY` for the collector host (OTLP
  exporters use the default transport).

### P3 — backlog

| ID | Item | Evidence |
|---|---|---|
| SDK-19 | Bucket views by unit (`unit="s"` histograms outside SDK scopes get `DefaultLatencyBuckets` unless an advisory is set) | newchat carries `WithExplicitBucketBoundaries` on every histogram as a workaround (`pkg/natsmetrics/metrics.go:206-210`, `search-service/metrics.go:15-19`) |
| SDK-20 | Views for `http.server/client.request/response.body.size` (byte-scale buckets or drop) | otelgin/otelhttp v0.68 emit them with OTel default ms-scale boundaries; `internal/metrics/metrics.go:288-320` has no view |
| SDK-21 | Cassandra: un-bypassable batch seam; cache statement tokenisation (`observer.go:107-140`, per attempt and per page); `db.namespace` cap (C2) | APP-5 |
| SDK-22 | Mongo: tie instrumentation cleanup to the client lifecycle (newchat keeps a `sync.Map` of cleanups, `pkg/mongoutil/mongo.go:17-20`); document `network.peer.*` vs `server.*` label divergence (`mongo/views.go:35-36`) | A8 in newchat docs |
| SDK-23 | Add `resource.WithTelemetrySDK()` (spec-required `telemetry.sdk.*` missing from `target_info`); otelslog scope version is set to the service version (`o11y.go:315-317`) | reproduced |
| SDK-24 | Dynamic log level (`slog.Leveler`), `WithOTLPLogLevel`, log burst limiting | `o11y.go:287-289, 321-324` |
| SDK-25 | `o11ytest.New(t)` returning an SDK wired to span/log/metric recorders | `o11ytest` has only ctx helpers |
| SDK-26 | Pin a released `otelmongo` tag before newchat's production tag | `go.mod:24` pseudo-version |
| SDK-27 | `/metrics` server `WriteTimeout`/`IdleTimeout`, optional `/healthz`, warn-and-continue on bind failure | `internal/metrics/metrics.go:378-404` |
| SDK-28 | Hot-path allocations: baggage `OnStart` calls `span.Attributes()` + per-key `SetAttributes` (14 vs 5 allocs); `MultiHandler` clones twice; NATS facade parses headers twice per message (`nats/conn.go:54-70`) | benchmarks in scratch |
| SDK-29 | Docs: state that `OTEL_TRACES_SAMPLER*` is honoured (newchat re-implemented it believing otherwise); JetStream `FilterSubject` span-name rule; `sampled` field next to `traceId` in logs; k8s env example; log→trace links dead-end for unsampled spans | `docs/guide.md`, README |
| APP-11 | 7 cron/one-shot services (`teams-*`, `es-index-migrator`) have no `obs.Init` at all; a Mongo write-storm from an HR sync is unattributable | `teams-hr-sync/main.go:141-143` passes `noop.NewTracerProvider()` |

---

## 4. Suggested o11y release plan

| Version | Contents | Why together |
|---|---|---|
| **v0.13.0** (before newchat launch) | SDK-1, SDK-2, SDK-3, SDK-4, SDK-6, SDK-7, SDK-8 docs, SDK-14, SDK-15, minimal SDK-5 (failure counters via the error handler) | All are additive or hardening; none changes existing series names. SDK-2 is opt-in. |
| **v0.14.0** | SDK-5 (full), SDK-9, SDK-10, SDK-12, SDK-13, SDK-16, SDK-17, SDK-11 docs | Behavioural changes to shutdown, gin, NATS handlers; announce. |
| **v0.15.0** | SDK-18 (NATS metrics, new series), SDK-19/20 (bucket changes), SDK-21/22 | Telemetry-shape changes grouped so dashboards move once. |
| later | SDK-23..29, and the open wave-2/3 items from the 2026-08-15 plan (C1 nil-provider, B2/N1 OTLP env precedence) | as planned there |

Keep the rule from the 2026-08-15 plan: changes that alter the shape of
telemetry must not share a release with each other.

---

## 5. newchat v0.11.0 → v0.12.0 upgrade checklist

Compile breaks (CONFIRMED against `elasticsearch/client.go:141,177`):

1. `pkg/searchengine/factory.go:79`:
   `o11yes.NewClient(*esCfg, obs.TracerProvider())` →
   `o11yes.NewClient(*esCfg, obs.TracerProvider(), obs.MeterProvider())`
   (`mp` nil is rejected).
2. `pkg/searchengine/observability.go:12-14`: add
   `MeterProvider() metric.MeterProvider` (mirror `pkg/cassutil`); update the
   doubles in `observability_test.go:21` and `integration_test.go:29`.
3. Nothing else changes shape; drivers already match o11y's `go.mod`
   (go-elasticsearch v8.19.3, mongo-driver v2.7.0, gocql v1.7.0, nats.go
   v1.50.0, minio v7.2.0, gin v1.12.0, resty v2.17.2, OTel v1.44.0).

Behaviour changes to plan for:

4. New series `db_client_operation_duration_seconds{db_system_name="elasticsearch",…}`
   from search-service and search-sync-worker; index label capped by
   `WithMaxUniqueCollections` under its own budget. Decide whether
   `search_service_es_duration_seconds` stays.
5. Redis `db_client_connection_create_time_bucket` boundaries change from OTel
   ms defaults to `DefaultLatencyBuckets()` — update PromQL on it.
6. `traceId`/`spanId` hoisted to top level under `WithGroup` — newchat has no
   `WithGroup` callers, no effect; `pkg/logctx/handler.go` is compatible with
   the clone-before-mutate contract.
7. Resty retry fixes do **not** apply until APP-8 (newchat wraps resty with
   `otelhttp`, not `o11y/resty`).
8. Refresh newchat docs that still say "ES is trace-only" or cite v0.9.1.

---

## 6. Checked and found fine (so nobody re-audits it)

- `Init` never dials the collector (1.4 ms with an unreachable endpoint); all
  failure paths shut down what was built. `Shutdown` is idempotent, sequential,
  honours the caller's ctx for trace/log/metric closers (SDK-9 is the exception).
- Logging never blocks on a stuck collector; both queues are bounded; `Debug`
  when disabled costs 12 ns / 0 allocs; `leveledHandler` survives
  `With`/`WithGroup`; `traceId` top-level incl. `WithGroup`.
- Baggage span processor only runs for recording spans; `chat.*` keys are not
  semconv-reserved; resource-key collision check at `Init`.
- Sampler precedence when used correctly: typed option wins, otherwise
  `OTEL_TRACES_SAMPLER*` is honoured (newchat's belief that it is ignored is
  wrong — SDK-29).
- `WithOTLPHeaders` reaches traces, logs and OTLP metrics; not logged.
- Prometheus naming, scope labels, export-boundary caps, exemplar filtering
  all behave as documented; runtime metrics use the new `go_*` names.
- NATS: W3C headers propagate verbatim-case with canonical fallback;
  `Respond` goes through the traced publish; `Request` shim parents and
  derives a deadline; `Fetch` buffers sized to `batch`; `MessageBatch.Stop`
  releases both goroutines; `WithTracingEnabled(false)` with no env/relay
  builds only the direct implementation (zero OTel code on the hot path).
- gin: 404s get span name `GET` and no `http.route` (bounded);
  `WithSkipPaths()` skips exact probe paths; `ErrorRecorder` records typed
  events. `http.server.request.duration` allow-keys drop Host-derived attrs.
- redis: command text off by default, connection-lifecycle and Pub/Sub
  commands filtered, per-node hooks fire for cluster commands and pipelines,
  pool views complete.
- mongo: no `_oteltrace` written; `db.query.text` off; bounded `error.type`.
  cassandra: bound values never captured; batch seam records one span per
  batch. elasticsearch: `tp`/`mp` required; typed vs low-level failure
  classification follows each API's contract; root build graph gated.
  minio: bounded labels; ListObjects contract documented.
- Semconv v1.39.0 keys on all hot instruments resolve against the pinned
  package; `MetricViews` for every o11y-owned instrument are scope-filtered.
- newchat: all 30 `obs.Init` services call SDK shutdown last; all ten gin
  services use `gin.New()` with the o11y middleware before `gin.Recovery()`;
  custom instrument labels come from closed enums with a registry guard test;
  `pkg/obs` sets the OTel globals so bare `otelhttp` transports do export.

---

## 7. Addendum — re-check against newchat `main@e48a37f` (o11y v0.12.0)

newchat has since merged one commit (#472): `go.mod` → o11y v0.12.0,
`pkg/searchengine` passes the `MeterProvider`, and four `docs/specs/o11y/*`
files were refreshed. Re-running every check above against that head:

| ID | Before | After | Note |
|---|---|---|---|
| APP-3 | open | **done** | `factory.go:80` passes `obs.MeterProvider()`; `Observability` has `MeterProvider()`; doubles updated; docs refreshed. |
| SDK-8 | open | **moot for newchat** | The migration was done by hand; a guide section is still worth adding for other consumers. |
| APP-1 | open | open | Still only `docker-local/` sets `O11Y_ENABLED`; the one `deploy/` file with any of the variables is `chat-frontend`; 18 compose files still lack `OTEL_SERVICE_NAME`. |
| APP-2 | open | open | `pkg/atrest/metrics.go` and `bot-message-worker/metrics.go` still use `promauto`. |
| APP-4 | open | open | 14 `ConnectWithMetrics` vs 17 `Connect` in `main.go`. |
| APP-5 | open | open | 10 raw `ExecuteBatch` sites. |
| APP-6..APP-11 | open | open | No `SetErrorHandler`/`SetLogger`, goroutine dispatch in `natsrouter`, bare `otelhttp` in `restyutil`/`oidc`, 0 `obsctx` users, 6 services without `obs.Init`. |
| SDK-1..7, 9..29 | open | open | Nothing on the newchat side can close these. |

Net: 1 of 42 items closed. The v0.12.0 upgrade delivers the ES metric to
search-service and search-sync-worker (and the Redis create-time bucket fix),
which is what the release was for; it does not touch the P0 items.

One new SDK item surfaced by newchat's upgrade notes
(`storage-dependency-metrics.md` §4):

#### SDK-30 · P2 · Low-level ES client counts routine 404s as failures; no per-operation override — CONFIRMED

- The facade follows `esapi.Response.IsError` (status > 299) for the low-level
  client (`elasticsearch/client.go:454-487`); the typed client's accept-list
  exemption does not apply. `elasticsearch` exposes only `WithSearchBody` and
  `WithCollectionMetricLabel`.
- newchat's `GetDoc` (a per-request Valkey-miss path) and `GetIndexMapping`
  legitimately 404, so every such call records `error_type="404"` and sets the
  span status to Error. newchat had to write a two-selector PromQL union to
  exclude 404 for exactly those two `db_operation_name` values, and pins the
  operation strings with tests so the query cannot drift.
- Fix: `elasticsearch.WithAcceptedStatuses(op string, statuses ...int)` (or a
  `WithSuccessFn(func(op string, status int) bool)`) applied to both the
  metric's `error.type` and the span status, mirroring the typed client's
  accept list; document the low-level default prominently.
