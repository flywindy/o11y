# ADR 0015 — Trace Sampling Strategy

**Status**: Accepted — implemented (Collector tail-sampling is deployment
config, out of SDK scope)
**Date**: 2026-05-25
**Relates to**: ADR 0002 (metrics strategy — exemplars depend on sampled spans),
ADR 0003 (global-state policy — no global provider/sampler mutation),
ADR 0012 (profiling — `pyroscope.profile.id` linkage depends on sampled spans),
ADR 0004 (NATS) / ADR 0005 (MongoDB) — the high-throughput facades that make
this matter

---

## Context

The SDK ships **no explicit sampler**. `internal/trace/trace.go` builds the
`TracerProvider` with only `WithBatcher(exporter)` + `WithResource(res)` — no
`WithSampler`. `options.go` exposes no sampling knob (21 `WithXxx` options, none
for sampling). The effective behavior is therefore the OTel Go SDK default,
**`ParentBased(AlwaysSample)` = 100 % of traces**, with no documented decision
about production posture.

This was deprioritized until a concrete signal arrived: a sibling project (not
o11y, but using the **same upstream Marz instrumentation libs** o11y wraps) ran
a **MongoDB change-stream watcher → JetStream → websocket worker** pipeline.
Both services spiked CPU and memory under load; the fix applied in production
was **`sampling = 0.001`**. Because o11y wraps the same libs and defaults to
100 %, **o11y has the same structural exposure**, so the strategy must be
decided and documented now.

### Why high-throughput tracing melts the producing service

- **Span count = event count.** The Marz `otel-nats` facade emits a span per
  inbound/outbound message (`nats/conn.go` is a thin wrapper; spans come from
  the upstream lib). MongoDB command monitoring emits a span per command, and a
  change stream issues `getMore` continuously. At thousands of events/sec this
  is thousands of spans/sec.
- **Per-span allocation.** Each recording span allocates the span object plus an
  attribute map. At high frequency this is dominated by **GC pressure → CPU
  spike**, and heap growth between GC cycles → **memory rise**. Optional
  aggravators (to investigate, not assume): large payloads / BSON filters / IDs
  attached as attributes; a never-ended subscription/watch span accumulating
  children.

### Why `0.001` worked — and why head sampling is the only lever for this

`ParentBased(TraceIDRatioBased(0.001))` returns a **Drop** decision for ~99.9 %
of root spans; the SDK then yields a **non-recording span** — no attributes
recorded, nothing queued to the BatchSpanProcessor, near-zero allocation. Span
creation cost collapses ~1000×.

The decision **cascades through context propagation**: when the watcher drops a
trace (`sampled=false`), the context carried over JetStream reaches the worker
unsampled, and the worker's `ParentBased` maps a not-sampled parent to
`NeverSample`. **Setting the rate at the producer automatically thins the
consumer.** This is why a single change fixed both services.

> **Key correction to an earlier internal lean toward "default 100 % + Collector
> tail-sampling".** Tail sampling drops spans **at the Collector**; the app still
> creates 100 % of spans and pays the full alloc/GC cost. Tail sampling cannot
> protect a producing service's CPU/memory. **Only head sampling (at the SDK)
> protects the source.** Head and tail solve different problems and both have a
> place (see Decision §2).

### The cost of sampling here is unusually high: correlation is the USP

o11y's headline value is cross-signal correlation, and two of the three links
depend on a **sampled** span:

- **Exemplars** (metric→trace) only attach a `trace_id` when a sampled span is
  active (`README.md:556`, ADR 0002). Head-sampling at 1 % means ~1 % of metric
  measurements carry an exemplar. **Tail sampling does not help** — the exemplar
  decision is made at span time with the then-current sampled flag.
- **Profiling→trace** (`pyroscope.profile.id`, ADR 0012) is likewise tied to
  sampled spans.

So aggressive head sampling degrades the product's differentiator. This argues
for keeping the **default high** and pushing rate-reduction to the specific
hot-path services that need it, rather than a low global rate.

---

## Dependency behavior (verified, OTel Go SDK v1.43.0)

Source inspection of `sdk/trace`:

- **A sampling escape hatch already works today.** `NewTracerProvider` applies
  env config **before** caller options (`provider.go`): `applyTracerProviderEnvConfigs`
  → `samplerFromEnv()` reads `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG`
  (`sampler_env.go`); only if still unset does `ensureValidTracerProviderConfig`
  fall back to `ParentBased(AlwaysSample())`. Since o11y passes **no**
  `WithSampler`, **the env vars are honoured right now** with zero code.
- **Precedence:** env is applied first, then explicit options. An explicit
  `sdktrace.WithSampler(...)` **overrides** the env var. → A typed o11y option
  must only inject a sampler **when the user set it**, else it silently disables
  `OTEL_TRACES_SAMPLER`.
