# ADR 0012 — Continuous Profiling Integration (Pyroscope)

**Status**: Proposed
**Date**: 2026-05-12

**Applies** ADR 0003 (global state policy), ADR 0006 (semconv upgrade
strategy), ADR 0008 (instrumentation sourcing policy).

---

## Context

The SDK ships three observability signals today: traces, metrics, and
logs. It does not ship the fourth signal — **continuous profiling** —
even though the rest of the stack is already Grafana-native
(Tempo / Loki / Prometheus). Without profiling:

- CPU regressions surface as latency in traces but not as a callgraph,
  so the operator still has to attach `pprof` manually to localize the
  hot path.
- Memory leaks visible in `runtime.memstats` metrics have no
  attribution to the call site that allocated.
- Trace ↔ profile cross-navigation in Grafana (the "View Profile"
  button on a Tempo span) is absent.

Pyroscope is the Grafana-stack-aligned profiling backend. This ADR
decides how the Go SDK integrates with it, what the public API shape
looks like, and how the integration coexists with the SDK's existing
principles (Zero Global State, OTel-first, opt-in by endpoint).

### State of the profiling ecosystem (2026-05)

Two paths exist for shipping pprof data from a Go process to
Pyroscope:

**Path A — `github.com/grafana/pyroscope-go` direct push.** The
client SDK collects pprof samples from the Go runtime
(`runtime/pprof`, `runtime.SetMutexProfileFraction`,
`runtime.SetBlockProfileRate`) and pushes them via HTTP to a
Pyroscope-compatible `/ingest` endpoint. Production-ready, stable v1.
Default delta encoding to bound bandwidth.

**Path B — OTLP Profiles signal (the OTel fourth signal).** The
OpenTelemetry Profiles signal specification moved to stable in late
2025, but as of this ADR's date:

- The OTel Go SDK does not yet ship a stable profiles exporter.
- The OTel Collector has profiles receiver / exporter components only
  in `contrib`, not in the base distribution.
- Backend support for OTLP profiles is in early days; Pyroscope
  accepts it through Grafana Alloy's bridge, but the on-wire schema
  is still moving between minor versions.

Path A is what production Go services run today. Path B is the
strategic destination once the spec settles and the Go SDK exporter
ships a stable line.

### Routing implications

Pyroscope ingest is **not** an OTLP endpoint and the SDK's existing
OTel Collector deployment has no Pyroscope exporter. The only
agent in the existing `k8s/infrastructure/base/` stack that can
receive Pyroscope-protocol profiles is Grafana Alloy
(`pyroscope.receive_http` + `pyroscope.write`). Profiling will
therefore traverse Alloy, not the OTel Collector, even though
traces / logs / metrics today traverse the OTel Collector.

This asymmetry is real and worth flagging, but **agent consolidation
(replacing OTel Collector with Alloy across all four signals) is a
separate strategic decision** with its own blast radius (it changes
the critical path for the three signals that already work). It is
explicitly out of scope here and tracked for a follow-up ADR
(ADR 0013, agent consolidation strategy). This ADR commits only to
the narrow fact that profiling — the new signal — uses Alloy as its
agent.

---

## Decisions

### 1. Sourcing — apply ADR 0008 §2

Two distinct libraries are evaluated, not one, because profiling has
two responsibilities the SDK cannot collapse: **collecting and
shipping samples** (the profiler) and **bridging trace context onto
samples** (the trace-to-profile link). They are independent
adoption decisions.

**Library 1 — sample collection & shipping: `github.com/grafana/pyroscope-go`**

| §2 checklist item | Result |
|---|---|
| ADR 0003 compliance | ⚠️ Conditional. The library does not touch OTel globals (it does not depend on OTel at all). It does mutate `runtime/pprof` global state — but this is unavoidable: Go's profiler is a process-wide singleton by language design. See §9 for the explicit exception. |
| Maintenance signal | ✅ Grafana-maintained; stable v1; tagged releases. |
| Semconv alignment | N/A — Pyroscope uses its own flat label model, not OTel semconv. Mapping spelled out in §6. |
| Configurability | ✅ Profile types, upload rate, tags, auth all configurable. |
| Framework signal access | N/A — no framework, this is a runtime profiler. |

