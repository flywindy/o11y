---
name: verify-semconv-attributes
description: Use whenever writing, reviewing, or changing an OpenTelemetry semantic-convention attribute key, metric name, or any `db.*` / `messaging.*` / `http.*` / `<system>.*` identifier — in Go code, ADRs under docs/adr/, docs/semconv.md, or PR review. ALWAYS use before asserting that an exact attribute-key string is correct, because semconv renames keys and moves them between namespaces across versions (e.g. `db.cassandra.*` → `cassandra.*`, `db.elasticsearch.node.name` → `elasticsearch.node.name`). Verify against the pinned semconv package source, never from memory, blogs, or web-search summaries.
---

# Verify Semconv Attributes

## Why this exists

Attribute keys you "remember" are almost always a **previous** semconv major.
The OpenTelemetry database conventions were restructured during stabilization
(stable in semconv **v1.33.0**); the SDK pins **v1.39.0**. Whole vendor
namespaces were deprecated and moved. Trusting memory, a blog, an old
hexdocs page, or even a one-line web-search summary has produced wrong keys in
this repo more than once (`db.cassandra.consistency.level` and
`db.elasticsearch.node.name` were both written wrong, then corrected to
`cassandra.consistency.level` / `elasticsearch.node.name`).

**The one rule:** the source of truth is the `attribute.Key("...")` literal in
the pinned semconv package on disk — not your memory, not a doc, not a search
result. Resolve every key there before you write it down or defend it.

## Workflow

### 1. Find the pinned semconv version

```bash
grep -rho 'go.opentelemetry.io/otel/semconv/v[0-9.]*' --include='*.go' . | sort -u
# Cross-check the catalog's declared pin:
sed -n '1,15p' docs/semconv.md
```

All SDK-owned code must use one version (currently `v1.39.0`). If imports
disagree, that is its own bug (see ADR 0006).

### 2. Resolve the EXACT key from the pinned package source (offline, deterministic)

The package is in the Go module cache. Grep the literal — do not paraphrase it:

```bash
SEMV=v1.39.0
SRC=$(find "$(go env GOMODCACHE)" -path "*otel/semconv/$SEMV/attribute_group.go" | head -1)
# Find every key in a namespace you care about:
grep -nE 'attribute\.Key\("(cassandra|db\.cassandra|elasticsearch|db\.elasticsearch|db\.)' "$SRC"
# Or resolve a specific constant by name:
grep -n 'CassandraConsistencyLevelKey' "$SRC"
```

`go doc` also works for browsing the constant names:

```bash
go doc go.opentelemetry.io/otel/semconv/v1.39.0 | grep -i cassandra
```

Read the literal string inside `attribute.Key("...")`. That string — verbatim,
including whether it has a `db.` prefix and whether it uses `.` or `_` — is the
answer. If the constant does not exist in the package, the key is **not** part
of the pinned version (it was renamed or removed; go to step 3).

### 3. Check for deprecation / namespace moves

A key existing in an *older* version tells you nothing about the pinned one.
Confirm the current home and whether the old spelling is deprecated:

```bash
# Registry shows deprecations and their replacements:
#   https://raw.githubusercontent.com/open-telemetry/semantic-conventions/main/docs/registry/attributes/db.md
#   https://raw.githubusercontent.com/open-telemetry/semantic-conventions/main/docs/registry/attributes/<system>.md
```

Known moves already hit in this repo (database stabilization, stable in
v1.33.0). When you see the left column, use the right:

| Deprecated (old) | Current (v1.39.0) |
|---|---|
| `db.system` | `db.system.name` |
| `db.statement` | `db.query.text` |
| `db.cassandra.consistency_level` (and the `db.cassandra.consistency.level` mid-form) | `cassandra.consistency.level` |
| `db.cassandra.page_size` | `cassandra.page.size` |
| `db.cassandra.idempotence` | `cassandra.query.idempotent` |
| `db.cassandra.speculative_execution_count` | `cassandra.speculative_execution.count` |
| `db.cassandra.table` | `db.collection.name` |
| `db.elasticsearch.node.name` | `elasticsearch.node.name` (no `db.` prefix) |
| `db.elasticsearch.cluster.name` | `db.namespace` |
| `db.elasticsearch.path_parts.<key>` | `db.operation.parameter.<key>` |

The pattern: system-specific concepts move to a **top-level `<system>.*`**
namespace; anything that generalizes folds into the **generic `db.*`** set.
Assume any `db.<vendor>.*` key you remember has moved — verify it.

### 4. In code: use the constant, never a string literal

`docs/semconv.md` Enforcement Rule #1: reference the semconv constant
(`semconv.CassandraConsistencyLevelKey`), never a hardcoded `"cassandra..."`
string. The compiler then guarantees the string; a version bump re-maps it for
free. String literals are only for the deviations table.

### 5. For T2 facades, separate two different keys

A third-party instrumentation library often **lags** semconv. Track both:

- **semconv target key** (what v1.39.0 says — from step 2/3).
- **what the upstream actually emits** (resolve from the upstream source, the
  same way: grep its `attribute.Key("...")`). It is commonly an older spelling
  (e.g. Elastic's transport emits `db.system`, `db.statement`,
  `db.elasticsearch.path_parts`).

Pin the **actually emitted** keys with a compatibility test and record the drift
in the integration ADR + `docs/semconv.md` Deviations. Do not assume the lib
emits the current key.

### 6. In ADRs / docs: write the verified key + its deprecated predecessor

State the current key, and when relevant note `replaces deprecated <old>` so the
next reader does not "correct" it back to the old form.

## Red flags — stop and run step 2

- You are about to type a `db.<vendor>.*` key from memory.
- A reviewer (human or bot) asserts a different key — verify against the package
  before agreeing **or** disagreeing; both sides have been wrong here.
- A web-search summary or hexdocs page (often pinned to an old version like
  v1.27.0) gives a key — treat it as a hint, not the answer.
- You read a WebFetch summary of the package — re-read the literal
  `attribute.Key("...")`; summaries have misreported the `db.` prefix here.

## Output

When you finish a verification, report: the pinned version, the file you grepped,
the verbatim key(s), and any deprecated predecessor — so the check is auditable,
not asserted.
