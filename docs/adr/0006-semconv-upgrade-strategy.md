# ADR 0006 — Semantic Convention Version Upgrade Strategy

**Status**: Accepted (placeholder; no upgrade currently scheduled)
**Date**: 2026-04-25

---

## Context

ADR 0002 §5 pinned the SDK to OpenTelemetry Semantic Conventions
**v1.27.0** (released September 2024). At the time of that pin, the
subsystems the SDK uses (Resource, HTTP, Messaging) were already stable
in v1.27.0 and Database semconv was still experimental.

Since then, OTel semconv has continued to evolve. As of this ADR's
date the upstream specification has reached **v1.40+**. Notable
movement relevant to us:

- **Database** attributes were promoted to stable around v1.30+ with
  renames (`db.system` → `db.system.name`, etc.).
- Other subsystems we do not currently use (GenAI, Feature Flags,
  Cache, …) reached initial stable status.

This creates a recurring decision: when does the SDK upgrade its pin?
Without a written framework, every upgrade discussion starts from
zero — including the temptation to upgrade subsystem-by-subsystem,
which mixes versions and defeats the pin.

This ADR establishes the framework. It does **not** schedule an
upgrade.

---

## Why we do not chase the latest version

1. **Cost is across the SDK.** Every import of
   `go.opentelemetry.io/otel/semconv/v1.27.0` must change. Every
   helper that emits an attribute must be reviewed. Every dashboard
   query that hard-codes a key may break.
2. **Library ecosystem lags.** Pinning ahead of our key dependencies
   (currently `otel-nats`, plus any future instrumentation libraries)
   creates alignment problems we currently do not have.
3. **Stability promotion is the meaningful signal.** Most semconv
   changes between minor versions are additive or in experimental
   areas. "Subsystem X is now stable" is the trigger that justifies
   work; raw version chasing is not.
4. **Multi-version imports are forbidden.** The SDK is one process;
   one pin. Mixing `semconv/v1.X.X` and `semconv/v1.Y.Y` in the same
   module is a `Do NOT` (AGENTS.md).

---

## Decision

### Upgrade triggers

The SDK pin moves only when **at least one** of the following triggers
fires. Any one of them is sufficient justification to open a dedicated
upgrade ADR.

1. **Stability-promotion trigger.** A subsystem the SDK already uses
   is promoted to stable in a newer version, **AND** the promotion
   includes a key rename that affects an attribute the SDK currently
   emits.
2. **New-subsystem trigger.** The SDK plans to instrument a new
   subsystem (Cache, GenAI, …) and the attribute set we need is only
   stable in a later version.
3. **Dependency-alignment trigger.** A primary dependency we use
   (e.g. `otel-nats`) bumps its `semconv/vX.Y.Z` import to a newer
   version, and we choose to align rather than translate at the
   boundary.
4. **Backend-requirement trigger.** A backend or downstream consumer
   (Grafana dashboard package, a Tempo / Loki / Mimir version requirement,
   an external alert rule library) requires a newer attribute key.

### Process when a trigger fires

1. Open a dedicated ADR titled `Upgrade semconv to vX.Y.Z`.
2. Audit every import of the current pin in the codebase (`grep -r
   semconv/v1` will catch them all).
3. Identify the target version using the criteria above plus the
   "decision tree" in `docs/semconv.md`.
4. Update in a **single PR**: imports, attribute helpers, ADR 0002
   §5, ADR 0005 §7, `docs/semconv.md` catalog, tests.
5. Run integration tests against the dashboards and alerts that
   consume the metrics, spans, and logs.
6. Tag the commit (e.g. `semconv-v1.30.0`) so a revert is trivial if
   downstream breakage is discovered after merge.

### What we explicitly do NOT do

- Upgrade subsystem-by-subsystem (e.g. "let DB use v1.30 but keep
  HTTP at v1.27").
- Mix `semconv/v1.X.X` and `semconv/v1.Y.Y` imports in the same
  module.
- Allow a third-party library that hand-rolls semconv string keys (no
  Go package import) to drive our pin choice. See ADR 0005's
  rejection of `Marz32onE/instrumentation-go/otel-mongo` for the
  precedent — we treat such libraries as "permanently drifting" and
  refuse to pin around them.

---

## Currently known upgrade candidates

| Trigger | Status | Estimated target version |
|---|---|---|
| DB stable (`db.system` → `db.system.name`) | **Pending** — no MongoDB query, dashboard, or alert in production yet depends on either name | v1.30+ |
| Backend requires newer keys | Not observed | — |
| `otel-nats` upgrades semconv import | Not observed (still `semconv/v1.27.0` in v0.2.3) | — |
| New subsystem instrumentation | Not planned | — |

When MongoDB instrumentation ships per ADR 0005, the resulting
`db.system="mongodb"` spans will be the first concrete pressure point
on the DB-stable trigger. At that moment, evaluate whether to:

- **(a)** accept the gap (we ship `db.system`; the broader OTel
  ecosystem prefers `db.system.name`; backends typically tolerate
  both), or
- **(b)** trigger an upgrade ADR.

---

## Consequences

**Positive**
- Upgrades are deliberate, scoped, and reviewable.
- Dependency upgrades cannot quietly drag the SDK's semconv version
  along with them.
- ADRs accumulate institutional memory of why each pin was held or
  moved.

**Negative / Trade-offs**
- The SDK will sometimes lag behind the OTel community-recommended
  version, especially for newly-stable subsystems.
- Translation wrappers (per `docs/semconv.md` "decision tree") may be
  required for libraries that move ahead of our pin.
- Operators who follow OTel ecosystem trends closely may find the SDK
  conservative; upgrade conversations should reference this ADR to
  keep the discussion structured.
