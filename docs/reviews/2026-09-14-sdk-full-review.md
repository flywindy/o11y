# o11y SDK full review (2026-09-14)

A complete pass over the o11y SDK at `main` after PR #100 merged (commit
`afbd622`, the state a v0.13.0 tag would cut), read as a senior engineer /
SRE would before the chat platform ("newchat", ~40 Go services) goes to
production on it.

> **Status**: findings only; nothing here is implemented. Every finding is
> marked CONFIRMED (verified against source, a scratch test, or the pinned
> upstream module in the module cache) or PLAUSIBLE (inference not exercised
> at runtime).

Method:

- Automated tools on the tree: `golangci-lint` with the repo config and with a
  broad 70-linter set, `staticcheck -checks=all`, `gosec -tests=true`,
  `make sast` (semgrep credential rule + rule tests), the ADR gate
  (`scripts/check_integrations.go`), `go vet`, `go test -race`, and
  statement coverage with cross-package attribution (`-coverpkg=./...`).
  `govulncheck` could not reach `vuln.go.dev` from the review container; the
  CI run on `afbd622` (run 515) is green, and it includes the govulncheck job.
- Five independent read-every-line passes: core (`o11y.go`, `options.go`,
  `diagnostics.go`, `internal/{log,trace,exportstats,repeat,redact,profiling,
  baggageattrs,testutil}`, `obsctx`), metrics (`internal/{metrics,metricscap,
  views}`, `o11ytest`), transports (`nats`, `gin`, `http`, `resty`), data
  stores (`mongo`, `redis`, `cassandra`, `elasticsearch`, `minio`), and a
  docs / public-API / test-coverage audit (README, guide, semconv catalog,
  CHANGELOG, all 27 ADRs, `go doc -all` of every public package).
- Upstream behaviour checked on disk: otel sdk v1.44.0, sdk/log v0.19.0,
  otlptracehttp / otlpmetrichttp v1.44.0 (`internal/otlpconfig`), otlploghttp
  v0.19.0, exporters/prometheus v0.65.0, otlptranslator v1.0.0, otelgin /
  otelhttp v0.68.0, go-redis v9.9.0, mongo-driver v2.7.0, gocql, resty
  v2.17.2, nats.go, pyroscope-go v1.3.0.
- Every finding an agent raised was re-read by the author of this document
  against the code and, where it is marked CONFIRMED, against the upstream
  module or a scratch test. Findings not surviving that check were dropped.
- The [2026-09-06 review](./2026-09-06-newchat-launch-readiness-review.md)
  is referenced by its IDs (SDK-n); §5 gives the status of each. Nothing
  already fixed there is re-derived.

IDs: **R-n** is a finding of this review.

## 1. Executive summary

The SDK is in good shape. The tooling gates are clean (repo lint 0, semgrep 0,
ADR gate pass, race tests green, 85.5 % statement coverage), ADR 0003 (no OTel
globals, no env mutation) holds in every package, and the areas the previous
review flagged as P0/P1 are fixed. What remains is a second tier:

1. **Three metric-correctness bugs** an operator would act on wrongly:
   the Mongo pool gauge drifts below reality after any failed handshake
   (R-2), the Mongo operation histogram never carries exemplars and trips
   `otel.Handle` on every scrape (R-1), and one attribute with an empty key
   removes a whole family from `/metrics` until restart (R-9).
2. **Four credential paths the SDK's own no-`@` rule does not cover**: the
   profiler's log adapter at DEBUG (R-3), `nats.Connect` errors (R-5),
   `url.full` on resty client spans (R-6), and `Shutdown`'s logged error
   (R-23).
