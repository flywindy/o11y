# ADR 0004 — NATS Integration

**Status**: Accepted (backfill; implementation already shipped)
**Date**: 2026-04-22

---

## Context

The SDK provides a tracing-aware NATS client at `github.com/flywindy/o11y/nats`.
Implementation shipped prior to this document; this ADR backfills the
rationale, confirms compliance with ADR 0003 (Global State Policy), and
establishes the audit discipline for future upstream bumps.

Relevant files:

- `nats/conn.go` — public API (`Connect`, `Subscribe`, `QueueSubscribe`,
  `JetStream`)
- `nats/middleware.go` — auxiliary helpers
- Upstream: `github.com/Marz32onE/instrumentation-go/otel-nats` (internal
  company library, covers NATS Core + JetStream with OTel semconv v1.39.0)

---

## Decisions

### 1. Library choice: `Marz32onE/instrumentation-go/otel-nats`

Selected over alternatives because it:

- Covers both NATS Core and all JetStream consumer patterns (push,
  pull-with-`Consume`, pull-with-`Fetch`) in a single library.
- Aligns attribute names with OTel semconv v1.39.0
  (`messaging.system`, `messaging.destination.name`,
  `messaging.operation.name`, `messaging.operation.type`, ...).
- Is internally owned, so semconv upgrades and bug fixes are within
  reach.

Rejected alternatives at the time:

- Hand-rolled `PublishMsg` / `SubscribeMsg` header injection — duplicates
  upstream work; every JetStream consumer pattern (especially pull +
  `Consume`) needs its own span-link handling.
- `go.opentelemetry.io/contrib/instrumentation/github.com/nats-io/...`
  (community contrib) — at evaluation time did not cover JetStream
  consumer span-link semantics and lagged on semconv version.

### 2. Wrapper location: `nats/` under module root

Mirrors the shape of future `mongo/` and `http/` wrappers. One package
per external system keeps import paths short and discoverable.

### 3. Public API

```go
func Connect(
    ctx context.Context,
    url string,
    tp trace.TracerProvider,
    prop propagation.TextMapPropagator,
    natsOpts ...natsgo.Option,
) (*Conn, error)
```

`Conn` embeds `*otelnats.Conn` so all publish / request / drain / close
methods are available directly. `Subscribe` and `QueueSubscribe` are
overridden to expose a simplified `MsgHandler func(ctx, *nats.Msg)`
signature, keeping handler call sites close to stdlib `nats.go`
ergonomics while still providing a ctx with the consumer span.

### 4. JetStream

`Conn.JetStream()` returns `oteljetstream.JetStream` via
`oteljetstream.New(c.Conn)`. The underlying `otelnats.Conn.TraceContext`
path carries the `tp`/`prop` supplied to `Connect`; no additional
configuration is required at the JetStream level.

### 5. `msg.Respond` caveat documented

Replying from inside a `Subscribe` handler with `msg.Respond(data)`
bypasses the tracing wrapper's header injection, because `msg.Respond`
routes through the raw NATS connection. To preserve trace context in
the reply, handlers must use `conn.Publish(ctx, msg.Reply, data)`. This
is called out in the `MsgHandler` godoc, in `AGENTS.md`, and in
`README.md`.

### 6. Context-canceled fast path in `Connect`

`Connect` checks `ctx.Err()` before dialing. The underlying NATS client
does not support context cancellation during an in-progress dial, but a
pre-dial check prevents leaking work when the caller has already
canceled.

---

## Global-state verification

### Library: `github.com/Marz32onE/instrumentation-go/otel-nats`
### Version: `v0.2.11` (per `go.mod`)
### Result: ✅ SAFE — does not set globals

**Verification method.** Source inspection of
`otel-nats/otelnats/conn.go`. Relevant pattern:

```go
// newConn (conceptual):
if cfg.TracerProvider == nil { cfg.TracerProvider = otel.GetTracerProvider() }
if cfg.Propagators    == nil { cfg.Propagators    = otel.GetTextMapPropagator() }
```

The upstream library reads the OTel globals **only as a fallback** when
no option is supplied. It does not call `otel.SetTracerProvider` or
`otel.SetTextMapPropagator`.

**Why the current wrapper is already compliant with ADR 0003.**
`nats.Connect` (`nats/conn.go:48`) always passes both options:

