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

### Fixed

- Test sleep removed: `internal/metrics` `TestInitMeter_HappyPath` no longer
  uses `time.Sleep(100ms)` for runtime-metrics readiness; it polls via
  `assert.Eventually` instead.
- `WithOTLPEndpoint` godoc now warns about the `http://` default and points
  to `WithOTLPHeaders` for managed-backend authentication.

---

## [0.x] - historical

The project does not maintain release tags prior to this changelog. See
`git log` for earlier history.