Verdict: **adoptable as a T2 facade** with the ADR 0003 exception in §9.

**Library 2 — trace-to-profile bridge: `github.com/grafana/otel-profiling-go`**

| §2 checklist item | Result |
|---|---|
| ADR 0003 compliance | ✅ Pure `trace.TracerProvider` wrapper; takes the upstream provider as a constructor argument; does not call `otel.SetTracerProvider` or `otel.SetTextMapPropagator`. To be re-verified at the pinned version per ADR 0003's per-version policy. |
| Maintenance signal | ✅ Grafana-maintained alongside Pyroscope. |
| Semconv alignment | ✅ Sets `pyroscope.profile.id` as a span attribute — the contract Tempo's "View Profile" UI reads. Library author tracks this convention as it evolves toward OTel Profiles semconv. |
| Configurability | ✅ Span-name / service-name / profile URL all overridable. |
| Framework signal access | ✅ Direct access to span lifecycle via the TracerProvider wrapper pattern. |

Verdict: **adoptable as a T2 facade**.

**Alternative considered and rejected: self-written SpanProcessor.**
An ~80-line `SpanProcessor` could call `pprof.SetGoroutineLabels` on
`OnStart` and reset on `OnEnd`. This was rejected for two reasons:

1. `pyroscope.profile.id` is not an implementation detail — it is the
   contract Tempo's UI reads to enable the "View Profile" cross-link.
   The attribute key will evolve as the OTel Profiles signal
   stabilizes (likely toward a standardized `profile.id` semconv
   key). A maintained library tracks this; a hand-rolled
   `SpanProcessor` becomes silent rot the first time the contract
   shifts.
2. The cost of the wrapper — a `trace.TracerProvider` interface
   instead of `*sdktrace.TracerProvider` from `SDK.TracerProvider()`
   — is independently desirable (see §2) and not a real disadvantage.

Per ADR 0008 §3, this T3 path is rejected because Library 1 + Library
2 jointly clear the §2 gate.

### 2. Public API — `SDK.TracerProvider()` returns the interface

This is a deliberate, pre-release breaking change. The SDK is not yet
consumed by services, and the new shape is the one we want to ship
v1 with.

```go
// Before
func (s *SDK) TracerProvider() *sdktrace.TracerProvider

// After
func (s *SDK) TracerProvider() trace.TracerProvider
```

Rationale:

- Returning the concrete `*sdktrace.TracerProvider` leaks
  `RegisterSpanProcessor`, `Shutdown`, and other lifecycle methods
  that callers must not touch (Shutdown is owned by `SDK.Shutdown`;
  `RegisterSpanProcessor` after Init is undefined behavior in the
  SDK contract).
- Every OTel instrumentation library accepts `trace.TracerProvider`
  (the interface); the concrete type is never required at the
  boundary.
- The same `SDK` field can hold the original `*sdktrace.TracerProvider`
  internally for shutdown (`s.shutdowns = append(..., tp.Shutdown)`)
  while exposing only the interface externally — including the
  `otelpyroscope.NewTracerProvider(tp)` wrapper when profiling is
  enabled.
- Future wraps (rate-limiting sampler injection, multi-tenant
  routing) become non-breaking because the public type is already
  an interface.

The same change applies, by symmetry, to `SDK.MeterProvider()`:
returns `metric.MeterProvider` (interface) instead of
`*sdkmetric.MeterProvider`. This is in scope for the implementation
PR (it is the consistent shape; no reason to ship a half-applied
abstraction).

### 3. Public API — `Option` surface

Profiling is opt-in. The trigger is the presence of a non-empty
endpoint, mirroring `WithMetricsOTLPEndpoint`. When no endpoint is
configured, no profiler goroutines start, no pprof globals are
touched, no `TracerProvider` wrapper is installed, and the
`pyroscope.profile.id` attribute is never set.

