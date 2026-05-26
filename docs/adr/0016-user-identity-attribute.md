# ADR 0016 — User Identity Attribute (`user.name`) on Telemetry

**Status**: Accepted — phased rollout, implementation pending (Phase 1 first;
Phase 2 gated behind an explicit opt-in)
**Date**: 2026-05-26
**Relates to**: ADR 0001 (log format — the slog handler is the log-enrichment
hook), ADR 0003 (global-state policy — any SpanProcessor must be wired into the
SDK-built provider, never via `otel.SetTracerProvider`), ADR 0004 (NATS — the
NATS facade is what carries baggage across services), ADR 0006 (semconv upgrade
— `user.name` key sourcing), ADR 0015 (sampling — span attributes only record on
sampled spans; both features touch `internal/trace` provider construction),
`docs/semconv.md` (attribute catalog)

---

## Context

We want the acting user's identity (`user.name`, i.e. the login username) to be
attributable in telemetry, for two stated purposes:

1. An engineer looking at a trace/log can see **who was affected**.
2. When a user reports an issue, support can **query by username** to find the
   relevant traces/logs.

The deployment is **many microservices for a single product, wired together over
NATS / JetStream** (plus HTTP). The desired ergonomics are: **identify the user
once at the source** (e.g. after auth at the edge) and have downstream services
**not re-supply** the field.

There are two fundamentally different mechanisms available, and they are not
interchangeable:

- **Explicit helpers** — the developer writes the attribute onto the span and
  onto logs at each call site. Nothing is automatic; nothing propagates.
- **Context auto-propagation via OTel Baggage** — the value is put in baggage
  once; the SDK auto-copies whitelisted baggage members onto spans and logs, and
  the existing propagators carry baggage across service boundaries.

This is a "vertical" decision (it touches log/trace hot paths and the network
boundary), so it is recorded here rather than decided ad hoc.

### Key clarification: a helper does not "enable filtering"

Whether a log can be filtered by `user.name` in Loki depends solely on whether
**that attribute is present on the log record** — not on whether a helper was
used. A helper (`o11y.UserName(name)`) and a literal
(`slog.String("user.name", name)`) produce **identical** data. The helper's only
value is: no hard-coded key string, decoupling from the semconv version, and
compile-time typo safety. Likewise, the span setter `SetUser(ctx, name)` writes
to the **span**, not to logs — so calling only the span setter leaves logs
without the field (correlation then requires a trace→log join on `traceId`).

### PII framing

`user.name` is **personal data / PII** (Personally Identifiable Information): it
identifies a specific account/person. It is *regular* personal data, **not** a
special category (health, biometrics, etc.) and is far less sensitive than
secrets (passwords/tokens, which must **never** enter telemetry). The two stated
purposes are legitimate, but the data still requires: access control on the
telemetry backends (Tempo/Loki), a retention policy, and awareness of **where it
spreads** — which is exactly where the two mechanisms diverge.

---

## Dependency behavior (verified)

- **Log enrichment hook exists today.** `internal/log/handler.go`
  `OtelSlogHandler.Handle(ctx, r)` already reads the span from `ctx` and injects
  `traceId`/`spanId` into every record. Reading whitelisted baggage from the same
  `ctx` and `r.AddAttrs(...)` is the natural extension point.
- **Span enrichment needs a SpanProcessor.** Spans do not read arbitrary context
  values. The only context-aware hook is
  `SpanProcessor.OnStart(parent context.Context, s ReadWriteSpan)`, which
  receives the start-time context. `internal/trace/trace.go:49` builds the
  provider with only `WithBatcher(exporter)` + `WithResource(res)`; a baggage→span
  processor would be added via a second `WithSpanProcessor(...)`.
- **Baggage already propagates.** The composite propagator includes
  `propagation.Baggage{}` (`internal/trace/trace.go:55-58`, `o11y.go:195`).
- **The NATS facade carries baggage with zero new code.** `nats/middleware.go:16`
  injects via `prop.Inject(ctx, propagation.HeaderCarrier(msg.Header))` using that
  composite propagator, and `js.Publish` auto-injects the active context. So a
  baggage member rides the `baggage` NATS header across every hop where
  Inject/Extract (or `js.Publish`/`Consume`) is used, and re-materializes as
  baggage in the downstream `ctx`. **This is the mechanism that satisfies the
  "set once at source" goal — and equally the mechanism by which PII reaches the
  wire.**
