# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the project is pre-1.0 (no `v1.x.x` tag yet), breaking changes are
permitted between minor versions but are still listed here so external
adopters can plan their upgrades.

---

## [Unreleased]

### Added

- Added `WithSamplingRatio` and `WithTraceSampler` so services can configure
  SDK-side head sampling explicitly while preserving OpenTelemetry
  `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG` support when no typed
  sampler is set.
- Documented trace sampling guidance, including the head-vs-tail sampling split,
  high-throughput producer recommendations, and `OTEL_BSP_*` batch span
  processor environment variables.

### Changed

### Deprecated

### Removed

### Fixed

### Security

### Breaking Changes (Migration Guide)

---

## [0.3.0] - 2026-06-01

### Added

- New `redis/` package: an SDK-owned T3 wrapper over
  `github.com/redis/go-redis/v9` that emits OTel semconv v1.39.0 spans and pool
  metrics for single, Cluster, and Ring topologies. Public surface is
  `Wrap` / `Unwrap` / `MetricViews` / `WithCommandTextEnabled` /
  `WithAttributes` / `WithPoolName`. `Wrap` is idempotent and installs its hook
  before any caller hooks so the span encloses them; `WithAttributes` cannot
  override the SDK's built-in semconv keys. See ADR 0013.
- `internal/metrics.Config.ExtraViews` lets integration packages contribute
  metric views at `MeterProvider` construction time; `o11y.Init` now composes
  `o11yredis.MetricViews()` automatically so Redis pool-metric labels stay
  within the SDK cardinality contract.
- MongoDB operation-duration metrics via the official contrib `otelmongo`
  CommandMonitor in metrics-only mode. The `mongo` wrapper now emits
  `db.client.operation.duration` with bounded labels and keeps existing Marz
  tracing behavior unchanged. See ADR 0014.
- Added the official MongoDB contrib instrumentation dependency, which requires
  `go.mongodb.org/mongo-driver/v2 v2.6.0` and the corresponding OTel
  `v1.43.1-0.20260521080857-e5bdc311108b` pseudo-version set.

### Changed

- `db.client.operation.duration` from both the `mongo` and `redis` wrappers now
  honors the SDK's configured histogram boundaries (`WithHistogramBuckets`),
  matching the HTTP duration histograms. Previously the MongoDB metric inherited
  the contrib instrument's baked-in boundaries and the Redis metric inherited
  the SDK default boundaries. `mongo.MetricViews` and `redis.MetricViews` now
  take a `[]float64` buckets argument. Each view is also scoped to its own
  instrumentation so the two `db.client.operation.duration` views no longer
  match each other's instrument (which produced a conflicting stream with the
  wrong attribute filter when both wrappers were active in one process).

### Security

- Bumped indirect dependencies `golang.org/x/crypto` to `v0.52.0` and
  `golang.org/x/net` to `v0.55.0` to clear the GO-2026-50xx advisory set. Both
  are transitive (the SDK does not import them directly) and the affected
  `ssh`/`html`/`idna` paths are unused, but the pins are raised to the fixed
  versions to keep vulnerability scanners clean.

### Breaking Changes (Migration Guide)

- `mongo.Connect` now requires an explicit `metric.MeterProvider` argument:
  pass `obs.MeterProvider()` between `obs.TracerProvider()` and
  `obs.Propagator`. This preserves the SDK's no-global-provider policy while
  enabling MongoDB operation-duration metrics.

---

## [0.2.0] - 2026-05-22

### Added

- `WithExemplars(bool)` controls OpenMetrics content negotiation on the
  Prometheus pull `/metrics` handler. Defaults to `true` so per-bucket
  exemplars carrying `trace_id` / `span_id` flow through to Grafana / Tempo
  by default. Set `false` only as a temporary mitigation for services
  whose dashboards / recording rules / alert rules hardcode integer
  histogram bucket boundaries — see the Breaking Changes entry below.

### Changed

### Deprecated

### Removed

### Breaking Changes (Migration Guide)

