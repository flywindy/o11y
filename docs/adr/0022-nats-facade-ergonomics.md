# ADR 0022 — NATS Facade Ergonomics for Consumer Integration

**Status**: Proposed
**Date**: 2026-06-06

---

## Context

The SDK's first real NATS/JetStream consumer is the public
`github.com/hmchangw/chat` project (a distributed multi-site chat system
built on `nats.go` v1.50.0, using both NATS core request/reply and the new
`nats.go/jetstream` API). chat is being migrated to adopt the o11y SDK
(alongside the Cassandra / Elasticsearch evaluations in ADR 0019 / 0020).

The concrete integration goal is: **chat replaces its direct imports of
`github.com/Marz32onE/instrumentation-go/otel-nats` with imports of
`github.com/flywindy/o11y/nats`**, so that the SDK is the single seam for
NATS observability. Before that migration starts, this ADR maps what chat
uses that the current `nats/` facade does **not** cover, and decides how to
close the gaps.

### What chat actually has today

chat does not use the o11y SDK yet. It carries its own parallel stack, all
layered over the **same** Marz `otel-nats` library the SDK wraps:

- `pkg/otelutil/otel.go` — sets up OTel from scratch and **calls
  `otel.SetTracerProvider` / `otel.SetTextMapPropagator`** (process globals).
- `pkg/natsutil` — `Connect` over `otelnats.ConnectWithOptions` /
  `…WithCredentials…`; `carrier.go` (a `propagation.TextMapCarrier` over
  `nats.Header`); `reply.go` (`ReplyJSON` → raw `msg.Respond`).
- `pkg/natsrouter` — a gin-style **request/reply router** for NATS core:
  subject patterns with `{param}` placeholders → NATS `*` wildcards, typed
  generic handlers (`Register[Req,Resp]`, `RegisterNoBody`, `RegisterVoid`),
  middleware chain (`Recovery / RequestID / Logging / HandlerTimeout`),
  `QueueSubscribe` dispatch, per-message goroutine with optional
  `WithMaxConcurrency` backpressure (503 `ErrUnavailable`), graceful
  `Shutdown`. Built on `*otelnats.Conn`; replies via `msg.Respond`.
- `pkg/stream` — JetStream **config defaults only** (`ConsumerSettings`,
  `DurableConsumerDefaults() jetstream.ConsumerConfig`). The actual
  JetStream operations (`CreateOrUpdateStream`, `Stream`, `Consume`, and the
  `oteljetstream.Msg` handler) live **directly in each worker**
  (e.g. `message-worker/bootstrap.go` imports `oteljetstream`).

### Coverage gap against the current facade

| chat usage | Marz surface touched | o11y facade today | Gap |
|---|---|---|---|
| connect | `otelnats.ConnectWithOptions` / `…WithCredentials…` | `nats.Connect(ctx,url,tp,prop,opts…)` | covered — creds pass through as `nats.UserCredentials(...)` option |
| router `QueueSubscribe` | `*otelnats.Conn`, `otelnats.Msg` | `QueueSubscribe(ctx,subj,queue, func(ctx,*nats.Msg))` | covered — flattened handler carries ctx + subject + header + reply + data |
| client request | `Conn.Request(ctx,…)` | available via embedding | covered |
| **router reply** | raw `msg.Respond(...)` | none | **HOLE ①** — no traced reply primitive |
| **JetStream** | `oteljetstream.{JetStream,Consumer,Stream,Msg}` | `conn.JetStream()` returns the **Marz type** (`nats/conn.go:128`) | **HOLE ②** — facade is pass-through; callers still import Marz |

The asymmetry: the **core / request-reply** path is ~90% covered (one hole);
the **JetStream** path is essentially un-wrapped. The Marz coupling for core
is *concentrated* (`pkg/natsutil` + `pkg/natsrouter`); for JetStream it is
*spread* across workers (each names `oteljetstream.Msg` in its handler). The
SDK's own examples prove the pass-through — `examples/jetstream/subscriber/main.go:15`
imports `oteljetstream` directly.

