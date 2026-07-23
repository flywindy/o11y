# Upstream otel-nats — fixes, feature requests, and discussion items

**Upstream**: `github.com/akira-core/instrumentation-go` (`otel-nats` module;
repo and module path renamed from `Marz32onE` in v0.6.0)
**o11y pin**: v0.7.0 (see ADR 0004, 2026-07-16 amendment, for the audit)
**Contribution model**: `flywindy/instrumentation-go` is a PR workspace only —
its `main` tracks upstream `main`, every change goes to upstream as a PR, and
o11y always imports the upstream module. Hard-forking (module rename, own
releases) is the documented fallback, to be triggered only if collaboration
stalls on a critical fix.

This document is the single place that tracks what we want changed upstream,
why, and what each change unlocks in this SDK. Update it on every upstream
release audit and whenever an item lands, and keep the linked o11y issues in
sync.

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
| Tracing controlled solely by two process-global env vars (default OFF); no per-Conn override; `ResetGatesForTest` removed in v0.6.0 | **R1** / [#71](https://github.com/flywindy/o11y/issues/71) part 2 | Added `WithTracingEnabled(v bool) Option`, overriding the env default in either direction per `Conn`. `o11ynats.Connect` now passes `WithTracingEnabled(true)`; removed the two-env-var setup from `examples/README.md`, `AGENTS.md`, tests' `TestMain`, and `docs/guide.md` |
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

Only three items remain after v0.7.0. Ordered by (value ÷ expected review
friction).

### Feature requests

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

### Discussion items (no code attached yet)

#### D2. Old-namespace protection after the module-path cutover

`go get` of pre-0.6.0 versions still resolves through the old
`Marz32onE/instrumentation-go` path (GitHub redirect + module-proxy cache).
That redirect holds only while the old namespace is never re-registered.
Confirm the `Marz32onE` account will be retained (never deleted, repo name
never recreated) so the historical import path cannot be squatted — a
supply-chain concern for anyone still pinned below v0.6.0.

---

## Engagement plan

The consumer-path fixes (F1–F6), the configuration surface (R1), the deliver-
span removal (R3), and the batch span lifecycle (R5) all landed in v0.7.0, so
the original multi-bundle plan is largely complete. What remains:

1. **Close/narrow the o11y tracking issues** per the cross-reference below now
   that v0.7.0 is pinned.
2. **Bundle C — request/reply ergonomics** (R2, R4): issue-first; PR after ack.
   Both touch the request/reply API contract, so they ride the same discussion.
3. **D2** rides an umbrella/discussion issue until a written retention
   commitment exists.

## o11y issue cross-reference

| o11y issue | Status after the v0.7.0 upgrade | Action |
|---|---|---|
| [#69](https://github.com/flywindy/o11y/issues/69) | Fully resolved (parts 1–2 in v0.6.0, part 3 / consumer.group.name in v0.7.0) | Close |
| [#70](https://github.com/flywindy/o11y/issues/70) | Resolved — deliver spans removed entirely in v0.7.0 | Close |
| [#71](https://github.com/flywindy/o11y/issues/71) | Resolved (part 1 in v0.6.0, part 2 / `WithTracingEnabled` in v0.7.0) | Close |
| [#72](https://github.com/flywindy/o11y/issues/72) | Upstream unfixed (→ R2); o11y facade shim shipped, SDK users unaffected | Keep open, comment with mitigation status |
| [#73](https://github.com/flywindy/o11y/issues/73) | Resolved — CHANGELOG + `VERSIONING.md` + release-tag CI guard in v0.7.0 | Close |