```go
// WithProfilingEndpoint sets the Pyroscope-compatible ingest endpoint
// (e.g. "http://alloy:4040" for Grafana Alloy's pyroscope.receive_http
// component, or "http://pyroscope:4040" for direct Pyroscope ingest).
// When empty, profiling is fully disabled. Default: "".
func WithProfilingEndpoint(endpoint string) Option

// WithProfilingAuthHeaders attaches HTTP headers to every profile push.
// Use for Grafana Cloud Profiles (Basic auth via Authorization header)
// or for multi-tenant routing (X-Scope-OrgID). Header values are not
// logged. Mirrors WithOTLPHeaders for symmetry.
func WithProfilingAuthHeaders(headers map[string]string) Option
```

**Deliberately deferred to a future option-PR** (not exposed in v1 of
this integration):

- `WithProfilingTypes([]ProfileType)` — for now the SDK ships a
  fixed default (CPU + alloc_objects + alloc_space +
  inuse_objects + inuse_space). Mutex and block profiling are
  **off by default** because both impose visible overhead on
  lock-contended workloads (`runtime.SetMutexProfileFraction(n)`
  samples 1/n contention events; `runtime.SetBlockProfileRate(rate)`
  samples blocking events with a global cost). Services that need
  them are rare and can be served by a follow-up option once a real
  consumer asks.
- `WithProfilingUploadRate(time.Duration)` — pyroscope-go's
  default (15 s) is fine.
- Tag injection beyond the SDK's resource attributes — see §6 for
  why static tags are the SDK's job, not the caller's.

Keeping the v1 option surface to two functions reduces the surface
the caller has to learn and matches the SDK's existing principle of
"sensible default + escape hatch when proven necessary."

### 4. Init wiring — provider construction order

The shape of `o11y.Init` becomes:

```text
1. Resource construction (unchanged)
2. TracerProvider build (sdktrace.NewTracerProvider, unchanged)
3. If WithProfilingEndpoint != "":
     a. Build the otelpyroscope wrapper around the sdk tp.
        s.tracerProviderPublic = otelpyroscope.NewTracerProvider(sdkTP,
            otelpyroscope.WithAppName(cfg.serviceName))
     b. Otherwise: s.tracerProviderPublic = sdkTP
4. MeterProvider build (unchanged)
5. LoggerProvider build (unchanged)
6. If WithProfilingEndpoint != "":
     pyroscope.Start(pyroscope.Config{
         ApplicationName: cfg.serviceName,
         ServerAddress:   cfg.profilingEndpoint,
         AuthToken / BasicAuth: encoded from cfg.profilingAuthHeaders,
         Tags: §6 mapping from resource attrs,
         ProfileTypes: [CPU, alloc_objects, alloc_space,
                       inuse_objects, inuse_space],
         Logger: slogAdapter(s.Logger),
     })
   Push the returned profiler.Stop into s.shutdowns (see §7 for order).
```

Shutdown order (LIFO of important resources):

```text
1. metricsCloser (drain /metrics scrape traffic)
2. mp.Shutdown
3. lp.Shutdown
4. profiler.Stop                  (NEW — stops the profiler before
                                   the tp is shut down, because the
                                   span processor on the wrapped tp
                                   still emits spans during
                                   shutdown of upstream resources)
5. sdkTP.Shutdown                 (always the original concrete tp;
                                   the otelpyroscope wrapper does not
                                   own a Shutdown method)
```

The profiler must stop **before** the tracer provider, because
otelpyroscope's per-span `pprof.SetGoroutineLabels` calls assume
the profiler is still running. Stopping the profiler last would
race with goroutine label cleanup.

### 5. Init failure semantics — log and continue

If `pyroscope.Start` returns an error (DNS, malformed endpoint,
construction-time validation), the SDK logs a warning via
`s.Logger.Warn` and continues without profiling. The
`otelpyroscope` wrapper is still installed because the labels it
sets are no-ops in the absence of an active profiler — there is no
correctness issue with leaving the wrapper in place.

Rationale:

- Mirror the existing OTel exporter behavior. OTLP exporters do
  lazy connection; Init does not fail because a Collector is
  unreachable. Profiling should not be the one signal that crashes
  the service at startup.
- Profiling is the most "nice-to-have" of the four signals; failing
  Init on a profiler-config issue would block deployments for a
  signal the service can ship without.
- The warning is structured and surfaces in stdout JSON + Loki, so
  operators see the regression on the first Init that fails.

