# o11y SDK full review (2026-11)

- **Target**: `main` after the v0.15.0 release, merge commit `03537c3`.
- **Baseline**: the previous full review stopped at `afbd622`
  (`docs/reviews/2026-09-14-sdk-full-review.md`). Code changed by
  `git diff afbd622 03537c3` is reviewed in depth; unchanged code gets a quick
  scan for obvious errors only.
- **Scope of this document**: findings only. No code was changed and no pull
  request was opened.
- **Sessions**: the review is split over three sessions that append to this
  one file. Each session owns the sections named after it.

Evidence labels used throughout:

- **Confirmed** — reproduced by running code (a scratch test, a tool run, or a
  mutated copy of the tree). The reproduction is described in the finding.
- **Speculative** — reasoned from reading the code; not run.

Severity:

- **P1** — data error, credential leak, panic, or telemetry lost at scale.
- **P2** — a clear bug or rule violation with limited reach.
- **P3** — small issue, clean-up, documentation.

Scratch programs lived outside the repository (a `go test -overlay` file for
in-package probes, and a `git archive` copy of the tree for gate mutation
tests). Nothing was written to the working tree.

## 1. Progress

Session 1 — core and infrastructure:

- [x] Root package: `Init`, options, resource, provider lifecycle
      (`Shutdown` / `ForceFlush`), endpoint resolution, diagnostics, toggles
- [ ] `internal/redact`
- [ ] `internal/profiling`
- [ ] `internal/log`
- [ ] `internal/trace`
- [ ] `internal/metrics`, `internal/metricscap`, `internal/views`, scrape server
- [ ] `internal/baggageattrs`, `internal/exportstats`, `internal/repeat`
- [ ] CI and tooling: `.github/workflows/`, `Makefile`, `scripts/`
- [ ] Tool results recorded

Session 2 — not started.

Session 3 — not started.

## 2. Executive summary

_To be written once all three sessions have reported._

## 3. Priority table

| ID | Sev | Evidence | Area | Summary |
|---|---|---|---|---|
| S1-01 | P3 | Confirmed | root / endpoints | `WithOTLPEndpoint` and `WithMetricsOTLPEndpoint` accept a URL with a trailing slash or a path prefix and then send traces and metrics to a path no collector serves, while logs still arrive |
| S1-02 | P3 | Confirmed | root / diagnostics | A spec-valid `OTEL_TRACES_SAMPLER` value the Go SDK does not implement (`jaeger_remote`, `xray`) fails `Init`, even when the option that makes the variable irrelevant is set |

## 4. Findings

### 4.1 Session 1 — core and infrastructure

#### S1-01 · P3 · Confirmed · An option endpoint with a trailing slash or a path splits the signals

`options.go:188` (`WithOTLPEndpoint`), `options.go:481`
(`WithMetricsOTLPEndpoint`), `internal/trace/trace.go:44`,
`internal/metrics/metrics.go:550`, `internal/log/provider.go:81`.

The three exporters complete an option-supplied endpoint differently. Trace and
metric setup add the signal path only when the URL has no path at all
(`u.Path == ""`); log setup trims trailing slashes first and treats `/` as no
path. An endpoint that came from an environment variable goes through
`newEnvSignalEndpoint` (`endpoint.go:124`) and is normalised the same way for
both signals, so only the option is affected.

Reproduced with a scratch test that points `Init` at an `httptest` server,
emits one span, one log record and one counter increment, shuts down, and
records the request paths:

| Option value | Trace path | Log path | Metric path |
|---|---|---|---|
| `http://c:4318` | `/v1/traces` | `/v1/logs` | `/v1/metrics` |
| `http://c:4318/` | `/` | `/v1/logs` | `/` |
| `http://c:4318/otlp` | `/otlp` | `/otlp` | `/otlp` |
| `http://c:4318/otlp/` | `/otlp/` | `/otlp` | not run |

Consequences:

- `WithOTLPEndpoint("http://collector:4318/")` — a value that differs from the
  documented one by a single character — delivers logs and loses every trace
  batch (an OTel Collector answers 404 on `/`). `Init` succeeds and the only
  signal is `o11y_export_failures_total` and the error handler, if installed.
- A base URL behind a path-routing gateway (`https://gw/otlp`) cannot work
  through the option at all: both signals post to the same `/otlp`. The same
  URL in `OTEL_EXPORTER_OTLP_ENDPOINT` works, because the generic variable is
  treated as a base.

The asymmetry is described in `docs/guide.md:534-539` and in the comment on
`logEndpointURL`, so this is documented behaviour rather than an oversight;
hence P3. It is listed because `Init` already validates scheme and host and
rejects endpoints "that would never send" (`diagnostics.go:499`), yet accepts
these, and because the godoc on both options says nothing about paths. Options
to consider: normalise the option like the generic variable, or reject / warn
on a path of `/`.

