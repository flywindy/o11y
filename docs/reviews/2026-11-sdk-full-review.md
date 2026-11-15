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
- [x] CI and tooling: `.github/workflows/`, `Makefile`, `scripts/`
- [x] Tool results recorded

Session 1 is complete: nine findings (two P2, seven P3, no P1), all confirmed
by a reproduction or, for the CI items, by a tool run against the files named. Sections 2, 6 and 7 are left for the last session.

Session 2 — integration packages:

- [x] `resty/` (changed; diff read in full)
- [x] `redis/` (changed; diff read in full)
- [x] `mongo/` (changed; diff read in full)
- [x] `nats/` (changed; diff read in full)
- [x] `cassandra/` (changed; diff read in full)
- [x] `gin/` (changed; diff read in full)
- [x] `http/`, `elasticsearch/`, `minio/`, `obsctx/`, `o11ytest/` (no non-test
      change since `afbd622`; constructors and error paths scanned, the rest
      not re-read)
- [x] Cross-integration contract comparison (section 4.2, S2-08)
- [x] Session 2 tool results recorded

Session 2 is complete: eight findings (two P2, six P3, no P1), all confirmed
by a scratch test, a benchmark or a mutation run described in the finding.

Session 3 — documentation and comparison with the earlier reviews:

- [x] Documentation drift: `README.md`, `docs/guide.md`, `docs/semconv.md`,
      `docs/adr/` (status lines and the sections changed since `afbd622`;
      the ADR bodies were not re-read in full), `AGENTS.md`, `examples/` and
      `examples/README.md`, package comments; `k8s/infrastructure/` for
      obvious errors (section 4.3)
- [x] Status of the earlier findings: R-1 to R-45 and SDK-1 to SDK-30
      (section 6)
- [x] Session 3 tool results and clean list (sections 5.3 and 8.3)
- [ ] Executive summary and suggested sequencing (sections 2 and 7)

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
| S1-07 | P2 | Confirmed | `scripts/` (ADR gate) | The "no OTel global setters" gate catches `otel.SetTracerProvider` but not `log/global.SetLoggerProvider`, `otel.SetErrorHandler` or `otel.SetLogger`; its `SetLoggerProvider` entry can never match |
| S1-08 | P3 | Confirmed | `scripts/` (semconv gate) | `make semconv-check` only recognises keys that exist in the pinned package, so a legacy or misspelt key literal (`db.statement`, `db.cassandra.table`) passes — the mistake Core Principle 5 warns about |
| S1-09 | P3 | Confirmed | CI / Makefile | No gate for `go mod tidy`; CI comments defer upgrades to Dependabot / Renovate but the repository configures neither; `make tools` does not pin golangci-lint or govulncheck; two of the three gate scripts have no tests; one stale step label |
| S2-01 | P3 | Confirmed | `resty/` | Every cookie the client or its jar holds is treated as a credential, so an ordinary cookie value (`consent=1`, `lang=en`) blanks matching text in the span's error description — the last octet of the failing address, for one |
| S2-02 | P3 | Confirmed | `resty/` | The client's credential forms are rebuilt on every request, although they are read only when a request fails: about 99 extra allocations and 25 µs per successful request for a client with a bearer token and one API-key header |
| S2-03 | P2 | Confirmed | `gin/`, `http/` | `o11ygin.Middleware`, `o11yhttp.NewServerHandler` and `o11yhttp.NewTransport` pass a nil tracer provider, meter provider or propagator straight to otelgin / otelhttp, which then use the process-global ones; their godoc says that never happens, and every other integration rejects nil or substitutes a no-op |
| S2-04 | P2 | Confirmed | `elasticsearch/` | `NewClient` / `NewTypedClient` return an error that quotes the whole address, username and password included, when an address fails to parse — the usual cause being an unescaped character in the password; the NATS facade redacts the same upstream behaviour |
| S2-05 | P3 | Confirmed | `mongo/` | The default pool name takes a number from one process-wide counter that is never reused: the name each host gets depends on the order unrelated clients are instrumented in, and every re-instrumentation starts a new set of pool series; the `WithPoolName` godoc describes a different scheme |
| S2-06 | P3 | Confirmed | `cassandra/` | `WithQueryText`, `WithHostAttributes` and `WithPoolName` are never called by a test; with all three turned into no-ops the package's tests still pass |
| S2-07 | P3 | Confirmed | `nats/` | A server URL that fails to parse yields `nats connect [endpoint redacted]: [endpoint redacted]`, which no longer says what is wrong with the URL |
| S2-08 | P3 | Confirmed | cross-integration | The integrations disagree on four contracts: what a nil provider does (three behaviours), whether a second instrumentation call is safe (`mongo.Instrument` emits every span twice), whether errors carry the package prefix, and how default pool names are built (three schemes) |
| S3-01 | P3 | Confirmed | `docs/guide.md` | The JetStream section says in one place that single-message `Consumer.Next` is "deferred, not wrapped" and in another that it is wrapped; the code wraps it |
| S3-02 | P3 | Confirmed | docs | Two documented paths do not exist: the guide's link to `k8s/infrastructure/base/prometheus.yaml`, and the `kubectl apply -k k8s/infrastructure/base` entry point in `AGENTS.md` (no kustomization there) |
| S3-03 | P3 | Confirmed | `k8s/` | `base/cassandra.yaml` and `base/elasticsearch.yaml` are unreferenced copies of the component manifests; the Elasticsearch copy has drifted (`runAsGroup: 0` against `1000`) |
| S3-04 | P3 | Confirmed (text) | docs / `kind-config.yaml` | Local profiling instructions say to reach Alloy on `localhost:4040`; the kind port mapping sends that port to Pyroscope directly, and `examples/README.md` refers to an Alloy port-forward its setup block does not contain |
| S3-05 | P3 | Confirmed | `examples/` | `examples/README.md` says no example passes `WithOTLPEndpoint`; the profiling example does, so the standard endpoint variables do not redirect it |
| S3-06 | P3 | Speculative | `examples/` | Forty `os.Exit(1)` sites skip the deferred `Shutdown`, so the error record that explains the exit never reaches the OTLP log pipeline — in the code services copy for their `main` |
| S3-07 | P3 | Confirmed | `.agents/skills/` | The lookup command in the semconv verification skill never matches the module-cache path (`otel@v1.46.0/semconv/...`), so the step Core Principle 5 mandates returns nothing |
| S3-08 | P3 | Confirmed | `AGENTS.md`, `README.md` | The package layout, tech stack, ADR summary and "Do NOT" list in `AGENTS.md` omit `resty/`, `minio/`, `cassandra/`, `obsctx/` and `o11ytest/`; the README's infrastructure, sub-package and example lists each omit some of Cassandra, Elasticsearch, MinIO and the background example |
| S3-09 | P3 | Confirmed | docs / comments | Nine stale statements: `deployment.environment` for `deployment.environment.name`, "Fluentd", a 0.1.0 migration note, a recommendation of a deprecated option, the `nats.Conn` type comment, duplicate package comments, and three ADR status or version lines |

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

#### S1-07 · P2 · Confirmed · The global-setter gate misses three of the OTel globals

`scripts/check_integrations.go:21` (`otelModulePath`), `:23-28`
(`setterNames`), `:210` (`isOTelPackageRoot`); run by `make adr-check` and
`.github/workflows/adr-check.yml`.

`checkNoGlobalSetters` is the mechanical enforcement of ADR 0003 and of the
AGENTS.md rule that SDK code must not mutate OTel globals. It flags a call or
selector named `SetTracerProvider`, `SetTextMapPropagator`, `SetMeterProvider`
or `SetLoggerProvider` when the package it resolves to is exactly
`go.opentelemetry.io/otel`.

- `SetLoggerProvider` does not live in that package. The logs global is
  `go.opentelemetry.io/otel/log/global.SetLoggerProvider`, so the entry in
  `setterNames` never matches anything.
- `otel.SetErrorHandler` and `otel.SetLogger` are in the root package and
  mutate process-global state, but are not in `setterNames`.

Reproduced on a `git archive` copy of the tree (baseline run exits 0) by
adding one non-test file under `internal/` that calls all four:

```go
logglobal.SetLoggerProvider(sdklog.NewLoggerProvider())
otel.SetErrorHandler(…)
otel.SetLogger(…)
otel.SetTracerProvider(nil)
```

The gate reported only the last line (`forbidden otel.SetTracerProvider
call`). With that line removed the file would pass.

No SDK code calls any of these today (see section 8.1), so nothing is broken;
the gap is that a future change installing the SDK's own `ErrorHandler()` or
`Logr()` globally "for convenience", or setting the global LoggerProvider,
would merge with the ADR gate green. Fix: match on the pair (package path,
name) with `go.opentelemetry.io/otel/log/global.SetLoggerProvider`,
`otel.SetErrorHandler` and `otel.SetLogger` added. The gate also does not look
at reads (`otel.GetTracerProvider()`, `global.GetLoggerProvider()`), which the
MongoDB section of AGENTS.md rules out for that package; whether to gate those
is a separate decision.

#### S1-08 · P3 · Confirmed · The semconv literal gate cannot see a wrong key

`scripts/check_semconv_literals.go:21-22` (stated limit), `:131` (`scan`).

The gate builds its key set from the `attribute.Key("…")` declarations of the
pinned package and reports a string literal in SDK code that equals one. A
literal that is *not* in that set is ignored. Its header says so.

