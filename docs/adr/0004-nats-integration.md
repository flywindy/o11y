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
  company library, covers NATS Core + JetStream with OTel semconv v1.27.0)

---

## Decisions

### 1. Library choice: `Marz32onE/instrumentation-go/otel-nats`

Selected over alternatives because it:

- Covers both NATS Core and all JetStream consumer patterns (push,
  pull-with-`Consume`, pull-with-`Fetch`) in a single library.
- Aligns attribute names with OTel semconv v1.27.0
  (`messaging.system`, `messaging.destination.name`,
  `messaging.operation`, …).
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
### Version: `v0.2.1` (per `go.mod`)
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