#### S1-02 · P3 · Confirmed · A sampler name the Go SDK does not implement stops the service from starting

`diagnostics.go:297` (`sdkEnvChecks`), `diagnostics.go:396` (`checkSamplerName`),
`diagnostics.go:421` (`checkCompression`).

`validateOTLPExporterEnv` fails `Init` when an OTel variable holds a value the
pinned Go SDK would echo while parsing. For `OTEL_TRACES_SAMPLER` the accepted
set is the six names the Go SDK implements. The OTel specification also defines
`jaeger_remote`, `parentbased_jaeger_remote` and `xray`, which other language
SDKs accept, and the check runs "whether or not Init passes a sampler".

Reproduced: with `OTEL_TRACES_SAMPLER=jaeger_remote` (or `xray`) set and
`WithSamplingRatio(0.5)` passed, `Init` returns
`o11y: OTEL_TRACES_SAMPLER cannot be used (it is not one of the sampler names the SDK knows)…`.
The same holds for `OTEL_EXPORTER_OTLP_COMPRESSION=zstd`,
`OTEL_EXPORTER_OTLP_TIMEOUT=10s` and `OTEL_BSP_SCHEDULE_DELAY=5s`.

Trigger: a fleet-wide environment block (an OTel Operator `Instrumentation`
resource, a shared Helm values file) written for services in several
languages. Every Go service on this SDK then fails at start-up, where the
upstream Go SDK would log the unsupported name once and fall back to its
default sampler.

This is deliberate and documented (`docs/guide.md:708`): the alternative is the
raw value reaching stderr before any redacting logger can be installed. It is
listed as a trade-off worth a recorded decision, because the cost is
availability and the protected value is a sampler name or a duration. A
narrower rule would keep the hard failure for variables that can carry a
credential (endpoints, headers, certificate paths) and downgrade the rest —
or at least the sampler variable when `WithSamplingRatio` / `WithTraceSampler`
already overrides it — to a start-up warning that names the variable without
its value.

## 5. Tool results

_Filled in at the end of session 1._

## 6. Status of earlier findings

_To be written once all three sessions have reported._

## 7. Suggested sequencing

_To be written once all three sessions have reported._

## 8. Checked and found clean

### 8.1 Session 1

Root package:

- **No OTel global mutation, no `init()`.** A grep over all non-test,
  non-example Go code for `otel.Set*`, `global.Set*`, `otel.Get*`,
  `slog.SetDefault`, the Prometheus default registry, and `func init()` finds
  nothing. The only package-level global read is `http.DefaultTransport` as
  the default base in `http/transport.go:18`.
- **`Init` failure paths release what was built.** Each early return after a
  provider exists calls `closeAll` with the closers built so far
  (`o11y.go:601`, `:625`, `:717`); `InitTracer`, `InitLogger`, `initOTLP` and
  `initPrometheus` each shut their exporter or provider down on their own
  error paths.
- **`Shutdown` / `ForceFlush` interlock.** `ForceFlush` increments the
  `WaitGroup` only under `flushMu` and only before `shutdownStarted` is set,
  so `Add` cannot race `Wait`; `Shutdown` is idempotent through `sync.Once`.
  The existing `forceflush_*_test.go` and `o11y_shutdown_test.go` pass under
  `-race`.
- **Endpoint precedence** (`endpoint.go`): option, then signal variable, then
  generic variable, then default, with the gRPC-protocol rejection. Matches
  the godoc and `docs/guide.md`.
- **Semconv keys.** Every attribute key the root package and `internal/`
  emit goes through a `semconv` constant of the pinned v1.39.0 package. The
  keys were resolved against
  `go.opentelemetry.io/otel@v1.46.0/semconv/v1.39.0/attribute_group.go`:
  `service.name`, `service.version`, `service.namespace`,
  `deployment.environment.name`, `host.name`, `k8s.pod.name`,
  `k8s.namespace.name`, `network.peer.address`, `http.route`,
  `http.request.method`, `http.response.status_code`, `server.address`,
  `server.port`, `error.type`, `db.collection.name`, `otel.component.type`,
  `user.name` each appear exactly once as `attribute.Key("…")`. The metric
  names `http.server.request.duration`, `http.client.request.duration`,
  `db.client.operation.duration`, `db.client.connection.create_time`,
  `rpc.server.call.duration` and `rpc.client.call.duration` are present in
  the pinned `httpconv`, `dbconv` and `rpcconv` packages. All 48 semconv
  imports in the tree name v1.39.0.
- **Public API test coverage.** Every exported function and option in the
  root package is referenced by at least one test file.