Reproduced on the tree copy with one file holding four keys: the literal
`"service.name"` was reported; `"service."+"namespace"`, the constant
`"db.statement"` and the literal `"db.cassandra.table"` were not.

So the gate enforces "use the constant for a key that exists" and is silent
on "this key does not exist in v1.39.0", which is the failure AGENTS.md Core
Principle 5 describes (`db.cassandra.*` → `cassandra.*`). That case still
rests on the `verify-semconv-attributes` skill and review. A cheap extension:
for the first argument of `attribute.Key(...)` / `attribute.String(...)` and
friends, require the literal to be either in the pinned set (already an
error) or in a short allow-list of SDK-owned keys. The tree has ten such
literals today (`object_store.*`, `minio.error.kind`, `redis.error.kind`,
`resty.*`, `cassandra.query.attempt`), all SDK-owned.

#### S1-09 · P3 · Confirmed · CI and Makefile hygiene

Each item was checked against the files named.

- **No `go mod tidy` gate.** AGENTS.md requires `go mod tidy` before every
  commit; neither workflow nor `make lint` checks it. `go mod tidy -diff` is
  clean at `03537c3`, so this is a missing guard, not a present defect.
  `go mod tidy -diff` in the `lint` job would close it.
- **Dependabot / Renovate are cited but not configured.**
  `.github/workflows/ci.yml:85` and `:113` say upgrades of the pinned
  golangci-lint and govulncheck are managed "via Dependabot / Renovate", and
  every action is pinned by commit SHA. `.github/` holds only `workflows/`,
  and there is no `renovate.json`. The pins — including
  `GOVULNCHECK_GOTOOLCHAIN`, whose comment asks for a review at every Go
  patch release — move only when someone remembers.
- **`make tools` covers half of the toolchain.** It pins gosec and semgrep so
  "CI and local `make sast` stay in lockstep" (`ci.yml:141`), but
  golangci-lint (CI: v2.11.4) and govulncheck (CI: v1.3.0) are whatever is on
  `PATH`. In this environment that was golangci-lint 2.5.0 built with
  go1.25, which refuses the go 1.26 module outright (see section 5).
- **Two gate scripts have no tests.** `check_semconv_literals.go` has
  `check_semconv_literals_test.go`; `check_credential_directives.go`
  (994 lines, with its own YAML rule reader) and `check_integrations.go`
  (302 lines) have none. S1-07 is the kind of defect a table of
  must-fail fixtures would have caught.
- **Stale step label.** `ci.yml:150` is named "…fail on medium+", while
  `GOSEC_FLAGS` is `-severity low -confidence low` and the Makefile comment
  says every rule blocks.

### 4.2 Session 2 — integration packages

Non-test code changed since `afbd622` in six of the eleven packages in scope:
`resty/`, `redis/`, `mongo/`, `nats/`, `cassandra/` and `gin/`. Those diffs
were read in full. `http/`, `elasticsearch/`, `minio/`, `obsctx/` and
`o11ytest/` have no non-test change and got a quick scan.

#### S2-01 · P3 · Confirmed · resty: an ordinary cookie blanks parts of the error text

`resty/hook.go:582-604` (`jarSecrets`), `:648-658` (`configuredSecrets`, the
cookie loop), `internal/redact/redact.go:392` (`shortSecretLen`).

When a request fails, the span's status description and `exception.message`
are passed through `redact.Error` with a secret list that includes the value
of **every** cookie configured on the request or client, and every cookie the
client's jar would send to the URL. Nothing distinguishes a session cookie
from a preference cookie.

Reproduced with a wrapped client, a `cookiejar.Jar` holding `lang=en`,
`consent=1` and `theme=refused` for `http://10.0.0.1:8080/`, and a transport
that fails with `dial tcp 10.0.0.1:8080: connect: connection refused (1
attempt, tenant en)`. The span carried:

```
Get "http://10.0.0.[redacted]:8080/orders": dial tcp 10.0.0.[redacted]:8080:
connect: connection [redacted] ([redacted] attempt, tenant [redacted])
```

`url.full` and `server.address` on the same span still read `10.0.0.1`, so
nothing is hidden; the description is only harder to read. A second run with
`client.SetCookie(&http.Cookie{Name: "region", Value: "timeout"})` turned
`i/o timeout` into `i/o [redacted]`.

This is the same fail-closed trade-off as S1-04, reached from a new source.
It is worth a separate entry because the input is not configuration the
service owner wrote: the jar is filled by whatever `Set-Cookie` the remote
server sends, so a one-digit tracking or consent cookie from a third-party
API is enough. Options: keep the rule and document it next to the resty
section in `docs/guide.md`, or skip cookie values shorter than
`shortSecretLen`, which are too short to be a bearer credential.

#### S2-02 · P3 · Confirmed · resty: credential forms are rebuilt on every request

`resty/hook.go:126` (`clientSecrets(c)` in `beforeRequest`), `:610-616`.

`beforeRequest` calls `clientSecrets` for every request so that a later
failure can redact with the invoking client's credentials. The result is read
only by `finishError`; a request that succeeds never uses it. Building it
walks the client's headers, lower-cases names, and renders each credential in
every form `redact.HeaderValueForms` lists.

Measured with a scratch benchmark against a loopback `httptest` server, a
never-sampling tracer provider, 20,000 requests, two runs each:

| Client | ns/op | B/op | allocs/op |
|---|---|---|---|
| no credentials | 72,637 – 76,260 | 10,281 | 98 |
| `SetAuthToken` + one `X-Api-Key` header | 98,159 – 101,144 | 13,935 | 197 |

So about 25 µs, 3.6 KB and 99 allocations are added to each successful
request — the allocation count doubles. The comment at `hook.go:49-53`
explains why the client must be read in `beforeRequest` (resty passes it to
no other hook). That constraint only requires capturing the *inputs* there:
the header map reference would not be safe to keep, but the handful of
strings (`Token`, `AuthScheme`, `UserInfo`, the credential-named header
values, the cookie values) could be copied cheaply and expanded into forms
only inside `finishError`.

#### S2-03 · P2 · Confirmed · gin and http: a nil provider silently selects the OTel globals

`gin/middleware.go:27-32`, `http/server.go:21-29`, `http/transport.go:16-22`;
upstream `otelgin@v0.71.0/gin.go:39-51`, `otelhttp@v0.71.0/config.go:55-56`,
`:79`, `:89`, `:109`.

The three facades hand `tp`, `mp` and `prop` to `otelgin.WithTracerProvider`
/ `otelhttp.WithTracerProvider` (and the meter and propagator equivalents)
without a nil check. Both upstreams treat a nil argument as "not set" and fall
back to `otel.GetTracerProvider()`, `otel.GetMeterProvider()` and
`otel.GetTextMapPropagator()`.

Reproduced with a scratch test that installed a recording tracer provider and
a W3C propagator as the process globals and then passed nil for all three
arguments:

- `o11ygin.Middleware("svc", nil, nil, nil)` — the request's `GET /x` server
  span was exported by the global provider.
- `o11yhttp.NewServerHandler(h, nil, nil, nil)` — same, span `GET`.
- `o11yhttp.NewTransport(nil, nil, nil, nil)` — the `HTTP GET` client span was
  exported by the global provider, and the outgoing request carried a
  `traceparent` header written by the global propagator.

The meter-provider fallback sits on the same lines of the upstream config and
was read, not separately observed.

Nothing here *writes* a global, so the letter of ADR 0003 ("must not mutate")
holds. What fails is the stated contract: `gin/middleware.go:23-24` says the
providers are passed "so otelgin never falls back to process-wide
OpenTelemetry globals", `http/server.go:17-18` says the same of otelhttp, and
`http/transport.go:14-15` says they are wired "to avoid reading OpenTelemetry
globals". A service that passes a nil provider — an unset struct field, a
disabled-signal code path — gets telemetry on whatever a third-party library
installed globally, or none, with no error. No test covers a nil argument for
these three functions.

Every other integration in scope has a nil check (table in S2-08). The fix
that matches the stricter majority is to substitute a no-op provider and an
empty composite propagator, since `Middleware`, `NewServerHandler` and
`NewTransport` have no error return.

#### S2-04 · P2 · Confirmed · elasticsearch: a malformed address is echoed with its credentials

`elasticsearch/client.go:141-155` (`NewClient`), `:177-190`
(`NewTypedClient`).

go-elasticsearch accepts credentials in the address
(`https://user:pass@host:9200`). When an address does not parse, the upstream
constructor returns net/url's parse error, which quotes the input verbatim,
and the facade wraps it unchanged. Reproduced:

```
NewClient(elastic.Config{Addresses: []string{"https://elastic:s3cr%ZZetpw@es.internal:9200"}}, tp, mp)
→ elasticsearch new client: cannot create client: cannot parse url:
  parse "https://elastic:s3cr%ZZetpw@es.internal:9200": invalid URL escape "%ZZ"
```

A second address, `https://elastic:pa ss@[bad`, was echoed the same way. The
trigger is not exotic: the address fails to parse precisely when the password
holds a character that needed percent-encoding (`%`, a space, `[`), so the
error appears on the deployments where the secret is in the URL. The SDK's
documented logging pattern is `slog.Any("error", err)`, which sends the text
to stdout and the OTLP log pipeline.

`nats/conn.go:169-177` handles the identical upstream behaviour by passing the
error through `redact.Error`, with a comment explaining why; that change
landed in this review window and was not carried over. The other constructors
were probed for the same leak and are clean: the MongoDB driver's URI errors
name the fault without quoting the URI (seven malformed URIs tried), and
`redis`, `cassandra` and `minio` take no credential-bearing URL.

