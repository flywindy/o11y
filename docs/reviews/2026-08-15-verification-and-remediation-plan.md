# Verification Results and Remediation Plan (2026-08-15)

This document records the **finding-by-finding verification** of
[`2026-08-15-project-technical-review.md`](./2026-08-15-project-technical-review.md),
and the PR plan derived from it.

> **Status**: wave 1 (PR#1–#4 below) is implemented. The remaining waves are
> proposals; the structural ones need an ADR before implementation.

Verification method — nothing in the original report was taken on trust; every
finding was re-evidenced:

- **Empirical reproduction** — runnable tests for A2 / A3 / A4 / B2 / B4,
  observing actual output.
- **Upstream source comparison** — reading the real code in `GOMODCACHE` for
  resty v2.17.2, otelhttp/otelgin v0.68.0, otlpconfig v1.44.0, pyroscope-go
  v1.3.0, and sdk/metric v1.44.0.
- **Prototype validation** — implementing A1's proposed fix on a copy of the
  repo to confirm it compiles and to measure the benefit.
- **Upstream fact-checking** — confirming the Loki exporter's deprecation and
  removal status against the published release record.

All verification artifacts lived in a scratchpad outside the repository; the
repo itself was left clean (`git status` empty).

---

## 1. Verdicts

| ID | Claim | Verdict | Evidence |
|---|---|---|---|
| A1 | The root package links four DB drivers | **Confirmed** | `go list -deps` + prototype rebuild |
| A2 | traceId nests after `WithGroup` | **Confirmed** | Reproduced output |
| A3 | resty request-level retry span leak | **Confirmed** | Reproduced + upstream source |
| A4 | redis pool metrics have no view | **Confirmed** | View-match test + control group |
| B1 | OTLP metrics MeterProvider leak | **Confirmed** (though near-unreachable) | Code + sdk/metric source |
| B2 | `OTEL_EXPORTER_OTLP_*ENDPOINT` ignored | **Confirmed** | Empirical (httptest comparison) |
| B3 | A failed profiling `Stop` poisons the process | **Partially confirmed** (logic holds; unreachable in the pinned version) | pyroscope-go source |
| B4 | slog `Handle` does not `Clone` | **Confirmed** (symptom is the `!BUG` sentinel) | Empirical sweep |
| B5 | Profiling endpoint leaks credentials | **Confirmed** | Code + net/http source |
| C1 | Inconsistent nil-provider posture | **Confirmed** | otelhttp/otelgin source + nine-package survey |
| C2 | cassandra `db.namespace` has no cap | **Confirmed** | Code + parser probe |
| C3 | Two mongo pool-metric problems | **Confirmed** (severity lowered) | Code + aggregate source |
| C4 | minio ListObjects leak "and no test coverage at all" | **Partially refuted** | See below |
| D1 | Duplicate manifest drifted to `runAsGroup: 0` | **Confirmed** | `diff` + kustomization search |
| D2 | NATS JetStream has no storage | **Confirmed** | Manifest inspection |
| D3 | Collector lacks memory_limiter/limits/probes | **Confirmed** | Manifest inspection |
| D4 | Loki exporter "removed; breaks on upgrade" | **Refuted** | Upstream release record |
| E1/E3/E4 | No dependabot, broken symlink, dead doc link | **Confirmed** | Filesystem inspection |

### Findings refuted or downgraded (recorded honestly)

**D4 — refuted.** The original report claimed the `loki` exporter had been
removed from collector-contrib around 0.121.0 and that any bump would break the
collector. In fact it has been **deprecated since 2024-07-09 but continues to
be published through v0.130.0** (2025-07), well past the project's pinned
0.121.0. **The collector is not broken today, and an upgrade will not break it
imminently.** The correct framing is scheduled tech debt, not a P1 incident.

**C4 — partially refuted.** The blocking leak is real, but (a)
`minio/client.go:350-352` already documents it as an inherited minio-go
contract rather than an unknown defect, and (b) "no test coverage at all" is
**wrong** — `client_test.go:436-443` does cover ListObjects; it only covers the
drain-to-completion happy path. Downgraded to P2.

**A doubt I raised and then withdrew myself:** I suspected the project had a
CHANGELOG but no git tags. Checking the remote confirmed **all 14 tags from
v0.1.0 to v0.11.0 exist**, so consumers do get semantic versions. Withdrawn.

### New findings surfaced during verification (not in the original report)

| ID | Location | Detail |
|---|---|---|
| **N1** | `otlpconfig/options.go:340-342` | `WithOTLPHeaders` **replaces rather than merges** with the environment — setting the option silently discards `OTEL_EXPORTER_OTLP_HEADERS`. Same semantic family as B2 and should be handled with it. |
| **N2** | `otelgin/config.go:135-139` | otelgin's `WithMeterProvider` has **no nil guard** (its sibling options do); today it is saved only by a later re-check at `gin.go:47-49`. If upstream reorders, it becomes a nil dereference. |
| **N3** | `mongo/client.go:85-95` | The `Connect` facade **discards the cleanup entirely** on the success path, invoking it only on the construction-error branch — which makes C3's freeze unreachable for most users, but also means the tracker is never disabled. |

---

## 2. Key verification details

### A1 — dependency coupling (quantified, and the fix is proven viable)

Removing the four integration imports from `o11y.go` and setting `ExtraViews`
to `nil` on a copy of the repo **compiles cleanly**. Measured:

| Metric | Before | After | Delta |
|---|---|---|---|
| Binary for a tracing-only consumer | **27.7 MB** | **23.8 MB** | **−3.9 MB (−14%)** |
| Driver packages linked | **77** | **0** | gocql / minio-go / mongo-driver / go-redis all gone |

All four `views.go` files **import no driver themselves** (the sole exception
is mongo, for the `otelmongo.ScopeName` **string constant**), proving the
coupling is purely at Go's package granularity and that decoupling has no
technical obstacle.

### A2 — traceId nesting (observed output)

```text
no group : {"msg":"...","traceId":"0102...","spanId":"0102..."}
WithGroup: {"msg":"...","req":{"traceId":"0102...","spanId":"0102..."}}   <- swallowed by the group
WithAttrs: {"msg":"...","k":"v","traceId":"0102...","spanId":"0102..."}   <- unaffected
```

### A3 — resty span leak (reproduced)

```text
client-level condition    attempts=2  endedSpans=2   <- correct
request-level condition   attempts=2  endedSpans=1   <- leak
```

Refined: the leak is **N−1** spans (the final attempt is always closed by
`OnSuccess`/`OnError`), and it requires **`SetRetryCount > 0`** to trigger. The
root cause is that resty's `r.retryConditions` is an **unexported field** that
o11y structurally cannot read — so the wrapper should not re-derive the retry
decision at all, but consume resty's own retry hook.

### A4 — missing redis views (test plus control group)

```text
db.client.operation.duration        MATCHED   -> ExplicitBucketHistogram
db.client.connection.create_time    NO MATCH  <- unit "s", but OTel's default millisecond-scale [0,5,...,10000] applies
db.client.connection.count/.idle.max/.idle.min/.max/.timeouts   NO MATCH
```

Decisive evidence: **mongo (`views.go:70`) and cassandra (`views.go:53`) both
pin the same `create_time` instrument**, and only redis omits it — confirming
an oversight rather than a design choice.

### B2 — OTLP environment variables (empirical comparison)

| Environment variable set | Actual result |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | **Ignored** (telemetry still goes to 127.0.0.1:4318) |
| `OTEL_EXPORTER_OTLP_TRACES/LOGS_ENDPOINT` | **Ignored** |
| `OTEL_EXPORTER_OTLP_HEADERS` | Honoured (but see N1) |
| `OTEL_RESOURCE_ATTRIBUTES` | Honoured |
| `OTEL_TRACES_SAMPLER` | Honoured |

Root cause: `options.go:603` fills in a literal default unconditionally, and on
the exporter side the environment is applied first and the programmatic option
overwrites it (`otlpconfig/options.go:92-95`), so the option always wins.

### Severity corrections for B3 / C3 / C4

- **B3**: the logic really does poison the process permanently, but
  pyroscope-go v1.3.0's `Stop()` is a hard `return nil` (`api.go:102-107`), so
  it is **unreachable in the pinned version** — only the test seam or a future
  upstream change reaches it.
- **C3**: both problems hold, but because the `Connect` facade discards the
  cleanup (N3), the risk is confined to callers who use `Instrument` directly
  **and** invoke cleanup early.
- **C4**: see the refuted section above.

---

## 3. Remediation plan: 12 PRs in four waves

Ordering principle: **fix the correctness bugs users are already suffering,
where the fix itself is low-risk, first**; then the structural and semantic
changes that need an ADR and a migration window; then the infrastructure and
tooling work that does not affect consumers at all.

> Legend: **consumer impact** means the effect on applications that use the
> o11y SDK. 🟢 none / 🟡 needs attention (dashboards or config) / 🔴 needs
> coordination (behavioural change)

### Wave 1: correctness bugs (can start immediately; mutually independent)

#### PR#1 — `fix(log)`: keep traceId top-level and honour the slog Clone contract (A2 + B4)

- **Approach**: `OtelSlogHandler` tracks its own `WithGroup` depth; with a
  group open it does not use `r.AddAttrs` but injects via the pre-group base
  handler (or restructures the chain so trace injection always happens before a
  group is opened). Add `r = r.Clone()` to the ordinary branch of both
  `OtelSlogHandler.Handle` and `BaggageHandler.Handle`.
- **Tests**: top-level field assertions for `WithGroup`, nested `WithGroup`,
  and `WithGroup` + `WithAttrs`; a regression test that an external fan-out no
  longer produces the `!BUG` sentinel.
- **Before, for consumers**: any service using `sdk.Logger.WithGroup(...)` has
  traceId swallowed by the group in its stdout logs, so Loki
  `| json | traceId=...` queries and Grafana log→trace correlation **fail
  silently**; an external fan-out of the SDK handler produces `!BUG` in the log.
- **After**: 🟡 traceId returns to the top level. **Any team that adapted to
  the bug by querying `req.traceId` must update.** The CHANGELOG should list
  this as a behavioural fix with migration notes.
- **Risk**: low. Behaviour returns to what the docs promise.

#### PR#2 — `fix(redis)`: add the missing pool-metric views (A4)

- **Approach**: in `redis/views.go`, follow mongo/cassandra by pinning
  `histogramBuckets` on `db.client.connection.create_time` and adding
  allow-keys filters to the other five pool instruments.
- **Tests**: the view-match test written during verification, asserting all six
  instruments are covered (so a future one cannot be missed).
- **Before**: `db_client_connection_create_time_bucket` puts nearly everything
  in the first bucket, so using it to estimate connection latency yields
  **wrong conclusions**; the pool labels have no allow-list protection.
- **After**: 🟡 the histogram becomes usable. **The bucket boundaries change**,
  so any PromQL already written against this (broken) metric will return
  different results. Real-world risk is low since the metric carried no signal,
  but it still belongs in the CHANGELOG.

#### PR#3 — `fix(resty)`: consume resty's retry hook and fix the span leak (A3)

- **Approach**: stop re-deriving the decision with `retryableResponse` (the
  request-level conditions are structurally unreadable). Remove the
  `err == nil` early return in `h.retry` so resty's own retry decision drives
  span completion, and thread a `retryable` flag into `finishResponse` so
  `resty.retry.exhausted` is correct on the request-level path too.
- **Tests**: span-count and duration-sample assertions for the
  `req.AddRetryCondition` path; add an `OnPanic` hook and a test for it.
- **Before**: services using `req.AddRetryCondition` **lose N−1 spans and N−1
  `http.client.request.duration` samples** per retried request — client error
  rates and P99s are systematically understated.
- **After**: 🟡 span and sample counts **rise** (a correction, not a
  regression). Dashboards computing QPS or error rate from
  `http.client.request.duration` will show a step change, so announce it in
  advance to avoid it being read as a traffic anomaly or tripping alerts.

#### PR#4 — `fix`: three small fixes (B1 + B3 + B5)

- **Approach**: (a) add `provider.Shutdown` to `initOTLP`'s failure path,
  making it symmetric with the Prometheus path; (b) reset profiling's
  `profilerStarted` unconditionally (the flag means "do we hold the pprof
  slot", not "was shutdown clean"); (c) add a `redactURL` helper that strips
  userinfo from endpoints before they are logged.
- **Before**: (a) near-unreachable; (b) unreachable in the pinned version;
  (c) anyone authenticating via `http://user:pass@pyroscope:4040` has the
  credential written in cleartext to stdout **and the OTLP log pipeline** —
  i.e. out of the process.
- **After**: 🟢 no visible change. (c) is a pure security improvement for
  services already using userinfo.

### Wave 2: structural changes (need ADRs; suggested for one minor release)

#### PR#5 — `refactor`: break the root → integration dependency edge (A1) + ADR

- **Approach**: move the four view definitions into leaf packages that import
  no driver (e.g. `internal/views/`, with scope names as string constants); the
  root imports only the leaf. **Each integration package keeps `MetricViews` as
  a thin re-export**
  (`func MetricViews(b []float64) []sdkmetric.View { return views.Redis(b) }`),
  so the public API is unchanged. mongo's `otelmongo.ScopeName` becomes a
  hard-coded constant, guarded against drift by an equality assertion test in
  the mongo package (which already imports otelmongo).
- **Why it is non-breaking**: `MetricViews` is public API documented in ADRs
  0013/0014/0018/0019 (for services that build their own MeterProvider), so it
  must be preserved — and the re-export preserves it exactly.
- **Before**: importing `github.com/flywindy/o11y` links gocql + minio-go +
  mongo-driver + go-redis (77 packages) even for a tracing-only service, which
  inherits their CVE surface and version constraints and adds 3.9 MB.
- **After**: 🟢 effectively invisible, and pure benefit: −14% binary, the four
  drivers leave `go.sum` after `go mod tidy`, and govulncheck noise drops.
  **The one risk**: a service that (badly) relied on the transitive dependency
  without its own `require` will fail to compile; the fix is to `go get` the
  driver it was already using. This needs to be explicit in the CHANGELOG.

#### PR#6 — `fix(http,gin)`: unify the nil-provider policy (C1) + amend ADR 0003

- **Approach**: the nine packages currently have **three** postures (return an
  error / silently no-op / silently use globals). `http` and `gin` do not
  return errors from their signatures, so switching to an error is breaking;
  the suggested route is therefore no-op: substitute noop providers and an
  empty composite propagator inside the facade, which **actually delivers the
  documented "never falls back to globals" promise**. Write the policy into ADR
  0003 as a single rule, and add defensive handling for otelgin's unguarded
  `WithMeterProvider` (N2).
- **Before**: passing nil **silently uses the global provider**, contradicting
  both the package docs and ADR 0003 — the only place where http and gin, the
  two most-used packages, violate their own contract.
- **After**: 🔴 **needs coordination.** A service that both (i) passes nil and
  (ii) has called `otel.SetTracerProvider(...)` gets telemetry today via the
  global and **will get a noop after the fix, so its telemetry disappears**.
  Such a service is misusing the API, but "telemetry suddenly vanished" is hard
  to debug. Mitigation: ship scanning guidance in the PR (grep
  `NewServerHandler(.*nil`), list it as Breaking in the CHANGELOG, and advise
  those services to pass `sdk.TracerProvider()`.

### Wave 3: semantic changes (highest impact; need announcement and a migration window)

#### PR#7 — `feat`: honour the standard OTLP environment variables (B2 + N1) + ADR

- **Approach**: make the `otlpEndpoint` default an empty sentinel and **pass
  `WithEndpointURL` only when the caller explicitly set `WithOTLPEndpoint`**,
  handing the standard environment precedence chain back to the OTel SDK. Same
  for headers (merge, or pass only when set, fixing N1's silent discard).
- **Before**: a service with `OTEL_EXPORTER_OTLP_ENDPOINT` in its k8s manifest
  still sends telemetry stubbornly to `localhost:4318` — teams using standard
  OTel deployment templates experience telemetry vanishing into thin air.
- **After**: 🔴 **the highest-risk item in this plan; audit before rolling
  out.** If a service's environment **already** carries
  `OTEL_EXPORTER_OTLP_ENDPOINT` (injected cluster-wide, or left over from
  another SDK), its telemetry will **suddenly reroute** to that address.
  Suggested process: (1) ship an observation-only release that **logs a WARN
  when the env var conflicts with the SDK default**; (2) collect for one or two
  weeks to identify affected services; (3) then switch the actual precedence.
  This must never share a release with other changes.

#### PR#8 — `fix(cassandra)`: add a cardinality cap for `db.namespace` (C2)

- **Approach**: add a third rule set to `otlpCapRules` / `prometheusCapRules`
  keyed on `semconv.DBNamespaceKey` / `db_namespace`, scoped to cassandra,
  sharing the collection budget or taking its own (a new
  `WithMaxUniqueNamespaces` is suggested); fold the new dimension into
  `cardinalityLimitBudget`.
- **Before**: under keyspace-per-tenant deployments, or when the tokenizer
  misreads a statement (the probe produced values such as `ns="'user-supplied"`
  and `ns="192.168.1"`), `db.namespace` grows **unbounded** on two instruments
  and can overwhelm Prometheus.
- **After**: 🟡 values past the cap collapse to `"other"`. A normal schema
  (tens of keyspaces) is unaffected; an `other` bucket appearing is itself a
  signal worth investigating.

#### PR#9 — `fix(mongo)`: frozen gauges and unbounded pool labels (C3)

- **Approach**: (a) make `cleanup()` **emit the zeroing itself** (walk
  `t.pools` under the lock calling `closePool`) before setting `disabled`, so
  ordering stops affecting correctness; (b) derive the fallback pool name
  **deterministically** from the host set (or require `WithPoolName` when
  metrics are on) so a client rebuild reuses the existing series instead of
  minting a new one.
- **Before**: callers using `Instrument` directly who invoke cleanup early see
  `db.client.connection.count` **frozen permanently at a non-zero value**
  (misleading capacity decisions); services that rebuild clients slowly
  accumulate series that never disappear.
- **After**: 🟡 (b) **changes the `db.client.connection.pool.name` label
  values**, so dashboards grouping by that label need updating. Suggested to
  release alongside PR#8 with a single announcement.

### Wave 4: infrastructure and tooling (no consumer impact; can run in parallel)

#### PR#10 — `fix(k8s)`: security and data correctness (D1, D2, D3, D5, D7, D8)

Delete the two drifted duplicate manifests (including the `runAsGroup: 0` one);
give NATS JetStream `volumeClaimTemplates`; add `memory_limiter` + resources +
`health_check` probes to the Collector; bring the monitor stack up to the
minio/cassandra standard with probes, resources, and securityContext plus
"production must override" comments; add `listenAddress: "127.0.0.1"` to
`kind-config.yaml`; configure retention for Loki and Prometheus.
**Consumer impact: 🟢 none** (local kind dev cluster only).

#### PR#11 — `chore(k8s)`: migrate the Collector from loki to otlphttp (D4) + Grafana service map (D6)

Switch to `otlphttp` against Loki's native OTLP endpoint, adjusting the Grafana
derived-field regex and label mapping together; for the service map, either add
Tempo's `metrics_generator` or remove the setting rather than leaving a dead
feature. **Note: this is scheduled tech debt, not urgent** — the loki exporter
in the pinned 0.121.0 still works. **Consumer impact: 🟢 none**, but Loki's
label mapping changes, so query syntax needs updating in step.

#### PR#12 — `chore`: CI and tooling (E1–E4)

Add `.github/dependabot.yml` (so the existing SHA and version pins actually
have an owner); add a manifest validation job (`kustomize build` + kubeconform
— precisely the mechanism that would have caught D1 and D4); make the
`CLAUDE.md` / `GEMINI.md` Windows absolute-path symlinks relative; fix the dead
link at `docs/guide.md:293`; add tests for `internal/trace`; evaluate adding
`gosec`. **Consumer impact: 🟢 none**.

---

## 4. Suggested release sequencing

| Version | Contents | What consumers must do |
|---|---|---|
| **v0.12.0** | PR#1–#4 (wave 1) + PR#10, #12 | Upgrade. Watch for the dashboard adjustments from PR#1/#2/#3 |
| **v0.13.0** | PR#5, #6 (wave 2) + PR#11 | PR#6 needs a nil-provider misuse scan first |
| **v0.14.0** | PR#8, #9 | Label values change; announce together |
| **v0.15.0** | PR#7 (released alone) | **Must be preceded by the observation release that audits environment variables** |

The reason for splitting into waves: **keep the changes that alter telemetry
apart from one another**. If PR#3 (span counts rise), PR#7 (telemetry
reroutes), and PR#9 (labels renamed) all shipped in one version, then when any
service looked wrong the person on call could not tell which change caused it —
a particularly ironic failure mode for an observability SDK.

## 5. Additional suggestion: migration tooling for consumers

Since this plan contains three changes that alter the shape of telemetry, wave
1 should carry:

1. **An upgrade checklist** (under `docs/upgrade/`) listing the PromQL/LogQL
   patterns to check for each version.
2. **A grep script** that helps a service check itself against the four
   affected situations: `NewServerHandler(..., nil, ...)`,
   `req.AddRetryCondition`, `WithGroup` plus traceId queries, and
   `OTEL_EXPORTER_OTLP_ENDPOINT`.
3. The CHANGELOG's existing "Breaking Changes (Migration Guide)" section is
   already good; keep using it.
