# ADR 0026 — SDK ↔ Integration Dependency Direction

**Status**: Proposed (decision required — see §Open decision)
**Date**: 2026-08-15
**Relates to**: ADR 0002 (metrics strategy — the views this ADR is about exist
to enforce its cardinality contract), ADR 0003 (global-state policy — why the
SDK composes providers explicitly rather than reading globals), ADR 0008
(instrumentation sourcing — the three-tier model that made T2 facades cheap to
add, and so made this coupling grow), ADR 0013 / 0014 / 0018 / 0019 (the four
integrations whose `MetricViews` the root package imports)

---

## Context

`o11y.Init` imports four integration packages — `cassandra`, `minio`, `mongo`,
`redis` — for one purpose: to collect their `MetricViews(...)` and pass them
through the `ExtraViews` seam so that a service gets correct histogram
boundaries and bounded metric labels without wiring anything itself.

```go
// o11y.go
ExtraViews: append(
    append(
        append(o11yredis.MetricViews(cfg.histogramBuckets), o11ymongo.MetricViews(cfg.histogramBuckets)...),
        o11yminio.MetricViews(cfg.histogramBuckets)...,
    ),
    o11ycassandra.MetricViews(cfg.histogramBuckets)...,
),
```

That zero-config correctness is a real property and worth keeping: PR-wave-1
had to fix a Redis histogram that shipped without a view, and the failure mode
was a metric that looked fine and carried no signal. Making view registration
the caller's job would turn that one-off oversight into a standing footgun.

The cost is that Go links at package granularity. Importing a package links
everything it imports, whether or not the imported symbols are used.

### Measured cost

A program that imports `github.com/flywindy/o11y` and calls `Init` with only
tracing and logging in mind:

| | with the current imports | with the edge removed |
|---|---|---|
| binary size | **27.7 MB** | **23.8 MB** (−3.9 MB, −14%) |
| driver packages linked | **77** | **0** |

The four modules pulled in are `gocql`, `minio-go/v7`, `mongo-driver/v2`
(including its AWS auth stack), and `go-redis/v9`. Verified with
`go list -deps` on a scratch module, and by building the same program against a
patched copy of the SDK with the four imports removed.

What a consuming service inherits today, whether or not it speaks any of these
protocols:

- **CVE surface.** `govulncheck` reports against the whole linked graph, so a
  Mongo-driver advisory becomes noise for a service that has never opened a
  Mongo connection — and noise is what makes a real advisory get skipped.
- **Version constraints.** MVS resolves these modules for every consumer. A
  service pinned to an older `mongo-driver` for its own reasons now negotiates
  with the SDK's pin.
- **`go.sum` and build time**, proportional to the above.

### What the coupling is *not*

Worth stating plainly, because it bounds how much this ADR has to solve:

- **There is no import cycle.** No integration package imports the root package
  (verified). The edge is one-directional; it is the *direction* that is wrong
  for an SDK core, not a knot that needs untying.
- **The view definitions are already driver-free.** All four `views.go` files
  import only OTel packages. The single exception is `mongo/views.go`, which
  imports `otelmongo` for `ScopeName` — a `const` string. So nothing about the
  views themselves requires a driver to be linked; the coupling is purely that
  the definitions live in packages that also contain driver code.

### Why this is being raised now

ADR 0008 made T2 facades the default, which is why there are nine integrations
and counting. Each new one that owns metrics adds another edge. The cost is
linear in integrations and paid by every consumer, so the longer this stands
the more expensive the correction — and the more services will have quietly
taken a dependency on the transitive graph.

---

## Decision drivers

1. **Zero-config correctness must survive.** A service that wraps a Redis
   client must get the right buckets without opting in. This is the property
   that makes the coupling tempting in the first place.
2. **`MetricViews` is public API.** ADRs 0013 §, 0014 §, 0018 §, and 0019 §
   all document it as the way a service that builds its own `MeterProvider`
   registers the SDK's views. Any option that removes or renames it is a
   breaking change for a documented use case.
3. **The fix should scale to the tenth integration**, not just clear the
   current four.
4. **Cost proportional to benefit.** Pre-1.0 permits breaking changes, but
   spending consumer migration budget needs to buy something.

---

## Options

### Option A — Move view definitions to driver-free leaf packages

Extract each `MetricViews` body into a package that imports only OTel (scope
names become string constants). The root package imports those; each
integration package keeps `MetricViews` as a one-line re-export.

