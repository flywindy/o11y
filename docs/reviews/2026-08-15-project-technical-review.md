# o11y Technical Review (2026-08-15)

> Scope: core SDK (root, `internal/`), the nine integration packages, the `k8s/`
> infrastructure, and CI/CD plus tooling.
> Perspective: senior backend engineer / SRE / architect.
> Method: full source read + `go build` / `go vet` / `go test` (all passing) +
> `go list -deps` dependency verification + manifest cross-comparison.
>
> **Update 2026-08-15: every finding here has since been independently
> verified** (empirical reproduction plus upstream source comparison). The
> verdicts and the resulting plan are in
> [`2026-08-15-verification-and-remediation-plan.md`](./2026-08-15-verification-and-remediation-plan.md).
> **Two original findings — D4 (Loki exporter) and C4 (minio test coverage) —
> did not survive verification and are corrected in place below.**
>
> **Status.** This document is the point-in-time review and its findings are
> left as they were written. Fixed since: **A2, A3, A4, B1, B3, B5**, and the
> resty items in **C5** (`OnPanic`, `server.address` on the error path). **B4**
> is fixed for the SDK's own handler chain. Everything else — A1, B2, C1, C2,
> C3, C4, the whole of D, and E1/E2/E4 — is open, and sequenced in the
> remediation plan. E3 is open; the symlink is still a Windows absolute path.

---

## Overall assessment

This is an observability SDK **well above the usual standard**: 25 ADRs
recording the design decisions, ADR 0003/0008 policy enforced as a CI gate by
`scripts/check_integrations.go`, no global OTel state mutation, idempotent
`Shutdown`, resource cleanup on every init-failure path, and systematic metric
cardinality protection. Tests are green under `-race`, and comments
consistently explain *why* rather than *what*.

Before wider adoption across teams, though, there are **four structural or
correctness problems that should be fixed first** (P0), plus a body of
consistency debt and infrastructure gaps. Ordered by priority below.

---

## P0 — fix before wider adoption

### A1. (Architecture) The root package links four database drivers into every consumer's binary

`o11y.go:24-32, 250-256` imports the `cassandra/`, `minio/`, `mongo/`, and
`redis/` sub-packages purely to collect `MetricViews(...)`. Verified with
`go list -deps`: **any service that only wants tracing and logging still links
gocql, minio-go/v7, mongo-driver/v2 (including its AWS auth stack), and
go-redis/v9 once it imports the root package.**

Impact:

- Consumers inherit the CVE surface of all four drivers (govulncheck noise),
  the binary size, and the `go.sum` growth.
- MVS version-conflict risk: a service pinned to an older mongo-driver now
  negotiates with the SDK's pin.
- Inverted dependency direction: an SDK core should not depend on its
  integration layer.

Each `views.go` imports only OTel (the view definitions are plain scope-name
string matches), but Go links at package granularity, so importing the package
links all of it. **Options, smallest first:**

1. Move each integration's view definitions into a leaf package (e.g. a
   `redis/views` sub-package or `internal/views`) that matches on scope-name
   string constants and imports no driver; the root imports the leaf.
2. Stop collecting views in the root entirely and have integrations register
   them at wiring time through the existing `ExtraViews` seam.
3. Longer term: follow otel-contrib and split into per-integration Go modules
   (`o11y/redis` with its own `go.mod`), fully isolating the dependencies. The
   single module also currently lists `nats-io/nats-server/v2` (a full NATS
   server used only in tests) as a direct dependency, which a split would
   resolve at the same time.

### A2. (Core correctness) `traceId`/`spanId` nest inside the group after `Logger.WithGroup`, breaking the log-trace correlation contract

`internal/log/handler.go:27-35, 49-51` — `OtelSlogHandler.Handle` injects
traceId with `r.AddAttrs` *after* `WithGroup` has been delegated to the
underlying JSON handler, so the record-level attrs are qualified into the open
group:

```go
sdk.Logger.WithGroup("req").ErrorContext(ctx, ...)
// stdout emits {"req":{"traceId":...}} instead of a top-level traceId
```

The top-level correlation field promised at `o11y.go:64-70` silently fails for
any logger derived via `WithGroup`, breaking Loki/Fluentd queries and trace
correlation outright. The two output paths also diverge: the OTLP side
(otelslog takes trace context from ctx and keeps it record-level) is
unaffected. This path has no test. Fix: have the handler track group depth, or
inject via the pre-group base handler.