- **Sampling interaction (ADR 0015).** `span.SetAttributes` is a no-op on a
  non-recording (unsampled) span. So `user.name`-on-span coverage tracks the head
  sampling rate. **Baggage propagation and log enrichment are independent of
  sampling** — baggage flows and logs are emitted regardless of the sampled flag.
- **semconv key sourcing.** Per `docs/semconv.md`, an SDK-owned attribute key must
  reference the pinned `semconv/v1.39.0` constant where one exists; otherwise the
  string key is a documented deviation. Whether v1.39.0 exposes a `user.name`
  constant must be confirmed in the implementing PR, and the catalog updated in
  the same commit.

---

## Decision

**Retain both mechanisms, delivered in two phases. They share the single
`user.name` key, so they compose rather than conflict.**

### Phase 1 — Explicit helpers (`SetUser` + `UserName`)

Ship two small, purely additive helpers. No change to `internal/`, no provider
wiring, no network behavior.

- `o11y.SetUser(ctx, name)` — writes `user.name` onto the current span.
- `o11y.UserName(name) slog.Attr` — returns the log attribute, e.g.
  `logger.InfoContext(ctx, "...", o11y.UserName(name))`.

This gives a safe, visible, in-process-only tool immediately, with effectively
zero blast radius and trivial maintenance. It does **not** propagate across
services — each service that wants the field calls the helpers itself.

### Phase 2 — Context auto-propagation via Baggage (opt-in feature, automatic once on)

Add the "set once, flows everywhere" capability:

- `o11y.ContextWithUser(ctx, name)` — sets the `user.name` baggage member.
- Extend `OtelSlogHandler.Handle` to copy **whitelisted** baggage members onto
  every log record.
- Add a `SpanProcessor` whose `OnStart` copies whitelisted baggage onto every
  span, wired into the provider in `internal/trace/trace.go`.
- A fixed **whitelist** (`user.name` only, to start) bounds baggage header size
  and prevents baggage sprawl. Both the setter and the enrichers respect it.

**Default posture: the feature is OFF at the SDK level, enabled explicitly per
service (e.g. a `WithUserBaggage()`-style option). Once enabled, enrichment is
fully automatic for that service — no per-call and no downstream code.** Default
OFF because Phase 2 puts PII on the wire and should be a deliberate choice, not
an implicit default forced on every SDK consumer. The product's NATS mesh enables
it on purpose.

