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
- [x] `internal/redact`
- [x] `internal/profiling`
- [x] `internal/log`
- [x] `internal/trace`
- [x] `internal/metrics`, `internal/views` (changed code in depth);
      `internal/metricscap` and the scrape handler are unchanged since
      `afbd622` and were not re-read
- [x] `internal/baggageattrs` (diff only), `internal/exportstats` and
      `internal/repeat` (unchanged; `repeat` read because the logr finding
      depends on it)
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

| S1-03 | P2 | Confirmed | `internal/redact` | SigV4 presigned-URL credentials (`X-Amz-Signature`, `X-Amz-Credential`, `X-Amz-Security-Token`) pass through `redact.URL` / `URLAttribute` unredacted; only the SigV2, GCS and Azure keys are covered |
| S1-04 | P3 | Confirmed | `internal/redact` | Redaction rewrites ordinary diagnostic text: a short header value such as tenant `1` blanks digits in addresses and counts, a header name such as `Authorization` is blanked wherever it appears, and any `@` outside a URL replaces the whole message |
| S1-05 | P3 | Confirmed | `internal/redact` | The error `Shutdown` and `ForceFlush` return is redacted only in its own `Error()`; `errors.Unwrap` hands back the original, unredacted error |
| S1-06 | P3 | Confirmed | `internal/log` | `Logr()` suppresses repeats by message text alone, so a second, different one-shot OTel warning that shares its message with the first (duplicate metric stream definitions for another instrument) is never logged |

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

#### S1-03 · P2 · Confirmed · SigV4 presigned URLs keep their credentials

`internal/redact/redact.go:640-645` (`credentialQueryKeys`), used by
`redactQuery` (`:841`), `hasCredentialQuery` (`:677`) and the closed rule
`unaccountedCredentialQuery` (`:718`).

The query keys the redactor treats as credentials are the four the semconv
v1.39.0 prose names for `url.full` (verified in the pinned
`attribute_group.go:15175-15178`): `AWSAccessKeyId`, `Signature`, `sig`,
`X-Goog-Signature`. Those cover AWS Signature Version 2, Azure SAS and GCS.
They do not cover Signature Version 4, which is what `minio-go` and the
current AWS SDKs generate: `X-Amz-Signature`, `X-Amz-Credential` and, with
temporary credentials, `X-Amz-Security-Token`.

Reproduced with a scratch test:

```
redact.URL("http://h/p?X-Amz-Signature=abc&X-Amz-Credential=AKIA%2F2026&X-Amz-Security-Token=tok")
  → returned unchanged
redact.URL("http://h/p?Signature=abc&sig=1")
  → "http://h/p?Signature=%5Bredacted%5D&sig=%5Bredacted%5D"
```

`access_token`, `api_key` and `password` query parameters are likewise
returned unchanged; that part follows the semconv list and is noted only for
completeness.

Where the value ends up (by reading, not run): `resty/hook.go:429` and `:473`
put `redact.URLAttribute` output in the `url.full` span attribute;
`redact.Error` substitutes the same output into `*url.Error` messages
(`redact.go:1352`), which reach logs and `exception.message`; the logr sink
uses `redact.URL` for `url.URL` values (`internal/log/logr.go:226`). A service
that fetches a MinIO or S3 presigned URL through the resty hook therefore
records a URL that anyone with trace access can replay until it expires.

The changelog (`CHANGELOG.md:654`) and the comments at `redact.go:40` and
`:108` state that a presigned URL "carries its credential in the query" and is
covered, which is true only for the SigV2-era keys. P2 rather than P1: the
leaked URL grants access to one object for a bounded time, and the SDK matches
the semconv minimum; but this repository ships a MinIO integration whose
presigned URLs are exactly the uncovered shape. The session that owns `resty/`
should confirm the end-to-end span attribute.

#### S1-04 · P3 · Confirmed · Redaction rewrites ordinary diagnostic text

`internal/redact/redact.go:392` (`shortSecretLen`), `:589`
(`replaceWholeToken`), `:140` (the `@` count rule in `InText`);
`diagnostics.go:661-668` (header names added as secrets).

Three behaviours, each reproduced by feeding realistic export errors through
`redact.Secrets(redact.InText(msg, diagnosticEndpoints…), diagnosticSecrets…)`
with the configuration shown:

1. `WithOTLPHeaders({"X-Scope-OrgID": "1"})` — the tenant header value the
   Loki / Mimir documentation uses. A value shorter than six bytes is replaced
   wherever it stands as a whole token:
   `dial tcp 10.0.0.1:4318: connect: connection refused` becomes
   `dial tcp 10.0.0.[redacted]:4318: …`, and `1 data points rejected` becomes
   `[redacted] data points rejected`.
2. `WithOTLPHeaders({"Authorization": "Bearer …", "X-Env": "production"})` —
   header *names* are listed as secrets, and names of six bytes or more are
   replaced as substrings: `401 Unauthorized: missing Authorization header for
   production cluster` becomes `…missing [redacted] header for [redacted]
   cluster`.
3. Any message holding an `@` that is not URL userinfo is replaced whole:
   `traces export: contact admin@example.com: quota exceeded` becomes
   `[endpoint redacted]`.

None of this leaks anything; the rules are fail-closed on purpose and the
comments say so. The cost is that the diagnostic an operator reads during a
collector outage can lose the failing address's last octet, the header name a
401 is complaining about, or everything. Worth knowing when reading these
logs, and a candidate for a note in `docs/guide.md`; a narrower fix would be
to stop treating a header name that `CredentialHeaderName` does not flag as a
secret.

#### S1-05 · P3 · Confirmed · The redacted error unwraps to the unredacted one