- The default Prometheus pull handler now content-negotiates to the
  OpenMetrics exposition format when scrapers advertise it (which real
  Prometheus does on every scrape). OpenMetrics is the only format
  otelprom emits per-bucket exemplars in, so this is what makes
  trace-to-metric linkage actually work — previously the SDK built
  exemplars in memory and discarded them at serialization time, leaving
  Prometheus's exemplar store empty regardless of
  `--enable-feature=exemplar-storage`.

  The renegotiation also normalises integer histogram bucket boundaries
  on the rendered `le` label. The underlying `float64` values are
  unchanged, but the text differs:

  | Prometheus text format (pre-v0.2) | OpenMetrics (v0.2+, default) |
  |---|---|
  | `le="1"` | `le="1.0"` |
  | `le="5"` | `le="5.0"` |
  | `le="10"` | `le="10.0"` |

  Queries that hardcode the integer form silently stop matching:

  - `http_server_request_duration_seconds_bucket{le="1"}` → no series
  - `histogram_quantile(0.95, sum by (le) (rate(..._bucket[5m])))` →
    unaffected (aggregates across all buckets)

  **Migration**: audit dashboards, recording rules, and alert rules for
  literal integer `le` values that match the SDK's default bucket set
  (`1`, `5`, `10`). Update to the `.0` form. If you cannot stage the
  update before rolling the SDK, set `o11y.WithExemplars(false)` on the
  affected service to keep the handler on plain Prometheus format and
  preserve the integer `le` labels; trace-to-metric exemplars stay
  disabled until the option is removed.

### Fixed

- Enable OpenMetrics format on the Prometheus pull `/metrics` handler so
  exemplars actually reach scrapers. `promhttp.HandlerFor` used the
  default `HandlerOpts{}`, which leaves `EnableOpenMetrics` off; content
  negotiation then always returned the plain Prometheus exposition
  format, and that format has no syntax for per-bucket exemplars. As a
  result, even with Prometheus running
  `--enable-feature=exemplar-storage` and a sampled trace context
  attached to the measurement, exemplar storage stayed empty and
  Grafana's histogram-to-trace links were dead. The new
  `WithExemplars(bool)` option (default `true`) wires
  `EnableOpenMetrics` so SDK-managed HTTP histograms and caller-defined
  histograms both gain working exemplars; the bucket-boundary
  serialisation change is documented under Breaking Changes.

### Security

---

## [0.1.0] - 2026-05-22

First tagged release. Subsequent minor versions may still introduce breaking
changes per the pre-1.0 policy noted above.

### Added

- **`WithExtraHTTPServerAttributeKeys(keys ...string)`** to promote
  caller-controlled attributes (e.g. `app_name`, `bot_name`) onto the
  SDK-managed `http.server.request.duration` series. By default that view
  keeps only `http.request.method`, `http.route`, and
  `http.response.status_code` to bound cardinality; any other attributes
  attached via `o11ygin.WithMetricAttributesFn` / `otelhttp` were silently
  dropped from the exported series. The option appends user-supplied keys
  to the view's allow-list so they participate in PromQL aggregations.
  Keys are checked at startup against the full Prometheus label-name
  normalization the otelprom exporter applies (non-alphanumeric →
  `_`, runs collapsed, leading digits prefixed with `key_`). The SDK
  drops — with a structured `WARN` log — any key that, after
  normalization, collides with a built-in SDK label (e.g.
  `"http_route"`, `"http.route"`, `"http-route"`, `"http__route"` all
  shadow the existing `http_route`), collides with another
  caller-supplied key (e.g. `"app.name"` + `"app_name"`), or
  normalizes to an invalid label name. Accepting either kind of
  collision would silently merge two attribute values into one
  exported label and corrupt PromQL grouping for that dimension.
  Cardinality is the caller's responsibility — prefer enumerable
  values with bounded keyspaces. Has no effect when
  `WithDisableDefaultViews` is set.

