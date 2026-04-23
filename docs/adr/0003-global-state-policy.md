# ADR 0003 — Global State Policy for OTel Providers

**Status**: Accepted
**Date**: 2026-04-22

---

## Context

`AGENTS.md` lists **Zero Global State** as a non-negotiable core principle:

> Encapsulate OTel providers in structs. No package-level `init()` with side
> effects. No global logger variables.

In practice this principle is applied by returning all OTel providers
(`TracerProvider`, `MeterProvider`, `LoggerProvider`, `TextMapPropagator`) from
`o11y.Init` on the `*SDK` struct, and wiring them explicitly into every caller
and every instrumentation wrapper (`nats/`, future `mongo/`, `http/`).

However, third-party OTel instrumentation libraries commonly take a shortcut
that violates this: they call `otel.SetTracerProvider(...)` and
`otel.SetTextMapPropagator(...)` inside their constructors so that internal
code can freely use `otel.Tracer("...")` and `otel.GetTextMapPropagator()`
without threading dependencies through.

The presence of such a call anywhere in the runtime mutates **process-wide
state**, not just the instrumented subsystem. This ADR establishes the policy
for how the SDK and its wrapper packages must handle that risk.

### Where this principle comes from

"Zero Global State" is rooted primarily in **Go 2020+ library idioms** —
the broader trend in the Go ecosystem to move away from the package-level
globals that dominated early stdlib (`log.Printf`, `http.DefaultClient`,
`rand.Seed`). Newer stdlib additions such as `log/slog` (Go 1.21) and
`rand/v2` (Go 1.22), together with the long-standing discipline of
explicit `context.Context` propagation (Go 1.7), all codify instance-based
state over ambient globals.

OpenTelemetry's own guidance for library authors happens to agree:
*"If you are building a library, you should avoid setting the global
TracerProvider. Instead, accept a TracerProvider as a parameter."* But
this is a **reinforcement**, not the origin — even without OTel's
stance, a Go SDK written in 2025 should arrive at the same conclusion
from Go idioms alone.

The practical consequence is that some third-party OTel instrumentation
libraries (written by authors who don't share this Go-idiomatic bias)
will not be adoptable as-is, and the SDK must do its own verification
at the boundary.

---

## Decision

**The SDK and every wrapper package it ships must not cause
`otel.SetTracerProvider` or `otel.SetTextMapPropagator` to be invoked**,
whether directly or transitively through a third-party constructor.

Concretely:

1. **Direct invocation is forbidden** in any package under
   `github.com/flywindy/o11y/...`.
2. **Transitive invocation** (calling a third-party constructor that internally
   calls the setters) is equally forbidden. Every third-party instrumentation
   library introduced into this repository must be verified before adoption.
3. **Application code** (a user's `main()`) may still choose to set globals
   if they want. That is an application-level decision outside this SDK's
   scope and is not affected by this ADR.

---

## Rationale

### Why globals are dangerous

1. **Initialization order becomes an implicit contract.**
   If any package-level variable or `init()` captures
   `otel.Tracer("foo")` *before* the setter runs, it permanently holds a
   noop tracer. Bugs rooted in this are silent, timing-dependent, and hard
   to trace.

2. **Multi-instance and multi-tenant scenarios break.**
   Tests running `o11y.Init` twice (parallel or sequential), processes
   hosting two services with distinct identities, or sidecars sharing a
   process all collapse into a single global — the last writer wins.

3. **Test pollution.**
   A global mutated by one test leaks into every subsequent test in the
   same binary unless restored manually. Parallel tests race.

4. **Upgrade risk.**
   If an upstream library silently changes its global-mutation behavior
   (or merges vs. overwrites), process behavior changes with no code
   diff on our side.

5. **Explicit dependency is better engineering hygiene.**
   Passing `tp` and `prop` through constructors makes the dependency
   visible, refactorable, and mockable.

### What we do NOT lose by avoiding globals

The MongoDB and NATS drivers themselves do not read `otel.GetTracerProvider()`
internally — that would defeat the purpose of OTel's dependency-injection
design. The only code that reads globals is code written *by* the
instrumentation library author. If we pass the provider explicitly, the
library works identically; we just carry one parameter through a constructor.

The single thing we "lose" is the convenience of the library author's
`otel.Tracer("mongo")` shortcut inside their own callbacks. That convenience
is not worth the principle violation.

---

## Enforcement

### For every new instrumentation integration

Before introducing any `github.com/<vendor>/otel-<thing>` library, verify:

1. **Read the constructor source.** Does it call `otel.SetTracerProvider`
   or `otel.SetTextMapPropagator`? `grep -r 'otel.Set' vendor/github.com/<vendor>/otel-<thing>`
   should produce zero matches in any code path reachable from the
   constructor the wrapper uses.
2. **Inspect option semantics.** A library that reads
   `otel.GetTracerProvider()` **only as a fallback when no option is passed**
   is acceptable — the wrapper must always pass the option so the fallback
   never fires.
3. **Document the finding.** The integration's own ADR must contain a
   "Global-state verification" section recording the library version,
   the verification command used, and the outcome.

### Approved integrations

| Library | Version | Verified | Behavior | Notes |
|---|---|---|---|---|
| `Marz32onE/instrumentation-go/otel-nats` | v0.2.1 | ✅ | Reads globals as fallback only; never sets. Safe when `WithTracerProvider` / `WithPropagators` options are supplied. | See ADR 0004 |
| `Marz32onE/instrumentation-go/otel-mongo/v2` | TBD | ❌ | `ConnectWithOptions` calls `otel.SetTracerProvider` and `otel.SetTextMapPropagator`. MUST NOT be used through that entry point. | See ADR 0005 |

When a new library is added or an existing one bumped, update this table
in the same PR as the version change.

### Code-review checklist item

> Reviewer confirms: no new call path causes
> `otel.SetTracerProvider` / `otel.SetTextMapPropagator` to execute,
> directly or through any imported dependency.

---

## Consequences

**Positive**
- Multiple `o11y.Init` calls in the same process are safe (test isolation,
  multi-service embedding, local benchmarks).
- Behavior of any instrumentation subsystem depends only on the providers
  explicitly passed to it, not on process-wide state.
- Future migrations between OTel library versions or vendors are limited
  to the wrapper boundary.

**Negative / Trade-offs**
- Some third-party instrumentation libraries cannot be adopted as-is and
  require either a fork, a vendor-and-patch, or a from-scratch
  reimplementation against the driver's native extension point
  (e.g. MongoDB's `event.CommandMonitor`).
- Wrapper packages carry extra lines of code to thread `tp`/`prop`
  through constructors instead of relying on ambient globals.
- The maintenance burden of verifying each upstream version (per the
  checklist above) falls on the SDK maintainers.
