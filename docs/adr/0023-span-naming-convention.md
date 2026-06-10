# ADR 0023 — Cross-Package Span Naming Convention for Data-Store Integrations

**Status**: Proposed
**Date**: 2026-06-10

**Amends** ADR 0013 (redis), ADR 0018 (minio), ADR 0021 (mongo
instrumentation mechanism); **applies forward to** ADR 0019 (cassandra) and
ADR 0020 (elasticsearch).

---

## Context

The SDK ships several data-store integrations whose client spans were named
independently, each following whatever its source library or originating ADR
established. The result was three different shapes in the same trace view:

| Integration | Old span name | Shape |
|---|---|---|
| redis | `redis.GET` | `{system}.{operation}` |
| mongo | `users.find` | `{collection}.{command}` (otelmongo default) |
| minio | `PutObject media` | `{operation} {target}` |

When `redis`, `mongo`, and `minio` spans appear under one parent span, an
operator cannot tell at a glance which system a span belongs to, and the
operation token lands in a different position each time (some lead with the
noun, some with the verb, some use `.`, some use a space). The system
identity is carried truthfully in attributes (`db.system.name`,
`object_store.system.name`), but span names are the human-facing label in
trace UIs, and the inconsistency is real friction.

OTel's stable database guidance is `{db.operation.name} {target}` — verb
first, space-separated, low-cardinality target — and deliberately does **not**
encode the system in the span name (it lives in `db.system.name`). That
guidance assumes a UI that surfaces attributes; in practice operators read
span names directly, and a bare `GET` or `find users` does not reveal the
backend.

The SDK is pre-1.0 with no tagged release, so there is no external dashboard
or saved-query contract to preserve. This is the cheapest moment to unify.

## Decision

**All data-store client spans use the name
`{system.name}.{operation} {target}`**, where:

- `{system.name}` is the exact value the integration emits for its
  system attribute (`db.system.name` / `object_store.system.name`) — `redis`,
  `mongodb`, `s3`, `cassandra`. The prefix is joined with a **dot** (`.`),
  read as namespace/ownership ("the GET that belongs to redis").
- `{operation}` is the logical operation / command, verb-first.
- `{target}` is the low-cardinality container the operation acts on
  (collection, bucket, table), joined with a **space** per OTel semconv.
  Omitted when the operation has no single target (`redis.GET`,
  `mongodb.ping`).

| Integration | New span name | Note |
|---|---|---|
| redis | `redis.GET`, `redis.pipeline` | already conformant — no code change |
| mongo | `mongodb.find users` | `WithSpanNameFormatter` override on the otelmongo monitor |
| minio | `s3.PutObject media` | default formatter change |
| cassandra | `cassandra.SELECT messages_by_room` | T3 seam sets it directly |

### Separator rationale: dot for system, space for target

The two separators carry different meaning, so mixing them is clearer than
picking one:

- **Dot** = namespace / ownership, like `package.method`. It cleanly isolates
  the "which system" segment.
- **Space** = the OTel-defined separator between operation and target. Keeping
  it as a space stays aligned with stable DB semconv for the operation/target
  portion.

`mongodb.find users` reads left-to-right as *system → operation → target*.
Using all-spaces (`mongodb find users`) would flatten the system segment into
the operation/target tokens; using all-dots (`mongodb.find.users`) would
collide with collection/bucket names that legally contain dots.

### Why the system prefix, against OTel's "system in attributes only"

This is a **deliberate deviation** from stable OTel guidance, scoped to span
names only — all system/operation/target attributes remain exactly as their
ADRs specify. The deviation buys two things OTel's model assumes the UI
provides but many do not: at-a-glance system identification, and a uniform
verb-first position across every data store. For redis in particular, the
strict guidance collapses to a bare command name (`GET`) — no target exists,
because the key is high-cardinality — which is the least readable of all; the
prefix is what makes it legible. A future migration back to a blessed
convention is a span-name change only (a rename), not a re-modeling.

### Scope: data-store integrations only

The convention applies to `redis`, `mongo`, `minio`, `cassandra`. It does
**not** apply to the HTTP-layer integrations (`http`, `gin`, `resty`), which
follow OTel HTTP semconv (`{METHOD} {route}`) and have no single "system"
dimension.

### User-supplied formatters are verbatim

Where an integration exposes `WithSpanNameFormatter`, a non-empty return is
used **as-is** — the system prefix is part of the *default* name only and is
never forced onto a caller's override. This matches how every formatter in the
repo already behaves (the http/gin/resty formatters pass straight through to
their upstream otel library).

## Elasticsearch exception

Elasticsearch (ADR 0020) is a **T2 facade** over `elastic-transport-go`'s
first-party instrumentation, which emits the span name itself and exposes no
span-name seam (unlike `otelmongo`, whose `WithSpanNameFormatter` is what makes
mongo conformable). The convention therefore **does not apply** to ES in v1;
it is a recorded, accepted divergence, to be revisited if upstream adds a
formatter or the facade is promoted to a T3 seam.

## Consequences

- Span names change for `mongo` and `minio` (and the planned `cassandra`).
  `redis` is unchanged. Pre-1.0, so no compatibility shim is provided; the
  change is listed under Breaking Changes in the CHANGELOG.
- The mongo formatter re-derives the collection from the raw command event,
  mirroring otelmongo's unexported `extractCollection`. This couples the
  package to stable MongoDB wire-format structure; a unit test pins the
  behavior and a code comment flags the coupling.
- Any trace-search query or `spanmetrics` connector keyed on the old span
  names must be updated. None exist in-tree.
