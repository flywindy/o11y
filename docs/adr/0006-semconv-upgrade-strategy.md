# ADR 0006 - Semantic Convention Version Upgrade Strategy

**Status**: Accepted
**Date**: 2026-04-25
**Updated**: 2026-05-02

---

## Context

ADR 0002 originally pinned the SDK to OpenTelemetry Semantic Conventions
v1.27.0. That was appropriate while the SDK used only Resource, HTTP, and
Messaging conventions and while MongoDB instrumentation was still pending.

Two upgrade triggers have now fired:

1. `github.com/Marz32onE/instrumentation-go/otel-nats` v0.2.11 imports
   `go.opentelemetry.io/otel/semconv/v1.39.0`.
2. MongoDB adoption is being reconsidered, and `otel-mongo/v2` v0.2.11 emits
   DB stable names such as `db.system.name` that align with semconv v1.39.0.

The current latest Go semconv package is v1.40.0, but the SDK intentionally
targets v1.39.0 in this migration to align with both primary upstream
instrumentation libraries (`otel-nats` and `otel-mongo/v2`) without introducing
a v1.39/v1.40 split.

---

## Decision

The SDK semconv pin moves from v1.27.0 to **v1.39.0**.

SDK-owned code must import:

```go
semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
```

SDK-owned code must not import older or newer semconv packages unless a future
upgrade ADR moves the pin again.

Third-party instrumentation is acceptable when it either:

- imports `semconv/v1.39.0`, or
- emits documented string keys that match the v1.39 catalog and has a local
  compatibility test proving the emitted attributes.

---

## Migration Scope

The semconv v1.39.0 migration updates:

- SDK-owned Go imports in `o11y.go`, `http/`, and `internal/metrics/`.
- `otel-nats` to v0.2.11.
- Documentation and ADRs that referenced v1.27.0.
- `docs/semconv.md` to list the v1.39 attribute catalog.
- MongoDB integration guidance in ADR 0005 so `otel-mongo/v2` v0.2.11 becomes
  an adoption candidate.

The migration does **not** implement the MongoDB wrapper itself. That should be
a follow-up PR so the semconv move and Mongo feature work remain reviewable.

---

## Future Upgrade Process

The SDK pin moves only when at least one trigger fires:

1. **Stability-promotion trigger.** A subsystem the SDK already uses is
   promoted to stable in a newer version and the promotion includes a key
   rename that affects an emitted attribute.
2. **New-subsystem trigger.** The SDK plans to instrument a new subsystem and
   the required attribute set is only stable in a later version.
3. **Dependency-alignment trigger.** A primary dependency bumps its semconv
   import and we choose to align rather than translate at the boundary.
4. **Backend-requirement trigger.** A dashboard, alert, or backend requires a
   newer attribute key.

When a trigger fires:

1. Open or update an ADR for the target version.
2. Audit every `semconv/v1` import in SDK-owned code.
3. Update imports, attribute helpers, `docs/semconv.md`, integration ADRs, and
   tests in a single PR.
4. Run `go fmt ./...`, `go mod tidy`, `go vet ./...`, `go test ./...`, and
   `go test -race ./...`.
5. Verify dashboards and alerts that hard-code changed attribute keys.

---

## Consequences

**Positive**

- NATS and MongoDB integration decisions align on v1.39.0.
- MongoDB can use DB stable keys such as `db.system.name`.
- The SDK avoids drifting to v1.40.0 ahead of its primary instrumentation
  dependencies.

**Negative / Trade-offs**

- Dashboards or queries written for `db.system` must migrate to
  `db.system.name` when MongoDB spans are introduced.
- The SDK intentionally does not chase the latest semconv package until a
  concrete trigger justifies moving beyond v1.39.0.
- Any future dependency that moves to v1.40.0 will require another deliberate
  alignment decision.