```go
nc, err := otelnats.ConnectWithOptions(url, natsOpts,
    otelnats.WithTracerProvider(tp),
    otelnats.WithPropagators(prop),
)
```

The fallback branch is never executed in practice, and even if it were,
it would only *read* globals, never set them.

**No refactor required.** Adoption of ADR 0003 does not change
`nats/conn.go` or any of its tests.

## Semconv Alignment

`otel-nats` v0.2.11 imports `go.opentelemetry.io/otel/semconv/v1.39.0`,
matching the SDK pin after the semconv upgrade. The emitted messaging
attributes use the v1.39 names documented in `docs/semconv.md`, including
`messaging.operation.name` and `messaging.operation.type`.

---

## Audit discipline for upstream bumps

Whenever `otel-nats` is upgraded (any version change in `go.mod`):

1. Re-run the inspection: search the upstream module for
   `otel.SetTracerProvider` and `otel.SetTextMapPropagator`.
2. If any match is introduced in a code path reachable from
   `ConnectWithOptions`, the upgrade is blocked until the wrapper is
   refactored or the upstream is forked.
3. Update the "Global-state verification" section above with the new
   version number.
4. Update the approved-integrations table in ADR 0003.
5. Diff the emitted **span names** against the table in `docs/semconv.md` and
   run `nats/conn_test.go`'s `TestSpanNames_*`. Names are entirely
   upstream-owned here (no span-name formatter is exposed), so a rename is
   both invisible to compilation and breaking for every dashboard keyed on
   them; v0.9.0 renamed all of them and this step is what would have caught
   it (added 2026-08-12).

---

## Consequences

**Positive**

- Single-line trace propagation over NATS Core and JetStream with no
  globals mutated.
- JetStream consumer spans link correctly to publisher spans in Grafana
  Tempo via upstream-provided span-link semantics.
- Subscribe handlers receive a ctx already carrying the consumer span,
  so `slog.InfoContext(ctx, ...)` and `tracer.Start(ctx, ...)` "just
  work" inside handlers.

**Negative / Trade-offs**

- Dependency on an internally owned library. Upstream changes must pass
  the ADR 0003 verification on every bump.
- `msg.Respond` is a known footgun; it cannot be closed without
  forking `nats.go` itself. Documentation and code review are the only
  mitigations.
- Handlers cannot directly access the upstream `otelnats.Msg`
  (which carries additional ctx metadata) because the wrapper flattens
  it to `(ctx, *nats.Msg)`. If future use cases need the richer type,
  expose a second handler signature rather than breaking the existing
  one.

---

## Amendment (2026-05-25) — Metrics scope: deferred

The original ADR shipped a **trace-only** facade. This amendment records the
deliberate decision to *not* add client-side NATS/JetStream metrics at this
time, after the same library/value re-survey we ran for Redis (ADR 0013) and
MongoDB (ADR 0014). `nats.Connect` stays `(tp, prop)` with no `MeterProvider`.

### 1. Library re-survey (ADR 0008 §6)

No trustworthy library exists for NATS client metrics — a weaker position than
either Redis or MongoDB:

- **No official OTel-contrib NATS instrumentation exists** (NATS has no
  contrib auto-instrumentation package as of this date).
- The **community contrib was already rejected** at original adoption (§1
  above: missing JetStream consumer span-link semantics, lagging semconv).
- The upstream `Marz32onE/.../otel-nats` v0.2.11 is **tracing-only**: source
  inspection shows no `Meter` / `metric.` usage and no `WithMeterProvider`
  option.