- **Env sampler names:** `always_on`, `always_off`, `traceidratio`,
  `parentbased_always_on`, `parentbased_always_off`, `parentbased_traceidratio`
  (+ `OTEL_TRACES_SAMPLER_ARG`).
- **`ParentBased` defaults:** remote/local-parent-sampled → `AlwaysSample`;
  remote/local-parent-not-sampled → `NeverSample`. So only the **root** span's
  sampler matters; children follow the parent → whole-trace consistency
  regardless of downstream services' rates.
- **`TraceIDRatioBased`** decides on a deterministic hash of the trace ID →
  consistent for a given trace ID across services.
- **BatchSpanProcessor is also env-tunable today** (`batch_span_processor.go`
  reads `env.BatchSpanProcessor*`): `OTEL_BSP_MAX_QUEUE_SIZE` (default 2048,
  drops on overflow → bounded memory), `OTEL_BSP_MAX_EXPORT_BATCH_SIZE`,
  `OTEL_BSP_SCHEDULE_DELAY`, `OTEL_BSP_EXPORT_TIMEOUT`. o11y exposes no Go option
  for these, but the env path is live.

---

## Decision

### 1. Default stays 100 % (`ParentBased(AlwaysSample)`)

Keep the SDK's default. It is correct for local dev (the project's stated
priority) and preserves full exemplar / profiling correlation coverage.
Rate-reduction is opt-in, applied where load demands it.

### 2. Head and tail have distinct, documented roles

- **Head sampling (SDK)** protects **the producing service's** CPU/memory and is
  **mandatory for high-throughput hot-path services** (message workers, DB
  watchers). It is set at the **trace origin**; downstream services inherit via
  `ParentBased` propagation.
- **Tail sampling (OTel Collector `tail_sampling`)** protects **backend storage
  cost** and enables intelligent retention (keep all errors / slow traces,
  probabilistic baseline for the rest). It is **deployment configuration, out of
  SDK scope** — recommended for the Grafana/Tempo stack in `k8s/`, documented
  separately, not shipped as SDK code.
- Guidance: **per-service** head rates, not one global value. Hot-path producers
  `0.01`–`0.001`; normal/low-traffic services stay `1.0`. Set the rate at the
  most upstream producer so the cascade thins consumers automatically.

### 3. Configuration: typed options **and** env coexist

Both surfaces are supported, with the precedence rule from the dependency
analysis (typed option overrides env **only when explicitly set**).