This is intentionally different from the existing required-options
behavior (`WithServiceName` missing → Init error). Required-options
errors are configuration bugs the caller must fix; transient
profiler-endpoint problems are runtime / environment issues.

### 6. Resource attribute → Pyroscope label mapping

Pyroscope uses a flat string-keyed label model (Prometheus-style),
not OTel semconv. The SDK is responsible for the mapping; the caller
never sets Pyroscope tags directly. Mapping table:

| OTel resource attribute | Pyroscope tag | Source |
|---|---|---|
| `service.name` | `service_name` | `WithServiceName` (required) |
| `service.namespace` | `service_namespace` | `WithServiceNamespace` (required) |
| `service.version` | `service_version` | `WithServiceVersion` (required) |
| `deployment.environment.name` | `service_env` | `WithEnvironment` (required) |
| `host.name` | `hostname` | `resource.WithHost()` detector |
| `k8s.pod.name` (if present) | `pod` | `resource.WithFromEnv()` from OTel resource env vars |
| `k8s.namespace.name` (if present) | `k8s_namespace` | same |

`service_name` is the **application name** Pyroscope's UI groups by;
no service-level dashboard is possible without it. The mapping
guarantees that traces, logs, metrics, and profiles all carry the
same identity in their respective storage backends.

**High-cardinality values must not become Pyroscope tags.** The same
constraint that applies to Prometheus labels (no user IDs, request
IDs, trace IDs in the static tag set) applies here. The SDK enforces
this by **not** exposing a caller-controlled tag-injection API in
v1. If a future requirement appears for caller-controlled tags (e.g.
sharding by feature flag), the option that exposes it must apply
the same `pathLimiter`-style cardinality cap that gin / resty
metric routes apply.

### 7. Trace-to-profile mechanism — `pyroscope.profile.id` and pprof labels

The integration relies on three mechanisms working in concert:

1. **Per-goroutine pprof labels.** The `otelpyroscope` wrapper, on
   span start, calls `pprof.SetGoroutineLabels(pprof.WithLabels(ctx,
   pprof.Labels("span_id", traceContext.SpanID().String(),
   "span_name", spanName, "profile_id", generatedID)))`. Every pprof
   sample taken on that goroutine for the duration of the span
   carries those labels. Pyroscope's storage indexes by label, so a
   query like `service_name="foo" AND profile_id="<id>"` returns
   exactly the samples taken during that span.
2. **`pyroscope.profile.id` span attribute.** The same generated
   `profile_id` is set as a span attribute via
   `span.SetAttributes(attribute.String("pyroscope.profile.id",
   id))`. This is what Tempo's UI reads to construct the "View
   Profile" deep link to Pyroscope.
3. **Tempo → Pyroscope datasource link.** Configured at the Grafana
   datasource level (`tempo.yaml` `tracesToProfiles` block, set in
   the infrastructure PR). Not in the Go SDK's responsibility.

**Cross-goroutine caveat — must be documented.** `pprof.SetGoroutineLabels`
binds labels to the calling goroutine only. When span-instrumented
code spawns `go func() { ... }()`, the spawned goroutine does **not**
inherit the labels by default. The user can opt in with
`pprof.Do(ctx, ...)` inside the goroutine, but it is not automatic.

Practically this means: "Span profile" in Pyroscope shows the work
done **on the goroutine that started the span**, including
synchronous work in that span's lifetime. Work fanned out to other
goroutines is captured by the service-level profile but is not
linked to the span unless the caller propagates labels explicitly.
This is a property of Go's pprof, not a limitation of this
integration, but it must be documented in godoc and README so that
operators reading Pyroscope's span view understand what they are
looking at.

### 8. Routing — through Grafana Alloy

The SDK's public contract is "supply an HTTP endpoint that speaks
the Pyroscope ingest protocol." Two valid configurations:

- `WithProfilingEndpoint("http://alloy:4040")` — recommended. Alloy
  receives via `pyroscope.receive_http` and writes to Pyroscope via
  `pyroscope.write`. Centralizes auth, retry, buffering; isolates
  the application from backend topology; mirrors the
  "everything-through-an-agent" pattern that already governs traces,
  logs, and metrics.
