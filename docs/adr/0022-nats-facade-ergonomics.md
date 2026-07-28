# ADR 0022 — NATS Facade Ergonomics for Consumer Integration

**Status**: Accepted (implemented; see amendments through 2026-07-09)
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
| connect | `otelnats.ConnectWithOptions` / `…WithCredentials…` | `nats.Connect(ctx,url,tp,prop,opts…)`; `nats.ConnectWithOptions(..., nats.WithTracingEnabled(sdk.Toggles.Trace), nats.WithNATSOptions(...))` for native-cost disabled modes | covered — creds pass through as `nats.UserCredentials(...)` via `Connect`, or via `WithNATSOptions` on the option-based constructor |
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
- The new-API pull consume surface, each wrapped in its **native shape** — do
  **not** collapse them to a single handler; in `nats.go` v1.50.0 only
  `Consume` is handler-based:
  - **`Consume(handler, ...)`** — handler `(ctx, jetstream.Msg)`; must
    **return a `ConsumeContext` preserving the full native contract**
    (`Stop()` **and** `Drain()` **and** `Closed()`), ideally by returning the
    native `jetstream.ConsumeContext` — a context exposing only `Stop()` would
    break drain-and-wait shutdown paths. Must also **pass through
    `jetstream.PullConsumeOpt`**, including the **`ConsumeErrHandler`** —
    consumer-side failures (connection loss, pull errors, heartbeat misses)
    surface only through that handler, so a wrapper that swallowed it would
    turn a broken consumer into a silent stall.
  - **`Messages(...)`** — returns a traced iterator whose `Next()` yields
    `(ctx, jetstream.Msg)`, preserving the native `MessagesContext` contract
    (`Next` / `Stop` / `Drain`).
  - **`Next(...)`** — the single-message pull convenience on
    `jetstream.Consumer` (Marz wraps it to return `(ctx, jetstream.Msg, error)`).
    Include a traced `Next`; omitting it leaves single-message pull callers
    unable to drop their Marz import, defeating the ADR's goal.
  - **`Fetch` / `FetchBytes` / `FetchNoWait`** — return a traced `MessageBatch`
    exposing the per-message trace `ctx`; the batch/channel contract preserved.

  This mirrors what Marz `oteljetstream` already does (iterator / batch / `Next`
  shapes kept, `ctx` added), so existing call sites migrate without behavior
  changes.
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
  deferred beyond Phase 1. This is separate from the Phase 2 JetStream typed
  wrapper work and should be handled as its own request/reply follow-up.
- Open question: whether `Respond` needs a header-carrying variant
  (`RespondMsg(ctx, msg, *nats.Msg)`). Today `Publish(ctx, subj, data)` sets
  no custom reply headers; chat's `ReplyJSON` puts status in the JSON
  envelope, so the byte form suffices for now — but echoing `request_id` /
  content-type on the reply would need the variant. Decide at implementation.
- `docs/semconv.md` catalogs most emitted NATS attributes
  (`messaging.operation.type` / `.name`, etc.), but it does **not** list
  `messaging.consumer.name`, which Marz `oteljetstream` hardcodes on JetStream
  consumer spans (`Next` / `Consume` / `Messages` / `Fetch`) and which is **not**
  a key in the pinned semconv v1.39.0 (the catalog only carries
  `messaging.consumer.group.name`). Phase 2 must add an explicit
  catalog/deviation entry for `messaging.consumer.name` — an already-emitted,
  currently-undocumented attribute, not something conditional on the wrapper
  changing the emitted set.

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

---

## Amendment (2026-06-16) — Phase 2 implementation decisions