```go
// redis/views.go — unchanged signature, unchanged behaviour
func MetricViews(buckets []float64) []sdkmetric.View { return views.Redis(buckets) }
```

- **Non-breaking.** `o11yredis.MetricViews` keeps working for the documented
  self-built-`MeterProvider` case.
- **Zero-config correctness preserved** — `Init` still composes every view.
- **Prototyped**: the edge removal compiles and yields the measured numbers
  above.
- The `otelmongo.ScopeName` constant is duplicated as a string, guarded by an
  equality assertion test in the `mongo` package (which already imports
  `otelmongo`), so a scope rename upstream fails a test rather than silently
  detaching a view from its instrument.
- **Does not** isolate anything else. If an integration later needs a
  driver-typed symbol in its view or option surface, the edge returns.

### Option B — Split each integration into its own Go module

Follow `go.opentelemetry.io/contrib`: `github.com/flywindy/o11y/redis` becomes
its own module with its own `go.mod` and release cadence.

- **Complete isolation.** A consumer's module graph contains exactly the
  drivers it requires. No leaf-package discipline to maintain.
- **Independent versioning** — a driver bump for one integration stops forcing
  a release of everything.
- **Costs**: N modules to tag and release per change (the repo currently has
  one CHANGELOG and one tag series); cross-module changes need coordinated
  releases and temporary `replace` directives during development; consumers add
  a `require` per integration; the ADR-check gate, CI matrix, and release
  process all need reworking.
- **Does not by itself solve zero-config views**: the root module still must
  not import the integration modules, so it needs Option A's leaf packages (or
  a registration mechanism) anyway — with the leaf packages now living in a
  module every consumer pulls.

### Option C — Caller registers views explicitly

Drop the collection from `Init`; a service passes
`o11y.WithExtraMetricViews(o11yredis.MetricViews(...))`.

- Cleanest possible dependency direction: the root package never names an
  integration.
- **Rejected.** It re-creates, as a standing requirement on every service, the
  exact failure PR-wave-1 fixed: an integration whose views are not registered
  emits a histogram with default millisecond boundaries that *looks* healthy.
  It also puts the caller in charge of threading `WithHistogramBuckets` through
  to each `MetricViews` call, so an override silently applies to some
  instruments and not others. Correct-by-default is worth more than the last
  increment of purity here.

### Option D — Status quo

Accept the 3.9 MB and the four drivers. Defensible only if consumers are all
first-party services that use most integrations anyway — which is not the
stated direction (ADR 0008 §Context: "preparing for adoption across multiple
internal services", each with a different stack).

---

## Recommendation

**Option A now; Option B as a separate, later decision.**

A is non-breaking, prototyped, buys the entire measured benefit, and is
reversible. B is the more complete answer but its cost is concentrated in
release engineering rather than in code, and that cost is worth paying only
once there is evidence the leaf-package discipline is failing — for example, the
first integration that genuinely needs a driver-typed symbol in its view
surface, or a real MVS conflict reported by a consumer.

Taking A first does not foreclose B: the leaf packages A creates are exactly
what a module split would need anyway.

---

## Consequences

**For consuming services** — no source change. After upgrading and running
`go mod tidy`, the four driver modules leave the graph of any service that does
not use them directly; binaries shrink by roughly 14%; `govulncheck` output
narrows to what the service actually links.

One migration hazard: a service that (incorrectly) relied on one of these
drivers reaching it transitively, without its own `require`, will fail to
compile. The fix is a `go get` of the driver it was already using. This should
be called out in the CHANGELOG rather than left to be discovered.

**For the SDK** — one more package layer to keep straight, and a duplicated
scope-name constant with a test to guard it. New integrations that own metrics
must put their view definitions in the leaf package; this belongs in the ADR
0008 checklist so it is enforced at review time rather than remembered.

---

## Open decision

1. **A or B?** The recommendation is A, with B deferred until there is evidence
   it is needed. A team that already intends to split modules for release-cadence
   reasons may prefer to spend the disruption once.
2. **Where do the leaf packages live?** `internal/views` (one package, four
   functions — simplest, but "internal" understates that these definitions are
   the substance of a public API) or `<integration>/views` per package (more
   files, keeps each integration's views beside it). Leaning `internal/views`;
   the public surface stays the existing `MetricViews` re-exports either way.
3. **Should the ADR 0008 checklist gain a "view definitions live in the leaf
   package" item**, enforced by `scripts/check_integrations.go`? Leaning yes —
   the gate already parses integration packages, and an unenforced convention
   is how the current edge accumulated.