- The `nats.go` client exposes **no per-message hook/monitor** (unlike
  go-redis hooks or the mongo driver's `CommandMonitor`/`PoolMonitor`), so any
  client metric must be hand-computed at our wrapper seam. NATS would therefore
  be the **most self-written (full T3)** of the three integrations.

### 2. Semconv maturity

NATS metrics fall under the **messaging** semconv, which is still wholesale
**Development** (the spec explicitly advises instrumentations not to change the
emitted convention version until messaging is marked stable, gated by
`OTEL_SEMCONV_STABILITY_OPT_IN`). This is less mature than the database metrics
adopted for Redis/MongoDB, raising churn risk for anything emitted now.

### 3. Value analysis (why even a minimal layer is not worth it)

A minimal client-side layer would use the connection snapshot
`nats.Conn.Stats()` (`InMsgs`, `OutMsgs`, `InBytes`, `OutBytes`, `Reconnects`).
Assessed against the server-side `prometheus-nats-exporter` + OTel Collector
path:

| `nc.Stats()` field | Server exporter (`/varz`, `/connz`, `/jsz`) | Unique to client? |
|---|---|---|
| InMsgs / OutMsgs / InBytes / OutBytes | yes (aggregate + per-conn via connz) | only the convenience of auto-attached SDK Resource labels (`service.name`, pod, …) |
| `Reconnects` | server sees churn as new connections, hard to attribute to a logical service | **yes** — the only genuinely client-only signal |

Crucially, the **high-value JetStream operational signals are NOT in
`nc.Stats()`**: consumer backlog/lag (`ConsumerInfo.NumPending`,
`NumAckPending`, `NumRedelivered`, `NumWaiting`) require polling
`consumer.Info()` (a larger T3) **and** are already exposed server-side via
`/jsz`. So the minimal layer's unique value is thin (client-attributed
throughput + perceived reconnects) while it omits the metric operators
actually want, risking a false impression of "NATS metrics coverage."

### 4. Decision

- `nats/` remains **trace-only** (T2 facade); no `MeterProvider`, no SDK-owned
  NATS metrics.
- Operational NATS/JetStream metrics (throughput, connections, slow consumers,
  JetStream storage, consumer lag) are obtained from **`prometheus-nats-exporter`
  scraped by the OpenTelemetry Collector** — infrastructure configuration, zero
  SDK code, and complementary to the SDK's client-side trace propagation.

### 5. Revisit triggers

Re-open this decision when **any** of the following holds:

- the messaging metrics semconv is marked **stable**; or
- a maintained library (official contrib or our corporate fork) starts emitting
  NATS client metrics and passes the ADR 0008 §2 checklist; or
- a concrete need arises for **client-attributed JetStream consumer-lag**
  metrics that the server-side exporter cannot satisfy (e.g. per-service-instance
  lag correlated with app metrics in the same backend).

At that point evaluate a justified-T3 metrics layer (consumer-lag via
`consumer.Info()` polling being the highest-value target) in its own ADR.

---

## Amendment (2026-07-09): upstream v0.6.0 upgrade and module-path cutover

The upstream module renamed its path from
`github.com/Marz32onE/instrumentation-go/otel-nats` to
`github.com/akira-core/instrumentation-go/otel-nats` in v0.6.0 (repo
transferred to the `akira-core` org; the go.mod cutover landed in the same
release). The SDK now pins **v0.6.0** under the new path.