Relevant prior decisions: ADR 0003 (no globals), ADR 0004 (NATS integration,
including the `msg.Respond` footgun §5), ADR 0008 (T1/T2/T3 sourcing policy).

---

## Decision driver — keep Marz (`otel-nats`)

PR #43 proposes dropping the Marz wrapper for **MongoDB** in favor of the
official contrib `otelmongo` CommandMonitor. That reversal is driven by
*premise collapse*: ADR 0005 chose Marz **solely** for document-level trace
propagation, now declined as an anti-pattern, and an official contrib
covers everything else.

**That reasoning does not transfer to NATS**, for two reasons:

1. **No official OTel-contrib NATS instrumentation exists** (confirmed
   current as of this date; the community contrib was already rejected in
   ADR 0004 §1 for missing JetStream consumer span-link semantics and
   lagging semconv). The realistic menu is *keep Marz* or *self-write T3* —
   there is no "switch to official contrib" option as there is for MongoDB.
2. **Marz's NATS mechanism is the blessed pattern, not the anti-pattern.**
   For MongoDB, Marz writes `traceparent` into the business document — the
   thing PR #43 objects to. For NATS, Marz writes `traceparent` into
   **message headers** — exactly what the OTel messaging conventions
   recommend (context on the envelope/headers + span links). The premise
   that collapsed for MongoDB does not exist for NATS.

Applied honestly, the PR #43 lens *strengthens* keeping Marz for NATS. This
ADR therefore reaffirms ADR 0004's library choice and changes nothing about
the propagation layer. All work below is **ergonomics on top of Marz**, not
re-instrumentation.

---

## Decisions

### 1. Add a traced reply primitive (`Respond`) — closes HOLE ①

Add to `nats/`:

```go
// Respond replies to msg over the traced publish path, preserving trace
// context end to end. It returns an error if msg is nil or if msg.Reply is
// empty, matching the up-front validation the facade already does in
// Subscribe / QueueSubscribe.
// Use this instead of msg.Respond, which routes through the raw NATS
// connection and drops trace context (ADR 0004 §5).
func (c *Conn) Respond(ctx context.Context, msg *natsgo.Msg, data []byte) error
```

**Mechanism — route through the traced publish path, not `RespondMsg`.**
`Respond` must be specified as *validation* (`msg != nil`, non-empty
`msg.Reply`) followed by the **existing traced publish**, i.e.
`c.Publish(ctx, msg.Reply, data)` (or an exactly equivalent helper) — which
is precisely the workaround ADR 0004 §5 already recommends.

It must **not** be `Inject` + raw `msg.RespondMsg`: `RespondMsg` bottoms out
in raw `nats.Conn.PublishMsg`, so although headers would be carried, that
path bypasses the o11y/Marz traced publish — **no producer send span, no
`Nats-Trace-Dest`, no `TracingEnabled()` gate, no span error recording**.
Going through `Publish` reuses the upstream's producer instrumentation
(injection included) instead of re-implementing a lesser version of it.