The working path is fine: with `http://elastic:s3cretpw@127.0.0.1:port` the
span's `url.full` and `server.address` carried no userinfo, on both a
successful search and a connection-refused one.

Rated P2 rather than P1 because it needs a malformed address and surfaces
once, at start-up, in a returned error rather than in emitted telemetry.

#### S2-05 · P3 · Confirmed · mongo: default pool names depend on global call order and are never reused

`mongo/pool_metrics.go:154` (`defaultPoolNameSeq`), `:159-171`
(`defaultPoolName`), `mongo/client.go:34-37` (the `WithPoolName` godoc).

Without `WithPoolName`, the `db.client.connection.pool.name` label is
`mongo-<first host>-<seq>`, where `<seq>` comes from one package-level
counter shared by every `Instrument` call in the process and only ever
increments. Reproduced with a scratch test:

- Instrumenting `orders-db` then `users-db` gives `mongo-orders-db-1` and
  `mongo-users-db-2`; the opposite order gives `mongo-users-db-1` and
  `mongo-orders-db-2`. A service that builds its clients concurrently
  therefore reports the same pool under different names from one restart to
  the next.
- Three `Instrument` + cleanup cycles for the same host moved the next name
  to `mongo-orders-db-6`. A service that rebuilds its client (credential
  rotation, a reconnect path) adds a new set of pool series each time; the
  old ones stay in the SDK at their last value. Growth stops only at the
  SDK's per-stream cardinality limit.
- The port is not part of the name: `localhost:27017` and `localhost:27018`
  differ only in the sequence number.

`redis/client.go:24-86` replaced exactly this pattern in this review window —
per-base numbering, lowest free number, names returned on `Unwrap` or
collection — and its comments name the two problems above as the reason. The
MongoDB package kept the older scheme.

The godoc at `mongo/client.go:34-35` says the SDK "derives a bounded name
from the first configured MongoDB host and the ClientOptions instance
identity". Neither half matches: the name does not use the options' identity,
and it is not bounded across repeated `Instrument` calls.

#### S2-06 · P3 · Confirmed · cassandra: three public options have no test

`cassandra/options.go:34` (`WithQueryText`), `:136` (`WithHostAttributes`),
`:154` (`WithPoolName`).

The package's tests build a `config{…}` literal directly
(`cassandra_test.go:236`, `:293`, `:490`) and call the unexported
`poolName(…)` helper, so the behaviour behind each option is covered but the
exported functions are not called anywhere under `cassandra/`, including the
`integration`-tagged file. Confirmed by mutation: with the assignment in each
of the three functions replaced by a no-op (through `go test -overlay`, no
change to the tree), `go test ./cassandra/` and
`go test -tags integration ./cassandra/` both still pass.

AGENTS.md requires a unit test for every public function. The same scan —
every exported function and method in the eleven packages, looked up by name
in that package's test files — found no other gap. `WithCoordinatorMetricLabel`,
added in this window, is called by its tests.

#### S2-07 · P3 · Confirmed · nats: an unparseable server URL leaves an error with no cause

`nats/conn.go:169-177`, `:422-445` (`redactedURLs`, `redactServerURL`).

When any entry of the server list fails to parse, both halves of the message
are replaced. Reproduced through `Connect` with seven malformed inputs — an
invalid escape in the password, a space in the password, an unescaped `/` or
`?` in the password, an unclosed IPv6 bracket, and two lists separated by a
space or `;` instead of a comma. Each returned exactly:

```
nats connect [endpoint redacted]: [endpoint redacted]
```

No credential survives, which is the point, and `errors.Is` / `errors.As`
still reach the upstream error. What is lost is the reason: `invalid URL
escape`, `invalid port`, `missing ']' in host` all read the same, and for a
list the operator cannot tell which entry is at fault. This is the
fail-closed rule of S1-04 seen from the caller's side. A message that keeps
net/url's reason and drops only the quoted URL — the reason text does not
repeat the input — would stay safe and remain actionable.

The well-formed cases behave as documented: `nats://alice:…@host`,
`alice:…@host` with no scheme, a bare token, and a comma-separated list each
come back with `redacted@` in place of the userinfo and the host intact.

#### S2-08 · P3 · Confirmed · The integrations disagree on four contracts

Compared across the eleven packages. Each row was either reproduced in this
session or read at the line cited.

**1. A nil tracer provider, meter provider or propagator — three behaviours.**

| Package | Entry point | nil `tp` / `mp` | nil `prop` |
|---|---|---|---|
| `redis` | `Wrap` | error (`client.go:372-377`) | no parameter |
| `cassandra` | `NewSession` | error (`session.go:61-66`) | no parameter |
| `elasticsearch` | `NewClient`, `NewTypedClient` | error (`client.go:204-209`) | no parameter |
| `nats` | `Connect`, `ConnectWithOptions` | error for `tp` (`conn.go:158-163`); no `mp` | error |
| `mongo` | `Instrument`, `Connect` | error (`client.go:134-139`) | error (`:140`), although the propagator is never used |
| `mongo` | `NewMonitor` | no-op provider (`client.go:166-171`) | no parameter |
| `resty` | `Wrap`, `NewClient` | no-op provider (`client.go:54-59`) | W3C TraceContext (`:60-62`) |
| `minio` | `New` | no-op provider (`client.go:87-92`) | accepted and ignored (`:93-97`) |
| `gin` | `Middleware` | OTel global (S2-03) | OTel global |
| `http` | `NewServerHandler`, `NewTransport` | OTel global (S2-03) | OTel global |

Two details inside the table: `mongo` rejects a nil propagator it has no use
for, while `minio` accepts and discards one for the same "API consistency"
reason; and `minio/client.go:71` documents `tp`, `mp` and `prop` as
"required" although nil is tolerated for all three. `resty` defaulting a nil
propagator to TraceContext means baggage is dropped on outgoing requests
without a signal, where the SDK's own propagator carries both.

**2. A second instrumentation call.** `resty.Wrap` and `redis.Wrap` are
idempotent by design and say so. `cassandra.NewSession` restores the caller's
observers on the `ClusterConfig` so a repeated call does not stack
(`session.go:79-86`). `mongo.Instrument` composes onto whatever monitor is
already on the options (`client.go:150-151`), so a second call on the same
`ClientOptions` — a retry loop around a failed connect is the likely way to
get there — installs a second otelmongo monitor and a second pool tracker.
Reproduced: after two `Instrument` calls one `find` command produced two
`mongodb.find users` spans. The godoc warns against sharing one instrumented
`ClientOptions` between clients but not against calling `Instrument` twice.

**3. Error wrapping.** `nats` (`nats connect …`), `mongo.Connect`
(`mongo connect: …`), `elasticsearch` (`elasticsearch new client: …`),
`minio` (`minio new: …`) and `redis` (`redis wrap: …`) prefix what they
return. `cassandra.NewSession` returns gocql's `CreateSession` error and its
own instrument-creation error bare (`session.go:70`, `:88`), and
`mongo.Instrument` called directly returns `tracer provider must not be nil`
with no package name (`client.go:131-141`) — the prefix is added only by
`Connect`. Only `nats` redacts the upstream error (S2-04).

**4. Metric labels.**

- *Default pool name.* Three schemes: `redis-<host>:<port>-<n>` with the
  lowest free `<n>` per address, reused; `mongo-<host>-<seq>` from a global
  counter, never reused, no port (S2-05); `cassandra/<host>:<port>/<keyspace>`
  with no number, so two sessions on the same contact point and keyspace
  share a name (documented at `session.go:121-128`).
- *`server.address` on `db.client.operation.duration`.* Present for `redis`,
  `mongo` (through otelmongo), `elasticsearch` and `minio`; absent by default
  for `cassandra` since this window (`WithCoordinatorMetricLabel`). The
  cassandra choice is argued in ADR 0019 §7 and documented in
  `docs/semconv.md:570-605`; it is listed here because a dashboard that
  groups the shared histogram by `server.address` now has an empty group for
  one system.
- *Process-level state.* `resty`, `redis` and `cassandra` each keep a
  package-level registry keyed by the client pointer with a
  `runtime.AddCleanup` eviction; `redis` adds the `poolNames` allocator and
  `mongo` the `defaultPoolNameSeq` counter. None is an OTel global and none
  runs at package initialisation, so ADR 0003 is not engaged; the effect is
  that default pool names are numbered across every SDK instance and every
  test in one process.

None of these is a defect on its own beyond what S2-03, S2-04 and S2-05
already record. Together they mean a caller cannot carry an expectation from
one integration to the next. A short "integration contract" section in
`docs/guide.md` — nil is an error, constructors are idempotent or say they
are not, errors are prefixed and redacted — would give new integrations
something to conform to.

### 4.3 Session 3 — documentation and examples

Scope: `README.md`, `docs/guide.md`, `docs/semconv.md`, `docs/adr/`,
`AGENTS.md`, `examples/` with `examples/README.md`, package documentation,
and `k8s/infrastructure/` for obvious errors only.

Method: every `o11y*.Name` and `With*` identifier the documents mention was
checked against the exported symbols of the tree; every backticked dotted key
in the four user-facing documents was checked against the key and metric-name
literals of the pinned `semconv/v1.39.0` package source in the module cache;
every `go run` path was checked to exist; the guide's integration sections
were read against the constructors and option defaults they describe. An
item is **Confirmed** when the two texts (or the text and the file listing)
were put side by side by a command, or when a scratch run reproduced the
behaviour; where a consequence was only reasoned, the item says so.