- **Typed options** (new) — discoverable, type-checked, unit-testable, can carry
  custom samplers:
  - `WithSamplingRatio(r float64) Option` — convenience; wraps
    `ParentBased(TraceIDRatioBased(r))`.
  - `WithTraceSampler(s sdktrace.Sampler) Option` — escape hatch for any OTel
    sampler (e.g. a future rate-limiting sampler not expressible via the env
    spec's six names).
- **Env** (already functional, to be **documented** not implemented):
  `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG` for same-binary, per-deploy
  rates without a rebuild; `OTEL_BSP_*` for queue/batch bounding.

Rationale for both: typed options serve code-level declaration, testing, and
custom samplers; env serves per-environment tuning of one artifact. Neither
subsumes the other (see Decision analysis Q2).

---

## API change

```go
// Config gains one field; nil = "user did not set" → env path stays live.
type Config struct {
    // ...
    sampler sdktrace.Sampler
}

// Convenience: head-sampling ratio, ParentBased so children follow the root.
func WithSamplingRatio(ratio float64) Option {
    return func(c *Config) {
        c.sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
    }
}

// Escape hatch: any sampler (custom rate-limiter, etc.).
func WithTraceSampler(s sdktrace.Sampler) Option {
    return func(c *Config) { c.sampler = s }
}
```

`InitTracer` takes the sampler and **only appends `WithSampler` when non-nil**,
preserving the env fallback:

```go
func InitTracer(ctx context.Context, endpoint string, headers map[string]string,
    res *resource.Resource, sampler sdktrace.Sampler) (*sdktrace.TracerProvider,
    propagation.TextMapPropagator, error) {

    tpOpts := []sdktrace.TracerProviderOption{
        sdktrace.WithBatcher(exporter),
        sdktrace.WithResource(res),
    }
    if sampler != nil {
        tpOpts = append(tpOpts, sdktrace.WithSampler(sampler))
    }
    tp := sdktrace.NewTracerProvider(tpOpts...)
    // ...
}
```

- Call site `o11y.go:199` passes `cfg.sampler`.
- `WithSamplingRatio` validates `0.0 ≤ ratio ≤ 1.0` at `Init` (alongside the
  existing `validateHistogramBuckets` check, `o11y.go:178`) and rejects
  out-of-range values. This is a **deliberate** addition on top of OTel's native
  behavior: `sdktrace.TraceIDRatioBased` itself silently clamps (`≤0`→0,
  `≥1`→1); we fail fast instead so a misconfigured rate surfaces at startup.
- This is a **non-breaking, additive** change (new options + an internal-package
  signature change); no existing caller changes behavior.

---

## Consequences

**Positive**

- Hot-path services (the watcher/worker class that triggered this ADR) get a
  first-class, in-code lever to protect their own CPU/memory — and the cascade
  thins downstream consumers automatically.
- Default 100 % keeps local-dev simple and preserves exemplar/profiling
  correlation everywhere it is not explicitly reduced.
- Env hatch is documented, so per-environment tuning needs no rebuild.
- `WithTraceSampler` leaves room for a rate-limiting sampler without another ADR.

**Negative / Trade-offs**

- Two configuration surfaces (typed + env) with a precedence rule that must be
  taught; the "typed option silently disables `OTEL_TRACES_SAMPLER`" footgun is
  mitigated by the **nil-means-unset** wiring but must be documented.
- Sampling below ~5 % on a service measurably degrades that service's exemplar
  and profile→trace coverage — the product USP. Mitigated by per-service rates
  (only hot paths go low) and by tail-sampling for backend cost instead of a low
  global head rate.
- Tail sampling (the intelligent "keep all errors" half) lives in Collector
  config, not the SDK, so the full story spans two artifacts (SDK docs + k8s
  Collector config).

---

## Decision analysis

### Q1 — Default rate: keep 100 % vs ship a conservative head rate

| | **Keep 100 % (chosen)** | **Default to e.g. 0.1** |
|---|---|---|
| Local dev | every trace visible — simplest | confusing: traces "missing" by default |
| Correlation (exemplars/profiling) | full coverage | ~10 % coverage out of the box — undercuts the USP |
| High-throughput services | must opt into a lower rate (explicit, visible) | protected by default but at hidden correlation cost |
| Surprise factor | matches OTel SDK default | diverges silently from upstream default |

- **Chosen: keep 100 %.** The failure mode (a hot-path service melting) is
  **specific and known to its owners**, who can apply a per-service rate; the
  cost of a low global default (silently gutting correlation everywhere) is
  diffuse and hard to debug. Make reduction explicit and local.

### Q2 — Configuration surface: env-only vs typed-only vs both

| | env-only | typed-only | **both (chosen)** |
|---|---|---|---|
| Extra SDK code | none (already works) | options + wiring + tests | options + wiring + tests |
| Discoverability | must know the var name | godoc / IDE autocomplete | both |
| Type safety | runtime string parse | compile-time | both |
| Per-deploy tuning w/o rebuild | yes | no | yes |
| Custom samplers (rate-limit) | no (six spec names only) | yes | yes |

- **Chosen: both.** They cover non-overlapping needs (code-level declaration +
  testing + custom samplers vs same-binary per-environment tuning). The marginal
  cost is one small options block; the env path already exists and only needs
  documenting.

### Q3 — Tail sampling: ship in SDK vs leave to Collector

- **Left to the Collector.** Tail sampling requires buffering whole traces and,
  when the Collector is scaled horizontally, trace-ID-aware load balancing — an
  operational concern of the collection tier, not a library. It also does
  nothing for the producing service's resource use (the problem this ADR is
  really about). The SDK owns **head** sampling; the deployment owns **tail**.
  A Collector `tail_sampling` example (errors + latency + probabilistic) belongs
  in `k8s/` docs and is tracked separately from this ADR.

---

## Implementation specifics (settle in the implementing PR)

1. **Precedence wiring** — `InitTracer` appends `WithSampler` **only when
   `cfg.sampler != nil`**, so an unset option leaves `OTEL_TRACES_SAMPLER` in
   effect. Add a test asserting: (a) no option + `OTEL_TRACES_SAMPLER` env →
   env sampler wins; (b) `WithSamplingRatio` set → overrides env; (c) neither →
   `ParentBased(AlwaysSample)`.
2. **Validation** — `WithSamplingRatio` rejects `ratio` outside `[0,1]` at
   `Init`, consistent with `validateHistogramBuckets`.
3. **Docs** — README + a short "sampling" section: the head/tail split, the
   per-service guidance, the `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG`
   and `OTEL_BSP_*` env vars, the propagation-cascade behavior, and the
   exemplar/profiling coverage trade-off. Use the watcher→JetStream→worker case
   as the worked example.
4. **Collector tail-sampling** — add a `tail_sampling` example to the `k8s/`
   Collector config in a follow-up (errors always-keep + latency policy +
   probabilistic baseline), explicitly labeled as backend-cost control, not
   service protection.
5. **No global-state impact** — `WithTraceSampler` accepts an SDK sampler value;
   nothing touches `otel.SetTracerProvider` or other globals (ADR 0003 holds).
6. **CHANGELOG** — `[Unreleased]`: add `WithSamplingRatio`, `WithTraceSampler`,
   and document the env-var support.
