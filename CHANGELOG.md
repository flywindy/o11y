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
  Use it to authenticate against managed observability backends — Grafana
  Cloud (`Authorization: Basic …`), Honeycomb (`X-Honeycomb-Team`), New Relic
  (`Api-Key`), Datadog (`DD-API-KEY`) — or to route through a multi-tenant
  Collector (`X-Scope-OrgID`).
- `SDK.Shutdown` is idempotent: a `sync.Once` gates the closer loop and the
  cached error is returned on subsequent calls. Safe to register both in a
  `defer` and inside a signal handler without double-flushing exporters.
- HTTP middleware recovers from handler panics, records the metric with
  `status_code=500` (or the previously committed status, if any), and
  re-raises the original panic value so `http.ErrAbortHandler` semantics and
  `http.Server`'s default panic logging still run.
- HTTP middleware now wraps `ResponseWriter` with one of eight variants based
  on which optional interfaces the underlying writer implements
  (`http.Flusher` × `http.Hijacker` × `io.ReaderFrom`). Legacy
  type-assertion feature detection (`w.(http.Flusher)`) once again returns
  the truth, fixing the previously silent SSE / chunked-stream flush no-op.
  `io.ReaderFrom` is now exposed when supported, restoring zero-copy
  `http.ServeFile`.
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

- HTTP middleware emits a `status_code=500` metric sample even when the
  handler panics (previously the request was not recorded at all).

### Validated

- `Init` rejects empty / non-positive / NaN / `±Inf` / unsorted histogram
  bucket lists at start-up rather than allowing the OTel SDK to emit
  silently broken histograms.

### Breaking Changes (Migration Guide)

Pre-1.0 these are technically allowed without a major-version bump, but
adopters should be aware:

#### `DefaultLatencyBuckets` is now a function

```go
// Before — package-level []float64 variable
buckets := o11y.DefaultLatencyBuckets
n      := len(o11y.DefaultLatencyBuckets)
o11y.WithHistogramBuckets(o11y.DefaultLatencyBuckets)

// After — function returning a defensive copy on each call
buckets := o11y.DefaultLatencyBuckets()
n      := len(o11y.DefaultLatencyBuckets())
o11y.WithHistogramBuckets(o11y.DefaultLatencyBuckets())
```

The motivation: the old exported slice could be mutated by any caller —
`o11y.DefaultLatencyBuckets[0] = 0.999` would silently corrupt every later
SDK initialization in the process. The function returns a fresh copy each
time, so callers can safely modify the returned slice.

#### `DefaultMetricsAddr` is now a `const` (was `var`)

```go
// Before
addr := &o11y.DefaultMetricsAddr // legal — taking address of a var
o11y.DefaultMetricsAddr = ":9090" // legal — mutation

// After
addr := o11y.DefaultMetricsAddr   // copy the const value
// Taking the address or assigning to it now fails to compile.
```

If you need to override the listen address, use `o11y.WithMetricsAddr(":9090")`
which has been the supported path since the option was added.

### Fixed

- Test sleep removed: `internal/metrics` `TestInitMeter_HappyPath` no longer
  uses `time.Sleep(100ms)` for runtime-metrics readiness; it polls via
  `assert.Eventually` instead.
- `WithOTLPEndpoint` godoc now warns about the `http://` default and points
  to `WithOTLPHeaders` for managed-backend authentication.

---

## [0.x] — historical

The project does not maintain release tags prior to this changelog. See
`git log` for earlier history.
