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