Most of what these documents say holds. The option tables, defaults,
constructor signatures, the NATS request/reply error contract, the Redis
dropped-command list and the gin skip paths all match the code. Section 8.3
lists what was checked.

#### S3-01 · P3 · Confirmed · The guide says both that `Consumer.Next` is not wrapped and that it is

- `docs/guide.md:1624-1625`: "Single-message `Consumer.Next` is a different
  case — deferred, not wrapped; see below."
- `docs/guide.md:1731-1732`: "Single-message `Consumer.Next` is wrapped since
  the otel-nats v0.6.0 upgrade".
- `nats/jetstream.go:628`: `func (c *consumer) Next(ctx, opts...)
  (context.Context, jetstream.Msg, error)` exists and restores baggage.

A reader who stops at the first statement drops to the upstream package for
single-message pulls, which is what the "Do NOT" list forbids. The earlier
review reported the same contradiction (R-45); the first sentence was not
removed. `AGENTS.md` has it right.

#### S3-02 · P3 · Confirmed · Two documented paths do not exist

- `docs/guide.md:371` links `k8s/infrastructure/base/prometheus.yaml`. The
  file is `k8s/infrastructure/base/monitor/prometheus.yaml`; the link is dead.
- `AGENTS.md:200` gives `kubectl apply -k k8s/infrastructure/base` as the
  usual Kustomize entry point. `k8s/infrastructure/base/` holds no
  `kustomization.yaml` (only `monitor/`, `components/` and two loose files),
  so that command fails. The same file's command list, lines 105-116, uses
  the correct `base/monitor` and `base/components/<name>` paths.

Both were confirmed with a directory listing. The `kubectl` failure itself
was not run (no cluster tooling here); `kubectl apply -k` requires a
kustomization file in the target directory.

#### S3-03 · P3 · Confirmed · Two orphaned manifests sit next to the live ones, and one has drifted

`k8s/infrastructure/base/cassandra.yaml` and
`k8s/infrastructure/base/elasticsearch.yaml` are referenced by no
kustomization (`grep -rn 'cassandra.yaml\|elasticsearch.yaml' k8s` finds only
the two `components/*/kustomization.yaml` entries, which point at their own
directory). The Cassandra copy is byte-identical to
`components/cassandra/cassandra.yaml` (`cmp`). The Elasticsearch copy differs
from `components/elasticsearch/elasticsearch.yaml` at line 32: `runAsGroup: 0`
in the orphan, `runAsGroup: 1000` in the live file.

Trigger: someone fixes or applies the file at the top of `base/` because it
is the first one a search finds, and either the fix never reaches the
cluster or the stale security context does. Delete the two files.

#### S3-04 · P3 · Confirmed (text) / Speculative (port conflict) · Local profiling instructions disagree about what listens on `localhost:4040`

- `README.md:47` and `docs/guide.md:480` describe the path as application →
  Alloy → Pyroscope, and `docs/guide.md:494-495` says "For local development,
  port-forward Alloy and use `WithProfilingEndpoint("http://localhost:4040")`".
- `kind-config.yaml:10-11` maps host port 4040 to node port 30002, and
  `k8s/infrastructure/base/monitor/pyroscope.yaml:74-81` gives node port 30002
  to the **Pyroscope** Service. `AGENTS.md` records the mapping the same way.
- `examples/README.md:36-37` says "4318 (otel-collector) and 4040
  (alloy/pyroscope) are already bound to host ports via kind-config.yaml — no
  port-forward needed", and `examples/README.md:81-82` then says "Keep the
  Alloy and Grafana port-forwards from the setup block running"; the setup
  block has no Alloy port-forward.

So on the documented kind cluster `localhost:4040` is Pyroscope itself, and a
local run pushes profiles past Alloy — the opposite of the flow the README
draws. Reasoned, not run: following the guide literally
(`kubectl port-forward svc/alloy 4040:4040`) should fail to bind, because
kind already holds host port 4040. Profiles still arrive either way, which is
why nobody has noticed; the cost is a reader debugging an Alloy pipeline that
their profiles never touch. Say in all three places that the kind mapping
goes to Pyroscope directly, or move the mapping to Alloy's Service.

#### S3-05 · P3 · Confirmed · `examples/README.md` says no example sets the OTLP endpoint; the profiling example does

`examples/README.md:17-22`: "the examples do not pass `WithOTLPEndpoint`, so
an `OTEL_EXPORTER_OTLP_ENDPOINT` … variable exported in your shell redirects
traces and logs". `examples/profiling/main.go:34-43` reads `OTLP_ENDPOINT`
(default `http://localhost:4318`) and passes it to both `WithOTLPEndpoint` and
`WithMetricsOTLPEndpoint`. For that one example the standard variable is
overridden and has no effect; the variable that does work, `OTLP_ENDPOINT`,
is named only in the profiling section (`examples/README.md:80-81`). Either
drop the option from the example or name the exception in the paragraph.

#### S3-06 · P3 · Speculative · The examples exit through `os.Exit(1)` past the deferred `Shutdown`

Forty call sites in `examples/` log a failure and call `os.Exit(1)`; the
ones after `Init` (for instance `examples/jetstream/publisher/main.go:57`,
`:66`, `:78`) sit below `defer func() { … obs.Shutdown(…) }()`. `os.Exit`
does not run deferred functions, so the batchers are never flushed: the
`ERROR` record that explains the exit reaches stdout but not the OTLP log
pipeline, and the spans leading to it are dropped. Not run; it follows from
`os.Exit`'s contract and the batch processors' export delay.

The examples are what a service copies for its `main`. The README's own
snippet (`README.md:140-143`) returns from `main` instead, which is the
pattern to copy: a `run() error` function with the defer inside it, and one
`os.Exit` in `main`.

#### S3-07 · P3 · Confirmed · The semconv verification skill's lookup command finds nothing

`.agents/skills/verify-semconv-attributes/SKILL.md`, step 2:

```bash
SRC=$(find "$(go env GOMODCACHE)" -path "*otel/semconv/$SEMV/attribute_group.go" | head -1)
```

The package lives at
`…/pkg/mod/go.opentelemetry.io/otel@v1.46.0/semconv/v1.39.0/attribute_group.go`.
The pattern requires the literal `otel/semconv`, and the module directory is
`otel@v1.46.0`, so it never matches. Run here with the modules downloaded,
the command printed an empty string; the `grep … "$SRC"` that follows then
fails on an empty file name.

Core Principle 5 makes this skill mandatory before any attribute key is
written or defended, and its "one rule" is to read the literal from this
file. With the first command returning nothing, the path of least
resistance is the one the skill exists to prevent — answering from memory.
Fix: `-path "*otel@*/semconv/$SEMV/attribute_group.go"`, or resolve the
directory with `go list -m -f '{{.Dir}}' go.opentelemetry.io/otel`.

#### S3-08 · P3 · Confirmed · `AGENTS.md` and the README overview stop short of the current package set

`AGENTS.md` is the canonical context file for assistants and the README is
the first contact for users. Compared with the tree:

| Place | Says | Tree |
|---|---|---|
| `AGENTS.md` Package Layout (lines 40-51) | root, `http/`, `gin/`, `nats/`, `mongo/`, `elasticsearch/`, `redis/`, `internal/`, `examples/` | also `resty/`, `minio/`, `cassandra/`, `obsctx/`, `o11ytest/` |
| `AGENTS.md` Tech Stack and ADR summary | no row for Cassandra, MinIO or Resty | ADR 0011, 0018, 0019 are accepted and implemented |
| `AGENTS.md` "Do NOT" | rules for NATS, MongoDB, gin, Redis | no rule for the Cassandra batch seam (`session.ExecuteBatch` emits no span, `docs/guide.md:1301-1304`) or for bypassing `o11yresty` / `o11yminio` |
| `README.md:24-35` Infrastructure | NATS, MongoDB, Redis, Cassandra | also Elasticsearch and MinIO |
| `README.md:376-379` sub-package list | NATS, MongoDB, Redis, Elasticsearch, HTTP, Resty, MinIO, gin | also Cassandra |
| `README.md:407-410` examples list | gin, Resty, MongoDB, Redis, NATS, MinIO | also Cassandra, Elasticsearch, background |
| `README.md:73` | "an extra port mapping for the OTel Collector (port `4318`)" | `kind-config.yaml` maps three ports (4318, 4223, 4040) |

The practical gap is the first three rows: an assistant working from
`AGENTS.md` alone does not learn that three integrations exist or that
Cassandra batches must go through `cassandra.ExecuteBatch`. The earlier
review listed the Package Layout and examples-list rows (R-45); they are
unchanged.

#### S3-09 · P3 · Confirmed · Stale statements in documents and comments

Each was checked with a grep against the file named.

- `docs/guide.md:238` and `internal/log/provider.go:19` name the Resource
  attribute `deployment.environment`. The pinned package has no such key; it
  is `deployment.environment.name` (`README.md:170` and `AGENTS.md` have it
  right).
- `o11y.go:70`, the `SDK.Logger` field comment, says stdout is collected "via
  Fluentd". The stack and every other document use Alloy.
- `README.md:245-252` is a "Migration note (pre-1.0 API change)" about
  `DefaultLatencyBuckets` becoming a function and the provider accessors'
  return types. `CHANGELOG.md` records those changes under 0.1.0
  (2026-05-22); the note still sits under the options table as if current.