### A3. (resty) Attempt spans leak when a request-level retry condition triggers the retry (never ended, sample never recorded)

`retryableResponse` at `resty/hook.go:292-302` checks only
`c.RetryConditions`, but resty v2.17.2 merges in the request-level conditions
(`request.go:1071`). When a caller uses `req.AddRetryCondition(...)` with no
client-level condition, `afterResponse` does not end attempt 1's span and
`h.retry` returns early because `err == nil` — so each retry drops one span
(never exported) and one `http.client.request.duration` sample. The
retry-exhausted marker fails the same way. No test covers this.

### A4. (redis) The package's own pool metrics have no views — `db.client.connection.create_time` exports with default (millisecond-scale) buckets

`redis/views.go:21-63` configures only `db.client.operation.duration` plus
drop-views for the legacy plural redisotel names. The singular instruments the
package actually emits at `redis/metrics.go:32-79`
(`db.client.connection.count/.idle.max/.idle.min/.max/.timeouts/.create_time`)
match no view. `create_time` is in seconds yet keeps OTel's default boundaries
`[0, 5, 10, … 10000]`, so essentially every dial lands in the first bucket and
the histogram is useless for the pool sizing it explicitly claims to support
(`redis/hook.go:88-95`). mongo and cassandra both pin buckets and allow-keys on
the same instrument; redis is the only one that does not.

---

## P1 — high priority (correctness / consistency / infrastructure)

### Core SDK

| # | Location | Problem |
|---|------|------|
| B1 | `internal/metrics/metrics.go:373-388` | OTLP push path: when `runtime.Start` fails only the exporter is shut down, leaking the `MeterProvider` (and its PeriodicReader goroutine), which then errors against a closed exporter every interval. The Prometheus path (`initPrometheus`) handles this correctly; the two should be symmetric. |
| B2 | `options.go:603` plus every exporter always passing `WithEndpointURL` | The standard `OTEL_EXPORTER_OTLP_*ENDPOINT` environment variables are **always silently overridden** (the `http://localhost:4318` default applies unconditionally), inconsistent with the SDK honouring `OTEL_TRACES_SAMPLER`, `OTEL_RESOURCE_ATTRIBUTES`, and `OTEL_EXPORTER_OTLP_HEADERS`. A service using standard OTel deployment manifests sends telemetry to localhost with no warning. Suggested: honour the env var when the caller has not set `WithOTLPEndpoint`, or at minimum say so loudly in the docs and at startup. |
| B3 | `internal/profiling/profiling.go:85-93` | `profilerStarted` is not reset when the profiler's `Stop()` fails; combined with `Shutdown`'s `sync.Once`, profiling can never be started again for the life of the process. Suggested: reset unconditionally. |
| B4 | `internal/log/handler.go:30-34, 96-105` | `Handle` calls `AddAttrs` without `Clone`, violating the slog handler guide. Safe inside the SDK because `MultiHandler` clones, but `sdk.Logger.Handler()` is public, so a caller's own fan-out can corrupt attrs across handlers. |
| B5 | `o11y.go:358-360, 388-390` | The profiling endpoint is logged verbatim; a URL with embedded userinfo (`http://user:pass@host`) leaks the credential into the logs. Suggested: redact userinfo before logging. |

### Integration packages