Implementing the JetStream wrapper (PR #58) surfaced three concrete API
decisions, recorded here so the rationale is durable (and so the recurring
"context-first" review flag has a documented answer).

### 1. `Consume` / `Messages` take a registration-time `ctx`; iterator `Next` does not

`Consume(ctx, handler, …)` and `Messages(ctx, …)` accept a `ctx`, consistent
with the core `Subscribe` / `QueueSubscribe` facade. **Honest scope of that
ctx:** it is an up-front guard only — an already-cancelled `ctx` is rejected,
but it is *not* plumbed into the upstream `oteljetstream` call (which has no
`ctx` parameter) and does *not* cancel a running consume loop (use
`ConsumeContext.Stop` / `MessagesContext.Stop`/`Drain`). Per-message trace
context flows from the **message headers** (delivered on the handler's `ctx`),
not from this registration `ctx`.

So the ctx here carries **no trace benefit** and only a weak cancellation
guard. It is kept purely for API-design reasons: uniform ctx-first entry points
(predictability, forward-compat) and consistency with `Subscribe` — the same
basis on which `Subscribe` itself takes a (equally guard-only) ctx. This is a
deliberate choice of "uniform ctx-first" over "ctx only where it does real
work."

`MessagesContext.Next(opts…)` deliberately stays **ctx-less**: it is a per-pull
iterator call whose upstream (and native `nats.go/jetstream`) signature takes
only `...NextOpt`; a `ctx` there could not cancel the pull and would be
misleading. Cancellation is via `Stop`/`Drain`. (Network/blocking operations
that genuinely use ctx — `CreateOrUpdateStream/Consumer`, `Publish`, the
deferred single-fetch `Consumer.Next` — do take and plumb ctx.)

### 2. Single-message `Consumer.Next` is deferred (not wrapped)

`oteljetstream` v0.2.11's `Consumer.Next` creates the receive span but discards
its context and returns the **producer's extracted remote context** instead of
the local receive-span context — inconsistent with `Consume`/`Messages`, which
return the receive-span context. Wrapping it as-is would mean
`tracer.Start(ctxFromNext, …)` parents work under the upstream producer span
rather than the local consumer span. Rather than ship that inconsistency (or
re-implement the span ourselves, which would be the T3 re-instrumentation this
ADR avoids), single-fetch `Consumer.Next` is **deferred** alongside push
consumers and ordered consumers. (`Fetch`/`FetchBytes`/`FetchNoWait` do not
have this inconsistency — each delivered message gets its own correctly-scoped
receive-span ctx, same as `Consume`/`Messages` — so they were wrapped in the
2026-07-01 amendment below.) Callers needing single fetch use
`Messages(ctx, jetstream.PullMaxMessages(1))`, which returns the correct
receive-span context. Revisit `Next` if upstream returns the receive-span
context from it.

### 3. No nil-receiver guards on the facade

`JetStream()` (and the other facade methods) do **not** defensively guard a nil
receiver / nil embedded `Conn`. A `Conn` obtained from `Connect` always has a
non-nil embedded connection; a nil embedded `Conn` is only reachable by
bypassing `Connect` (hand-constructing `&Conn{}`), which is misuse that panics
identically across `Subscribe` / `QueueSubscribe` / `Respond` / `JetStream`.
Guarding only `JetStream` would be asymmetric; guarding all of them is not
warranted for a misuse-only path. Left to panic, consistent with the package.

---

## Amendment (2026-07-01) — closing the remaining chat-integration gaps

The `hmchangw/chat` migration (this ADR's original motivation) surfaced four
concrete gaps once JetStream pull consumers, request/reply, and Grafana
readability were exercised end to end. This amendment closes the three that
are in scope for a Go SDK and records the decision for the fourth.

### 1. `Fetch` / `FetchBytes` / `FetchNoWait` are now wrapped

The 2026-06-16 amendment §2 deferred these alongside `Consumer.Next` on the
theory that batch delivery would need "an o11y-owned carrier type for the
channel" (§3, "Surface to wrap"). That type is now added:
`FetchedMessage{Ctx, Msg}`, and a `MessageBatch` interface
(`Messages() <-chan FetchedMessage`, `Error() error`) mirroring the native
`jetstream.MessageBatch` contract. Unlike `Consumer.Next` (deferred, see
above), the upstream `oteljetstream.Fetch`/`FetchBytes`/`FetchNoWait` do
**not** have the receive-span-context bug: each `oteljetstream.Msg` on the
batch channel already carries the correct local receive-span ctx (verified
against `otel-nats` v0.2.11 source), so wrapping is a straight adapter, not a
re-instrumentation. `FetchNoWait` takes a registration-time `ctx` guard only,
consistent with `Consume`/`Messages` (§1 above) — the native API gives it no
`FetchOpt` list to plumb `ctx` into. `Fetch`/`FetchBytes` go further: `ctx` is
also passed into the native call via `jetstream.FetchContext(ctx)`, so
cancelling or timing out `ctx` after the call returns actually ends the
in-flight pull and closes the batch channel early, instead of only being
checked once up front — see the "ctx propagation" sub-point below for why
this differs from `Consume`/`Messages`/`FetchNoWait`.

This was `search-sync-worker`'s largest gap: JetStream batch pull is its
primary consume pattern, and every batch message previously arrived with no
trace context at all once callers dropped the direct `oteljetstream` import.

**Forwarding-channel buffering (goroutine leak).** Each delivered message is
forwarded from the upstream channel to the o11y one by a background
goroutine (`wrapMessageBatch`), because `FetchedMessage` — an o11y-owned type
pairing `Ctx` and `Msg` — can't be produced by a simple type-cast over the
channel. That goroutine originally forwarded onto an **unbuffered** channel,
so a caller that read only part of a batch and stopped (a realistic pattern —
e.g. bailing out after the first error) left the goroutine blocked forever on
the next send, with no reader and no way to cancel it (neither
`oteljetstream.MessageBatch` nor the native `jetstream.MessageBatch` expose a
`Stop`/`Close`). Fixed by sizing the forwarding channel's buffer to each
call's own message-count bound where one exists: `batch` for `Fetch` and
`FetchNoWait` is the API's own hard cap, so the goroutine can always drain
the whole upstream batch into the buffer and exit on its own — early
abandonment leaks nothing. `FetchBytes` has no message-count bound (only a
byte budget, satisfiable by an unbounded number of small messages), so it
gets a fixed best-effort buffer (`fetchBytesBatchBuf = 256`) instead of a
provable one; a batch larger than that can still leak on early abandonment.
No interface change (`MessageBatch` gained no `Stop`/`Close`) — see
`nats/jetstream.go`'s `MessageBatch` doc comment for the full guarantee.
Locked down by `TestJetStream_Fetch_NoGoroutineLeakOnEarlyAbandon`.

That buffering has a tracing cost, found in review after the fact: upstream
`oteljetstream.wrapMessageBatch` ends message N's receive span as soon as its
own internal goroutine reads message N+1 off *its* channel — not when the
caller finishes processing message N. Before this buffering fix, our own
forwarding channel being unbuffered end-to-end kept that upstream loop
naturally throttled to roughly the caller's own consumption pace. Buffering
removes that throttle: our forwarding goroutine can now drain the entire
upstream batch into the buffer in one tight loop, letting upstream race
through ending every span in the batch well before the caller has read past
the first message. Net effect: for a batch of more than one message,
`trace.SpanFromContext(m.Ctx).SetAttributes(...)` is a silent no-op for most
of the batch by the time the caller's loop body runs. This is a genuine
regression relative to the (leaky) unbuffered version, but not one worth
re-opening the goroutine-leak trade-off over: log correlation
(`obs.Logger.*Context(m.Ctx, ...)`, which only reads the immutable trace/span
IDs off `ctx`) and starting a child span via `tracer.Start(m.Ctx, ...)` — the
pattern `examples/jetstream/fetch-worker` already uses — are both unaffected,
and neither this repo's own example nor `docs/guide.md`'s Fetch section ever
demonstrated the `SetAttributes`-on-the-receive-span pattern for the batch
path (that pattern is `Subscribe`-specific, callback-based rather than
channel-based, so its span never needs to outlive processing). Documented as
a caveat on `MessageBatch`'s doc comment and in `docs/guide.md` rather than
worked around. Locked down (as a known-behavior regression test, not a fix)
by `TestJetStream_Fetch_ReceiveSpanEndsBeforeConsumption`.

**`ctx` propagation into `Fetch`/`FetchBytes` (not just a registration
guard).** Unlike `Consume`/`Messages` (§1 of the 2026-06-16 amendment), the
native `jetstream.Consumer.Fetch`/`FetchBytes` accept a `jetstream.FetchOpt`
list, and `jetstream.FetchContext(ctx)` is a real, documented option that
cancels the in-flight pull when `ctx` is done — so unlike `Consume`/
`Messages`/`FetchNoWait`, there was a native mechanism sitting unused. The
wrapper now prepends `jetstream.FetchContext(ctx)` to the caller's `opts` on
every call. This collides with an explicit `jetstream.FetchMaxWait` in the
caller's own `opts`: the native API rejects combining the two
(`jetstream.ErrInvalidOption`, checked internally after all opts are
applied), because the point of `FetchMaxWait` is to make the call an
authoritative deadline. Rather than surface that error to every caller who
happens to already set `FetchMaxWait` (e.g. this repo's own
`examples/jetstream/fetch-worker`), the wrapper retries without the
`FetchContext` injection when that specific error comes back, deferring to
the caller's explicit `FetchMaxWait` exactly as before this change. Confirmed
against `nats.go` v1.50.0 source that this fallback is side-effect-free (the
collision check runs before any network I/O). Locked down by
`TestJetStream_Fetch_CtxCancellation_MidFetch`.

The fallback match is deliberately narrower than "any `jetstream.ErrInvalidOption`":
that sentinel is also returned for unrelated rejections native to `Fetch`'s own
option validation — e.g. a ctx-derived expiry too tight for an explicit
`jetstream.FetchHeartbeat` (`"expiry time should be at least 2 times the
heartbeat"`), or a ctx whose deadline has already passed
(`"context deadline already exceeded"`). An early version of this fix matched
the sentinel alone and would have silently retried without `FetchContext` in
those cases too — passing option validation on the retry (no `FetchContext`
means no ctx-derived expiry to conflict with the heartbeat) and quietly
running the fetch to its 30s default wait, discarding the caller's real
cancellation/deadline instead of surfacing the actual problem. `isFetchMaxWaitCollision`
matches on the specific `"cannot specify both FetchContext and FetchMaxWait"`
message instead, so only that one collision falls back; every other invalid
combination is returned to the caller as an error. Locked down by
`TestJetStream_Fetch_CtxOptionCollision_NotSwallowed` (confirmed to fail
against the sentinel-only match by temporarily reverting it), and by
`TestJetStream_Fetch_FetchMaxWaitCollision_Retries` for the positive case —
a live ctx plus an explicit `FetchMaxWait` retries transparently and
actually honors that `FetchMaxWait` (confirmed to fail the same way if the
fallback is removed).

**Not defended against, found in a self-review pass:** a caller passing their
own `jetstream.FetchContext` directly in `opts` (redundant with, and
different from, the `ctx` parameter `Fetch`/`FetchBytes` already take —
unlikely, but nothing stops it). Unlike `FetchMaxWait`, the native
`pullRequest` has no "already set" guard for `FetchContext`: `fetchOptsWithCtx`
always applies this package's own `FetchContext(ctx)` first, so a
caller-supplied `FetchContext` later in `opts` just silently overwrites
`req.ctx`, taking over cancellation from the documented `ctx` parameter with
no error. Detecting this from outside the `jetstream` package isn't possible
without probing `pullRequest`'s unexported fields, which this facade
deliberately doesn't reach into (see the header-carrier and `FetchMaxWait`
decisions above for the same "wrap, don't reimplement" boundary). Documented
as a caveat on `Consumer.Fetch`'s doc comment, `fetchWithCtxFallback`'s doc
comment, and `docs/guide.md`'s Fetch section instead: pass the `ctx` meant to
govern cancellation as the method's own `ctx` argument, not as a
`FetchContext` opt.

**Upstream-upgrade note:** `isFetchMaxWaitCollision` couples to that exact
upstream message string — there is no typed/sentinel error more specific
than `jetstream.ErrInvalidOption` for this one collision (confirmed against
`nats.go`'s source; `ErrInvalidOption` is shared by every option-validation
rejection in the package). If a future `nats.go` upgrade rewords it, the
fallback silently stops matching and any caller combining a live ctx with
their own `FetchMaxWait` starts seeing a hard `ErrInvalidOption` instead of
the transparent retry — both regression tests above would start failing
(`TestJetStream_Fetch_FetchMaxWaitCollision_Retries` directly; the
`NotSwallowed` test's `assert.NotContains` would still pass but the intent
would be moot), which is the intended signal to check this string against the
new `nats.go` release when bumping the `github.com/nats-io/nats.go` version.

### 2. `Conn.Request` closes the requester-side reply-link gap

The base ADR (Decision 1, "Header propagation ≠ requester-side span linkage")
and the 2026-06-16 amendment left this as an open follow-up: `Respond`
guarantees the reply *carries* trace context, but nothing on the requester
side turned that into a visible span. `Conn.Request` now wraps the embedded
`otelnats.Conn.Request` and, when the reply carries a valid trace context,
starts a `receive {subject}` span (`SpanKind` CONSUMER) **as a child of the
caller's ctx** — not a new trace, unlike the JetStream/Core consumer spans
above — carrying a **link** to the trace context extracted from the reply.
Child-of-ctx (rather than "new trace + link back," the pattern used
everywhere else in this ADR) is deliberate: the reply-receive span is a
synchronous continuation of a call the requester is already tracking in its
own trace, not an independent unit of work triggered by an inbound message;
the link still supplies the cross-service correlation to the responder's
reply-send span. No span is created when the reply has no trace context
(untraced responder, or one using the raw `msg.Respond`) — `Request` then
behaves exactly like the embedded method, so this is purely additive.

`Request` also takes an optional variadic `attrs ...attribute.KeyValue`,
attached to the reply-link span only (see item 4 below for why it can't reach
the other spans in the exchange).

**Known limitation, found in review (codex, 2026-07-02):** "child-of-ctx"
only delivers a single shared trace when `ctx` already carries an active span
at the `Request` call site. Both the embedded send span and the reply-link
span are parented on the *same* `ctx` — so when `ctx` is bare
(`context.Background()`, or any ctx with no ambient span), they become two
disconnected root traces, correlated only by the Link, not by shared
ancestry. This works exactly as advertised for the expected common case
(`Request` called from inside an HTTP handler or a consumer callback, where
`ctx` already has one), but silently degrades for a bare-ctx caller (e.g. a
background worker's own top-level request loop) to "two separate one-hop
traces, connected by a Link" rather than one cohesive round-trip trace.

Considered and rejected two code-level fixes: (a) when `ctx` has no active
span, parent the reply-link span on the trace context extracted from the
reply instead of `ctx`, making it a real child of the responder's reply-send
span — closes the gap without re-instrumentation, but introduces conditional
parenting logic (different behavior depending on whether `ctx` already has a
span) and nests the reply-link span under the responder's side of the trace
rather than the requester's own call site, inconsistent with the "new trace +
link, never actual parent" pattern this SDK uses for other async consumer
spans; (b) stop delegating to embedded `otelnats.Conn.Request` and create the
send span in this package instead, so both spans' parenting is controlled
unconditionally — guarantees correlation in every case, but is a real T3
re-instrumentation this ADR has deliberately avoided everywhere else (adds
maintenance burden duplicating otel-nats's send-span/header-injection logic
for a P2-severity gap).

Decision: document the limitation rather than code around it (`Request`'s and
`linkReply`'s doc comments in `nats/conn.go`, and `docs/guide.md`'s Request
section), with the recommended workaround for affected callers — open a span
before calling `Request`:

```go
ctx, span := tracer.Start(ctx, "request "+subject)
defer span.End()
reply, err := conn.Request(ctx, subject, payload, timeout)
```

Downstream services adopting this SDK should audit request/reply call sites
made from a background loop, cron job, or startup path (not from within a
handler or consumer callback) when upgrading, since those are the call sites
most likely to be passing a bare `ctx` and losing round-trip trace
correlation silently.

### 3. Browser receive-side helper — documented pattern, not new Go code

A browser frontend on `nats.ws` is outside this SDK's surface (Go-only, per
the package doc). The gap is real but the fix is a documented **pattern**,
not a Go API: `examples/nats-ws-browser/src/tracing.js` now exports
`receiveWithSpan(msg, { name, attributes }, callback)`, extracted from what
was previously inlined ad hoc in that example's `main.js`. It extracts
`traceparent`/`tracestate` from `msg.headers`, starts a `SpanKind.CONSUMER`
span, and wraps `callback` — the render/dispatch work — inside it, recording
and re-throwing any callback exception and always ending the span. This is
the reusable version of the same three-step recipe (extract → CONSUMER span →
wrap the dispatch) that `Conn.Subscribe` and the JetStream consume paths
already apply on the Go side; frontend teams integrating with this SDK's
backends should copy this pattern rather than re-deriving header extraction
from scratch.

### 4. Span naming / attributes — attributes, not names, and only where o11y owns the span

Chat's readability complaint ("a wall of indistinguishable `nats.request`
spans in Grafana") is **not** an o11y span-naming defect: every span this SDK
or `otel-nats` emits is already `{operation} {subject}` (`send
events.created`, `process events.created`, `receive events.created`, …), so
the subject is already in the name. The actual missing piece is
**attributes** for high-cardinality, app-specific identifiers — a request ID,
a room ID, a site ID — which the SDK cannot know and must not put in the span
*name* regardless (unbounded cardinality in span names defeats trace-backend
indexing; see "Known Cardinality Risks" in `docs/semconv.md`). Two concrete,
bounded outcomes, no ADR 0023 (`{system}.{operation} {target}`) scope
expansion — ADR 0023 is explicitly data-store-only and NATS is a messaging
system with no single "target" dimension:

- **Spans o11y creates itself** (only `Conn.Request`'s reply-link span so
  far) accept caller-supplied `attribute.KeyValue`s directly — see item 2.
- **Spans `otel-nats` creates** (`Subscribe`/`QueueSubscribe` process spans,
  JetStream consumer spans) cannot take extra attributes through this facade
  without forking `otel-nats`, which this ADR has already rejected (Decision
  driver, above). The correct, already-available pattern — no new SDK code
  needed — is for the handler to call
  `trace.SpanFromContext(ctx).SetAttributes(...)` on the consumer span it was
  handed, using the domain values it already has in scope. Documented in
  `AGENTS.md` / `docs/guide.md` rather than wrapped, since wrapping would add
  no capability over the three-line stdlib OTel call.

### 5. Header-carrier case-sensitivity fix (found while implementing item 2)

Building the reply-link span surfaced a latent bug in `nats/middleware.go`'s
public `Inject`/`Extract`: they used
`go.opentelemetry.io/otel/propagation.HeaderCarrier`, which is backed by
`http.Header` and canonicalizes keys to MIME form (`"traceparent"` →
`"Traceparent"`) on both `Get` and `Set`. `nats.Header.Get`/`Set`, unlike
`http.Header`, is **case-sensitive** with no canonicalization — so a header
written by `otel-nats` itself (which uses its own case-sensitive
`otelnats.HeaderCarrier` internally, storing the literal lowercase
`"traceparent"` the W3C propagator passes in) could never be read back by
this package's own `Extract`, and would silently return `ctx` unchanged. The
bug was self-masked until now: `Inject` and `Extract` always canonicalized
the *same* way, so a round trip through only this package's own two functions
happened to still work. Both now use a package-local `headerCarrier` backed
directly by `nats.Header`'s own case-sensitive `Get`/`Set` — the same
approach `otelnats.HeaderCarrier` already uses — so extraction is correct
against headers written by `otel-nats`, this package's own `Inject`, or any
other W3C-compliant writer (e.g. the `nats.ws` browser client). Locked down
by `TestExtract_HeaderCasing`.

---

## Amendment (2026-07-03) — header-casing fix's scope, found in review

Item 5 above fixed this package's *own* `Inject`/`Extract` (used by the
legacy `nats.JetStreamContext` API and internally by `Conn.Request`'s
reply-link span). Codex correctly pointed out that scope doesn't cover
everything: the JetStream consume paths this facade wraps —
`Consumer.Consume` / `Messages` / `Fetch` / `FetchBytes` / `FetchNoWait` —
extract per-message trace context entirely *inside* the vendored
`oteljetstream` package, using its own `otelnats.HeaderCarrier`. Confirmed
against the vendored source (`otelnats/propagation.go`): that carrier is
case-sensitive with **no** MIME-canonical fallback — the doc comment on it
even says "used by oteljetstream and by Conn internally," meaning it's the
one carrier both the core and JetStream paths share, entirely inside the
third-party package. This facade has no hook into that internal extraction:
it calls `oteljetstream`'s public `Fetch`/`Consume`/etc. and receives back a
`(ctx, msg)` pair with the span already built.

Practical consequence: a message whose trace header sits under a
canonicalized key ("Traceparent") rather than the literal lowercase
"traceparent" this SDK's own `Publish` always writes — written by any
pre-`headerCarrier`-fix version of this SDK, or by any other producer that
canonicalizes — will not be linked to its producer's trace when consumed via
any of the five methods above, even though this package's own `Extract` (item
5) already handles that exact case correctly for the paths it owns.