- `README.md:220` tells callers to pair `WithExtraHTTPServerAttributeKeys`
  with `o11ygin.WithMetricAttributesFn` "/ `otelhttp` equivalent";
  `http/options.go:29-31` marks `o11yhttp.WithMetricAttributesFn`
  `Deprecated:` in favour of otelhttp's Labeler.
- `nats/conn.go:36-39`, the `Conn` type comment, lists `Request` among the
  methods "available directly" from the embedded connection. `Request` and
  `RequestMsg` are overridden by ctx-first methods (`nats/conn.go:336`,
  `:356`), and `Respond` and `JetStream` are not mentioned.
- `nats` and `mongo` each carry two package comments (`nats/conn.go:1` and
  `nats/doc.go`; `mongo/client.go:1` and `mongo/doc.go`), which godoc joins
  into one text.
- `docs/adr/0015-trace-sampling-strategy.md:83` heads its verification
  "OTel Go SDK v1.43.0"; `go.mod` pins v1.46.0.
- `docs/adr/0023-span-naming-convention.md:39` argues from "pre-1.0 with no
  tagged release"; `CHANGELOG.md` lists releases from 0.1.0 to 0.15.0.
- `docs/adr/0017-echo-integration.md` has status "Accepted" and its scope
  note defers the `echo/` package to a follow-up. No such package exists and
  the status does not say so, where its neighbours write "Accepted
  (implemented)".

The earlier review listed the Fluentd, ADR 0015, ADR 0017, ADR 0023,
migration-note and `WithMetricAttributesFn` items (R-45); none was changed.
ADRs are historical records, so the three ADR items want a one-line status
amendment, not a rewrite.

## 5. Tool results

### 5.1 Session 1

All runs were against `03537c3` with a clean working tree.

| Command | Result |
|---|---|
| `go vet ./...` | exit 0, no output |
| `go test -race ./...` (run once) | exit 0; all 22 packages with tests report `ok` (root, `cassandra`, `elasticsearch`, `gin`, `http`, the ten `internal/*` packages, `minio`, `mongo`, `nats`, `o11ytest`, `obsctx`, `redis`, `resty`) |
| `make semconv-check` | exit 0; the script's own test passes and the scan reports nothing |
| `make lint` with the golangci-lint found on `PATH` (2.5.0, built with go1.25) | `adr-check` and `semconv-check` pass; golangci-lint exits 3: "the Go language version (go1.25) used to build golangci-lint is lower than the targeted Go version (1.26.0)" |
| `make lint` with golangci-lint v2.11.4 (the CI pin), built with go1.26.11 into a scratch `GOBIN` | exit 0; `0 issues.` |
| `make sast` (gosec v2.26.1 built with go1.26.11, semgrep 1.163.0) | exit 0; `semgrep=PASS directives=PASS gosec=PASS rule-tests=PASS`. gosec: 235 files, 61,779 lines, 142 `#nosec`, 0 issues. semgrep: 1 rule over 172 files, 0 findings; rule fixture test 1/1 |
| `go mod tidy -diff`, `gofmt -l .` (extra) | both clean |
| `govulncheck ./...` | not run: the proxy blocks vuln.go.dev. CI runs it |

Notes:

- The two `SA5011` reports in `minio/client_test.go` that are known from other
  local runs did not appear with v2.11.4 built from source; the run reported
  zero issues.
- Tools were installed from a directory outside the module (`go install` with
  `GOTOOLCHAIN=go1.26.11`, `pip install --user semgrep==1.163.0` because pipx
  is not present here); `go.mod` and `go.sum` are unchanged.
- semgrep reports "Scan was limited to files tracked by git", so an untracked
  file is not scanned locally. CI checks out tracked files only, so the gate
  is unaffected.

### 5.2 Session 2

- `go test -race` on the eleven packages in scope (`./http/... ./gin/...
  ./nats/... ./mongo/... ./elasticsearch/... ./redis/... ./cassandra/...
  ./resty/... ./minio/... ./obsctx/... ./o11ytest/...`): all pass, run once.
- Scratch tests were injected with `go test -overlay` as an extra
  `_test.go` file in the package under test, so unexported helpers could be
  called without writing to the tree. One mutation run replaced
  `cassandra/options.go` the same way (S2-06).
- One scratch benchmark in `resty/` (S2-02).
- Not run in this session: golangci-lint, govulncheck, `make sast`,
  `make semconv-check` (session 1 covered the whole tree).
- Attribute keys: every key added or changed in the six diffs is written
  through a constant of the pinned `semconv/v1.39.0` package
  (`DBClientConnectionPoolName`, `DBClientConnectionStateUsed` / `Idle`,
  `ExceptionType`, `ExceptionMessage`, `ExceptionEventName`), so there was no
  string literal to resolve against the package source. The two SDK-owned
  literals touched, `gin.error.type` and `resty.error.kind`, are outside
  semconv by design.

### 5.3 Session 3

- `go build ./...` (examples included): exit 0.
- Identifier check: every `o11y*.Name`, `obsctx.Name`, `sdk.Name` /
  `obs.Name` and `With*` token in `README.md`, `docs/guide.md`,
  `docs/semconv.md`, `examples/README.md`, `AGENTS.md` and the 27 ADRs was
  compared with the exported symbols of the non-test, non-example tree. In
  the four user-facing documents and `AGENTS.md` every miss is an upstream
  option (`resource.WithProcess`, `jetstream.WithMsgID`, `context.WithTimeout`
  and the like) or `WithTeam`, which `AGENTS.md` names as removed. The misses
  in the ADRs are mostly upstream options; the SDK-shaped ones that were
  read in context (ADR 0005, 0007, 0012, 0021, 0026) are each marked by the
  ADR as proposed, deferred, hypothetical or removed.
- Attribute-key check, per the `verify-semconv-attributes` skill: the key
  literals of `attribute_group.go` and the metric names of the `*conv`
  packages under
  `go.opentelemetry.io/otel@v1.46.0/semconv/v1.39.0` (1,020 distinct strings)
  were extracted from the module cache, and every backticked dotted token in
  `docs/semconv.md`, `docs/guide.md`, `README.md` and `AGENTS.md` was
  compared with that list. Each token not in the list was read in context:
  all are SDK-owned names with a Deviations row, legacy keys the catalog
  attributes to an upstream, keys listed as deliberately not emitted, or
  example values — except `deployment.environment` (S3-09). The skill's own
  lookup command returned nothing (S3-07); the package directory was located
  by hand.
- Instrument names: the 21 instrument creations in non-test code were
  listed and each name found in `docs/semconv.md`.
- `go run` paths in `AGENTS.md` and `examples/README.md`: all exist.
- `AGENTS.md`'s image-coverage command: the 13 images under
  `k8s/infrastructure/base/monitor` and `base/components` each have a
  `newName` entry in the private-registry overlay. The three `NodePort`
  services match the three `extraPortMappings` in `kind-config.yaml`.
- One scratch test file, injected into the root package with
  `go test -overlay` (five tests, all run once): the toggle variable with a
  trailing space (R-25), `obsctx.Go` with a zero timeout (R-26), an empty
  metrics address (R-36), the repeated scope-guard warning (R-37) and the
  provider replacement through `http.Option` (R-32). Results are in
  section 6.1.
- `go list -m -versions` for `otelmongo` (run outside the module): no tagged
  version (SDK-26).
- Not run in this session: `go test -race ./...`, golangci-lint,
  govulncheck, `make sast`. No kind cluster or `kubectl` was available, so
  nothing under `k8s/` was applied.

## 6. Status of earlier findings

Two earlier reports were read from
`origin/claude/o11y-newchat-integration-priority-sbwb9s`:
`docs/reviews/2026-09-14-sdk-full-review.md` (findings R-1 to R-45, written
against `afbd622`) and `docs/reviews/2026-09-06-newchat-launch-readiness-review.md`
(SDK-1 to SDK-30; its APP-n items concern the newchat repository and are
skipped). Each SDK item was compared with the code at `03537c3`.

How to read the status column:

- **Fixed** — the code or text the finding named has changed so that the
  trigger no longer applies; the location of the change is given. Unless a
  row says "reproduced", this was established by reading the current code,
  not by re-running the earlier scratch test.
- **Still present** — the code the finding named is unchanged in the
  relevant respect. "Reproduced" means a scratch test was run in this
  session (section 5.3); otherwise it was established by reading.
- **Partly fixed / changed** — some of it was addressed, or the finding now
  takes a different form; the row says which.

### 6.1 The 2026-09-14 review (R-1 to R-45)