`internal/redact/redact.go:1291-1311` (`Error`), `:1535-1546`
(`redactedError`); callers `o11y.go:202` (`Shutdown`) and `o11y.go:271`
(`ForceFlush`).

`redact.Error` returns a wrapper whose `Error()` is the scrubbed text and
whose `Unwrap()` is the original error, so `errors.Is` / `errors.As` keep
working. Reproduced:

```
red := redact.Error(fmt.Errorf("export: %w", &url.Error{URL: "http://alice:s3cret@collector:4318/v1/traces", …}), endpoints, nil)
red.Error()                 → export: Post "http://redacted@collector:4318/v1/traces": boom
errors.Unwrap(red).Error()  → export: Post "http://alice:s3cret@collector:4318/v1/traces": boom
```

`slog.Any("error", err)` and `%v` call the outer `Error()` and are safe. Code
that renders each link of the chain is not: an error reporter that walks
`Unwrap()` and records every layer's message, or a handler that logs
`errors.Unwrap(err)`. In a real export failure `net/http` has already masked
the password (`alice:***@`), so what this route exposes is the username and
any header value an exporter quoted from a response body.

The design is deliberate and the comment on `Error` explains it. It is P3
because the godoc on `Shutdown` and `ForceFlush` says the credentials are
"removed" without the qualifier that only the top-level message is. A sentence
there would do.

#### S1-06 · P3 · Confirmed · Logr drops a second, different warning that shares its message

`internal/log/logr.go:102-105` (`Info`), `:132-135` (`write`);
`internal/repeat/repeat.go` (`SuppressedAt`); behaviour pinned by
`internal/log/logr_test.go:63`.

The logr sink suppresses repeats for one minute keyed on the message text
alone (for `Error`, the message plus the error text); key-values are not part
of the key. That is right for the case it was built for — the log batcher's
"dropped log records" with a changing count. It is wrong for OTel diagnostics
whose message is constant and whose key-values identify the subject, and which
are emitted once. `sdk/metric@v1.46.0/pipeline.go:471,488` logs
`global.Warn("duplicate metric stream definitions", "names", …, "kinds", …)`
once per conflicting instrument, at instrument creation.

Reproduced: two `V(1).Info("duplicate metric stream definitions", …)` calls
with different `names` within the window write one record; the second
instrument's conflict is never logged, and since the SDK does not repeat the
warning, it is lost for the life of the process. A service with two such
conflicts at start-up sees one. Including the redacted key-values in the
suppression key for WARN and above, or exempting the first N distinct
key-value sets per message, would keep the anti-flood property.

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

`internal/redact`:

- **No panic and no surviving secret under randomized input.** A scratch
  loop ran about 260,000 strings assembled from escapes, `@`, credential
  query keys, `Basic` / `Digest` fragments and invalid UTF-8 through `URL`,
  `InText`, `Secrets`, `URLAttribute` and `Error`. Nothing panicked, and no
  output still held a listed secret.
- **Fail-closed order.** `InText` and `Secrets` re-check every decoded view
  (HTML, JSON, Go and percent escapes, up to eight passes) and replace the
  whole message when a view does not converge or still holds a credential.

`internal/profiling`:

- **Log adapter redaction** (the 2026-09-14 finding "profiler log adapter
  echoes the endpoint's userinfo"). Every `Infof` / `Debugf` / `Errorf` now
  goes through `scrub` (`profiling.go`, `pyroscopeSlogAdapter`), which applies
  the endpoint rule and the header-value list, including the Basic credential
  derived from the endpoint's userinfo and from
  `PYROSCOPE_ADHOC_SERVER_ADDRESS`. By reading plus the package's passing
  tests; not separately reproduced.
- **Process-global state is limited to what pprof forces.** `profilerMu` /
  `profilerStarted` guard the one-profiler-per-process rule and surface as
  `ErrAlreadyStarted`; there is no `init()`.
- **Tag truncation** cuts on a rune boundary at or below 1024 bytes for every
  position of a multi-byte rune around the limit (walked by hand).

`internal/log`:

- **`ErrorStringHandler`** (new). Error values are converted at any group
  depth, including an empty-key group the bridge would inline. A `LogValuer`
  that resolves to an error is left alone, which is safe: the pinned
  `otelslog@v0.20.1` promotes only a direct `KindAny` error
  (`handler.go:474-479`) and converts a resolved one to a string attribute.
- **Stdout handler and multi handler** are unchanged since `afbd622`; a
  quick scan found nothing.

`internal/trace`:

- Exporter is shut down when provider construction fails; the propagator is
  the W3C TraceContext + Baggage composite and is not installed globally.

`internal/metrics`, `internal/views`:

- **Bucket overrides** (`bucketoverride.go`, new). A view that already claims
  the instrument keeps its attribute filter and gets only new boundaries; an
  unclaimed histogram gets one view of its own; the Prometheus reserved-key
  guard wraps both, so no second stream appears. Dropped streams,
  non-histogram instruments and exponential aggregations are left alone. The
  eight `TestInitMeter_BucketOverride*` cases cover each branch.
- **Empty attribute key** (the 2026-09-14 finding "an empty attribute key
  removes the family from `/metrics`"): `IsReservedAttributeKey("")` now
  returns true (`reserved.go:86-92`), so the reserved-key filter drops it.
- **MongoDB peer label cap.** `network.peer.address` on the otelmongo
  operation histogram is capped at 50 values on both export paths
  (`metrics.go:258`, `:297`, `:341`).
- **Init failure paths** close the listener and shut the provider down; on the
  OTLP path the provider, not just the exporter, is shut down so the periodic
  reader's goroutine stops.