3. **Endpoint configuration contract**: `OTEL_EXPORTER_OTLP_ENDPOINT` and
   its per-signal forms are validated at `Init` (since #100) but never used,
   because `Init` always passes the option's default (R-4); a scheme-less or
   path-prefixed `WithOTLPEndpoint` passes validation and produces an
   exporter that can never deliver (R-25, R-26).
4. **Redis pool observation on Cluster/Ring** is still the shape SDK-13
   described, plus a new angle: `ForEachShard` reloads `CLUSTER SLOTS` on
   every scrape and skips down Ring shards forever (R-7, R-8).
5. **Contract debt against AGENTS.md**: 16 public functions without a test,
   five semconv literals where the pinned package has a constant, two
   SDK-owned attributes missing from the catalog, and no `### Migration`
   section for what will be v0.13.0 (R-13, R-14, R-15, R-16).

None of this blocks tagging v0.13.0 as planned; R-1, R-2, R-4 and the four
credential items are the ones worth folding in before the tag, because each
changes what an operator sees on day one.

## 2. Priority table

| ID | Sev | Area | Finding | Status |
|---|---|---|---|---|
| R-1 | P1 | metrics/mongo | `db.client.operation.duration` (Mongo) never carries exemplars; every scrape calls `otel.Handle` with an exemplar-overflow error | CONFIRMED |
| R-2 | P1 | mongo | `db.client.connection.count{state=idle}` drifts permanently after a handshake that fails before `ConnectionReady` | CONFIRMED |
| R-3 | P1 | profiling | Pyroscope endpoint userinfo is logged verbatim at DEBUG on every upload | CONFIRMED |
| R-4 | P2 | core | `OTEL_EXPORTER_OTLP_{,TRACES_,LOGS_}ENDPOINT` are preflighted but never honoured | CONFIRMED |
| R-5 | P2 | nats | `Connect` errors echo the URL, credentials included | CONFIRMED |
| R-6 | P2 | resty | `url.full` records userinfo and the full query string | CONFIRMED |
| R-7 | P2 | redis | Cluster pool observation issues `CLUSTER SLOTS` on every scrape (SDK-13, new evidence) | CONFIRMED |
| R-8 | P2 | redis | Ring shards that are down at `Wrap` time are never instrumented | CONFIRMED |
| R-9 | P2 | metrics | An attribute with an empty key removes its family from `/metrics` until restart | CONFIRMED |
| R-10 | P2 | gin | `ErrorRecorder` records every error twice as an `exception` event | CONFIRMED |
| R-11 | P2 | nats | `Conn.Request` timeout is `context.DeadlineExceeded`, never `nats.ErrTimeout` | CONFIRMED |
| R-12 | P2 | metrics | No `SDK.ForceFlush`; the OTLP push path advertised for serverless has no flush short of `Shutdown` | CONFIRMED |
| R-13 | P2 | contract | 16 public functions with no test reference (AGENTS.md test rule) | CONFIRMED |
| R-14 | P2 | contract | Literal semconv keys in `redis` where v1.39.0 has constants | CONFIRMED |
| R-15 | P2 | docs | `gin.error.type` and `pyroscope.profile.id` missing from `docs/semconv.md` | CONFIRMED |
| R-16 | P2 | release | Unreleased has no `### Migration`; nine behaviour changes need one | CONFIRMED |
| R-17 | P2 | docs | `WithHistogramBuckets` is documented as HTTP-only but shapes every DB histogram | CONFIRMED |
| R-18 | P2 | redis | Default pool name is a heap address: new series on every restart | CONFIRMED |
| R-19 | P2 | redis | `used` gauge can wrap to 4.29e9 (`uint32` subtraction before widening) | PLAUSIBLE |
| R-20 | P2 | metrics/docs | Custom-metric ergonomics for the fleet are undocumented (private registry, unit suffixes, default buckets) | CONFIRMED |
| R-21..R-45 | P3 | various | Backlog, §3.3 | mixed |

## 3. Findings

### 3.1 P1

#### R-1 · Mongo operation histogram: no exemplars, `otel.Handle` on every scrape

`internal/views/mongo.go:44-60` gives the Mongo `db.client.operation.duration`
stream an `AttributeFilter` (drops `db.namespace`, `db.collection.name`,
`network.transport`, which otelmongo records) but no
`ExemplarReservoirProviderSelector`. `internal/metrics/reserved.go:257-266`
(`withReservedKeyFilter`) composes the reserved-key filter onto every
configured view and also adds no selector; `dropFilteredAttrsExemplarSelector`
is attached only to the two HTTP views (`metrics.go:365,380`) and the
catch-all (`reserved.go:284`). The OTel SDK routes filtered attributes into
`FilteredAttributes`, otelprom renders them as exemplar labels, and
client_golang rejects an exemplar whose labels exceed 128 runes.

Scratch test (realistic namespace + collection): the Mongo histogram exposes
no exemplar and `otel.Handle` receives `exemplar labels have 133 runes,
exceeding the limit of 128`. Even a short namespace leaves
`trace_id`+`span_id` (63) + `network_transport=tcp` (20) +
`db_collection_name=…`, so practically every Mongo operation overflows. With
`ErrorHandler()` installed this is one ERROR per minute forever; with OTel's
default handler it is a stderr line per scrape. `docs/guide.md:326` promises
exemplars for SDK-managed histograms.

The same shape applies to every other view stream with an `AttributeFilter`
(redis, cassandra, elasticsearch, minio) whose upstream records more
attributes than the allow-list; only Mongo was exercised.

Fix: in `withReservedKeyFilter`, set
`stream.ExemplarReservoirProviderSelector = dropFilteredAttrsExemplarSelector`
whenever the stream has a filter (or set it in each `views.*` stream); add an
exemplar test per integration histogram.

#### R-2 · Mongo pool idle gauge drifts after a failed handshake

`mongo/pool_metrics.go:180-184` increments `total` and the idle gauge on
`ConnectionReady`; `closeConnection` (`:306-319`) decrements `total` on
every `ConnectionClosed` and, when the connection is not checked out and
`hadTotal && total >= len(checkedOut)`, decrements idle. mongo-driver v2.7.0
emits `ConnectionCreated → ConnectionClosed(ReasonError)` with no
`ConnectionReady` for a connection whose handshake fails
(`x/mongo/driver/topology/pool.go`, `removeConnection`).

Scenario: two ready idle connections, a third dial fails its handshake.
`total` 2 → 1, idle gauge 2 → 1, reality 2. Every later handshake failure
(auth outage, TLS flap, pool clear + reconnect storm) drifts it further; only
`ConnectionPoolClosed` resets. Scratch test emitting exactly that sequence:
`idle = 1, expected 2`. ADR 0014's count model ("ConnectionReady −
ConnectionClosed = total") has the same blind spot.

Fix: track ready connection IDs (a set filled on `ConnectionReady`) and ignore
`ConnectionClosed` for an ID never seen ready; amend ADR 0014; add the test.

#### R-3 · Profiler log adapter echoes the endpoint's userinfo

`internal/profiling/profiling.go:82` installs `pyroscopeSlogAdapter` and
`:197-213` forward `fmt.Sprintf` output unredacted. pyroscope-go v1.3.0
`upstream/remote/remote.go:193` logs `"uploading at %s", u.String()` on every
upload, where `u` is the parsed `cfg.Address` with userinfo intact
(`String()` does not redact). `redact.URL`'s godoc says the SDK keeps
honouring `http://user:pass@host` profiling endpoints, and `o11y.go:503,534`
redact that same endpoint everywhere else.

Scenario: `WithProfilingEndpoint("http://alice:s3cretpw@pyroscope:4040")` +
`WithLogLevel(slog.LevelDebug)` → every 15 s a DEBUG record
`uploading at http://alice:s3cretpw@…/ingest?…` on stdout and in Loki.
Scratch run reproduced it. Gated on DEBUG, so P1 for the leak class rather
than for likelihood.

Fix: route all three adapter methods through `redact.InText(msg,
cfg.Endpoint)` and `redact.Secrets` with the profiling auth header values;
use `slog.*Context` (AGENTS.md rule 4); consider mapping pyroscope's six
`Infof` startup lines to DEBUG.

### 3.2 P2

#### R-4 · The standard OTLP endpoint variables are validated, then ignored

`internal/trace/trace.go:34` always passes
`otlptracehttp.WithEndpointURL(endpoint)` and `options.go:721` defaults the
endpoint to `http://localhost:4318`, so the option is always set. otlpconfig
v1.44.0 `NewHTTPConfig` (`options.go:92-95`) applies the environment first
and the options after, so the option wins unconditionally; otlploghttp's
option-first lazy resolvers give the same result. Since #100,
`validateOTLPExporterEnv` rejects a malformed `OTEL_EXPORTER_OTLP_ENDPOINT`
(`diagnostics.go:213`), so an operator learns the variable is read and
infers it is honoured.

Scenario: the OTel Operator or a Helm chart injects
`OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.infra:4318` and the
service omits `WithOTLPEndpoint` → traces and logs go to `localhost:4318`,
`o11y_export_failures_total` climbs, and neither README nor the guide says
why. Fix: track whether `WithOTLPEndpoint` was given and pass
`WithEndpointURL` only then (the spec precedence: option > signal env >
generic env > default), or state in the README env table that the SDK does
not honour the endpoint variables and stop preflighting them.

#### R-5 · `nats.Connect` errors carry the URL verbatim

`nats/conn.go:155,158,166` format `"nats connect %s: …", url`. nats.go's own
error does not include the URL; the facade adds it. With
`nats://alice:s3cr3t@nats:4222` and the AGENTS.md logging pattern
(`slog.ErrorContext(ctx, …, slog.Any("error", err))`) the password lands on
stdout and in Loki. Scratch test confirmed the leak and the clean upstream
error. Fix: parse each comma-separated URL and use `Redacted()`, or drop the
URL from the message.

#### R-6 · resty `url.full` keeps userinfo and the query string

`resty/hook.go:405-423` (`targetFromURL`) records `u.String()`. Scratch
test recorded
`url.full = http://bob:hunter2@127.0.0.1:…/orders?access_token=tok123`.
otelhttp v0.68.0, the SDK's other client facade, strips `req.URL.User`
before emitting `url.full` (`internal/semconv/client.go:116-122`), so the
same call is redacted through `o11yhttp.NewTransport` and not through
`o11yresty`. semconv v1.39.0 says `url.full` SHOULD redact credentials and
scrub `AWSAccessKeyId`, `Signature`, `sig`, `X-Goog-Signature`.
`docs/semconv.md:109` currently pushes this to callers. Fix: clear `User`
and scrub those query keys before `String()`.

#### R-7 · Cluster pool observation reloads `CLUSTER SLOTS` per scrape (SDK-13)

`redis/metrics.go:138,154` and `redis/client.go:128,146` use
`ClusterClient.ForEachShard`. go-redis v9.9.0 `osscluster.go:1160-1167`
calls `c.state.ReloadOrGet(ctx)`, and `ReloadOrGet` (`:914-920`) calls
`Reload` first, unconditionally: a `CLUSTER SLOTS` round trip on every
metrics collection (every scrape on the pull path, every export tick on the
push path). ADR 0013 §7.B says "a few atomic-counter reads". Consequences:
scrape latency and failure are coupled to Redis reachability; each scrape
produces a self-inflicted `redis.CLUSTER SLOTS` span and histogram sample
through the hooked node client unless `WithRequireParentSpan(true)`
(PLAUSIBLE, not exercised); `Wrap` itself (`client.go:324`,
`context.Background()`) blocks on that reload with no caller deadline.
`ForEachMaster`/`ForEachSlave`, which the ADR prescribes, also go through
`ReloadOrGet`. Fix: enumerate only the node clients already hooked through
`OnNewNode` (the SDK holds them), and give `Wrap` a `ctx`.

#### R-8 · Ring shards down at `Wrap` time are never instrumented

`Ring.ForEachShard` (go-redis `ring.go:651-663`) skips shards whose
`IsDown()` is true; the shard `*Client` already exists, so no `OnNewNode`
fires when it recovers. Its commands stay unhooked and its pool unobserved
for the process lifetime. ADR 0013 §10 forbids `ForEachShard` for Ring and
requires `GetShardClients()`, but in v9.9.0 `GetShardClients`
(`ring.go:853-862`) also filters on `IsUp()`, so no public enumeration
covers a down shard. Fix: hook the Ring itself with `Ring.AddHook` for
tracing (covers every shard) and document that pool metrics for a shard
start when it is first seen up; add the down-shard test the ADR's plan
lists.

#### R-9 · An empty attribute key removes the family from `/metrics`

`internal/metrics/reserved.go:85-88` treats `""` as not reserved (and
`scrapelog_internal_test.go:150` pins that). `attribute.NewSet` keeps an
empty-key KV, otlptranslator `Build("")` fails with "label name is empty",
and because aggregation is cumulative the family stays broken until restart,
the failure the reserved-key guard exists to prevent. Scratch test: family
absent, WARN `error gathering metrics: label name is empty`. Input like
`attribute.String(os.Getenv("LABEL"), v)` or a map-driven attribute function
is realistic across 40 services. Fix: treat `""` as reserved (drop) and flip
the test.

#### R-10 · `ErrorRecorder` double-records gin errors

`gin/errors.go:32-36` calls `span.RecordError` for every `c.Errors` entry;
otelgin v0.68.0 `gin.go:124` already does `span.RecordError(err.Err)` for
each. Scratch probe on the canonical chain: one `c.AbortWithError` → two
`exception` events (one with `gin.error.type`, one without). Exception
counts in Tempo and alerts read 2× reality. ADR 0010's checklist row
("otelgin does not call `span.RecordError` per error") is false at the pinned
version, and the matrix tests count only events carrying `gin.error.type`, so
they cannot see the duplicate. Fix: emit a distinct `gin.error` event (or
attach `gin.error.type` through `SetAttributes`) instead of a second
`RecordError`; amend ADR 0010; assert the total event count.

#### R-11 · `Conn.Request` timeout surfaces as `context.DeadlineExceeded`

`nats/conn.go:304-308` wraps the timeout in `context.WithTimeout` and calls
`RequestWithContext`, whose timeout path returns `ctx.Err()`. Scratch:
`errors.Is(err, nats.ErrTimeout) == false` on both the traced and the direct
path, while native `nc.Request` returns `nats: timeout` and
`nats.ErrNoResponders` still works through the facade. A service migrating
`nc.Request(subj, d, 2s)` to `conn.Request(ctx, subj, d, 2s)` keeps its
`errors.Is(err, nats.ErrTimeout)` retry / fallback branch and it silently
stops firing. Not mentioned in godoc, guide, README or CHANGELOG. Fix: when
the caller's ctx is still live and the error is `DeadlineExceeded`, return
`fmt.Errorf("%w: %w", nats.ErrTimeout, err)` so both checks hold; document.

#### R-12 · No `ForceFlush`

`options.go:432-441` and README sell `WithMetricsOTLPEndpoint` for
Lambda / Cloud Run, but the only flush is `SDK.Shutdown` and the
PeriodicReader's 60 s tick, and `SDK.MeterProvider()` returns the interface
(a wrapper type on the pull path), so `*sdkmetric.MeterProvider.ForceFlush`
is unreachable. A frozen function loses up to a minute of metrics and the
batchers' queued spans and logs per invocation. Fix: `SDK.ForceFlush(ctx)`
covering the three providers with the same deadline sharing as `Shutdown`.

#### R-13 · Public functions with no test (AGENTS.md "every public function")

CONFIRMED by `grep -rn -w --include=*_test.go` and by cross-package
coverage (0.0 % on each):

| Package | Function |
|---|---|
| root | `WithLogLevel`, `WithRuntimeMetrics`, `WithMetricsOTLPEndpoint` (only a doc mention), `WithProfilingAuthHeaders` (only through `internal/profiling`), `DefaultLatencyBuckets` (copy semantics untested) |
| http | `WithFilter`, `WithMetricAttributesFn`, `WithPublicEndpoint`, `WithSpanNameFormatter` |
| resty | `WithSpanNameFormatter` |
| minio | `WithAWSS3CompatAttributes`, `WithHTTPChildSpans`, `WithObjectKeyAttribute`, `WithSpanNameFormatter`, `MetricViews` |
| redis | `WithCommandTextEnabled` (behaviour tested through the config struct, the option itself never called) |

Behaviour this leaves unexercised: `minio.WithHTTPChildSpans` swaps the
transport (`client.go:107-125`) and the AWS-alias dual-emit branch in
`begin` (`:176-184`), both specified by ADR 0018; the root `Init` OTLP
metrics push path (tests set `cfg.metricsOTLPEndpoint` directly; nothing
asserts caps or the guarded Resource ship through `metricscap.NewExporter`).
`internal/trace.InitTracer` has no test file.

#### R-14 · Literal semconv keys where the pinned package has a constant

`redis/metrics.go:167,170` (`"db.client.connection.state"` = `"used"` /
`"idle"`; `semconv.DBClientConnectionStateUsed` / `Idle` exist),
`redis/metrics.go:186` and `redis/hook.go:65`
(`"db.client.connection.pool.name"`; `semconv.DBClientConnectionPoolName()`
exists and `mongo/pool_metrics.go:363` uses it). AGENTS.md rule 5 and the
`verify-semconv-attributes` guard require the constants. Lower priority:
`o11y.go:359` (`"service.name"` stdout field, ADR 0001) and the reserved-key
table in `internal/baggageattrs/baggageattrs.go:114-120`. Single semconv
version confirmed (41 files import v1.39.0, no other).

#### R-15 · Catalog gaps

`gin.error.type` (`gin/errors.go:13`, SDK-owned) and `pyroscope.profile.id`
(set by `otelpyroscope`, `o11y.go:531`, promised in README and guide) have
no row in `docs/semconv.md` and no Deviations entry, against the catalog's
own "new attribute? update this catalog" rule.

#### R-16 · v0.13.0 needs a `### Migration` section

The Unreleased entry describes every change but, unlike 0.12.0, has no
migration list. A cut needs at least: `Init` now fails on malformed
`OTEL_EXPORTER_OTLP_*`, `OTEL_BSP_*`, `OTEL_BLRP_*`, span/log-record limit,
`OTEL_TRACES_SAMPLER[_ARG]`, `OTEL_GO_X_CARDINALITY_LIMIT`,
`OTEL_METRIC_EXPORT_*` values and on non-token header names (startup-failure
risk, audit env before rollout); `SDK.MeterProvider()` on the pull path is a
wrapper (type assertions to `*sdkmetric.MeterProvider` break); `target_info`
loses `process_command_args`, `process_owner`, `process_executable_path`,
`process_runtime_description` and gains `telemetry_sdk_*`; the per-stream
cardinality limit drops from 1,024,000 to 4,000 (instruments with more
attribute sets now show `otel_metric_overflow="true"`); `OTEL_RESOURCE_ATTRIBUTES`
alias keys are dropped; `WithTraceSampler(nil)` no longer resets a ratio;
`Shutdown` per-component share semantics; `Logr()` at INFO surfaces
previously silent OTel warnings; after `go mod tidy` the four drivers leave
a consumer's go.mod unless imported directly. Also missing from the
CHANGELOG: the SAST tooling (`make tools/sast*`, semgrep rule, CI job) and
the ADR gate now blocking all five driver prefixes.

#### R-17 · `WithHistogramBuckets` reaches every DB histogram

`options.go:462-470`, `options.go:44-48` and README say the buckets apply to
"HTTP server and client latency histograms"; `o11y.go:389-395` passes
`cfg.histogramBuckets` into `views.Cassandra/Elasticsearch/Minio/Mongo/Redis`,
so it also pins `db.client.operation.duration`,
`db.client.connection.create_time` and `minio.client.operation.duration`.
An operator tuning HTTP buckets silently reshapes the DB histograms. Fix the
docs or split the option.

#### R-18 · Redis default pool name is a heap address

`redis/client.go:296`: `fmt.Sprintf("redis-%x", key)` where `key` is the
client pointer. `db.client.connection.pool.name` changes on every restart
and rollout: unbounded label churn and dashboards that cannot key on it.
Mongo uses a process-local sequence for exactly this reason
(`mongo/pool_metrics.go:148-166`); Cassandra derives from host and keyspace.
With 40 services most will not set `WithPoolName`. Fix: `redis-<addr>-<seq>`.

#### R-19 · Redis `used` gauge underflow (PLAUSIBLE)

`redis/metrics.go:165`: `int64(stats.TotalConns - stats.IdleConns)`
subtracts two `uint32` before widening. `ConnPool.Stats()` reads `Len()` and
`IdleLen()` under separate lock acquisitions, so `IdleConns` can momentarily
exceed `TotalConns`; one scrape then reports
`db.client.connection.count{state=used} = 4294967295`, which pages anyone
alerting on saturation. Fix: `max(int64(Total) - int64(Idle), 0)`.

#### R-20 · Custom-metric ergonomics for the fleet

README and guide have no section on application-defined instruments (zero
hits for `WithUnit`, `Int64Counter`, `promauto`, `DefaultRegisterer`,
"custom metric"; `examples/metrics` shows none). Pitfalls nothing warns
about: the private registry (`internal/metrics/metrics.go:396`) means every
existing `promauto` / `MustRegister` metric silently never appears on the
SDK's `/metrics` (SDK-3's `WithPrometheusGatherers` was dropped with PR5;
that decision needs to be written down); otelprom appends unit suffixes and
`_total`, so `Int64Counter("orders.processed")` becomes
`orders_processed_total`; a `Float64Histogram` in seconds gets OTel's
ms-scale default boundaries unless `WithExplicitBucketBoundaries` is passed
(the trap `views/redis.go:70-75` documents for `create_time`), so either
say so or extend the catch-all view to apply `DefaultLatencyBuckets` to
`unit="s"` histograms (SDK-19).