- `WithProfilingEndpoint("http://pyroscope:4040")` — supported but
  not recommended. Bypasses the agent layer; couples the
  application to the backend deployment.

The k8s manifest PR will deploy Pyroscope and configure Alloy with a
`pyroscope.receive_http` block forwarding to Pyroscope. Example
config snippet (lives in the implementation PR, recorded here so the
ADR captures the intended wiring):

```alloy
pyroscope.receive_http "ingest" {
  http {
    listen_address = "0.0.0.0"
    listen_port    = 4040
  }
  forward_to = [pyroscope.write.default.receiver]
}

pyroscope.write "default" {
  endpoint {
    url = "http://pyroscope.infra.svc.cluster.local:4040"
  }
}
```

### 9. ADR 0003 — process-singleton exception

`pyroscope-go` mutates two pieces of process-wide state by design:

- `runtime/pprof` CPU profiler — only one CPU profile may run at a
  time per Go process. `pyroscope.Start` claims it.
- `runtime.SetMutexProfileFraction` / `runtime.SetBlockProfileRate`
  — global runtime knobs (we leave both at 0 by default per §3, so
  this only applies if mutex/block profiling is added later).

This is structurally different from the OTel global-state risks
ADR 0003 was written to address:

- ADR 0003's concern is that third-party libraries silently call
  `otel.SetTracerProvider`, which would interfere across SDK
  instances and across instrumentation packages. `pyroscope-go`
  does not import OTel.
- The Go pprof globals are language-level singletons. There is no
  alternative API. Even a hand-rolled profiler would have to claim
  the same globals.

The exception is therefore narrow and named:

- The SDK calls `pyroscope.Start` at most once per process, gated by
  `sync.Once` on a package-level guard. A second `o11y.Init` call
  with a non-empty profiling endpoint returns an error from Init
  if the first Init already started the profiler. (This is stricter
  than the SDK's general "Init is idempotent" stance because
  `pyroscope.Start` is not idempotent — calling it twice produces
  undefined behavior in pyroscope-go.)
- The exception is recorded explicitly in the ADR 0003 "Approved
  integrations" table when this ADR moves to Accepted.

### 10. ADR 0006 — semconv alignment

The OTel Profiles signal will define its own semconv (profile.*
attribute keys) once it stabilizes. Today, `pyroscope.profile.id` is
the de facto pre-standard key. When OTel ratifies a profile-id
attribute, the SDK migrates via the `otelpyroscope` library's own
update — which is precisely why the wrapper is preferred over
self-written labels (§1).

The semconv version pin in this SDK (v1.39.0) remains unchanged by
this ADR. Profile labels are Pyroscope-native, not semconv attributes.
Span attributes carrying profile links (`pyroscope.profile.id`) are
sourced from `otel-profiling-go`'s constants; the SDK does not
hard-code the key string.

### 11. Failure modes the SDK does not handle

These belong to the operator, not the SDK:

- **Pyroscope endpoint unreachable.** pyroscope-go retries internally
  with backoff and logs to the configured slog adapter. The SDK does
  not surface a separate "profiling unhealthy" signal. Operators
  monitor Pyroscope ingest metrics in Alloy / Pyroscope itself.
- **Pyroscope storage full / quota exceeded.** Same — operator
  concern, not SDK concern.
- **Profile size overshooting.** pyroscope-go truncates oversized
  profiles; we do not add a second cap.

### 12. Future OTel Profiles signal migration

When all three of the following hold, the SDK switches to OTLP
Profiles (mirroring the existing `WithMetricsOTLPEndpoint` pattern
for metrics):

1. OTel Go SDK ships a stable (v1+) profiles exporter.
2. OTel Collector base distribution (not just contrib) supports
   profiles receive/export.
3. Pyroscope or the chosen backend accepts OTLP profiles as a
   first-class ingest path, not a transitional bridge.

At that point this ADR is superseded; a successor ADR records the
migration. Until then, this ADR is the contract.

---

## Out of scope

