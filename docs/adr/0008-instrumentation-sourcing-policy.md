# ADR 0008 — Instrumentation Sourcing Policy

**Status**: Accepted
**Date**: 2026-05-08

---

## Context

The SDK is preparing for adoption across multiple internal services.
Each service uses a different technology stack: some use gin, some
use stdlib `net/http`, some use NATS, some use MongoDB, some are
candidates for Cassandra or Kafka in the future.

Until now the SDK has accumulated instrumentation packages without an
explicit policy on **whether to self-write or wrap an existing
library**:

- `nats/` wraps an internal corporate library
  (`Marz32onE/instrumentation-go/otel-nats`) — a thin facade.
- `mongo/` is planned to follow the same pattern (ADR 0005).
- `http/middleware.go` was written from scratch — ~320 lines that
  reimplement what `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`
  already does, with one differentiator (a `pathLimiter` cardinality cap)
  that exists only because the agnostic middleware has no concept of
  routes.
- `gin/` and `resty/` are being designed and the same question recurs.

The `http/` decision was not deliberate; it was merged without
discussion of "self-build vs wrap". Before more integrations land and
before the SDK is consumed by other teams, this policy fixes the
default and makes self-building the **exception that requires
justification**, not the default that requires no justification.

---

## Decisions

### 1. Three-tier model

Every component the SDK ships fits into exactly one tier:

| Tier | Description | Examples | Sourcing rule |
|---|---|---|---|
| **T1 — Core SDK** | The opinionated layer that defines the SDK: `Init`, slog ↔ trace bridge, OTLP exporters wiring, Resource construction, Provider lifecycle, shutdown semantics, ADR 0003 enforcement. | `o11y.go`, `options.go`, `internal/log`, `internal/trace`, `internal/metrics` | **Always self-written.** This is the SDK's reason to exist. |
| **T2 — Thin facade over an instrumentation library** | A package that accepts SDK providers (`tp`, `mp`, `prop`) and configures a maintained third-party (community or corporate) instrumentation library. May add small ergonomics (typed handler signatures, panic glue, framework-specific signal extraction the upstream lib doesn't expose). Should not exceed ~100 lines per integration. | `nats/`, planned `mongo/`, planned `gin/` (otelgin + ErrorRecorder) | **Default for all new integrations.** The upstream lib must pass the checklist in §2. |
| **T3 — Self-written instrumentation** | A package that reimplements span creation, attribute population, metric recording from primitives. Permitted only when the §2 checklist for every candidate library fails. | Currently none should remain after ADR 0009 lands. Resty (planned, ADR 0011) is a justified T3 because no maintained `otelresty` exists. | **Exception only.** Each T3 package's ADR must enumerate which checklist items failed for which candidate libraries. |

### 2. Library evaluation checklist (T2 gate)

A candidate library qualifies for T2 facade adoption when **all five**
items pass. Failing any item means T3 (self-written) is on the table,
to be justified in the integration's own ADR.

1. **ADR 0003 compliance.** No code path reachable from the constructor
   we use mutates `otel.SetTracerProvider` or `otel.SetTextMapPropagator`.
   Reading globals as a fallback is acceptable when our wrapper always
   passes the explicit option (the existing `nats/` discipline).
2. **Maintenance signal.** Either (a) maintained by OpenTelemetry
   contrib (`go.opentelemetry.io/contrib/instrumentation/...`),
   (b) maintained by an internal corporate fork we control, or (c) a
   community library with commits in the last 6 months and an open
   issue tracker with maintainer responses. Stale community libraries
   are not eligible.
3. **Semconv alignment.** Emits attributes from a stable semconv
   version supported by the SDK (currently v1.39.0; see ADR 0006).
   Pre-stable or out-of-date attribute sets are not eligible.
4. **Configurability of names and attributes.** Span names, metric
   names, and attribute keys are either correct out of the box or
   overridable via library options. We must not be forced to fork to
   rename `http.server.duration` (old) to `http.server.request.duration`
   (current).
5. **Framework signal access.** Framework-native information that
   matters for observability (gin's `c.Errors`, resty's retry attempt
   index, MongoDB's command name, NATS JetStream consumer link) is
   either populated by the lib or accessible through a documented
   extension point that lets our T2 facade fill the gap.

### 3. Cardinality control is a cross-cutting concern, not a per-package one

The `pathLimiter` in `http/middleware.go` was the original justification
for self-building. It exists because that middleware operates at
`http.Handler` level with no route concept and has to defend against
`r.URL.Path`-shaped cardinality.

Under this policy the constraint is reframed:

- **Framework-aware instrumentation already bounds cardinality.**
  `otelhttp` with the Go 1.22+ `r.Pattern`, `otelgin` with `c.FullPath()`,
  `otelchi` with the chi route pattern — all emit `http.route` from a
  finite route table, not from the raw URL path.
- **Pathological cases (404 handlers writing arbitrary paths,
  redirect handlers) and label-explosion defense in depth move to the
  metrics pipeline** as SDK-managed views, an OTel SDK cardinality
  limit, and export-boundary presentation caps registered once at SDK
  init. The shared pipeline limits the attribute keyspace for every
  instrumentation library at once, instead of each middleware carrying
  its own limiter.

```go
// Conceptual; concrete API in ADR 0009.
sdkmetric.NewView(
    sdkmetric.Instrument{Name: "http.server.request.duration"},
    sdkmetric.Stream{
        AttributeFilter: attribute.NewAllowKeysFilter(
            "http.request.method", "http.route", "http.response.status_code",
        ),
    },
)
```

A separate hard cap on exported distinct `http.route` values
(replacing `pathLimiter`) is implemented at the export boundary, not
duplicated per middleware. In-process memory protection uses the OTel
SDK's supported cardinality limit because public views cannot mutate
attribute values and external `Reader` wrappers cannot implement the
SDK's unexported reader methods. Design detail lives in ADR 0009.

### 4. Approved-integrations registry stays in ADR 0003

ADR 0003 already maintains the table of vetted libraries against the
no-globals rule. Rather than duplicating a parallel table here, this
policy adds a discipline: when a new library is approved, the PR
adding it must update **both**:

- ADR 0003 §"Approved integrations" — global-state verification row.
- The integration's own ADR — checklist evaluation against §2 above.

ADR 0003 §"Code-review checklist item" gains a parallel line:

> Reviewer confirms: this integration is T2 by default, or its
> originating ADR enumerates which §2 checklist items failed to
> justify T3.

### 5. Application to current state

This policy implies the following actions, each tracked by its own
ADR:

| Component | Currently | Under this policy | Tracked by |
|---|---|---|---|
| `o11y.Init` & co. | T1 self-written | T1 — keep | (no ADR needed) |
| `nats/` | T2 facade over corp lib | T2 — keep | ADR 0004 (already accepted) |
| `mongo/` | T2 facade over corp lib | T2 — keep | ADR 0005 (already accepted) |
| `http/` | T3 self-written by accident | **Replace with otelhttp facade**; cardinality moves to the metrics pipeline | ADR 0009 |
| `gin/` | (planned T3) | **T2 facade over otelgin + ErrorRecorder** | ADR 0010 (forthcoming) |
| `resty/` | (planned T3) | **Justified T3** (no maintained otelresty passes §2) | ADR 0011 (forthcoming) |
| Future: gRPC | n/a | T2 over `otelgrpc` | future ADR |
| Cassandra (`gocql`) | n/a | **Justified T3** — SDK-owned observers. The `otelgocql` contrib module this row originally predicted was **removed** in contrib v1.19.0 and emitted pre-stable semconv, so no candidate passes §2. | ADR 0019 |
| Elasticsearch (`go-elasticsearch/v8`) | n/a | T2 over the client's first-party OTel instrumentation (`elastic-transport-go`) for spans, plus a **justified-T3** SDK-owned `db.client.operation.duration` recorded at the same `Instrumentation` seam (the upstream is trace-only and no candidate metric library exists). | ADR 0020, ADR 0027 |
| Future: Redis (`go-redis`) | n/a | T2 over `redisotel` (built into `go-redis` v9) | future ADR |
| Future: Valkey (`valkey-go`) | n/a | T2 over the upstream OTel hook, kept **architecturally consistent with Redis**: same package shape, same option names, same metric naming. Valkey and Redis differ at the protocol-fork level but at this SDK's API surface they should be interchangeable. | future ADR |
| Future: pgx | n/a | T2 over `otelpgx` | future ADR |
| Future: stdlib http client | n/a | T2 over `otelhttp.NewTransport` | covered by ADR 0009 |

Kafka and other messaging systems are out of immediate scope and not
listed; when one becomes a target, it goes through the §2 checklist
on entry.

The reversal of `http/` is the price of adopting this policy. It is
acceptable because no external project consumes the SDK yet.

### 6. Periodic re-evaluation discipline

The §2 evaluation is a snapshot at adoption time. Library landscapes
shift: stale community libs get adopted by maintainers, OTel contrib
adds new instrumentation, our own pinned versions drift behind
upstream stable channels.

Two cadences:

**Quarterly — T2 health check** (lightweight):

For every entry in ADR 0003's Approved-integrations table:
1. Confirm the pinned version is within 2 minor versions of upstream
   stable; if not, either bump or document the gap.
2. Re-run the global-state grep used at adoption time
   (`grep -r 'otel.SetTracerProvider\|otel.SetTextMapPropagator' vendor/<lib>`).
3. Confirm the upstream maintenance signal (last commit date, open
   issues with maintainer reply within 30 days).

The check produces an issue tagged `adr-quarterly-review` with the
results and any required follow-ups.

**Annually — T3 escape-hatch review** (deeper):

For every T3 integration (currently `resty/`, possibly more in the
future), revisit the §2 checklist for **all** candidate libraries
that exist at review time, not just the ones that existed at original
adoption. If any new candidate now passes the full checklist:

1. Open an ADR amending the relevant integration's ADR (e.g.
   "ADR 0011 amendment: adopt `xyz/otelresty` as upstream").
2. Schedule the migration as a separate PR; the original T3 code is
   removed only after the T2 facade reaches behavioral parity.

The annual review prevents T3 packages from becoming forgotten dead
weight when the ecosystem catches up.

### 7. CI gate

The policy is enforced at PR time by a small check that runs in CI.
The gate has three concrete responsibilities. The contract is fixed
here; the implementation shipped in `scripts/check_integrations.go`
(originally targeted at the ADR 0009 PR) and is described as-built in
the "Implementation note (as shipped)" under responsibility 3 below:

1. **OTel instrumentation imports must appear in ADR 0003.**
   The check parses `go.mod` for module paths matching
   `go.opentelemetry.io/contrib/instrumentation/...` (and the
   approved corporate prefix `github.com/Marz32onE/instrumentation-go/...`).
   Every match must have a corresponding row in ADR 0003's
   Approved-integrations table. Unmatched imports fail the build.

2. **T3 packages must self-declare via tier annotation.**
   Every package directory under the module root **that ships
   instrumentation** (currently `nats/`, `mongo/`, `http/`, `gin/`,
   `resty/`, and any future sibling) must contain a `doc.go` with a
   `// Tier: T2 ...` or `// Tier: T3 ...` line. The check fails the
   build if such a directory is missing the annotation. T3-tagged
   packages must additionally have a corresponding ADR file under
   `docs/adr/` that mentions the package path.

   Out of scope: T1 packages (`o11y.go` at module root, `options.go`,
   `internal/*`, `examples/*`, `docs/*`) — the gate uses an explicit
   include-list of integration directories rather than scanning every
   `doc.go`. The list is part of the gate's config, updated in the
   same PR that adds a new integration package.

3. **No direct `otel.SetX` calls in non-test code.**
   No `otel.SetTracerProvider` / `otel.SetTextMapPropagator` /
   `otel.SetMeterProvider` / `otel.SetLoggerProvider` call may appear
   in any non-test `.go` file under the module root. This enforces ADR
   0003 at the source level for our own code.

   **Implementation note (as shipped).** The "open future work" the
   original draft flagged was done in the first cut of the gate: the
   check is **AST/`go/types`-based**, not `grep`-based. `checkNoGlobalSetters`
   in `scripts/check_integrations.go` parses each non-test file with
   `go/parser` and resolves the `otel` package identity through a
   `go/types` importer, so a forbidden call is matched only when the
   selector's receiver actually resolves to `go.opentelemetry.io/otel`.
   That removes the grep-era false-positive surface (matches inside
   comments and string literals), so the `// nolint:o11y-globals`
   suppression the draft proposed has not been needed. `make adr-check`
   runs this pass together with the go.mod↔ADR-0003 and tier-annotation
   checks.

The gate runs on every PR via GitHub Actions, plus locally via
`make adr-check` and `make lint` (the existing `Makefile` target).

When the gate fails, the PR description must either fix the import
graph or reference the ADR PR that adds the missing row. The two-PR
workflow is acceptable: the integration PR can land in the same
merge queue as its ADR PR.

---

## Rationale

### Why default to facade, not self-write

1. **Semconv evolves.** Stable HTTP / messaging / DB semantic
   conventions still receive non-breaking refinements (added optional
   attributes, recommended error.type values, span name guidance).
   A facade inherits these for free; a self-written package requires
   manual catch-up that competes with feature work.
2. **Edge cases are won by widely-used libraries.** SSE-style
   streaming, `http.Hijacker`, `io.ReaderFrom` fast paths, baggage
   propagation across retries, gRPC half-close — these are areas where
   `otelhttp` and `otelgrpc` have absorbed years of bug reports.
   Our `http/middleware.go` had to grow a 7-variant `recX` switch
   (`http/middleware.go:226-285`) to handle ResponseWriter feature
   detection that `otelhttp` already gets right.
3. **Consistency across services.** External engineers reading code
   from one of our services will recognize standard `otelhttp` /
   `otelgin` idioms. A bespoke `o11y/http` middleware costs them ramp
   time without buying the SDK consumer anything.
4. **Footprint.** A facade is auditable in one screen of code. The
   ADR 0003 audit and the §2 checklist run once per upstream version
   bump, not on every internal change.

### Why not always wrap, with self-written as last resort

There are real cases where wrapping does not work:

- No maintained library exists (resty today; pre-stable frameworks).
- The library cannot be configured to emit the attribute set we
  require, or it leaks globals with no opt-out.
- The framework's signal we need is not exposed at any extension
  point (e.g. resty's `OnRetry` index would not be visible to a
  pure `RoundTripper` wrapper).

For those, self-writing is the right call — but the ADR must say so
explicitly, listing the alternatives that were tested.

### Why not also support a "T2.5" (combine multiple libs)

An earlier draft of ADR 0011 explored a hybrid resty design that
composed `otelhttp.NewTransport` with resty hooks. The design was
abandoned (see ADR 0011 §"Conclusion") because `otelhttp`'s
per-RoundTrip span ends before resty's `OnAfterResponse` /
`OnError` hooks fire, so resty-level signals could not reach the
otelhttp-created span. The lesson generalizes: hybrids only work
when the lifecycles of the composed libraries align, which is rarely
true across instrumentation boundaries. Codifying a "T2.5" would
encourage exploring more such hybrids and produce more failed
designs. The three-tier model stays.

---

## Consequences

**Positive**

- New integrations have a default starting position (T2) and a
  documented gate to escape it (§2 checklist). Reviewers no longer
  have to relitigate "should we use otelX or write our own" on every
  PR.
- The SDK's surface area grows linearly with the number of integrations
  but its self-maintained code grows much more slowly. Most new
  integrations are <100 LOC of glue.
- Cardinality discipline becomes a property of the SDK as a whole
  (views, SDK cardinality limits, and export-boundary caps) rather
  than something each instrumentation package re-implements.
- The SDK is more recognizable to external Go engineers consuming it,
  because it composes standard OTel contrib packages they have likely
  used before.

**Negative / Trade-offs**

- The §2 audit + ADR 0003 verification are repeated work on every
  upstream bump. This is the cost of leaning on third-party libraries.
- We become exposed to upstream behavioral changes (e.g. an
  `otelgin` major version that renames an attribute). We must run
  integration tests against pinned upstream versions and unpin
  deliberately.
- Reversing `http/` (per ADR 0009) is one-time work and a one-time
  doc cleanup. It is the right time because no external project has
  adopted the SDK yet; doing it after adoption would be much more
  expensive.

---

## Accepted clarifications

- **Vendor pinning policy.** Instrumentation libraries are intentionally
  pinned in `go.mod`. Every bump refreshes ADR 0003's verification row
  and reruns the ADR 0008 gate.
- **Test coverage expectation per tier.** T1 code carries direct unit or
  integration coverage. T2 facades test provider / propagator wiring and
  any SDK-owned defaults. T3 packages must test the full instrumentation
  behavior they own.