| # | Location | Problem |
|---|------|------|
| C1 | `http/server.go:17-19`, `gin/middleware.go:17-19` | The docs promise "never falls back to process-wide OpenTelemetry globals", but otelhttp/otelgin ignore nil options, so `NewServerHandler(next, nil, nil, nil)` **silently uses the global provider** — precisely the behaviour the docs exclude. Across the nine packages there are three different postures for a nil provider (return an error / silently no-op / silently use globals); these should be unified (suggested: return an error everywhere, matching redis/mongo/cassandra/nats/es). |
| C2 | `cassandra/observer.go:104-108, 270-286` | The `db.namespace` label is derived from parsed statement text with **no value cap** (`WithMaxUniqueCollections` bounds only `db.collection.name`). Cardinality is unbounded under keyspace-per-tenant deployments or when the tokenizer misreads a statement. |
| C3 | `mongo/pool_metrics.go:146-160, 214-219, 323-339` | (a) The fallback pool name carries a monotonically increasing sequence, so every client rebuild (reconnect / config reload) mints a new label value and the stale series live forever on sync UpDownCounters — slow unbounded growth. (b) Running `cleanup()` before `Disconnect` (the natural order of two defers) swallows the zeroing events, freezing `db.client.connection.count` at a non-zero value. |
| C4 | `minio/client.go:353-382` | The blocking behaviour is real (abandoned channel + non-cancellable ctx leaves the goroutine and span suspended forever), but **the severity is lowered**: `client.go:350-352` already documents this as an inherited minio-go contract. The claim of "no test coverage at all" **does not hold** — `client_test.go:436-443` does test ListObjects; it only covers the drain-to-completion happy path. |
| C5 | Low-severity batch | Elasticsearch marks 3xx as span Error (inconsistent with ≥400 in the SDK's other HTTP packages); redis `commandText` builds the full string (a 10 MB SET allocates 10 MB transiently) before truncating; mongo pool events funnel through a single mutex with `Add` calls inside the critical section; the resty error path loses `server.address`; resty has no `OnPanic` hook (a panic leaves the span unended); minio metric samples omit the system attribute. |

### Kubernetes infrastructure (positioned as a kind dev environment, but needs fixes)

| # | Location | Problem |
|---|------|------|
| D1 | `k8s/infrastructure/base/{cassandra,elasticsearch}.yaml` | **Duplicate manifests referenced by no kustomization, and already drifted**: the top-level `elasticsearch.yaml:32` has `runAsGroup: 0` (root group) where the components copy has `1000`. Delete both top-level files. |
| D2 | `components/nats/nats.yaml:10` | JetStream is enabled but the StatefulSet has **no storage volume at all** (not even an emptyDir), so streams and KV write to the container overlay filesystem and vanish on restart. Add at least a volumeClaimTemplate, or an explicit emptyDir. |
| D3 | `monitor/otel-collector.yaml:25-28, 92-102` | The single ingress point for all telemetry has no `memory_limiter` processor, no resource limits, and no probes (the collector ships a `health_check` extension). A telemetry burst OOM-kills it and drops everything in flight. |
| D4 | `monitor/otel-collector.yaml:35-50, 67-70` | ~~Uses a loki exporter that has been removed; the next image bump breaks it~~ **← corrected after verification.** In fact the `loki` exporter has been **deprecated since 2024-07-09 but is still published** (latest v0.130.0, well past the project's pinned 0.121.0), so nothing is broken today and nothing is imminent. This is scheduled tech debt: migrate to `otlphttp` against Loki's native OTLP endpoint (`/otlp`), adjusting the Grafana derived-field regex at the same time. |
| D5 | All of monitor, plus mongodb and nats | The entire monitor stack (prometheus/loki/tempo/grafana/alloy) has no probes, no resources, no securityContext, and uses emptyDir throughout (Grafana has no volume at all). That contrasts sharply with the exemplary minio/cassandra/es/pyroscope manifests (non-root, drop ALL, seccomp, PVCs, explicit production-override comments) — **the half most likely to be copied to production is the half with no hardening**. mongodb runs unauthenticated as root, also with no dev-only comment. |
| D6 | `monitor/grafana.yaml:117-118` vs `monitor/tempo.yaml` | Grafana's service map points at Prometheus, but Tempo has no `metrics_generator` enabled, so the node graph is a permanently empty dead feature. Either add the metrics_generator or remove the setting. |
| D7 | `kind-config.yaml:5-11` | No `listenAddress`, so host ports 4318 (unauthenticated OTLP), 4223 (`no_tls` NATS WebSocket), and 4040 (Pyroscope ingest) bind to `0.0.0.0` and anyone on a shared network can inject. Add `listenAddress: "127.0.0.1"`. |
| D8 | Loki / Prometheus | No retention configured (Loki disables retention by default, so it grows unbounded; Prometheus has no size cap), so a long-lived kind node will evict other pods under disk pressure. |

### CI/CD and tooling

| # | Problem |
|---|------|
| E1 | Several CI comments claim upgrades are "managed via Dependabot / Renovate", but the repo has **no `.github/dependabot.yml`**: the SHA-pinned actions, golangci-lint v2.11.4, govulncheck v1.3.0, and the `GOVULNCHECK_GOTOOLCHAIN` pin all rot silently, and the vuln job's value decays as its toolchain ages. |
| E2 | `k8s/` has no CI validation at all (no `kustomize build`, kubeconform, or yamllint) — exactly the class of job that would have caught D1's drift and D4's deprecated exporter. |
| E3 | The `CLAUDE.md` / `GEMINI.md` symlinks point at `C:/Users/SheepRocket/Projects/o11y/AGENTS.md` (a Windows absolute path) and are broken everywhere except the author's machine. Make them relative symlinks to `AGENTS.md`. |
| E4 | Others: coverage is uploaded but has no threshold; no release/tag automation (the CHANGELOG is hand-maintained); `internal/trace` has no test file; the action SHA pins carry no `# vX.Y.Z` comments; the Makefile `examples` target lacks CI's no-directory guard; `docs/guide.md:293` links to the relocated `base/prometheus.yaml`; `.golangci.yml` could consider adding `gosec`. |

---

## P2 — observations and suggestions (not defects)

1. **Dependency weight**: the upstream `akira-core/instrumentation-go/otel-nats`
   drags in open-feature, go-feature-flag, antlr, quic-go and other indirect
   dependencies unrelated to NATS instrumentation. Worth raising upstream or
   evaluating a slimmed fork.
2. **Internal cardinality budget**: `cardinalityLimitBudget(1000, 200)` =
   1000×16×64 ≈ 1.02 M attribute sets per stream, which is nominal as a memory
   guard (a runaway key can consume hundreds of MB first). The 16×64
   multipliers are worth revisiting.
3. **`/metrics` server**: `server.Serve` errors are swallowed entirely (a
   post-bind failure dies silently); there is no way to discover the actual
   port after binding `:0` (tests use a TOCTOU workaround that can flake in
   parallel).
4. **`o11ytest.CanceledRequestContext`** returns a live context while the name
   implies it is already cancelled — a naming hazard on a public helper.
5. **API evolution**: `resty.Wrap` returns no error while `redis.Wrap` returns
   `(client, error)`; if resty ever needs an error path that is a breaking
   change. Unifying now, pre-1.0, is the cheapest it will ever be.

---

## Strengths worth keeping and spreading

- **ADR discipline plus policy-as-code**: ADR 0003 (no global state) and 0008
  (the three-tier sourcing model) are enforced in CI by
  `check_integrations.go`; the semconv baseline (v1.39.0) and upgrade strategy
  (ADR 0006) are explicit; upstream bumps are diffed against span-name baseline
  tests (`docs/upstream-otel-nats.md`).
- **Init and shutdown semantics**: layered cleanup on init failure, idempotent
  Shutdown with joined errors, profiling's warn-and-continue, and the deferred
  installation of the trace-to-profile wrapper (avoiding dangling profile IDs)
  are all carefully designed.
- **Cardinality engineering**: export-boundary caps (route/collection), shared
  budgets, scope-restricted views, and Prometheus label-normalization collision
  detection — a depth most internal SDKs do not have.
- **The weak-pointer + `runtime.AddCleanup` dedup machinery in redis/resty**
  and the nats `wrapMessageBatch` goroutine lifecycle were verified correct and
  leak-free.
- **CI fundamentals**: SHA-pinned actions, race + coverage, govulncheck against
  a deliberately patched toolchain (with the reasoning in a comment),
  least-privilege permissions, timeouts, and concurrency cancellation.
- The four datastore component manifests (minio/cassandra/es/pyroscope) are
  model dev manifests and can serve as the template when the monitor stack
  catches up.

---

## Suggested execution order

1. **This week**: delete the D1 drifted duplicates; fix A2 (traceId under
   WithGroup), A3 (resty span leak), and A4 (redis views) — all three are
   local, cheap fixes; fix the E3 symlink and add E1's dependabot.yml.
2. **Short term (1–2 sprints)**: A1, breaking the root→integration import
   (start with the leaf-package option; a per-module split deserves its own
   ADR); B1/B3/B4; C1 unifying the nil-provider posture (pre-1.0 is the last
   window); D2/D3/D4; E2 manifest CI.
3. **Medium term**: B2 (exact OTLP env-var semantics, worth an ADR); C2/C3/C4;
   D5–D8, bringing the monitor stack up to standard (probes, resources,
   securityContext, retention, production-overlay comments); release automation
   and a coverage threshold.