- **Agent consolidation.** Whether the entire stack should collapse
  onto Grafana Alloy and retire the standalone OTel Collector is a
  separate strategic question (ADR 0013). This ADR's scope is
  bounded to: profiling goes through Alloy because OTel Collector
  has no Pyroscope exporter. The existing OTel Collector routing
  for traces / logs / metrics is unchanged.
- **eBPF profiling.** Pyroscope supports unprivileged eBPF profiling
  via a node-level agent. That is an infrastructure decision (DaemonSet
  on every node), not an SDK decision. The SDK ships in-process pprof
  profiling, which is the per-service signal.
- **Profile-guided optimization (PGO).** Go 1.21+ accepts pprof
  profiles for PGO at build time. Wiring Pyroscope-captured profiles
  back into a build pipeline is a CI/CD topic, not an SDK topic.
- **Custom profile types beyond Go's standard set.** Goroutine-stack
  custom profiles, heap-shape profiles, etc. are out of scope until
  a concrete use case appears.

---

## Consequences

**Positive**

- Fourth observability signal lands aligned with the existing
  Grafana stack, with one new option pair (`WithProfilingEndpoint` /
  `WithProfilingAuthHeaders`) and zero changes to existing required
  options.
- Trace-to-profile cross-navigation works in Tempo UI without
  per-service configuration: every span on a goroutine that ran
  during a profile carries `pyroscope.profile.id`, and Tempo's
  `tracesToProfiles` datasource link does the rest.
- Resource attribute mapping is centralized; services cannot drift
  in how they label profiles (no `application_name` overrides).
- The breaking change to `SDK.TracerProvider()` / `SDK.MeterProvider()`
  return types arrives **before** the SDK ships to its first
  consumer, so the cost is paid at the cheapest possible moment.
- The integration is opt-in (empty endpoint = no profiler started),
  so services not yet ready for profiling pay no overhead.

**Negative / Trade-offs**

- Two new third-party dependencies (`pyroscope-go`,
  `otel-profiling-go`) enter the module graph. Both are
  Grafana-maintained, but ADR 0008 requires per-version
  re-verification on every bump.
- The cross-goroutine caveat (§7) is real and will surprise some
  operators reading the Pyroscope UI. The mitigation is documentation,
  not a code fix — Go's pprof gives us no other option.
- Routing diverges from the existing OTel-Collector pattern (§8):
  profiles go via Alloy, the other three signals via OTel Collector.
  This is an honest reflection of the ecosystem state today, but it
  is one more concept for operators to hold.
- A `sync.Once` on the profiler global means a second `Init` call
  with profiling enabled in the same process returns an error
  (§9). This is stricter than the SDK's general posture on multiple
  Inits but is forced by pyroscope-go's non-idempotent start.
- Mutex / block profiling is off by default and not exposable as an
  option in v1 (§3). Services that genuinely need them have to wait
  for a follow-up option. Acceptable because their absence is
  observable (a service notices it cannot see lock contention) and
  fixable by a small follow-up PR.

---

## Open questions

- **Slog adapter for pyroscope-go logger.** pyroscope-go accepts a
  `Logger` interface with `Errorf` / `Warnf` / `Infof` / `Debugf`
  methods. The implementation PR writes a thin adapter that routes
  these to `s.Logger`. Trivial; recorded here so the implementation
  PR does not invent a separate logger.
- **Auth headers shape — Authorization header vs split user/password.**
  Grafana Cloud Profiles documents Basic auth as
  `Authorization: Basic base64(stackID:token)`. The SDK accepts the
  caller's headers as-is and does not encode for them; if the
  ergonomics turn out to bite users, a `WithProfilingBasicAuth(user,
  pwd)` helper can be added in a later PR. Lean: defer, evaluate
  once a real user lands.
- **Pyroscope-go logger verbosity at Info level.** pyroscope-go logs
  per-upload at Info by default. The implementation PR may need to
  pin it to Warn to avoid log noise in stdout JSON. To be decided by
  empirical noise measurement in the example app, not by this ADR.
- **Tag size limits.** Pyroscope enforces a max label name + value
  size. The SDK's resource attributes (especially `host.name` on
  long hostnames or `k8s.pod.name` on long pod names) are usually
  well under the cap, but the implementation PR should add a
  defensive truncation with a one-shot warning rather than fail an
  upload silently.