Re-audit summary (same discipline as the original v0.2.11 audit; v0.5.x was
skipped after a review found regressions — see issues #69–#73):

- **Fixed upstream in v0.6.0**: semconv restored to v1.39.0 (the v0.5.x line
  had regressed to v1.37.0); `Consumer.Next` returns the local receive-span
  context; `ConsumeContext` mirrors the full native surface (Stop/Drain/
  Closed); the v0.5.1 propagation env gate was removed (inject/extract follow
  the tracing gate unconditionally); `MessageBatch` gained a `Stop()` escape
  hatch; `Version()` is guarded by a test; **the requester-side reply-receive
  span is now emitted upstream** (`recordReply`), replacing the o11y-owned
  reply-link span from the ADR 0022 2026-07-01 amendment.
- **Still present, tracked upstream**: tracing is gated by two process-wide
  env vars (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` +
  `OTEL_NATS_TRACING_ENABLED`, default off) latched by a `sync.Once` at first
  Connect, with no exported reset (v0.5.1's `ResetGatesForTest` was removed —
  downstream test suites must set the env in `TestMain`); the deliver-span
  path still builds its own TracerProvider/exporter from
  `OTEL_EXPORTER_OTLP_ENDPOINT` with no sampler; `messaging.consumer.name`
  remains a non-semconv literal; the upstream `HeaderCarrier` remains
  exact-case with no canonical fallback (the ADR 0022 2026-07-03 documented
  limitation stands).
- **Facade changes**: `Conn.Request` drops its variadic `attrs` parameter and
  shims ctx+timeout over upstream `RequestWithContext` (the upstream primary
  `Request` is ctx-less); `linkReply`/`replyAttrs` deleted; `Consumer.Next`
  wrapped; `ConsumeContext` widened; `MessageBatch.Stop` added.

## Amendment (2026-07-16): upstream v0.7.0 upgrade

The SDK now pins **v0.7.0**. This release cleared almost the entire upstream
backlog tracked in `docs/upstream-otel-nats.md`; the re-audit found no new
global-state or provider-routing concerns.

- **Fixed upstream in v0.7.0** (retiring the "still present" list above):
  - `WithTracingEnabled(v bool) Option` added — a per-`Conn` override of the
    env-gate default, in either direction. `o11ynats.Connect` defaults to
    `WithTracingEnabled(true)`, so NATS tracing follows the SDK's own toggle
    instead of requiring two process-wide env vars. With a real TracerProvider
    this emits spans and propagates context; with the noop provider (SDK tracing
    disabled) NATS spans are non-recording and carry no span context, so no
    active trace is propagated while the pillar is off (upstream starts each
    publish/consume span from a fresh context, so a noop tracer yields nothing
    to inject) — cross-service correlation is inactive, not broken, and resumes
    when the trace pillar is enabled. Callers with a hard native-cost
    disabled-observability mode can now use `o11ynats.ConnectWithOptions` with
    `o11ynats.WithTracingEnabled(sdk.Toggles.Trace)` to follow the SDK's
    resolved trace toggle and select the upstream direct path when tracing is
    off.
    Tests no longer need a `TestMain` env-var setup.
  - **Deliver spans removed entirely** — the implicit `OTEL_EXPORTER_OTLP_ENDPOINT`-
    gated second TracerProvider/exporter (no sampler) is gone, closing the
    sampling-inconsistency concern (issue #70) by deletion. The corresponding
    caveat in ADR 0003's approved-integrations row is dropped.
  - `messaging.consumer.name` → `messaging.consumer.group.name` (the semconv
    v1.39.0 key), resolving the last open part of #69.
  - `HeaderCarrier` gained `propagation.ValuesGetter` plus MIME-canonical and
    case-folded read fallbacks, so the ADR 0022 2026-07-03 canonical-header
    limitation is resolved; the `nats/jetstream.go` known-limitation block is
    deleted.
  - `Consumer.Next` honors live ctx cancellation (`jetstream.FetchContext`);
    the request/reply send span no longer overwrites `body.size` with the reply
    size and now carries `conversation_id`; `MessageBatch.Stop` releases the
    upstream goroutine even while it is parked receiving.
- **Behavioral changes to note (BREAKING for observers)**: span kinds
  corrected — reply-receive and the JetStream pull-**receive** spans (`Next`/
  `Messages`/`Fetch`/`FetchBytes`/`FetchNoWait`) are now `CLIENT` (were
  `CONSUMER`); the `Consume` callback and core Subscribe `process` spans stay
  `CONSUMER` (unchanged); batch/`Messages` receive spans end at handover (shorter
  durations); `Consumer.Next` with a cancelable ctx can no longer be combined
  with a caller `FetchMaxWait` (returns `jetstream.ErrInvalidOption`). See the
  o11y CHANGELOG's v0.7.0 entry for migration notes.
- **Facade changes**: `Connect` defaults to `WithTracingEnabled(true)`, and
  `ConnectWithOptions` / `WithTracingEnabled(v)` expose the caller-controlled
  direct path for disabled-observability modes; `Conn.Request` keeps its
  ctx+timeout shim (upstream `Request` is still ctx-less, tracked as R2/#72).
  Docs and test assertions updated for the corrected span kinds and
  span-lifecycle behavior.

## Amendment (2026-08-10): otel-nats v0.8.0 dynamic feature flags

The SDK now pins **v0.8.0** and accepts the upstream dynamic-control model.
Existing Go call sites remain source-compatible, but `WithTracingEnabled` now
supplies a connection-local default rather than a hard override. Effective
NATS tracing resolves as `relay > OTEL_NATS_TRACING_ENABLED > option > false`,
then the process-wide master switch is applied. The relay is authoritative in
both directions and is re-evaluated for every operation.

Consequences for the facade and adopting applications:

- With no relay and no overriding upstream environment value,
  `WithTracingEnabled(false)` still constructs only the direct implementation:
  no spans, no trace propagation, and no per-operation feature-flag evaluation.
- Once a relay can exist, the connection must retain both implementations so a
  later remote enable can take effect. The disabled state therefore still pays
  the relay evaluation cost; enabling a relay is a separate capacity-reviewed
  rollout, not part of this dependency bump.
- `OTEL_NATS_TRACING_ENABLED` now outranks the o11y option. Applications passing
  `sdk.Toggles.Trace` are expressing a default, not an unbreakable native-cost
  ceiling. A deployment requiring that ceiling must leave the relay unconfigured
  and avoid an overriding upstream environment value.
- Upstream flag values are strict. Only `1`/`true`/`yes`/`on` and
  `0`/`false`/`no`/`off` are accepted; malformed values fail NATS wrapper
  construction instead of being ignored.
- A relay provider installed by the application must exist before any NATS
  wrapper is constructed. The zero-code endpoint path may instead install a
  named provider in the process-global OpenFeature registry and start a poller
  for which o11y has no shutdown handle. o11y does not set the endpoint and
  continues to pass its own OTel provider and propagator explicitly, so no OTel
  global is read or mutated on the default path. Applications opting into the
  relay own its lifecycle implications and the documented startup window in
  which local values win until the first fetch.
- The module now brings `otel-flags`, the OpenFeature SDK, and the GO Feature
  Flag provider dependency graph into builds. The default no-relay hot path is
  still upstream's zero-evaluation path, but binary/SBOM and supply-chain
  review surfaces increase.

The upgrade audit re-read the v0.8.0 constructors, gate resolution, direct and
traced implementations, JetStream inheritance, module file, and upstream
CHANGELOG. No new call to `otel.SetTracerProvider` or
`otel.SetTextMapPropagator` is reachable when the facade supplies its explicit
providers. Regression tests in `nats/conn_test.go` pin environment-over-option,
master-veto, malformed-value, and no-relay direct-path behavior.

## Amendment (2026-08-12): otel-nats v0.9.1 span-name conformance

The SDK now pins **v0.9.1**. This is a narrower bump than the one above: the
dynamic-control model, the flag precedence, the dependency graph and every
consequence listed in the 2026-08-10 amendment are unchanged — the upstream
module's own `go.mod` is identical between v0.8.0 and v0.9.1, `oteljetstream`'s
exported surface is byte-identical, and `otelnats` gains one additive method
(`Conn.InboxPrefixes`). No facade code changed.

What changed is span **names**, which this integration has always delegated
entirely to upstream (otel-nats exposes no span-name formatter, and ADR 0023's
naming convention is explicitly data-store-only). v0.9.0 moved them to the
semconv v1.39.0 messaging shape `{messaging.operation.name} {destination}` and
omits `{destination}` where no low-cardinality value exists — most visibly, a
request/reply inbox is no longer in any span name. Three destination attributes
were added (`messaging.destination.template`, `.temporary`, `.anonymous`) so the
inbox stays queryable, and `messaging.message.conversation_id` now appears on
every inbox-destination span, including JetStream ones whose subject is an inbox.

Consequences for this integration:

- **Dashboard-breaking, code-compatible.** Nothing in `nats/` needed a change;
  four test assertions and every doc that quoted a span name did. The migration
  table is in the CHANGELOG.
- **The audit gap this exposed is now closed.** The integration had no written
  span-name baseline anywhere — not in this ADR, not in `docs/semconv.md` — so an
  upstream rename was undetectable by review. `docs/semconv.md` now carries a
  span-name table and `nats/conn_test.go` pins the shapes, and "Audit discipline
  for upstream bumps" above gains a step 5 requiring the diff — alongside the
  existing global-state and semconv-version checks.
- **Residual cardinality stays a deployment concern.** Subjects that embed
  identifiers, on paths with no subscription or consumer filter to resolve
  against, keep the concrete subject in the span name; upstream will not infer a
  `messaging.destination.template` and neither will the SDK. This is the same
  division of labour §5's cardinality guidance already assumes: the SDK
  documents the risk, the deployment bounds it (collector `span` processor).

Metrics scope is untouched by this release — the module is still trace-only, so
the deferral in §5 and its revisit triggers stand. See
`docs/upstream-otel-nats.md` for the v0.9.1 surface audit and the re-checked
upstream backlog, and ADR 0022's 2026-08-12 amendment for the request/reply
span-topology wording this rename affected.
