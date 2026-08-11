# Upstream otel-nats — fixes, feature requests, and discussion items

**Upstream**: `github.com/akira-core/instrumentation-go` (`otel-nats` module;
repo and module path renamed from `Marz32onE` in v0.6.0)
**o11y pin**: v0.8.0 (see ADR 0004, 2026-08-10 amendment, for the audit)
**Contribution model**: `flywindy/instrumentation-go` is a PR workspace only —
its `main` tracks upstream `main`, every change goes to upstream as a PR, and
o11y always imports the upstream module. Hard-forking (module rename, own
releases) is the documented fallback, to be triggered only if collaboration
stalls on a critical fix.

This document is the single place that tracks what we want changed upstream,
why, and what each change unlocks in this SDK. Update it on every upstream
release audit and whenever an item lands, and keep the linked o11y issues in
sync.

**Last surface audit**: 2026-08-10 against v0.8.0 — a full re-read of the
constructors, flag resolution, direct/traced implementations,
`oteljetstream` inheritance, module dependencies, and release notes.

---

## v0.8.0 adoption decision

o11y adopts the upstream dynamic-control semantics without enabling a relay by
default. The upgrade and the operational rollout are deliberately separate:

- Existing `WithTracingEnabled` call sites compile unchanged, but the option is
  now the third rung of `relay > env > option > default`; it is a local default,
  not a hard override.
- No relay plus an effective false local value still builds only the direct
  implementation and performs no per-operation flag evaluation. This preserves
  the previous hot-path cost for deployments that only upgrade the dependency.
- A relay-capable process keeps the traced implementation available and makes
  two flag evaluations per instrumented operation even while the NATS flag is
  false. High-throughput services must capacity-test that mode before enabling
  it fleet-wide.
- Relay disable is not restart-durable until the first successful fetch; an
  incident response that must survive restarts must also land the corresponding
  environment value.
- Invalid upstream flag values and invalid relay endpoint/poll interval values
  now fail connection construction. Deployment configuration must be validated
  before rollout.
- The endpoint-driven zero-code path may install a process-global named
  OpenFeature provider and poller without exposing an o11y shutdown handle.
  o11y sets neither the endpoint nor any OpenFeature global; an adopting
  application owns this opt-in lifecycle implication.

The dependency adds `otel-flags`, OpenFeature, GO Feature Flag, and their
transitive parsing/rule-evaluation modules. They expand build, SBOM, licence,
and vulnerability-review surfaces even when the relay is unused.

Upstream's CHANGELOG states the cost "lands on `go.sum`, vulnerability-scanning
surface and licence review, not on runtime; the linker drops unreached code."
The runtime half holds — a process with no relay configured measures 0
allocations per operation — but **the linker half does not, and should not be
relied on for image-size planning**. Measured here on a minimal `main` whose
only SDK call is `o11ynats.Connect`, with no relay configured:

| Build | Binary | `antlr`/`gofeatureflag`/`jsonlogic` symbols |
|---|---|---|
| o11y on otel-nats v0.7.0 | 9,382,454 B | 0 |
| o11y on otel-nats v0.8.0 | 12,497,169 B | 1248 |

That is **+3.11 MB (+33%)**, and `go tool nm` shows the rule-engine symbols are
linked in rather than elided (1174 `antlr`, 69 `jsonlogic`, 5 `gofeatureflag`,
plus 127 `openfeature`). The provider is reachable through the endpoint-driven
auto-install path, so the linker cannot prove it dead — configuring no relay is
a runtime decision, not a build-time one. Container images grow accordingly;
budget for it before rolling the upgrade out fleet-wide.

---

## Resolved by upstream v0.7.0 (for the record)

v0.7.0 cleared almost the entire backlog below. Each row links the item as it
was tracked here and what the o11y-side upgrade deleted or simplified in
response.