**Mandatory guardrails when Phase 2 is enabled (documented, partly the
deploying service's responsibility):**

1. **Ingress: do not trust inbound baggage from untrusted callers.** An external
   client can forge `baggage: user.name=admin`. At the edge, set `user.name`
   **after** authentication, overriding/ignoring inbound baggage. Internal
   service-to-service hops may trust each other.
2. **Egress: strip baggage on calls to external third parties** so `user.name`
   does not leave the product boundary.
3. **Never route `user.name` into a metric dimension.** High cardinality on
   traces/logs is fine and expected; on metrics it is a cardinality explosion
   (one time series per distinct username). This is the dangerous failure mode,
   and it is *not* prevented by the whitelist — it must be prevented by keeping
   the attribute off `internal/metric`.
4. **Backend access control + retention** on Tempo/Loki, as for any PII.

---

## API change (sketch — settle in the implementing PRs)

```go
// Phase 1 — additive, no internal change.
func SetUser(ctx context.Context, name string) {
    trace.SpanFromContext(ctx).SetAttributes(/* semconv user.name */ name)
}
func UserName(name string) slog.Attr { return slog.String(/* user.name */, name) }

// Phase 2 — opt-in carrier + whitelist; enrichment wired inside Init.
func ContextWithUser(ctx context.Context, name string) context.Context { /* baggage */ }

// Enabled via an Init option; OFF by default.
func WithUserBaggage() Option { /* registers handler enrich + SpanProcessor */ }
```

Both phases are **non-breaking, additive**. Phase 2 changes the *internal* log
handler and trace provider construction but only activates when the option is set;
an SDK consumer that does nothing sees no behavior change.

---

## Consequences

**Positive**

- Phase 1 lands immediately: cheap, safe, visible, no PII on the wire, no blast
  radius. Useful on its own for single-service attribution.
- Phase 2 delivers the actual product requirement — identify once at the source,
  appears on every downstream service's spans and logs with no extra code —
  reusing the NATS/HTTP propagation that already exists.
- One shared `user.name` key means Phase 1 helpers and Phase 2 enrichment
  coexist; a service can mix them.

**Negative / Trade-offs**

- Phase 2 touches the log hot path and span start; a bug there affects all
  telemetry of an enabled service. Mitigated by the feature being opt-in and the
  whitelist being a tiny fixed list.
- Phase 2 puts PII in HTTP/NATS headers across every hop — spoofable at untrusted
  ingress, leakable at egress. Mitigated by the mandatory guardrails, which are
  partly operational (not enforced by code).
- `user.name`-on-span coverage is subject to head sampling (ADR 0015); on heavily
  sampled hot paths the span attribute is mostly absent. Logs and baggage are
  unaffected, so cross-signal lookup still works via logs.
- Phase 2 is "magic": a reader cannot see at the call site why `user.name` is
  present. Mitigated by documentation and by Phase 1 remaining available for
  explicit cases.
- For Phase 2 coverage to be uniform, **every** service must enable the feature;
  baggage still forwards through a service that has not enabled it, but that
  service's own spans/logs will not show the field.

---

## Decision analysis

### Q1 — Explicit helpers vs Baggage auto-propagation

| Dimension | Phase 1: explicit helpers | Phase 2: Baggage auto-propagation |
|---|---|---|
| SDK change | two small pure funcs, no `internal/` change | modify log handler hot path + add SpanProcessor + provider wiring + setter |
| Developer usage | call per span and per log | set once at source; plain `tracer.Start` / `obs.Logger` thereafter |
| Cross-service | does **not** propagate; each service re-identifies | propagates across the whole mesh (the stated goal) |
| Coverage | only the exact call sites | every span/log after the set point, in this and downstream services |
| Blast radius | minimal, purely additive | large — affects all telemetry of an enabled service |
| PII exposure | stays in-process | rides HTTP/NATS headers; spoofing + egress concerns |
| Maintenance | near-zero, easy to test | SpanProcessor lifecycle, whitelist governance, ingress/egress policy, third-party-lib dependence |
| Visibility | high — call site is explicit | low — implicit/magic |

- **Chosen: both, phased.** Phase 1 alone cannot meet the cross-service "identify
  once" requirement; Phase 2 alone forces PII-on-wire and a larger surface on
  everyone. Phasing delivers the safe tool now and the propagation capability
  deliberately, behind an opt-in.

### Q2 — Phase 2 default: ON vs OFF

| | Default ON | **Default OFF, explicit opt-in (chosen)** |
|---|---|---|
| PII on the wire | forced on every SDK consumer | deliberate, per service |
| Convenience | maximal | one option call per service |
| Surprise factor | high (headers carry PII unexpectedly) | low |
| Fit for a shared SDK | poor | good |

- **Chosen: default OFF.** Putting PII into propagated headers must be an explicit
  choice. Within an enabling service the enrichment is still fully automatic, so
  the "set once, no downstream code" ergonomics are preserved where wanted.

### Q3 — Where `user.name` may land

- **Traces and logs: yes.** This is their purpose; high cardinality is expected.
- **Metrics: forbidden.** A per-user metric dimension is a cardinality explosion.
  The whitelist bounds *baggage*, not metric labels, so this must be enforced by
  keeping `user.name` out of `internal/metric` instrumentation.

---

## Implementation specifics (settle in the implementing PRs)

1. **Phase 1** — add `SetUser` / `UserName`; source the `user.name` key from the
   pinned semconv constant if `semconv/v1.39.0` exposes one, else record a
   documented string-key deviation in `docs/semconv.md` (ADR 0006). Update the
   semconv catalog in the same commit. Unit-test both helpers.
2. **Phase 2** — `ContextWithUser` + `WithUserBaggage()` option; extend
   `OtelSlogHandler.Handle` and add the baggage SpanProcessor wired in
   `internal/trace/trace.go`; the processor and handler share one whitelist
   constant. No global-state mutation (ADR 0003): the processor goes into the
   SDK-built provider, not via `otel.SetTracerProvider`.
3. **Whitelist** — start with `["user.name"]`; expanding it is a conscious change
   (header-size and PII review), not an open door.
4. **Docs** — README section covering the helper-vs-baggage choice, the ingress/
   egress/metrics guardrails, the sampling interaction, and a worked
   publisher→JetStream→subscriber example showing `user.name` appearing on a
   downstream service with no downstream code.
5. **CHANGELOG** — `[Unreleased]`: Phase 1 helpers; Phase 2 option (when shipped).