This stays a small T2 ergonomic helper (ADR 0008 §T2: "framework-specific
signal the upstream lib doesn't expose"): no Marz API is forked and it is not
self-written instrumentation. It structurally fixes the `msg.Respond` footgun
that ADR 0004 §5 only documented — and that chat's `natsutil.ReplyJSON` /
`natsrouter` currently trip (replies carry no trace context today).

**Caller contract.** `Respond` returns its error rather than swallowing it: a
failed reply leaves the requester blocked until its own timeout, which reads
as "the responder is slow" and is painful to diagnose. Callers (the router
included) must log and count reply failures, not discard them.

**Header propagation ≠ requester-side span linkage.** `Respond` guarantees the
reply *carries* `traceparent` (the responder's context on the reply headers).
It does **not**, by itself, create a requester-side receive span that links
back to the responder: `otelnats.Conn.Request` waits on
`RequestMsgWithContext` and returns the reply without extracting reply headers
or starting a receive span. Closing that link would require extending
`Request` (or a `RequestTraced` helper) and is **out of Phase 1 scope** —
tracked as an open question in Follow-up.

### 2. The request/reply router stays in chat — NOT in o11y (HOLE ① option a)

o11y ships the `Respond` *primitive*; it does **not** absorb the router.
chat's `pkg/natsrouter` is rewired onto `*o11ynats.Conn` (its
`QueueSubscribe` handler already carries everything the dispatcher needs) and
calls the new traced `Respond` internally.

Rationale:

- **Sample size of one.** Standardizing chat's specific routing opinions —
  `{param}` subject syntax, generics `Register*`, **`pkg/errcode` envelope
  coupling**, middleware ordering — into the SDK before a second consumer
  exists is the *least* flexible move. We do not yet know the right
  cross-service abstraction.
- **Scope.** A router is an ergonomics/framework layer, not instrumentation;
  it has no slot in the ADR 0008 T1/T2/T3 model and would force a new
  category decision for a layer we are not confident about.
- **Evolvable.** Shipping the primitive now keeps "promote the router into
  o11y later" open; baking the router in now is hard to reverse. Revisit if
  and when a second consumer validates the pattern.

### 3. JetStream typed wrapper in `nats/` — closes HOLE ② (Option 2)

`conn.JetStream()` today returns the Marz `oteljetstream.JetStream`
interface, so every caller still imports Marz. Instead, o11y provides its
**own** JetStream types in the `nats/` package (same package as the core
facade — short import path, consistent with `Connect` / `MsgHandler`):

- For the **`Consume`** mode, a handler shaped as
  **`func(ctx context.Context, msg jetstream.Msg)`**, mirroring the core
  `MsgHandler`: the wrapper flattens `oteljetstream.Msg` → `(ctx, jetstream.Msg)`
  per delivery. The handler receives the **native `jetstream.Msg`** (no
  o11y-owned `Msg` type), so `Ack` / `Nak` / `Term` / `InProgress` /
  `Metadata` come for free and the only thing the wrapper adds is the
  trace-carrying `ctx`. (`Messages` and `Fetch` are **not** handler-based —
  they keep their native iterator/batch shapes; see "Surface to wrap".)
- **Configuration types pass through `nats.go/jetstream`** (`StreamConfig`,
  `ConsumerConfig`, `AckExplicitPolicy`, `PubAck`, …). These are already
  aliases in `oteljetstream`, so sourcing them from `nats.go/jetstream` is
  **not a Marz dependency** — only the behavioural interfaces and `Msg` need
  wrapping.

**Core rationale is decoupling, not just coverage.** A bare alias
re-export (`type JetStream = oteljetstream.JetStream`) would be cheaper but
would leak Marz's exact interface shape into o11y's public API; an upstream
swap (ADR 0008 §6 annual review) would then break every caller. A typed
wrapper separates "o11y's public type" from "the backing library", so a
future backing-library change is contained inside `nats/`. This is the
flexibility the integration is meant to buy.

Tier: **T2**, unchanged. `nats/doc.go` keeps its `// Tier: T2` annotation.

#### Surface to wrap

- Stream/consumer management used by chat: `CreateOrUpdateStream`, `Stream`,
  `CreateOrUpdateConsumer`.
- **`Publish` with publish-option pass-through** —
  `Publish(ctx, subj, data, opts ...jetstream.PublishOpt)`. Required, not
  cosmetic: chat's `pkg/natsutil/canonical_dedup.go` relies on
  `jetstream.WithMsgID()` (the `Nats-Msg-Id` header + the stream duplicate
  window) for idempotent publishes. A wrapper that dropped the options would
  **silently disable JetStream dedup** — a reliability regression, not an
  ergonomics one.
- The three new-API pull consume modes, each wrapped in its **native shape** —
  do **not** collapse them all to a handler; only `Consume` is handler-based in
  `nats.go` v1.50.0:
  - **`Consume(handler, ...)`** — handler `(ctx, jetstream.Msg)`; must
    **return the `ConsumeContext`** (callers need `Stop()` for graceful
    shutdown) and **pass through `jetstream.PullConsumeOpt`**, including the
    **`ConsumeErrHandler`** — consumer-side failures (connection loss, pull
    errors, heartbeat misses) surface only through that handler, so a wrapper
    that swallowed it would turn a broken consumer into a silent stall.
  - **`Messages(...)`** — returns a traced iterator (`MessagesContext`-shaped)
    whose `Next()` yields `(ctx, jetstream.Msg)`; iterator semantics and
    `Stop()` preserved.
  - **`Fetch` / `FetchBytes` / `FetchNoWait`** — return a traced `MessageBatch`
    exposing the per-message trace `ctx`; the batch/channel contract preserved.

  This mirrors what Marz `oteljetstream` already does (iterator/batch shapes
  kept, `ctx` added), so existing iterator/batch call sites migrate without
  behavior changes.
- **Deferred:** `PushConsumer` and the ordered-consumer surface — wrap only
  when a consumer needs them, to keep the initial surface auditable.

### 4. Metrics stay out of scope (unchanged from ADR 0004)

Adding a JetStream type surface must not be read as adding NATS metrics. The
facade remains **trace-only** per the ADR 0004 amendment (2026-05-25):
operational signals — consumer lag (`NumPending` / `NumAckPending` /
`NumRedelivered`), throughput, JetStream storage — come from
`prometheus-nats-exporter` scraped by the OTel Collector, not from this
wrapper. This subsection exists to stop the wider type surface from creating a
false impression of metrics coverage (the same risk ADR 0004 amendment §3
flagged, replayed on the JetStream types).

Honest nuance for the record: the new `Consume` / `Msg` seam *would* lower the
cost of a future per-message consume-metric layer (ADR 0004 assumed no such
seam existed, since `nats.go` has no per-message hook). It does **not** change
today's decision — the high-value signal (consumer lag) is not a per-message
quantity, is already exposed server-side, and messaging-metrics semconv is
still Development. Any revisit stays governed by ADR 0004 amendment §5's
triggers.

---

## Global-state verification

No change to the ADR 0003 posture. `nats.Connect` still passes `tp`/`prop`
explicitly; the new `Respond` injects via the propagator carried on the
`Conn`, never `otel.GetTextMapPropagator()`; the JetStream wrapper inherits
`tp`/`prop` through `otelnats.Conn.TraceContext` exactly as today
(`nats/conn.go:128`). No new code path reaches `otel.SetTracerProvider` /
`otel.SetTextMapPropagator`.

**Integration side effect (positive).** When chat replaces `pkg/otelutil`
(which sets globals) with `o11y.Init` (which returns providers and sets
nothing), Marz's global *fallback* path is no longer relied upon, and chat's
divergence from ADR 0003 disappears. Any other chat code that reads
`otel.GetTracerProvider()` must then be threaded explicitly or have chat keep
setting globals in `main()` — chat's call, outside this ADR.

---

## Migration phasing

This ADR is docs-only. The implementing work is intended to land as two PRs:

1. **Phase 1 (concentrated, low risk).** Add traced `Respond`; chat rewires
   `pkg/natsutil` + `pkg/natsrouter` onto `*o11ynats.Conn`. The core /
   request-reply path becomes Marz-import-free; workers untouched.
2. **Phase 2 (spread, larger).** Add the JetStream typed wrapper; migrate
   workers off `oteljetstream` onto o11y. The per-worker handler-signature
   churn is unavoidable for *any* approach that removes `oteljetstream.Msg`
   from worker code; placing the wrapper in o11y makes its marginal cost over
   a chat-private adapter small while making it reusable by future consumers.

---

## Rollout & rollback

- **Mixed-version fleet is safe.** During a phased rollout some services run
  the old stack (Marz direct + globals) and some the new (o11y + explicit
  providers). Both inject the same W3C `traceparent` into NATS headers via the
  same propagator, so the **on-the-wire format is unchanged** and traces keep
  linking across the boundary — there is no flag-day requirement.
- **Rollback is a revert.** The change is import-level with no wire or schema
  change, so reverting the integration PR restores prior behavior with no data
  migration and no consumer reset.
- **Do not leave a service half-migrated.** A service still on the old stack
  relies on the OTel globals; the new stack does not read them. A service that
  has o11y wiring but still has code reading `otel.GetTracerProvider()` would
  get a noop provider for that code path (see Global-state section).

---

## Consequences

**Positive**

- chat can import only `github.com/flywindy/o11y/nats` for both core and
  JetStream; Marz becomes an implementation detail behind the facade.
- The `msg.Respond` footgun is closed by construction for anyone using the
  facade's `Respond` (and for the router, which calls it internally).
- o11y's public NATS type surface is decoupled from Marz's interface shape,
  containing any future backing-library change inside `nats/`.
- Core subscribe and JetStream consume share one `(ctx, msg)` handler shape.

**Negative / Trade-offs**

- o11y owns a larger NATS surface to maintain, version, and document
  (JetStream management + three consume modes). The handler delivers the
  native `jetstream.Msg`, so there is no o11y `Msg` type to maintain.
- The request/reply router remains duplicated per consumer until a second
  consumer justifies promoting it; cross-service envelope/middleware
  consistency is not centrally owned yet.
- Phase 2 requires touching every chat worker's consume path.

---

## Follow-up (implementing PRs — intentionally not done here)

- Phase 1 code: `Conn.Respond`; rewire chat `natsutil`/`natsrouter`.
- Phase 2 code: JetStream typed wrapper in `nats/`; migrate workers;
  update `examples/jetstream/*` to drop the `oteljetstream` import.
- `nats/doc.go` Tier annotation stays `T2` (re-confirm under ADR 0008 §7 CI
  gate).
- ADR 0003 / 0004 cross-references; CHANGELOG entry on implementation.
- Decide whether `PushConsumer` / ordered consumers are in or out of the
  first wrapper cut (the consume handler already settles on native
  `jetstream.Msg`, so no o11y `Msg` method surface needs deciding).
- Tests (ADR 0008 §"Accepted clarifications" — T2 facades test
  provider/propagator wiring): assert the reply **carries `traceparent`**
  (header propagation only — *not* requester-side span linkage, which is out
  of Phase 1 scope, see Decision 1); `Respond` errors on nil `msg` / empty
  `Reply`; the JetStream consumer span links to the publisher span; and
  `Publish` option pass-through preserves `WithMsgID` dedup.
- Open question: a requester-side receive span that links to the responder (so
  the requester's trace shows the reply as a linked span, not just propagated
  headers). Needs `Request` to extract reply headers and start a span;
  deferred beyond Phase 1.
- Open question: whether `Respond` needs a header-carrying variant
  (`RespondMsg(ctx, msg, *nats.Msg)`). Today `Publish(ctx, subj, data)` sets
  no custom reply headers; chat's `ReplyJSON` puts status in the JSON
  envelope, so the byte form suffices for now — but echoing `request_id` /
  content-type on the reply would need the variant. Decide at implementation.
- `docs/semconv.md` already catalogs the emitted NATS attributes (including
  `messaging.operation.type` and `messaging.operation.name`); verify and
  update that catalog **if** the JetStream wrapper changes the emitted
  attribute set, rather than treating it as new documentation.

---

## Review asks

1. Confirm `Proposed` status before any implementing PR.
2. The HOLE ① decision: ship the `Respond` primitive and keep the router in
   chat (option a), rather than absorbing the router into o11y.
3. The JetStream wrapper boundary: typed wrapper in `nats/` (Option 2) with
   config types passed through `nats.go/jetstream`; the handler delivering the
   native `jetstream.Msg`; `Publish` / `Consume` passing through their options
   (notably `WithMsgID` dedup and `ConsumeErrHandler`) and `Consume` returning
   the `ConsumeContext`; and the deferral of `PushConsumer` / ordered
   consumers.