| Item | Was tracked as | Resolution in v0.7.0 |
|---|---|---|
| `HeaderCarrier` exact-case-only, no `Values`, canonicalized keys lose their trace link | **F1** | Implements `propagation.ValuesGetter`; `Get`/`Values` match the verbatim key first, then fall back to MIME-canonical and case-folded forms. Deleted the "Known limitation" block in `nats/jetstream.go` and the matching guide/semconv notes |
| `messaging.consumer.name` is not a semconv v1.39.0 key | **F2** / [#69](https://github.com/flywindy/o11y/issues/69) part 3 | Consumer/durable name now attached under the semconv key `messaging.consumer.group.name`; removed the deviation entry in `docs/semconv.md`. **BREAKING** for dashboards keyed on the old attribute |
| `Consumer.Next` ignored live ctx cancellation (deadline only) | **F3** | Wires `jetstream.FetchContext`, so a deadline-less canceled ctx aborts promptly. Facade `Next` doc drops its "deadline yes, cancellation no" caveat. **BREAKING**: a cancelable ctx can no longer be combined with a caller `FetchMaxWait` (returns `ErrInvalidOption`) |
| `MessageBatch.Stop` left the forwarding goroutine parked on a waiting batch | **F4** | Forwarding loop now selects on `done` on the receive side too, so `Stop` releases promptly. The o11y facade's own cancelable-fetch-ctx workaround stays (still helps the `FetchMaxWait` path) but is no longer load-bearing |
| `recordReply` overwrote the request span's `messaging.message.body.size` with the reply size | **F5** | Request span's body size is left untouched; reply size stays on the reply "receive" span. Reversed the deviation note in `docs/semconv.md` |
| Request "send" span never carried `messaging.message.conversation_id` | **F6** | Core request/reply send span now gets the reply inbox as `conversation_id` via a late `SetAttributes` before `End` (invisible to samplers by design). JetStream spans deliberately excluded (`$JS.ACK.…` is not a conversation ID) |
| Tracing controlled solely by two process-global env vars (default OFF); no per-Conn override; `ResetGatesForTest` removed in v0.6.0 | **R1** / [#71](https://github.com/flywindy/o11y/issues/71) part 2 | Added `WithTracingEnabled(v bool) Option`, overriding the env default in either direction per `Conn`. `o11ynats.Connect` defaults to `WithTracingEnabled(true)`, while `o11ynats.ConnectWithOptions(..., o11ynats.WithTracingEnabled(sdk.Toggles.Trace))` lets integrations follow the SDK's resolved trace toggle and exposes the direct/native path when tracing is off; removed the two-env-var setup from `examples/README.md`, `AGENTS.md`, tests' `TestMain`, and `docs/guide.md` |
| Implicit env-gated second `TracerProvider` with no sampler for synthetic "deliver" spans | **R3** / [#70](https://github.com/flywindy/o11y/issues/70) | **Deliver spans removed entirely** (`initNATSProvider`/`deliverTracer`/`ConsumerContextWithDeliver` and every call site gone; the package no longer reads `OTEL_EXPORTER_OTLP_ENDPOINT` for span emission). Removed the sampling-inconsistency / second-exporter caveats in ADR 0003 and ADR 0004 |
| Batch / `MessagesContext.Next` receive spans ended only when the next message arrived (per-message enrichment a silent no-op) | **R5** | Receive spans now end **at handover** (just before the channel send / on `Next` return); `IsRecording()==false` at delivery is the deterministic contract. Updated the span-lifecycle blocks in `nats/jetstream.go` (`MessageBatch` doc) and `docs/guide.md`. **BREAKING**: these span durations are now shorter (receive-to-handover only) |
| CHANGELOG absent from the v0.6.0 module zip; no written semver policy | **D1** / [#73](https://github.com/flywindy/o11y/issues/73) remainder | `CHANGELOG.md` (Keep-a-Changelog format, starting at 0.6.0) plus a root `VERSIONING.md` with the pre-1.0 semver policy are now shipped in the module |
| `Version()` tag-consistency not automated | **D3** / [#73](https://github.com/flywindy/o11y/issues/73) remainder | A release-tag CI guard now keeps the version constant and the release tag in sync (noted in the CHANGELOG coverage note); the constant reads `0.7.0` |

Span kinds were also corrected to the OTel spec in v0.7.0: reply-receive and
the JetStream pull-**receive** spans (`Next`/`Messages`/`Fetch`/`FetchBytes`/
`FetchNoWait`) are now `CLIENT` (were `CONSUMER`); `publish` stays `PRODUCER`.
The JetStream `Consume` callback span and the core Subscribe handler span keep
`process` semantics and stay `CONSUMER` (unchanged) — upstream's own CHANGELOG
lumps `Consume` in with the CLIENT group, but the code
(`oteljetstream.tracedConsumeHandler`) still starts it `SpanKindConsumer`, so
only the pull-receive/reply paths actually changed. Pull-receive spans
additionally carry `messaging.operation.type=receive`. This is reflected in
`docs/semconv.md` and the o11y test assertions.

### Resolved earlier, by upstream v0.6.0 (for the record)

| Item | Was tracked in | Resolution |
|---|---|---|
| semconv regression v1.39.0 → v1.37.0 | [#73](https://github.com/flywindy/o11y/issues/73) | Restored to v1.39.0 |
| `Version()` shipped stale ("0.4.1" in v0.5.0) | [#73](https://github.com/flywindy/o11y/issues/73) | Correct in v0.6.0 and guarded by `version_test.go` |
| Release tags not on `main` | (found during upgrade) | `main` == `otel-nats/v0.6.0` tag |
| Propagation gate (`OTEL_NATS_PROPAGATION_ENABLED`) defaulting OFF | [#71](https://github.com/flywindy/o11y/issues/71) part 1 | Gate removed; inject/extract follow the tracing gate unconditionally |
| `Consumer.Next` returned the producer's remote ctx instead of the receive-span ctx | [#69](https://github.com/flywindy/o11y/issues/69) part 1 | Fixed; facade wraps `Next` |
| `ConsumeContext` narrowed to `Stop()` only | [#69](https://github.com/flywindy/o11y/issues/69) part 2 | Mirrors native `Stop`/`Drain`/`Closed` |
| `MessageBatch` had no abandon/cancel escape hatch | (documented limitation) | Upstream added `Stop()`; facade propagates it |
| Requester-side reply-receive span missing (o11y carried its own `linkReply`) | ADR 0022 amendment 2026-07-01 | Upstream `recordReply` emits it; facade span deleted |

---

## Open items

v0.7.0 cleared the original backlog down to three carried items (R2, R4, D2).
A fresh surface audit against v0.7.0 (2026-07-16, after the upgrade landed)
added seven more.

The table below is ordered by (value ÷ expected review friction), with one
deliberate exception: **R6 is listed first on value alone**, because it is the
single largest capability gap in the integration and sets the context for
everything else — but it is also the highest-friction item and is blocked on a
decision rather than on effort. **R7 is the better first PR**, and the
engagement plan below sequences it accordingly.

| Item | Kind | Value | Friction | Status |
|---|---|---|---|---|
| R6 — messaging metrics | Feature | High | High (design) | New |
| R7 — `Fetch`/`FetchBytes` ctx parameter | Feature | High | Low | New |
| F8 — CHANGELOG mis-describes the `Consume` span kind | Fix (docs) | Medium | Trivial | New |
| F7 — unbuffered `MessageBatch` channel leaks on abandon | Fix | Medium | Low | New |
| R2 — `RequestWithTimeout(ctx, …)` | Feature | Low (mitigated) | Low-med | Carried ([#72](https://github.com/flywindy/o11y/issues/72)) |
| R4 — caller-attribute hook on reply-receive span | Feature | Medium | Medium | Carried |
| R8 — exported message-level `Inject`/`Extract` | Feature | Low | Low | New |
| F9 — `Consume`/`Messages` take no ctx | Fix | Low | Medium | New |
| R9 — KeyValue / ObjectStore wrapping | Feature | Deferred | High | New |
| D2 — old-namespace retention | Discussion | Medium | None (ask) | Carried |

### Feature requests

#### R6. Messaging metrics — the module is trace-only

- **What**: otel-nats emits **no metrics at all** — there is not a single
  `metric` / `Meter` / `MeterProvider` reference in either `otelnats` or
  `oteljetstream`. semconv v1.39.0 already defines the messaging instruments in
  its `messagingconv` package (`messaging.client.operation.duration`,
  `messaging.process.duration`, `messaging.client.sent.messages`,
  `messaging.client.consumed.messages`).
  Request: emit them from the same call sites that already produce spans,
  behind a `WithMeterProvider(...)` option mirroring `WithTracerProvider`.
  Note the correct counter name is `messaging.client.sent.messages` —
  `messaging.client.published.messages` does **not** exist in v1.39.0 and has
  appeared in at least one internal proposal.
- **Evidence**: v0.7.0 — `grep -rl "metric\|Meter" otelnats/ oteljetstream/`
  returns nothing outside tests.
- **Why metrics rather than reading it off spans**: this SDK samples
  (`sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))`, `o11y.go:491`),
  so counts and rates **cannot** be derived from spans at all, and latency
  histograms only cover the sampled fraction. Unsampled per-operation counters
  and histograms are the one thing neither spans nor the server-side exporter
  can provide. This is the strongest argument for the feature and is easy to
  omit — ADR 0004's original deferral analysis assessed a `nc.Stats()` snapshot
  layer, not per-operation instruments at the wrapper seam.
- **Where each instrument belongs** — the facade is *not* categorically unable
  to do this (an earlier draft of this entry claimed so; that was wrong). o11y
  wraps `Subscribe`/`QueueSubscribe`/`Request`/`RequestMsg`/`Respond` and the
  whole JetStream surface, so it can time and count at those seams. The split
  is per-instrument:

  | Instrument | Best owner | Why |
  |---|---|---|
  | `messaging.process.duration` | **Upstream** | `wrapMsgHandler` / `tracedConsumeHandler` already bracket the handler with `tracer.Start(...)` … `defer span.End()`. Recording a histogram there is nearly free; o11y would have to double-bracket the same call. |
  | `messaging.client.sent.messages` | **Upstream** | o11y does not currently wrap core `Publish` at all (it is inherited from the embedded `*otelnats.Conn`), and this is the one path with a real cardinality problem — see below. |
  | `messaging.client.consumed.messages` | Either | Both layers wrap the handler; the low-cardinality label is free at both. |
  | `messaging.client.operation.duration` (request/reply RTT) | **o11y is trivial** | The facade already owns the ctx-first `Request`/`RequestMsg` shims and brackets the round trip. |
  | JetStream `delivered` / `redelivered` | Either | Readable at delivery from `msg.Metadata().NumDelivered`, no API change. |
  | JetStream `ack` / `nak` | **Neither, as things stand** | Both layers hand the caller the **native** `jetstream.Msg` (`nats/jetstream.go:366`, upstream `Msg{Msg: msg, Ctx: ctx}`), so `msg.Ack()` is invoked directly on the native type and is invisible to both. Counting these requires wrapping the message type — an upstream-only, breaking change that also conflicts with ADR 0022's "deliver native types" contract. Treat as out of scope unless separately proposed. |

- **Cardinality is asymmetric — but the safe set is narrow.** A blanket "subject
  pattern" rule would over-engineer the consume side, while a publish-only rule
  (as an earlier draft of this entry said) would under-protect the rest. The
  split, per path:

  | Path | Destination label source | Safe as a metric label? |
  |---|---|---|
  | Core subscribe / queue-subscribe | The **subscription** subject — upstream names the span `"process " + subject` (`otelnats/conn_traced.go:193`), i.e. the pattern the developer registered (`orders.*`), not `msg.Subject` | ✅ naturally low-cardinality |
  | JetStream consume / pull-receive | Durable consumer name, already emitted as `messaging.consumer.group.name` | ✅ naturally low-cardinality |
  | Core publish | Concrete caller-supplied subject | ⚠️ high — needs a pattern hook |
  | `Request` / `RequestMsg` | Concrete caller-supplied subject | ⚠️ high — same as publish |
  | Reply-receive | The **reply inbox** — `recordReply` names the span `"receive " + reply.Subject` (`otelnats/conn_traced.go:181`) and sets it as `conversation_id` | 🔴 **unbounded** — a fresh inbox per request, so this is strictly worse than publish and must never be a metric label |

  So the pattern-extraction / caller-supplied-label hook must cover **every path
  that labels a destination**, not just publish; and the reply-receive path
  needs the destination dropped from its metric labels outright (keep it on the
  span, where high cardinality is fine). This is also a repo rule, not just a
  preference — `AGENTS.md` forbids high-cardinality values as metric label
  values.
- **Unlocks**: closes ADR 0004's "Metrics scope: deferred" amendment. NATS is
  one of only **two** trace-only integrations in this SDK — the other is
  Elasticsearch (ADR 0020 §6, which explicitly places ES "with NATS") — while
  Redis, MongoDB, Cassandra, MinIO, HTTP, Gin and Resty all emit
  operation-duration metrics. Note ADR 0020's own framing of why: for both,
  *"the first-party instrumentation is trace-only; any metric is self-written"*.
  That is precisely the condition this request removes — once upstream emits
  the instruments, the NATS half of that rationale no longer holds and the
  deferral can be revisited on its merits rather than on the absence of a
  library to lean on.
- **Counter-argument to weigh first**: ADR 0020 §6 also notes NATS has a strong
  server-side metrics story (`prometheus-nats-exporter`) and that per-call
  latency is already visible as span duration. So this is *not* a blind spot in
  the "no data exists" sense; the gain is client-side, per-operation,
  per-subject latency correlated with traces and exemplars — plus the unsampled
  counts spans structurally cannot give (above). Worth stating plainly in the
  upstream issue rather than overselling the gap.
- **How this interacts with ADR 0004 §5's revisit triggers** — that amendment
  lists three conditions for reopening the o11y-side deferral. Reading them
  against today:
  - *"messaging metrics semconv marked stable"* — **unresolved**. v1.39.0 now
    ships typed constructors in `messagingconv`, which is more mature than when
    the amendment was written (2026-05-25), but the Go packages carry no
    stability annotation (`messagingconv` and `dbconv` are structurally
    identical), so this must be checked against the spec, not the module.
  - *"a maintained library starts emitting NATS client metrics"* — **do not
    use this one to argue against R6.** It is a trigger for revisiting whether
    *o11y* should hand-write metrics; R6 is the request that upstream become
    that library, so citing it here is circular. (Noted because it was applied
    that way once.)
  - *"a concrete need for client-attributed JetStream consumer-lag"* —
    **not met**; lag lives in `consumer.Info()` / `/jsz` and is explicitly out
    of scope for a client-side instrument set.
- **Demand**: no committed consumer today. The internal proposal that prompted
  this analysis marks itself `OPTIONAL` because the RPC SLOs it cares about are
  already covered app-side by a router middleware — but that is a statement
  about one service's needs, not about the platform-wide value, which the same
  proposal argues for. The blocker here is a decision, not effort.
- **Friction**: high — a genuine feature with API-surface and cardinality
  decisions (which attributes become metric labels). Issue-first.

#### R7. Upstream `Fetch` / `FetchBytes` take no ctx (asymmetric with `Next`)

- **What**: on the **upstream `oteljetstream.Consumer` interface**, v0.7.0 wired
  ctx into `Consumer.Next(ctx, opts...)` via `jetstream.FetchContext`, including
  the `FetchContext`/`FetchMaxWait` mutual-exclusion handling, but left
  `Fetch(batch, opts...)` and `FetchBytes(maxBytes, opts...)` with **no ctx
  parameter at all**. (The o11y facade's own `Fetch`/`FetchBytes` do take ctx
  and honor it — that is exactly the compensation described below.)
  Request: add ctx to both upstream methods, reusing the `Next` implementation.
- **Evidence**: v0.7.0 `oteljetstream/consumer.go:81-83` — `Next(ctx context.Context, …)`
  sits directly above `Fetch(batch int, …)` / `FetchBytes(maxBytes int, …)`.
  The logic to copy already exists at `oteljetstream/consumer_direct.go:77`
  (`applyCtxToFetchOpts`).
- **o11y-side cost today**: `nats/jetstream.go` carries
  `fetchWithCtxFallback` + `fetchOptsWithCtx` + `isFetchMaxWaitCollision` —
  14 lines of logic wrapped in ~50 lines with the rationale comments, plus
  dedicated tests — that exist *solely* to compensate, and are near-duplicates
  of upstream's own `Next` helper.
- **Unlocks**: deletes those three helpers and their tests from the facade;
  direct upstream users get cancelable batch pulls for free.
- **This is NOT additive — plan the shape before proposing it.** An earlier
  draft called this "low friction, additive, the maintainer already shipped the
  equivalent for `Next`". That was wrong on both counts. The v0.6.0 and v0.7.0
  `oteljetstream.Consumer` interfaces are **byte-identical**: `Next` already
  took a ctx in v0.6.0, so v0.7.0 changed only its *implementation* (wiring
  `FetchContext`) and needed no signature change at all. Adding ctx to `Fetch` /
  `FetchBytes` is a different thing — it changes their arity, so every direct
  caller fails to compile and every external implementation of `Consumer` stops
  satisfying the interface. Two viable shapes:
  1. **Additive**: new `FetchWithContext(ctx, batch, opts...)` /
     `FetchBytesWithContext(ctx, maxBytes, opts...)`, leaving the existing
     methods untouched. No breakage; costs API surface and a naming decision.
  2. **Breaking, documented**: change the signatures in a minor bump, which the
     upstream `VERSIONING.md` pre-1.0 policy permits (breaking → minor), with a
     CHANGELOG migration note. Cleaner long-term surface; forces a coordinated
     upgrade.
- **Friction**: low-medium. The *implementation* is near-mechanical (the `Next`
  ctx-wiring already exists to copy), but the API-shape decision above needs
  the maintainer's call, so this is issue-first rather than PR-first.

#### R2. `RequestWithTimeout(ctx, subject, data, timeout)` (name TBD)

- **What**: the API mirrors `nats.Conn`'s shape, so the primary `Request`
  takes a `timeout` but no ctx (its span parents to `context.Background()`),
  and `RequestWithContext` takes a ctx but no timeout. Add one additive method
  carrying both, as the recommended traced entry point.
- **Evidence**: v0.7.0 `otelnats/conn_traced.go` — `Request(subject, data,
  timeout)` vs `RequestWithContext(ctx, subject, data)`; no method takes both.
- **Tracked in**: [#72](https://github.com/flywindy/o11y/issues/72).
- **Unlocks**: `o11y/nats.Conn.Request` drops its `context.WithTimeout` shim.
  (Low urgency for o11y — the shim fully hides this from SDK users.)
- **Friction**: low-medium — additive, but naming needs the maintainer's
  API-mirror philosophy applied.

#### R4. Caller-attribute hook on the reply-receive span

- **What**: `recordReply` emits the requester-side reply-receive span but
  offers no way for callers to attach domain attributes (request/correlation
  ID, room/site ID). o11y's pre-0.6.0 reply-link span had a variadic `attrs`
  parameter for exactly this; it was removed in the v0.6.0 upgrade because
  keeping the facade span would duplicate the upstream one. Propose an option
  or per-call hook (SDK-owned keys protected from collision, last-write-wins
  ordering documented).
- **Evidence**: v0.7.0 `otelnats/conn_traced.go` `recordReply` — no caller
  attribute path.
- **Unlocks**: restores the searchable-domain-identifier capability the facade
  had to drop (documented as a migration note in `nats/conn.go` and the guide).
- **Friction**: medium — API-shape discussion.

#### R8. Export message-level `Inject` / `Extract` helpers

- **What**: v0.7.0 exports `HeaderCarrier` but no convenience pair for a raw
  `*nats.Msg` — every consumer that touches an un-instrumented NATS path has to
  hand-roll `prop.Inject(ctx, &otelnats.HeaderCarrier{H: msg.Header})` plus the
  nil-header guard. Request: `Inject(ctx, prop, msg)` / `Extract(ctx, prop, msg)`.
- **Evidence**: v0.7.0 `otelnats/propagation.go` exports the carrier only.
- **o11y-side cost today**: `nats/middleware.go` implements exactly this pair
  (public `Inject`/`Extract`) for the legacy `nats.JetStreamContext` API, which
  the wrapper deliberately does not cover.
- **Friction**: low — thin, additive, no behavior change.

#### R10. Carry the per-message dispatch decision on `otelnats.Msg`

- **What**: since v0.8.0 the tracing switch is resolved per operation.
  `Conn.msgHandler` evaluates `c.gate.tracing()` to choose the traced or direct
  handler, but `otelnats.Msg` carries only `Msg` and `Ctx`, so a wrapper cannot
  learn which path ran. Request: expose the decision on the message (a `Traced
  bool` field, or an accessor), so a consumer reads the dispatch that actually
  happened instead of recomputing it.
- **Evidence**: v0.8.0 `otelnats/conn.go:27-33` (the `Msg` type) and `:84-98`
  (`msgHandler`, which resolves the gate per message and hands off to one of two
  pre-built handlers).
- **o11y-side cost today**: `nats/conn.go`'s `restoreBaggage` must call
  `nc.TracingEnabled()`, which routes through `Conn.impl()` and so re-resolves
  the same gate. Two consequences: a relay-capable process pays **two extra
  OpenFeature evaluations per delivered message** on top of upstream's own two;
  and the two resolutions are independent, so a relay flip landing between them
  makes them disagree — restoring wire baggage on a message upstream delivered
  untraced, or dropping it on one upstream traced.
- **Why the obvious workaround does not work**: the direct path hands back
  `context.Background()` and the traced path a span context, so
  `trace.SpanContextFromContext(m.Ctx).IsValid()` discriminates them — but only
  while the `TracerProvider` is real. With the SDK's trace pillar off the
  provider is a noop, the traced path's span context is invalid too, and the
  signal collapses. A wrapper cannot distinguish "upstream went direct" from
  "upstream traced into a noop provider" without help from upstream.
- **Friction**: low — additive field or accessor, no behavior change.

#### R9. KeyValue / ObjectStore wrapping (deferred)

- **What**: the wrapper explicitly does not re-expose JetStream's KeyValue and
  ObjectStore surfaces, so any consumer using them drops to the native client
  and loses trace propagation.
- **Evidence**: v0.7.0 `oteljetstream/jetstream.go:96` documents the omission.
- **Status**: **not requested yet** — no o11y consumer needs KV/ObjectStore
  today. Recorded so the gap is known if one does.
- **Friction**: high — a large new surface.

### Fixes

#### F7. `MessageBatch` channel is unbuffered — abandoning without `Stop` parks the goroutine

- **What**: `newTracedMessageBatch` forwards onto `ch := make(chan Msg)` — an
  **unbuffered** channel. v0.7.0 correctly fixed `Stop()` to be observed while
  the goroutine is parked on either side, but a caller that simply stops
  reading `Messages()` *without* calling `Stop` still leaves the forwarding
  goroutine blocked on the send forever, holding its NATS pull subscription.
  Request: size the buffer to the request's own message-count bound where one
  exists (`Fetch`/`FetchNoWait` both take an explicit `batch` cap).
- **Evidence**: v0.7.0 `oteljetstream/consumer.go:197`.
- **o11y-side status**: worked around in `wrapMessageBatch`, which buffers to
  exactly that bound so the goroutine always drains and exits on its own. The
  upstream fix is still wanted so direct upstream users get the same guarantee.
- **Friction**: low — a one-line channel-capacity change for the bounded cases.

#### F8. CHANGELOG mis-describes the `Consume` span-kind change (docs)

- **What**: the v0.7.0 CHANGELOG's BREAKING entry reads "reply-receive and
  JetStream pull-consume (`Consume`/`Fetch`/`Messages`) spans are now `CLIENT`
  (were `CONSUMER`)". `Consume` did **not** change — `tracedConsumeHandler`
  still starts its `process` span with `trace.SpanKindConsumer`, which is
  correct. Only the pull-*receive* paths (`Next`/`Messages`/`Fetch`/
  `FetchBytes`/`FetchNoWait`) and the reply-receive span moved to `CLIENT`.
- **Evidence**: v0.7.0 `oteljetstream/consumer_traced.go:159`
  (`SpanKindConsumer`, reached from `Consume` at :23-25) vs the CHANGELOG text.
- **Impact**: this is a dashboard-breaking instruction. Anyone who follows it
  and moves their `Consume` filters from `CONSUMER` to `CLIENT` silently loses
  every durable-consumer processing span. It already propagated into this SDK's
  docs before an automated review caught it.
- **Friction**: trivial — a CHANGELOG wording fix, high leverage.

#### F9. Upstream `Consume` / `Messages` accept no ctx

- **What**: on the **upstream `oteljetstream.Consumer` interface** neither
  method takes a `context.Context`, so there is no way to pass a base context
  (deadline, cancellation, or ambient values) into the consume loop;
  per-message context comes only from the message headers. (The o11y facade's
  `Consume`/`Messages` do take ctx — see the status note below for what it
  does and does not do.)
- **Evidence**: v0.7.0 `oteljetstream/consumer.go:79-80`.
- **o11y-side status**: the facade accepts ctx and uses it as a
  registration-time guard only, documenting that it does not stop a running
  consume (`ConsumeContext.Stop`/`Drain` do).
- **Friction**: medium — the native nats.go API has the same shape, so this
  needs the maintainer's view on how far to diverge from the mirror.

### Discussion items (no code attached yet)

#### D2. Old-namespace protection after the module-path cutover

`go get` of pre-0.6.0 versions still resolves through the old
`Marz32onE/instrumentation-go` path (GitHub redirect + module-proxy cache).
That redirect holds only while the old namespace is never re-registered.
Confirm the `Marz32onE` account will be retained (never deleted, repo name
never recreated) so the historical import path cannot be squatted — a
supply-chain concern for anyone still pinned below v0.6.0.

---

## o11y-side follow-ups (no upstream dependency)

Cleanups this SDK can do on its own, independent of the items above:

- **`nats/middleware.go`'s `headerCarrier` is now duplicative.** It was written
  because upstream's carrier was exact-case-only with no `Values`. v0.7.0's
  `otelnats.HeaderCarrier` implements `propagation.ValuesGetter` and falls back
  to MIME-canonical *and* case-folded lookups — a superset of this SDK's
  behavior. The local carrier can shrink to a thin delegate (or be dropped in
  favor of the upstream type), keeping the public `Inject`/`Extract` signatures
  unchanged. See also R8, which would remove the remaining wrapper entirely.

## Engagement plan

The consumer-path fixes (F1–F6), the configuration surface (R1), the deliver-
span removal (R3), and the batch span lifecycle (R5) all landed in v0.7.0, so
the original multi-bundle plan is complete. The post-upgrade audit reopened a
new, smaller set:

1. **Bundle D — quick wins** (F8, R7): F8 is a CHANGELOG wording fix that
   prevents downstream dashboard breakage; R7 is a near-mechanical port of the
   `Next` ctx wiring to `Fetch`/`FetchBytes`. Both are low-friction and can go
   out together as one PR with tests.
2. **Bundle E — metrics** (R6): the largest remaining gap and the only one that
   blocks an ADR-level decision here (ADR 0004's deferred metrics scope).
   Issue-first — it needs agreement on the instrument set, the
   `WithMeterProvider` option shape, and which attributes become labels.
3. **Bundle F — leak + ergonomics** (F7, R8, F9): small, independent, can ride
   whichever bundle lands first.
4. **Bundle C — request/reply ergonomics** (R2, R4): issue-first; PR after ack.
   Both touch the request/reply API contract, so they ride the same discussion.
5. **R9** (KeyValue/ObjectStore) stays unrequested until a consumer needs it.
6. **D2** rides an umbrella/discussion issue until a written retention
   commitment exists.

## o11y issue cross-reference

| o11y issue | Status after the v0.7.0 upgrade | Action |
|---|---|---|
| [#69](https://github.com/flywindy/o11y/issues/69) | Fully resolved (parts 1–2 in v0.6.0, part 3 / consumer.group.name in v0.7.0) | **Closed** |
| [#70](https://github.com/flywindy/o11y/issues/70) | Resolved — deliver spans removed entirely in v0.7.0 | **Closed** |
| [#71](https://github.com/flywindy/o11y/issues/71) | Resolved (part 1 in v0.6.0, part 2 / `WithTracingEnabled` in v0.7.0) | **Closed** |
| [#72](https://github.com/flywindy/o11y/issues/72) | Upstream unfixed (→ R2); o11y facade shim shipped, SDK users unaffected | Open — mitigation status commented |
| [#73](https://github.com/flywindy/o11y/issues/73) | Resolved — CHANGELOG + `VERSIONING.md` + release-tag CI guard in v0.7.0 | **Closed** |

The seven items added by the 2026-07-16 post-upgrade audit (R6–R9, F7–F9) have
no o11y issues yet; open them only if/when a bundle above is actually taken to
upstream, to avoid a second stale tracking layer.
