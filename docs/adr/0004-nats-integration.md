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