- **Per-pillar feature toggles** for progressive SDK adoption:
  `WithTraceEnabled(bool)`, `WithMetricsEnabled(bool)`, `WithLogEnabled(bool)`,
  and `WithProfilingEnabled(bool)`. Trace, metrics, and log default to `true`
  (no change to existing behaviour); profiling defaults to `false` because it
  is an opt-in fourth signal. When a pillar is disabled the SDK returns a
  no-op provider for that signal while keeping everything else fully
  operational. All four toggles are also controllable without code changes
  via the `O11Y_TRACE_ENABLED`, `O11Y_METRICS_ENABLED`, `O11Y_LOG_ENABLED`,
  and `O11Y_PROFILING_ENABLED` environment variables (same defaults as the
  code options); explicit option calls take precedence. `sdk.Toggles`
  (`FeatureToggles{Trace, Metrics, Log, Profiling}`) reports the active state
  at runtime for health-check endpoints and startup logging. Notable
  per-pillar behaviour: Trace-disabled still parses and forwards W3C
  `traceparent` headers; Metrics-disabled does not start the Prometheus HTTP
  server; Log-disabled falls back to stdout-only JSON output. Profiling is
  doubly gated: it starts only when both `WithProfilingEnabled(true)` AND a
  non-empty `WithProfilingEndpoint` are set — either alone is insufficient,
  and the SDK emits a startup `WARN` when only one of the two is configured.
  `Toggles.Profiling` reflects whether the SDK actually started a profiler.
  The `otel-profiling-go` trace-to-profile wrapper is now installed only
  after `pyroscope.Start` returns successfully, so spans no longer carry
  `pyroscope.profile.id` when the profiler failed to start.

- Continuous profiling integration via Pyroscope:
  `WithProfilingEndpoint(url)` enables Pyroscope-compatible profile pushes,
  `WithProfilingAuthHeaders(map[string]string)` forwards auth / tenant headers,
  Grafana Alloy receives profiles on `:4040`, and the infrastructure stack now
  provisions Pyroscope plus Grafana trace-to-profile links. Includes
  `examples/profiling` for end-to-end local validation.
- `WithOTLPHeaders(map[string]string)` attaches arbitrary headers to every
  OTLP/HTTP request the SDK emits across traces, logs, and OTLP-push metrics.
  Use it to authenticate against managed observability backends like Grafana
  Cloud (`Authorization: Basic ...`), Honeycomb (`X-Honeycomb-Team`), New Relic
  (`Api-Key`), Datadog (`DD-API-KEY`), or to route through a multi-tenant
  Collector (`X-Scope-OrgID`).
- `SDK.Shutdown` is idempotent: a `sync.Once` gates the closer loop and the
  cached error is returned on subsequent calls. Safe to register both in a
  `defer` and inside a signal handler without double-flushing exporters.
- `http.NewServerHandler` and `http.NewTransport` wrap `otelhttp` while
  threading the SDK TracerProvider, MeterProvider, and Propagator explicitly.
- `gin.Middleware` wraps `otelgin` while threading the SDK TracerProvider,
  MeterProvider, and Propagator explicitly, and records typed `gin.error.type`
  span events for errors pushed through `c.Error` / `c.AbortWithError`.
- `WithDisableDefaultViews()` and `WithMaxUniqueRoutes(int)` configure the
  SDK-owned HTTP metric label governance added during the `otelhttp`
  migration.
- ADR 0008 CI gate (`make adr-check` and `.github/workflows/adr-check.yml`)
  validates approved instrumentation imports, package tier annotations, and
  absence of direct OTel global setter calls.
- `nats.Conn.Subscribe` and `QueueSubscribe` reject empty `subject` (and
  empty `queue` for the queue variant) up front. An empty NATS subject
  silently matches no messages and was almost always a programming error.
- `internal/testutil` package consolidates duplicated test fixtures
  (`FakeOTLPServer`, `NewCapturingOTLPServer`, `FreeAddr`, `ScrapeMetrics`,
  `TryScrapeMetrics`, `MustShutdown`).
- ADR `0007-otlp-authentication.md` documents the `WithOTLPHeaders` design
  and rejected alternatives.
- README **Logging Guidelines** section covering PII handling, log injection,
  and attribute payload size limits.
- GitHub Actions CI: race-detector tests with coverage, golangci-lint v2,
  govulncheck, and a build of every example program.
- `Makefile` mirroring CI targets for local parity (`test`, `lint`, `vuln`,
  `examples`, `bench`, `cover`, `fmt`, `tidy`).

### Changed

- `http/` is now a Tier-2 facade over `otelhttp`; inbound HTTP requests create
  server spans, extract `traceparent`, and emit standard OTel HTTP metrics.
- `o11y.Init` registers default HTTP metric views that keep
  `http.server.request.duration` labels to method, route, and status code.
- `SDK.TracerProvider()` now returns the `trace.TracerProvider` interface and
  `SDK.MeterProvider()` now returns the `metric.MeterProvider` interface, so
  profiling and future provider wrappers can stay internal to the SDK.