| ID | Was | Finding | Status at `03537c3` |
|---|---|---|---|
| R-1 | P1 | Mongo operation histogram never carries exemplars and reports an exemplar-overflow error on every scrape | **Fixed.** `internal/metrics/reserved.go:278` composes `dropFilteredAttrs` onto every view stream that has a filter, not only the HTTP views |
| R-2 | P1 | Mongo pool idle gauge drifts after a handshake that fails before `ConnectionReady` | **Fixed.** `mongo/pool_metrics.go:382-416` tracks ready connections; the close of a connection that never became ready is not counted |
| R-3 | P1 | Profiler log adapter echoes the endpoint's userinfo at DEBUG | **Fixed.** `internal/profiling/profiling.go:205-298` redacts the endpoint, its Basic-auth form and the auth header forms in every adapter line |
| R-4 | P2 | Standard OTLP endpoint variables validated, then ignored | **Fixed.** `endpoint.go:69-93` resolves option > signal variable > generic variable > default; documented in `docs/guide.md` "OTLP endpoint" |
| R-5 | P2 | `nats.Connect` errors carry the URL with credentials | **Fixed.** `nats/conn.go:422-428` (`redactedURLs`). One residue, new in this review: an unparseable URL now yields an error with no cause (S2-07) |
| R-6 | P2 | resty `url.full` keeps userinfo and the query string | **Fixed** for userinfo and the four keys semconv names: `resty/hook.go:446-473` through `redact.URLAttribute`. Residue: SigV4 presigned-URL keys are not on the list (S1-03) |
| R-7 | P2 | Redis Cluster pool observation issues `CLUSTER SLOTS` on every metrics collection | **Still present.** `redis/metrics.go:138` still calls `ClusterClient.ForEachShard`; go-redis is still v9.9.0, where `ForEachShard` calls `ReloadOrGet`, which reloads first (`osscluster.go:914-920`, `:1160-1167`, read in the module cache). Not re-run: no cluster here |
| R-8 | P2 | Ring shards that are down at `Wrap` time are never instrumented | **Still present.** `redis/client.go:280` and `redis/metrics.go:154` still enumerate through `Ring.ForEachShard`, which skips a shard whose `IsDown()` is true (`ring.go:651-662`). `docs/adr/0013-redis-valkey-integration.md:739-742` and `:1533-1541` now describe the gap. Not re-run |
| R-9 | P2 | An attribute with an empty key removes its family from `/metrics` | **Fixed.** `internal/metrics/reserved.go:86-92` treats `""` as reserved |
| R-10 | P2 | `ErrorRecorder` records every gin error twice | **Fixed.** otelgin is now v0.71.0, which records no exception event itself; the chain's handler records one typed event per error (`gin/errors.go`, `chainErrors`), and `ErrorRecorder` is documented as for chains without `Middleware` only (`gin/errors.go:21-33`) |
| R-11 | P2 | `Conn.Request` timeout is `context.DeadlineExceeded`, never `nats.ErrTimeout` | **Fixed.** `nats/conn.go:391-407` wraps both sentinels; contract in the `Request` godoc and `docs/guide.md:1552-1568` |
| R-12 | P2 | No `ForceFlush` | **Fixed.** `o11y.go:239` (`SDK.ForceFlush`); `docs/guide.md:327-369` |
| R-13 | P2 | 16 public functions with no test | **Fixed** for the 16 named: each is now referenced from a test file of its package, and `internal/trace` has `trace_test.go`. A new gap of the same kind: three `cassandra` options (S2-06) |
| R-14 | P2 | Literal semconv keys in `redis` | **Fixed.** No `"db.client.connection.*"` or `"used"` / `"idle"` literal remains in `redis/`; `make semconv-check` now gates this (with the blind spot in S1-08) |
| R-15 | P2 | `gin.error.type` and `pyroscope.profile.id` missing from the catalog | **Fixed.** Both have rows in `docs/semconv.md` |
| R-16 | P2 | v0.13.0 needs a `### Migration` section | **Fixed.** `CHANGELOG.md:935`, under 0.13.0 |
| R-17 | P2 | `WithHistogramBuckets` documented as HTTP-only | **Fixed** in the documents: `README.md:214` and `docs/guide.md:386-407` list every histogram the option reaches, and `WithInstrumentHistogramBuckets` reshapes one alone |
| R-18 | P2 | Redis default pool name is a heap address | **Fixed.** Names are `redis-<host>:<port>-<n>` (`docs/guide.md:1071-1099`). The three integrations now use three different schemes (S2-08), and Mongo's has a restart-order problem (S2-05) |
| R-19 | P2 | Redis `used` gauge can wrap to 4.29e9 | **Fixed.** `redis/metrics.go:167-168` widens before subtracting and caps idle at total |
| R-20 | P2 | Custom-metric ergonomics undocumented (private registry, unit suffixes, default buckets) | **Still present.** A search of `README.md` and `docs/guide.md` for `promauto`, "custom metric", "private registry" and `DefaultRegisterer` finds nothing. What was added nearby: the cardinality limit is documented as covering the application's own instruments, and the RPC histograms have views |
| R-21 | P3 | Scheme-less endpoints pass validation | **Fixed.** `endpoint.go:143-181` requires an `http://` or `https://` URL with a host and names the option or variable in the error |
| R-22 | P3 | `WithOTLPEndpoint` with a path prefix sends both signals to the wrong path | **Changed.** The generic variable is now a base URL and behaves correctly. The option keeps its own per-signal rules, which the guide now documents (`docs/guide.md:534-539`) and which still split the signals for a trailing slash or a path (S1-01) |
| R-23 | P3 | `Shutdown` logs and returns exporter errors unredacted | **Fixed** for the rendered text (`o11y.go:328`). Residue: `errors.Unwrap` returns the unredacted error (S1-05) |
| R-24 | P3 | otelslog instrumentation scope version is set to the service version | **Still present.** `o11y.go:637-639` |
| R-25 | P3 | `O11Y_*_ENABLED` values are not trimmed | **Still present, reproduced.** `options.go:871-885`. `O11Y_TRACE_ENABLED="false "` logs a WARN and leaves `Toggles.Trace` true |
| R-26 | P3 | `obsctx.Go(ctx, 0, fn)` runs `fn` with an expired context | **Still present, reproduced.** `obsctx/obsctx.go:47-49`, `:64`. Inside `fn`, `ctx.Err()` is already `context deadline exceeded` |
| R-27 | P3 | A configured header name is redacted wherever it appears in diagnostic text | **Still present**; S1-04 reproduces it together with two related cases |
| R-28 | P3 | `MessagesContext.Next` returns a nil ctx on error | **Fixed.** `nats/jetstream.go:654-667` |
| R-29 | P3 | `Fetch(ctx, -1)` panics | **Fixed.** `nats/jetstream.go:405` (`checkBatch`) |
| R-30 | P3 | `Inject` / `Extract` panic on nil | **Fixed.** `nats/middleware.go:76-95` |
| R-31 | P3 | `restoreBaggage` re-parses `traceparent` per message | **Not re-measured.** `nats/conn.go:72-85` still runs the full propagator's `Extract`; it returns early for empty headers and for disabled tracing |
| R-32 | P3 | `http.Option` is an alias of `otelhttp.Option`, so a caller option can replace the SDK provider | **Still present, reproduced.** `http/options.go:11`. With `otelhttp.WithTracerProvider(other)` passed to `NewServerHandler`, the span was recorded on `other` and none on the SDK provider |
| R-33 | P3 | resty `Wrap` swallows the histogram-creation error | **Still present.** `resty/client.go:91-102` |
| R-34 | P3 | minio `New` substitutes no-op providers for nil and discards the histogram error | **Still present.** `minio/client.go:87-92`, `:136-142`; now one of the three nil behaviours in S2-08 |
| R-35 | P3 | mongo `Connect` drops the pool-metrics cleanup on success | **Still present.** `mongo/client.go:86-97` |
| R-36 | P3 | `WithMetricsAddr("")` binds a random port silently | **Still present, reproduced.** `options.go:490-494`. `Init` succeeds and logs nothing about the address |
| R-37 | P3 | The scope guard logs on every `Meter()` call | **Still present, reproduced.** `internal/metrics/scopeguard.go:56-63`. Three `Meter()` calls with a colliding scope attribute produced three WARN records |
| R-38 | P3 | Exemplar comments overstate the failure as a failed scrape | **Fixed.** `internal/metrics/exemplar.go:25-29` |
| R-39 | P3 | Scrape-error log line is unredacted and at WARN | **Still present.** `internal/metrics/scrapelog.go:79-83` |
| R-40 | P3 | `metricscap` clones every capped datapoint on every export; non-string values are never capped | **Still present.** `internal/metricscap/reader.go:405-406`; the package is unchanged since `afbd622` |
| R-41 | P3 | Scrape server has only `ReadHeaderTimeout`; `Serve` error discarded | **Still present.** `internal/metrics/metrics.go:534-536` |
| R-42 | P3 | Per-command attribute sets rebuilt on every call (redis, cassandra) | **Not re-checked** (performance only) |
| R-43 | P3 | `examples/nats-ws-browser` sets `messaging.operation.type` to `publish` | **Still present.** `examples/nats-ws-browser/backend/main.go:122`. The pinned package defines `create`, `send`, `receive`, `process` and `settle` only (`attribute_group.go:11070-11090`). The hand-rolled carrier (`:100`, `:133`) is also unchanged |
| R-44 | P3 | Deprecated calls and G115 conversions | **Partly fixed.** `attribute.Value.Emit` is gone from `internal/metrics/targetinfo.go`; the G115 conversions are bounds-checked (CHANGELOG 0.15.0). The deprecated `gocql.NewBatch` calls in tests were not re-checked |
| R-45 | P3 | Documentation drift list | **Partly fixed.** Fixed: the `url.full` row in `docs/semconv.md`; ADR 0010 carries an amendment correcting its `RecordError` row (`docs/adr/0010-gin-integration.md:33`, `:366-368`); ADR 0013 now describes the Ring gap. Unchanged, and restated in this review: the `Consumer.Next` contradiction (S3-01), the `prometheus.yaml` link (S3-02), the `AGENTS.md` package layout and README examples list (S3-08), "Fluentd", the 0.1.0 migration note, the deprecated-option recommendation, the `nats.Conn` type comment, ADR 0015 / 0017 / 0023 (S3-09), the `mongo.WithPoolName` godoc (S2-05), and the ADR gate's coverage (S1-07). Not re-checked: `//nolint` without a reason, and the hand-rolled env restore in `o11y_test.go` |

