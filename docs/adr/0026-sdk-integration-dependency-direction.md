# ADR 0026 — SDK ↔ Integration Dependency Direction

**Status**: Accepted (Option A) — 2026-09-11. Option B is **not** adopted and
remains open; see §Decision.
**Date**: 2026-08-15 (proposed), 2026-09-11 (accepted)
**Relates to**: ADR 0002 (metrics strategy — the views this ADR is about exist
to enforce its cardinality contract), ADR 0003 (global-state policy — why the
SDK composes providers explicitly rather than reading globals), ADR 0008
(instrumentation sourcing — the three-tier model that made T2 facades cheap to
add, and so made this coupling grow), ADR 0013 / 0014 / 0018 / 0019 (the four
integrations whose `MetricViews` the root package imports)

---

## Context

*Describes the state at the time this ADR was proposed. The coupling below has
since been removed — see §Decision.*

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
- **`go.mod` / `go.sum` entries.** A consumer that imports only the root package
  still records all four drivers as indirect requirements.
- **Version constraints.** MVS resolves these modules for every consumer: a
  service that pins an older `mongo-driver` for its own reasons is still raised
  to the SDK's version. Note this one survives Option A — see §Options.
- **Build time**, proportional to the above.

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
- **Prototyped and measured** (all figures below verified on a scratch consumer
  module against a patched copy of the SDK):
  - binary 27.7 MB → 23.8 MB; driver packages linked 77 → 0;
  - the consumer's `go.mod` indirect requirements and `go.sum` entries for all
    four drivers **disappear** after `go mod tidy` — Go 1.17+ graph pruning
    records only what the consumer's import graph needs;
  - the pruned graph shrinks accordingly (`go mod graph` gocql edges 9 → 1).
- **What it does *not* deliver — MVS isolation.** The integrations stay in the
  same module, so `o11y`'s own `go.mod` keeps requiring all four drivers and
  that requirement edge stays in the graph. Measured: a consumer that explicitly
  requires `gocql v1.6.0` is still resolved to **v1.7.0**, exactly as before.
  Any goal phrased as "let consumers pin their own driver versions" needs
  Option B; Option A does not move it at all.
- The `otelmongo.ScopeName` constant is duplicated as a string, guarded by an
  equality assertion test in the `mongo` package (which already imports
  `otelmongo`), so a scope rename upstream fails a test rather than silently
  detaching a view from its instrument.
- If an integration later needs a driver-typed symbol in its view or option
  surface, the edge returns.

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

Drop the collection from `Init`; a service registers the views itself, through
an option this ADR would have to add — there is no such public option today
(the existing seam is the internal `metrics.Config.ExtraViews` field), so read
`o11y.WithExtraMetricViews(o11yredis.MetricViews(...))` below as the API Option
C implies rather than one that exists.

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

A is non-breaking, prototyped, and buys the linkage benefits — binary size,
linked-package count, the consumer's `go.mod`/`go.sum`, and govulncheck noise —
in full, while being reversible. It buys **none** of the module-resolution
benefit: version selection is unchanged, so a consumer that needs to pin a
driver version is no better off than today.

That makes the choice sharper than it first looks. If the motivation is
"consumers should not carry, link, or scan drivers they do not use", A is
sufficient and cheap. If the motivation includes "consumers should control
their own driver versions", **only B delivers it**, and adopting A first would
mean paying the disruption twice.

B's cost is concentrated in release engineering rather than in code, so the
recommendation stands only under the first motivation. Which motivation applies
is the decision this ADR is asking for.

Taking A first does not foreclose B: the leaf packages A creates are exactly
what a module split would need anyway.

---

## Consequences

**For consuming services** — no source change, and no migration step. After
upgrading and running `go mod tidy`, the four driver modules leave the
`go.mod` and `go.sum` of any service that does not import them directly;
binaries shrink by roughly 14%; `govulncheck`, which works from the call graph,
narrows to what the service actually links.

Two things checked and found *not* to be consequences, because an earlier draft
of this ADR asserted both:

- **No compile break for transitive users.** A service that imports a driver
  without requiring it still builds, because `o11y`'s requirement edge remains
  in the module graph and resolves; `go mod tidy` then promotes it to an
  explicit `require`. There is nothing for the CHANGELOG to warn about.
- **No change to version selection.** See §Options — a consumer pinning an
  older driver is still raised to the SDK's version.

**For the SDK** — one more package layer to keep straight, and a duplicated
scope-name constant with a test to guard it. New integrations that own metrics
must put their view definitions in the leaf package; this belongs in the ADR
0008 checklist so it is enforced at review time rather than remembered.

---

## Decision

**Option A, adopted 2026-09-11.** Every integration's view definitions live in
the driver-free `internal/views` package; each integration keeps `MetricViews`
as a one-line re-export, so no consumer changes a line. `scripts/check_integrations.go`
enforces it via `go list -deps` on the root package, which asserts against the
build graph rather than against source imports and so cannot be worked around by
an indirect import.

Measured on the root package after the migration: transitive dependencies
**590 → 457**, and gocql, minio-go, mongo-driver and go-redis all at **0**
linked packages (they were 4, 14, 49 and 9).

**Option B is not adopted and is not foreclosed.** It remains the only way to
give consumers control of their own driver versions, and the only way to
decouple release cadence. Two things bound how urgent it is:

- MVS only raises versions, never lowers them. A service that needs a driver
  *newer* than the SDK's already gets it today. B changes only the case where a
  service needs an *older* one — which no service has yet asked for.
- The leaf packages A creates are exactly what B needs, so A is a strict prefix
  of B rather than a detour. An earlier draft of this ADR warned that taking A
  first would "pay the disruption twice"; that was wrong and is withdrawn. A is
  non-breaking, so consumers pay nothing for it, and B's own consumer cost is one
  `require` line that `go mod tidy` adds automatically. B's real cost is
  maintainer-side release engineering, and that cost is the same whenever it is
  paid.

**Revisit B when** a service is actually blocked by the SDK's driver version, or
when releasing everything together to ship one integration's fix becomes a real
drag on cadence. Neither has happened yet.

### How the two subsidiary questions were settled

They were answered in practice before this ADR was formally accepted, by
ADR 0027's elasticsearch work:

1. **Where do the leaf packages live?** `internal/views` — one package, one file
   per integration. The public surface stays each integration's existing
   `MetricViews` re-export, so "internal" describes where the definitions sit,
   not how reachable they are.
2. **Should the convention be enforced?** Yes, and it already is:
   `checkRootDoesNotLinkDrivers` in `scripts/check_integrations.go` fails the
   build if a driver module prefix appears in the root package's dependency
   graph. Elasticsearch was the first integration held to it; the other four
   joined on adoption of this ADR. An unenforced convention is how the original
   edge accumulated, which is the argument for the gate.

### Notes for the next integration

- Put the view definition in `internal/views/<integration>.go` and make
  `<integration>/views.go` a one-line re-export.
- Define the instrumentation scope as `views.<Integration>Scope` and alias the
  integration package's `instrumentationName` to it, so the scope recorded under
  and the scope matched against are one constant.
- Add the driver's module prefix to `rootForbiddenDepPrefixes`.
- If a view must name a constant owned by an upstream instrumentation package
  whose import would link the driver, copy the literal into `internal/views` and
  add an equality assertion test in the integration package, which already
  imports it — see `mongo.TestMongoContribScopeMatchesUpstream`. A silently
  detached view is the failure mode being guarded against: the instrument keeps
  emitting, just with default boundaries and an unbounded label set.