### Validated

- `Init` rejects empty / non-positive / NaN / `+Inf` / unsorted histogram
  bucket lists at start-up rather than allowing the OTel SDK to emit
  silently broken histograms.

### Breaking Changes (Migration Guide)

Pre-1.0 these are technically allowed without a major-version bump, but
adopters should be aware:

#### `DefaultLatencyBuckets` is now a function

```go
// Before: package-level []float64 variable
buckets := o11y.DefaultLatencyBuckets
n      := len(o11y.DefaultLatencyBuckets)
o11y.WithHistogramBuckets(o11y.DefaultLatencyBuckets)

// After: function returning a defensive copy on each call
buckets := o11y.DefaultLatencyBuckets()
n      := len(o11y.DefaultLatencyBuckets())
o11y.WithHistogramBuckets(o11y.DefaultLatencyBuckets())
```

The motivation: the old exported slice could be mutated by any caller:
`o11y.DefaultLatencyBuckets[0] = 0.999` would silently corrupt every later
SDK initialization in the process. The function returns a fresh copy each
time, so callers can safely modify the returned slice.

#### `DefaultMetricsAddr` is now a `const` (was `var`)

```go
// Before
addr := &o11y.DefaultMetricsAddr // legal: taking address of a var
o11y.DefaultMetricsAddr = ":9090" // legal: mutation

// After
addr := o11y.DefaultMetricsAddr   // copy the const value
// Taking the address or assigning to it now fails to compile.
```

If you need to override the listen address, use `o11y.WithMetricsAddr(":9090")`
which has been the supported path since the option was added.

#### `http.New` was replaced by `http.NewServerHandler`

```go
// Before
handler := o11yhttp.New(ctx, obs.Meter("svc"))(mux)

// After
handler := o11yhttp.NewServerHandler(
    mux,
    obs.TracerProvider(),
    obs.MeterProvider(),
    obs.Propagator,
)
```

Use Go 1.22+ `http.ServeMux` patterns or router-native route patterns to keep
`http.route` bounded. The old `WithPathNormalizer` callback was removed.

#### Provider accessors now return interfaces

```go
// Before
var tp *sdktrace.TracerProvider = obs.TracerProvider()
var mp *sdkmetric.MeterProvider = obs.MeterProvider()

// After
var tp trace.TracerProvider = obs.TracerProvider()
var mp metric.MeterProvider = obs.MeterProvider()
```

Instrumentation libraries already accept these interfaces. Lifecycle methods
such as `Shutdown` and concrete-only mutation hooks remain owned by
`SDK.Shutdown` and SDK initialization.

### Fixed

- Suppress `exemplar labels have N runes, exceeding the limit of 128` noise
  on the SDK-managed HTTP histograms. The OTel SDK routes attributes
  dropped by a view's `AttributeFilter` into the exemplar's
  `FilteredAttributes`; the Prometheus exporter then asks `client_golang`
  to build an exemplar from `trace_id` + `span_id` plus every filtered
  attribute, and `client_golang` rejects exemplars whose combined label
  runes exceed the OpenMetrics cap of 128. With typical otelhttp /
  otelgin default attributes (`server.address`, `url.scheme`,
  `network.protocol.*`, `user_agent.original`) the rejection fired on
  every scrape and surfaced via `otel.Handle` as a recurring error log.
  The SDK now wires a thin `ExemplarReservoirProviderSelector` wrapper on
  `http.server.request.duration` and `http.client.request.duration` that
  drops `FilteredAttributes` before the exemplar is offered: exemplars
  retain `trace_id` + `span_id` (so trace-to-metric linking in Tempo /
  Grafana still works) but no longer carry the verbose attribute payload
  that overflows the rune cap. Custom user views and AP-defined metrics
  are unaffected.
- Test sleep removed: `internal/metrics` `TestInitMeter_HappyPath` no longer
  uses `time.Sleep(100ms)` for runtime-metrics readiness; it polls via
  `assert.Eventually` instead.
- `WithOTLPEndpoint` godoc now warns about the `http://` default and points
  to `WithOTLPHeaders` for managed-backend authentication.

---

## [0.x] - historical

The project does not maintain release tags prior to this changelog. See
`git log` for earlier history.