### 3.3 P3 backlog

| ID | Area | Finding | Evidence |
|---|---|---|---|
| R-21 | core | Scheme-less endpoints pass validation and yield an exporter that never sends: `WithOTLPEndpoint("localhost:4318")` parses as an opaque URL (host empty) → every export fails `http: no Host in request URL`; `"127.0.0.1:4318"` fails `Init` with "not a valid URL" and no hint | `diagnostics.go:493-508`, scratch |
| R-22 | core | `WithOTLPEndpoint` with a path prefix (`https://otlp-gateway.example/otlp`, Grafana Cloud's documented form) sends traces and logs to `/otlp` (path kept as-is, `/v1/logs` appended only when the path is empty) → both 404; with R-4 the per-signal env cannot rescue it | `internal/log/provider.go:74-84`, `TestLogEndpointURL` pins it |
| R-23 | core | `Shutdown` logs and returns exporter errors unredacted (`o11y.go:166,170`); net/http masks the password but keeps the username, against the SDK's no-`@` rule | code read |
| R-24 | core | Instrumentation scope version set to the service version (`otelslog.WithVersion(cfg.serviceVersion)`, `o11y.go:443-456`); `service.version` is already on the Resource; the `!= ""` guard is dead (SDK-23) | code read |
| R-25 | core | `O11Y_*_ENABLED` values are not trimmed while `WithEnvironment` is (`options.go:755-768` vs `o11y.go:819`): `"false "` from a ConfigMap warns and leaves the pillar on | code read |
| R-26 | obsctx | `obsctx.Go(ctx, 0, fn)` runs `fn` with an already-expired context; no "no deadline" form | `obsctx/obsctx.go:64-68` |
| R-27 | diag | Every configured header *name* is a wholesale-replaced secret, so a collector 401 body "missing Authorization header" logs as "missing [redacted] header" | `diagnostics.go:636-643` |
| R-28 | nats | `MessagesContext.Next` returns a nil ctx on the error path while `consumer.Next` substitutes a non-nil one "to avoid nil-dereference"; `baggage.FromContext(nil)` panics | `nats/jetstream.go:604-633`, scratch |
| R-29 | nats | `Fetch(ctx, -1)` panics (`makechan: size out of range`, upstream) although the facade validates the other arguments; a `batch < 1` guard is cheap | `nats/jetstream.go:396-416,490-505` |
| R-30 | nats | `Inject`/`Extract` panic on nil `msg` or nil `prop`; `Respond`/`RequestMsg` guard nil | `nats/middleware.go:75-90` |
| R-31 | nats | `restoreBaggage` re-parses `traceparent` on every delivered message (693 ns / 5 allocs vs 187 ns / 2 for a baggage-only extract); skip when the propagator carries no `baggage` field or the header is absent (SDK-28) | benchmark |
| R-32 | http | `http.Option = otelhttp.Option` alias lets callers pass `otelhttp.WithMeterProvider(otel.GetMeterProvider())`, appended last, silently replacing the SDK provider; ADR 0009 promised a sealed type like `gin` has | `http/options.go:11`, `server.go:22-29` |
| R-33 | resty | `Wrap` swallows the histogram-creation error (noop fallback, no log); `wrapEntry.cleanup` is never read | `resty/client.go:96-114` |
| R-34 | minio | `New` substitutes noop providers for nil `tp`/`mp`/`prop` although the godoc says they are required and the other four packages error; histogram creation error discarded; `TestNew_Validation` enshrines it | `minio/client.go:87-92,138-144` |
| R-35 | mongo | `Connect` drops the pool-metrics cleanup on success; `Instrument` users are told to defer it | `mongo/client.go:85-95` |
| R-36 | metrics | `WithMetricsAddr("")` binds `0.0.0.0:<random>` silently (no empty guard) | `net.Listen("tcp","")` |
| R-37 | metrics | Scope guard logs on every `Meter()` call, not once as its comment says; `scrapelog` uses `repeat.Suppressor`, `scopeguard.go:51-58` does not | 3 calls → 3 WARN |
| R-38 | metrics | Exemplar docs overstate the failure: `exemplar.go:25-27` and `options.go:548` say the overflow fails "the entire /metrics scrape"; otelprom v0.65 drops that datapoint's exemplars and calls `otel.Handle` (which is what R-1 hits) | upstream `exporter.go:791-806` |
| R-39 | metrics | Scrape-error log line is unredacted and WARN (`scrapelog.go:80-84`); client_golang consistency errors embed rendered label values; a family lost until restart deserves ERROR (AGENTS.md rule 4) | code read |
| R-40 | metrics | `metricscap` clones every datapoint of every capped instrument on every export even when nothing is rewritten (`reader.go:345-396`); `rewriteSet` uses `Value.AsString()` so non-string values consume a slot and are never capped (`reader.go:405`) | code read |
| R-41 | metrics | Scrape server: only `ReadHeaderTimeout`; no `ReadTimeout`/`WriteTimeout`/`IdleTimeout`, `Serve` error discarded (SDK-27) | `metrics.go:473-477` |
| R-42 | redis/cassandra | Per-command attribute sets rebuilt on every call (`redis/hook.go:215-236`, `spanAttrs`, `strings.ToUpper(cmd.FullName())`); `cassandra/observer.go` computes `metricAttrs` twice per callback, per attempt and per page | code read |
| R-43 | examples | `examples/nats-ws-browser` sets `messaging.operation.type = "publish"` (not a valid enum value), hand-rolls a header carrier instead of `o11ynats.Inject/Extract`, and demonstrates browser-controlled baggage reaching the backend through the composite propagator (the ADR 0025 §7 anti-pattern) | `backend/main.go:30-133`, `src/*.js` |
| R-44 | tooling | `staticcheck -checks=all`: `attribute.Value.Emit` deprecated (`internal/metrics/targetinfo.go:74`), `gocql.NewBatch` deprecated ×9 in `cassandra_test.go`; `gosec`: G115 `uint64→int64` on `MinPoolSize`/`MaxPoolSize` (`mongo/pool_metrics.go:265,271`, harmless at real pool sizes but a bound check is cheap) | tool output |
| R-45 | docs | Drift list: `docs/guide.md:1364` vs `:1470` on whether `Consumer.Next` is wrapped (it is); `docs/semconv.md:109` says `url.full` is resty-only (otelhttp emits it too); `mongo.WithPoolName` godoc describes the old derivation; `nats.Conn` type doc omits `Request`/`RequestMsg`/`Respond`/`JetStream`; `SDK.Logger` doc says Fluentd; `http.WithMetricAttributesFn` is deprecated upstream but README:211 and `WithExtraHTTPServerAttributeKeys` godoc point at it; guide link to `k8s/infrastructure/base/prometheus.yaml` (file is under `monitor/`); README env table omits `T/True/F/False`; README "Migration note (pre-1.0 API change)" is 0.1.0-era; README examples list omits cassandra/elasticsearch/background; AGENTS.md package layout omits `resty/ minio/ cassandra/ obsctx/ o11ytest/`; ADR 0003 Decision names two setters while the gate forbids four and lacks rows for gocql/go-redis/mongo-driver (the gate's module list is hard-coded, so they are never checked); ADR 0013 §7.B/§10 (R-7, R-8); ADR 0014 count model (R-2); ADR 0010 RecordError row (R-10); ADR 0015 cites otel v1.43.0; ADR 0017 (Echo) is "Accepted" with no package; ADR 0023 says "no tagged release"; `//nolint` without reason in examples and four test files; `o11y_test.go:401-411` hand-rolls env restore instead of `t.Setenv` | audit |

## 4. Tool results

| Tool | Result |
|---|---|
| `golangci-lint run` (repo config) | 0 issues |
| `golangci-lint` broad set (70 linters) | 343 lines; non-test, non-example: 8 `spancheck` (all false positives, the span is ended by a `finish` helper), 11 `contextcheck` (deliberate `context.Background()` in minio/mongo cleanup and the nats callback shims), 2 `noctx` (`net.Listen`), G304/G115 as in R-44; nothing actionable beyond R-44 |
| `staticcheck -checks=all` | 10 × SA1019 (R-44) |
| `gosec -tests=true` | 29: 10 × G101 test fixtures (credential-shaped strings in redaction tests, flagged by design), G304/G703 in `diagnostics.go` (reading the certificate paths the exporter reads, by design), G115 ×3 (R-44), examples G404/G102/G104 |
| `make sast` | semgrep 0 findings, rule tests pass |
| ADR gate | pass |
| `go vet`, `go test -race` | clean, green |
| `govulncheck` | not runnable from the container (vuln DB blocked); CI job green on `afbd622` |
| Coverage (`-coverpkg=./...`, examples excluded) | 85.5 % statements; per package: root 92.7, cassandra 85.4, elasticsearch 89.7, gin 92.5, http 66.7, baggageattrs 91.8, exportstats 100, log 88.9, metrics 92.6, metricscap 41.2 (its exporter path is reached only through `InitMeter`), profiling 85.5, redact 98.2, repeat 100, minio 79.7, mongo 85.5, nats 87.4, o11ytest 57.1, obsctx 90.9, redis 77.5, resty 80.7 |

## 5. Status of the 2026-09-06 findings

| ID | Status on `afbd622` |
|---|---|
| SDK-1, SDK-4, SDK-5, SDK-6, SDK-7, SDK-9, SDK-14, SDK-15 | Fixed (#92, #93, #94, #96, #100) |
| SDK-3 | Dropped with PR5 (the app moves its own metrics to OTel instruments); needs a written decision and the R-20 docs |
| SDK-2 | Deferred (`WithLinkConsistentSampling`; production does not sample yet) |
| SDK-8 | Open; belongs to the v0.13.0 migration guide (R-16) |
| SDK-10, SDK-11, SDK-12, SDK-16, SDK-17, SDK-18 | Open, unchanged |
| SDK-13 | Open; R-7 and R-8 add the `ForEachShard` reload and the Ring gap |
| SDK-19 … SDK-29 | Open; SDK-23 = R-24, SDK-27 = R-41, SDK-28 = R-31, SDK-29 partly done by #100's guide section |

## 6. Suggested sequencing

1. **Before tagging v0.13.0** (small, each a single package, each with a
   test): R-1, R-2, R-9, R-3, R-5, R-6, R-23, R-14, R-15, R-16, R-17. These
   change day-one observations or leak credentials, and R-16 is required for
   the release anyway.
2. **v0.13.x**: R-4 with R-21/R-22 (one endpoint-contract PR: honour the env
   precedence, require scheme and host, treat the option as a base URL),
   R-10, R-11, R-12, R-18, R-19, R-13 (tests only).
3. **v0.14.0**: R-7/R-8 (Redis Cluster/Ring observation redesign, ADR 0013
   amendment), R-20 with SDK-19 (custom-metrics guide + unit-based views),
   the remaining P3 backlog, and the SDK-1x carry-overs.

## 7. Checked and found clean

- ADR 0003 in every package: no `otel.Set*`, `slog.SetDefault`, `os.Setenv`
  or side-effecting `init()` outside tests; providers and propagator always
  explicit; `nats` rejects nil providers before upstream could fall back to
  globals; `otelmongo`'s global fallback is pre-empted by noop providers;
  ADR 0026 holds (`go list -deps .` links none of the drivers or facades).
- `Init` / `Shutdown` lifecycle: `sync.Once`, deadline sharing, closer
  ordering, nil-closer elision, cleanup of earlier pillars on failure,
  exporter shutdown guards; the BSP background drain vs log `BatchProcessor`
  ctx-bound flush claims match sdk v1.44.0 / sdk/log v0.19.0.
- #100's preflight rules match the pinned parsers (trace env-first + option
  override, log lazy option-first, verbatim vs trimmed, client-cert pair,
  `firstInt` fallback, sampler env parsed regardless of `WithSampler`).
- `exportstats`, `repeat`, `redact` (closed `@` rule holds for opaque and
  scheme-relative URLs, short-secret token boundary), `logr` sink (bounded
  reflection walk, typed-nil and panic recovery), `OtelSlogHandler` /
  `MultiHandler` / `BaggageHandler` record-cloning semantics, `baggageattrs`
  budgets and catalog generation, the profiler closer.
- Metrics: reserved-key normalisation mirrors otlptranslator v1.0.0 exactly
  and does not allocate; catch-all view keeps advisory boundaries;
  `target_info` guard closes the env-alias gap; cardinality-limit arithmetic
  matches the SDK; `metricscap` concurrency and shared budgets; Prometheus
  registry uses `Register`, never `MustRegister`.
- Metric catalog vs instruments: every instrument created in code matches
  `docs/semconv.md` in kind, unit and label set; every SDK-owned name except
  the two in R-15 has a Deviations row.
- Transports: resty span lifecycle against v2.17.2 (every attempt ends
  exactly once, no orphan on cancellation between retries); nats header
  carrier casing, `Respond` routing, baggage restoration honouring the
  configured propagator, `wrapMessageBatch` buffering and cancellation;
  http `r.Pattern` naming; gin ordering matrix rows 1–10 and skip-path logic.
- Data stores: Mongo command monitor mirrors upstream `extractCollection`;
  redis hook filters AUTH/HELLO/SELECT/READONLY/CLIENT SETINFO|SETNAME and
  Pub/Sub so credentials never reach `db.query.text`, `redis.Nil` treated as
  success, spans ended once, weak-pointer `Wrap` registry race-free;
  Cassandra observer fires only on application queries, batch seam,
  restart-stable pool name, `filterReservedAttrs`; Elasticsearch per-request
  state in ctx (no map, no leak), nil body guard, `url.full` cannot carry
  credentials; MinIO span lifecycle including `ListObjects` drain on cancel,
  bounded `error.type`, object key span-only.
- Repo hygiene: `go mod tidy -diff` clean; examples build and vet; no
  TODO/FIXME in non-test code; Makefile targets reference real files.