Count: of the 45, 23 are fixed (five of them — R-5, R-6, R-13, R-18 and
R-23 — with a residue or a neighbouring gap recorded as a new finding), 17
are still present, 3 are partly fixed or changed (R-22, R-44, R-45), and 2
were not re-checked (R-31, R-42).

### 6.2 The 2026-09-06 review (SDK-1 to SDK-30)

The 2026-09-14 review already recorded SDK-1, 4, 5, 6, 7, 9, 14 and 15 as
fixed. Three were spot-checked by reading and still hold:
`resource.WithProcess()` appears only in the comment that explains its
absence (`o11y.go:817`), `WithTraceSampler(nil)` returns without touching the
configuration (`options.go:227-236`), and the environment name is trimmed
and lower-cased before matching (`o11y.go:1073`). The others were not
re-tested.

| ID | Was | Finding | Status at `03537c3` |
|---|---|---|---|
| SDK-2 | P1 | Link-consistent sampling across NATS hops | **Still open** (deferred by decision). No such option exists; `docs/guide.md:228-232` describes `ParentBased` thinning only |
| SDK-3 | P1 | Prometheus default-registry bridge | **Dropped** by decision in the earlier review. The written decision and the accompanying documentation it asked for are still missing (R-20) |
| SDK-8 | P1 | Soften the v0.12.0 `elasticsearch.NewClient` break | **Fixed** as a migration note: `CHANGELOG.md:1144-1146` |
| SDK-10 | P2 | gin: a panic leaves no metric and no status when `gin.Recovery` runs first | **Changed.** The required order is documented (`gin/doc.go`, `docs/guide.md:842-845`); the facade still does not recover or detect the wrong order |
| SDK-11 | P2 | Document that the NATS consumer span ends when the callback returns | **Still present.** No such statement in `docs/guide.md` or the `Subscribe` / `Consume` godoc. (The guide does say it for `Fetch`: `docs/guide.md:1717-1729`) |
| SDK-12 | P2 | No panic recovery in `Subscribe` / `Consume` | **Still present.** No `recover()` in `nats/` |
| SDK-13 | P2 | redis Cluster: `CLUSTER SLOTS` per scrape; `Wrap` has no deadline | **Still present**; see R-7 and R-8 |
| SDK-16 | P2 | Public-ingress propagator and inbound-baggage stripping helpers | **Partly fixed.** `ContextWithoutBaggageValues` (`attributes.go:85`) and the guidance in `docs/guide.md:138-153`; no ready-made TraceContext-only propagator |
| SDK-17 | P2 | Collector-outage runbook, compression and queue-size options | **Partly fixed.** `docs/guide.md` "Export failures & OTel diagnostics" covers the outage signals and the `OTEL_BSP_*` variables; there is no compression option |
| SDK-18 | P2 | SDK-owned NATS client metrics | **Still open.** `nats/` creates no instrument |
| SDK-19 | P3 | Bucket views by unit | **Still open.** Only named instruments have views; `WithInstrumentHistogramBuckets` is the per-name workaround |
| SDK-20 | P3 | Views for the HTTP body-size histograms | **Still open.** No view names `body.size` |
| SDK-21 | P3 | Cassandra: un-bypassable batch seam, tokenisation cache, `db.namespace` cap | **Not re-checked** beyond the seam, which is unchanged by design (`docs/guide.md:1301-1308`) |
| SDK-22 | P3 | Mongo: tie instrumentation cleanup to the client lifecycle | **Still present**; see R-35 |
| SDK-23 | P3 | `telemetry.sdk.*` missing; otelslog scope version | **Partly fixed.** `telemetry.sdk.*` is on the Resource (`README.md:177`); the scope version is R-24 |
| SDK-24 | P3 | Dynamic log level | **Still open.** `WithLogLevel` takes a fixed level |
| SDK-25 | P3 | `o11ytest.New(t)` | **Still open.** `o11ytest` has `CanceledRequestContext` and `RequireNotCanceled` only |
| SDK-26 | P3 | Pin a released `otelmongo` tag | **Still present.** `go.mod:25` is the pseudo-version `v0.0.0-20260622212340-49857026d46e`; `go list -m -versions` on 2026-11-14 shows the module has no tagged version to move to |
| SDK-27 | P3 | `/metrics` server timeouts and bind-failure handling | **Still present**; see R-41 and R-36 |
| SDK-28 | P3 | Hot-path allocations | **Not re-measured** |
| SDK-29 | P3 | Documentation items | **Partly fixed.** `docs/guide.md:200-212` states that `OTEL_TRACES_SAMPLER` is honoured. A search of the guide finds no JetStream `FilterSubject` span-name rule and no `sampled` log field; the remaining items were not checked |
| SDK-30 | P2 | Low-level Elasticsearch client counts routine 404s as failures; no override | **Changed.** `docs/guide.md:1208-1212` documents the rule; `elasticsearch` still exposes only `WithSearchBody` and `WithCollectionMetricLabel` |

### 6.3 Earlier findings still present that sessions 1 and 2 did not report

Sessions 1 and 2 reviewed the code changed since `afbd622` in depth and
scanned the rest, so a finding that sits in unchanged code could be missed
by design. These are the ones that are still present and appear nowhere in
sections 4.1 and 4.2. The severity is the earlier review's.

| ID | Sev | Where | Problem |
|---|---|---|---|
| R-7 / SDK-13 | P2 | `redis/metrics.go:138` | On a Redis Cluster client every metrics collection — each Prometheus scrape, or each export tick on the push path — makes a `CLUSTER SLOTS` round trip, so scrape latency and scrape failure depend on Redis. (The earlier review also thought a span and a histogram sample per scrape plausible; that part was never exercised, then or now) |
| R-8 | P2 | `redis/client.go:280`, `redis/metrics.go:154` | A Ring shard that is down when `Wrap` runs is never hooked, so its commands and pool stay invisible after it recovers |
| R-20 | P2 | `README.md`, `docs/guide.md` | Nothing tells a service that metrics registered on the Prometheus default registry never appear on the SDK's `/metrics`, that names gain unit suffixes, or that a seconds histogram needs explicit boundaries |
| SDK-12 | P2 | `nats/conn.go`, `nats/jetstream.go` | A panic in a `Subscribe` or `Consume` handler is not recovered by the facade |
| SDK-11 | P2 | `docs/guide.md`, `nats/` godoc | Not documented that the consumer span ends when the handler returns, so work handed to a goroutine is not covered by it |
| SDK-30 | P2 | `elasticsearch/client.go` | A routine 404 from the low-level client is recorded as an error on the metric and the span; documented, but there is no way to exempt an operation |
| R-25 | P3 | `options.go:871-885` | `O11Y_TRACE_ENABLED="false "` (trailing space) leaves tracing on with a warning, while `WithEnvironment` trims |
| R-26 | P3 | `obsctx/obsctx.go:64` | `obsctx.Go(ctx, 0, fn)` hands `fn` a context that is already expired |
| R-32 | P3 | `http/options.go:11` | A caller can pass `otelhttp.WithTracerProvider` / `WithMeterProvider` through the facade and replace the SDK's providers |
| R-33 | P3 | `resty/client.go:91-102` | A failed histogram creation falls back to a no-op with no log, so the client silently has no metric |
| R-35 / SDK-22 | P3 | `mongo/client.go:86-97` | `Connect` discards the cleanup function that `Instrument` callers are told to defer |
| R-36, R-41 / SDK-27 | P3 | `options.go:490-494`, `internal/metrics/metrics.go:534-536` | An empty metrics address binds a random port without a log line; the scrape server sets only `ReadHeaderTimeout` and discards the `Serve` error |
| R-37 | P3 | `internal/metrics/scopeguard.go:56-63` | The dropped-scope-attribute warning repeats on every `Meter()` call |
| R-39 | P3 | `internal/metrics/scrapelog.go:79-83` | The scrape-error record is at WARN and carries the gather error text unredacted |
| R-40 | P3 | `internal/metricscap/reader.go:405-406` | A capped label with a non-string value takes a cap slot and is never rewritten |
| R-24 | P3 | `o11y.go:637-639` | The log bridge's instrumentation scope version is the service's version |
| R-43 | P3 | `examples/nats-ws-browser/backend/main.go:122` | The example sets `messaging.operation.type` to `publish`, which is not a value the pinned semconv defines |
| SDK-26 | P3 | `go.mod:25` | `otelmongo` is pinned to a pseudo-version; no tag exists upstream yet |

Still open as feature requests, not defects: SDK-2, SDK-18, SDK-19, SDK-20,
SDK-24, SDK-25.

### 6.4 New in this review

Everything in sections 4.1 to 4.3 is new relative to the two earlier
reports, with these overlaps:

- S1-01 continues R-22, S1-04 includes R-27, S1-05 is the residue of R-23,
  S1-03 the residue of R-6, S2-07 the residue of R-5.
- S2-08's nil-provider row includes R-34.
- S3-01, S3-02 (the link), S3-08 (two rows) and S3-09 (six items) restate
  entries of R-45 that were not acted on.

The findings with no predecessor are S1-02, S1-06, S1-07, S1-08, S1-09,
S2-01, S2-02, S2-03, S2-04, S2-05, S2-06, S3-03, S3-04, S3-05, S3-06 and
S3-07.

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

CI and tooling:

- **Each gate fails on the violation it names.** On a copy of the tree:
  `check_semconv_literals.go` rejected a planted `"service.name"` literal;
  `check_credential_directives.go` rejected a `#nosec G101` line with no
  `nosemgrep: gosec.G101-1` beside it; `check_integrations.go` rejected a
  planted `otel.SetTracerProvider` call (its blind spots are S1-07). All three
  exit 0 on the unmodified tree.
- **Workflow coverage matches `make lint` + `make sast`.** `ci.yml` runs
  build, `go test -race`, example builds, golangci-lint, govulncheck and
  `make sast`; `adr-check.yml` runs the ADR gate, the semconv literal gate
  with its test, and regenerates
  `internal/baggageattrs/semconv_reserved_gen.go` to check for drift.
- **Workflow hardening.** Both workflows set `permissions: contents: read`,
  pin every action by commit SHA, cancel superseded runs, and the SAST job
  checks out with `persist-credentials: false`.
- **gosec scope.** `-tags integration` brings `cassandra/integration_test.go`
  into the scan, and `check_credential_directives.go` fails on a
  `//go:build ignore` file that is not in its `gosecExempt` list, so a build
  tag is not a way out of the scan.

### 8.2 Session 2

`resty/`:

- **URL and error redaction.** `url.full` goes through `redact.URLAttribute`
  on every path, including the fallbacks for an unresolved or unparseable URL
  (`hook.go:386-430`). The span status description and the exception event
  are built from `redact.Error` with the request's own credentials: userinfo
  and the Basic header derived from it, the bearer token, basic-auth pair and
  cookies set on the request or the client, the caller's own
  `HeaderAuthorizationKey`, and cookies the jar adds on a redirect. A
  cancelled request to `http://user:pw@host/x?sig=abc` recorded
  `http://redacted@host/x?sig=%5Bredacted%5D` in all three places. The
  remaining gap is S1-03 (SigV4 query keys), which reaches `url.full` here
  too: `X-Amz-Signature=…` set with `SetQueryParam` was recorded verbatim.
  Generic keys such as `access_token` and `api_key` are also recorded; the
  pinned semconv text for `url.full` lists only `AWSAccessKeyId`,
  `Signature`, `sig` and `X-Goog-Signature`.
- **`exception.type`** matches what `span.RecordError` would have produced
  (`exceptionType` mirrors the pinned SDK's `typeStr`).
- **No new data race against resty.** The hook reads `Token`, `UserInfo`,
  `Header` and `BaseURL` from the client without a lock; resty v2.17.2's
  setters and its own middleware do the same, so the hook adds no race resty
  does not already have.

`redis/`:

- **Pool-name allocator** (`client.go:24-86`). Every path that takes a name
  gives it back exactly once: the three pre-commit failures, `Unwrap`, the
  GC cleanup and the address-reuse eviction all go through `detach`, which
  clears `poolName` after releasing it. A caller-chosen name is held, so a
  generated name is not placed on top of it or under its per-shard prefix.
  An unwrapped wrapper's hooks stay attached but are gated by `disabled`, so
  a reused name is not written by two wrappers.
- **`connectionCounts`** caps idle at total, so the used count cannot wrap
  when go-redis reads the two under separate locks.
- `go-redis@v9.9.0/sentinel.go:104` does set `Addr` to `FailoverClient`, as
  `sentinelAddr` assumes.

`mongo/`:

- **Connection-ID normalisation** (`connid.go`). The three facts it rests on
  hold at the pinned versions: the driver formats the ID as `%s[-%d]`
  (`topology/connection.go:128`); otelmongo derives the peer address with
  `net.SplitHostPort` and falls back to the whole string and port 27017
  (`otelmongo/mongo.go:214-217`); and otelmongo's span map is keyed by
  `{ConnectionID, RequestID}`, where `RequestID` comes from one
  process-global counter (`wiremessage.go:27-30`), so collapsing connections
  onto one address cannot make two in-flight commands collide. Events are
  copied, not mutated, so an application monitor composed alongside still
  sees the driver's own ID.
- **Pool gauges** (`pool_metrics.go`). Walked against the driver's event
  order in `topology/pool.go`: a failed handshake emits `ConnectionCreated`
  then `ConnectionClosed` with no `ConnectionReady` and is now ignored; a
  checked-out connection closed after `ConnectionPoolClosed` finds a fresh
  empty state and is ignored; with two pools behind one state the first
  `ConnectionPoolClosed` no longer unwinds the survivor's connections. One
  comment is imprecise without affecting behaviour: `pool.close` emits
  `ConnectionClosed` for *idle* connections before `ConnectionPoolClosed`
  and for the rest after it, whereas `poolClosed`'s comment says the pool
  event precedes them all.
- **Connect errors carry no credential**: seven malformed URIs produced
  driver errors that name the fault without quoting the URI.

`nats/`:

- **Connect error redaction** for well-formed URLs, scheme-less entries, bare
  tokens and comma-separated lists (see S2-07 for the malformed case).
- **`Request` / `RequestMsg` timeout contract.** A sent request that runs
  out of time satisfies both `nats.ErrTimeout` and
  `context.DeadlineExceeded`; a call with no time left and a cancelled
  context are left unwrapped; a nil context returns `nats.ErrInvalidContext`.
  Read against `withRequestTimeout` and its three dedicated tests.
- **JetStream**: a negative batch is rejected before the native call that
  would panic; `MessagesContext.Next` no longer returns a nil context on
  error.

`cassandra/`:

- The query-path metrics drop `server.*` by default and the span keeps the
  coordinator; batches carry no server label either way. Code,
  `docs/semconv.md` and the option godoc agree.

`gin/`:

- **Filtered requests.** With `WithSkipPaths` on a chain nested inside
  `o11yhttp.NewServerHandler`, a skipped `/healthz` that called `c.Error`
  put no exception event on the outer span; a traced `/x` put exactly one,
  carrying `gin.error.type`, on the chain's own span. The per-chain
  `chainKey` is removed from `c.Keys` after it is read.
- A 4xx response with `c.Errors` set ends with span status Error. That is
  otelgin's behaviour and is recorded as scenario 3 in ADR 0010; not a
  finding.

`http/`, `elasticsearch/`, `minio/`, `obsctx/`, `o11ytest/`:

- No non-test change since `afbd622`. Constructors and error paths were
  read for the cross-integration comparison (S2-03, S2-04, S2-08); the rest
  was not re-read.

### 8.3 Session 3

Documentation that was compared with the code and matches:

- **README options table.** Every option exists with the documented name
  and signature. Defaults checked: `:2112`, `WithMaxUniqueRoutes` 1000,
  `WithMaxUniqueCollections` 200, eight baggage keys
  (`MaxBaggageAttributeKeys`), 256-byte baggage values, `WithExemplars`
  true, the profiling double gate. The toggle variables go through
  `strconv.ParseBool`, which accepts the values the README lists (and
  `T`, `True`, `F`, `False`, which it does not list).
- **README telemetry flow and `AGENTS.md` port table** against
  `kind-config.yaml` and the three `NodePort` services, apart from S3-04.
- **Guide, HTTP and gin.** `NewServerHandler`, `NewTransport` and
  `Middleware` signatures; the eight `DefaultSkipPaths()` entries; the
  ordering statement (chain before `gin.Recovery()`) agrees with
  `gin/doc.go`.
- **Guide, Resty, MongoDB, Redis.** `NewClient`, `Connect`, `Instrument`
  (including the `func(context.Context) error` cleanup) and `Wrap`
  signatures; the Redis always-dropped command list against
  `redis/hook.go:354-393`.
- **Guide, Elasticsearch and Cassandra.** Constructor signatures, the
  option names, the span-name shapes (`elasticsearch.search my-index`,
  `cassandra.SELECT rooms`), and the keys `cassandra.coordinator.id` /
  `.dc`, `network.peer.*` and `db.response.status_code` against the pinned
  package.
- **Guide and `AGENTS.md`, NATS.** Every facade method the two documents
  call exists (`Publish`, `Subscribe`, `Respond`, `Request`, `RequestMsg`,
  `JetStream`, `CreateOrUpdateStream`, `Stream`, both
  `CreateOrUpdateConsumer` forms, `Consume`, `Messages`, `Fetch`,
  `FetchBytes`, `FetchNoWait`, `Next`, `MessageBatch.Stop`). The
  request/reply error contract in `docs/guide.md:1552-1568` matches
  `withRequestTimeout` (`nats/conn.go:391-407`) and the `Request` godoc line
  for line.
- **Guide, MinIO.** `New` signature, the four options and their defaults,
  and `aws.s3.bucket` / `aws.s3.key` against the pinned package.
- **`docs/semconv.md`.** No key in the catalog contradicts the pinned
  package; the Mongo and Redis pool-metric tables match the instruments in
  `mongo/pool_metrics.go` and `redis/metrics.go` in name and kind.
- **CHANGELOG 0.15.0.** The three `nats` fixes and the
  `cassandra.WithCoordinatorMetricLabel` change it describes are in the
  code; 0.12.0 and 0.13.0 have their `### Migration` sections.
- **Examples against the "Do NOT" list.** No example calls `msg.Respond`,
  an upstream instrumentation package, an OTel global setter or
  `slog.SetDefault`; all nineteen `WithEnvironment` calls use the canonical
  `"development"`. The one `panic` (`examples/gin/main.go:80`) is the
  simulated panic the example exists to show.
- **ADRs.** Status lines of all 27 read; apart from the three in S3-09 none
  contradicts the tree. ADR bodies were not re-read in full.