Considered and rejected fixing this: it would require reimplementing
`oteljetstream`'s consumer-span creation ourselves so we control the header
extraction — the same T3 re-instrumentation this ADR has consistently
avoided (see the 2026-06-16 amendment §2 on `Consumer.Next`, and the
2026-07-01 amendment's `Conn.Request` and `MessageBatch` span-lifecycle
decisions, for the same reasoning applied elsewhere). No test was added
locking this down, unlike other documented limitations in this ADR: this one
lives entirely in vendored code, not in anything this package's own tests
exercise, and constructing it cleanly would require bypassing this SDK's own
`Publish` (which always injects its own literal-case header on top of
whatever the caller pre-set) down to the raw `nats.go`/`jetstream` client —
testing `otelnats`'s behavior, not this facade's. Documented instead, in
`nats/jetstream.go`'s package doc comment and here. Self-resolves once
messages predating the header-casing fix have drained from any durable
streams; worth reporting upstream to `Marz32onE/instrumentation-go` as a
`otelnats.HeaderCarrier` interop gap independent of this SDK.

---

## Amendment (2026-07-09): otel-nats v0.6.0 — reply-link span superseded, Next wrapped, teardown surface completed

Upstream v0.6.0 absorbs three decisions this ADR previously worked around:

1. **The 2026-07-01 `Conn.Request` reply-link span is superseded.** Upstream
   `recordReply` now emits the requester-side reply-receive span itself —
   named `receive {inbox}`, parented under (and linked to) the responder's
   remote reply-send context when the reply carries one, emitted without a
   link otherwise. The facade's `linkReply`/`replyAttrs` are deleted to avoid
   a duplicate span; `Request`'s variadic `attrs` parameter is removed with
   them (upstream offers no caller-attribute hook on that span — proposed
   upstream as an enhancement). Topology note: the receive span now lands in
   the responder's trace rather than the requester's.
2. **`Consumer.Next` is now wrapped** (the 2026-06-16 §2 deferral is lifted):
   upstream returns the local receive-span context, consistent with
   `Messages().Next`.
3. **`ConsumeContext` mirrors the native Stop/Drain/Closed** and the facade
   interface widened to match; `MessageBatch` gained `Stop()` upstream and the
   facade propagates it, so FetchBytes early abandonment no longer risks a
   parked forwarding goroutine.

The 2026-07-03 canonical-header extraction limitation remains (upstream
carrier still exact-case, verified in v0.6.0) and its documentation stands.
