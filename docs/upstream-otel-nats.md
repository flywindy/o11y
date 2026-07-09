# Upstream otel-nats — fixes, feature requests, and discussion items

**Upstream**: `github.com/akira-core/instrumentation-go` (`otel-nats` module;
repo and module path renamed from `Marz32onE` in v0.6.0)
**o11y pin**: v0.6.0 (see ADR 0004, 2026-07-09 amendment, for the audit)
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

## Resolved by upstream v0.6.0 (for the record)

| Item | Was tracked in | Resolution |
|---|---|---|
| semconv regression v1.39.0 → v1.37.0 | [#73](https://github.com/flywindy/o11y/issues/73) | Restored to v1.39.0 |
| `Version()` shipped stale ("0.4.1" in v0.5.0) | [#73](https://github.com/flywindy/o11y/issues/73) | Correct in v0.6.0 and guarded by `version_test.go` |
| Release tags not on `main` | (found during upgrade) | `main` == `otel-nats/v0.6.0` tag |
| Propagation gate (`OTEL_NATS_PROPAGATION_ENABLED`) defaulting OFF — silent cross-service trace fragmentation | [#71](https://github.com/flywindy/o11y/issues/71) (part 1) | Gate removed entirely; inject/extract follow the tracing gate unconditionally |
| `Consumer.Next` returned the producer's remote ctx instead of the receive-span ctx | [#69](https://github.com/flywindy/o11y/issues/69) (part 1) | Fixed; facade now wraps `Next` |
| `ConsumeContext` narrowed to `Stop()` only | [#69](https://github.com/flywindy/o11y/issues/69) (part 2) | Mirrors native `Stop`/`Drain`/`Closed`; facade interface widened |
| `MessageBatch` had no abandon/cancel escape hatch | (documented limitation in `nats/jetstream.go`) | Upstream added `Stop()`; facade propagates it |
| Requester-side reply-receive span missing (o11y carried its own `linkReply`) | ADR 0022 amendment 2026-07-01 | Upstream `recordReply` emits it; facade span deleted. Note the topology change: the span lands in the responder's trace (remote parent + link), not the requester's |

---

## Open items

Ordered roughly by (value ÷ expected review friction). "Unlocks" describes the
o11y-side code or docs that can be deleted/simplified once the item ships in an
upstream release.

### Fixes

#### F1. `HeaderCarrier`: add `ValuesGetter` and a MIME-canonical fallback

- **What**: port o11y's `nats/middleware.go` carrier semantics into
  `otelnats.HeaderCarrier`: (a) implement `propagation.ValuesGetter` so
  multi-instance `baggage` headers are not silently truncated to the first
  value; (b) `Get`/`Values` look up the verbatim key first, then fall back to
  `textproto.CanonicalMIMEHeaderKey` form, so messages written by producers
  that canonicalize header keys (including pre-fix o11y versions, for messages
  still sitting in durable streams) keep their trace link.
- **Evidence**: v0.6.0 `otelnats/propagation.go` — exact-case `Get` only, no
  `Values`, no fallback.
- **Unlocks**: deletes the "Known limitation" block in `nats/jetstream.go`'s
  package doc (canonical-header extraction gap on all JetStream consume
  paths, ADR 0022 amendment 2026-07-03) and the matching guide/semconv notes;
  `nats/middleware.go` shrinks to a thin delegate.
- **Friction**: low — additive, behavior-preserving for all current writers,
  easy to test.

#### F2. `messaging.consumer.name` is not a semconv v1.39.0 key

- **What**: the literal `messaging.consumer.name` is attached to every
  JetStream consumer span but does not exist in the pinned semconv (only
  `messaging.consumer.group.name` does). Either move it to an explicitly
  library-owned namespace (e.g. `nats.consumer.name`) or drop it until the
  messaging semconv stabilizes a consumer-name key.
- **Evidence**: v0.6.0 `oteljetstream/consumer.go:89`.
- **Tracked in**: [#69](https://github.com/flywindy/o11y/issues/69) part 3
  (the only unresolved part).
- **Unlocks**: removes the deviation entry in `docs/semconv.md`.
- **Friction**: low code-wise; needs the maintainer's naming preference.

### Feature requests

#### R1. Per-Conn `WithTracing` / `WithPropagation` options (env gates stay as defaults)

- **What**: tracing is controlled solely by two process-global env vars
  (`OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` AND `OTEL_NATS_TRACING_ENABLED`,
  default **off**), latched by a `sync.Once` at the first `Connect`. Add
  per-Conn options that override the env default, mirroring how
  `WithTracerProvider`/`WithPropagators` already work. Keep the env gates and
  their default untouched (the maintainer's documented default-OFF posture).
- **New evidence from the v0.6.0 upgrade**: v0.5.1's exported
  `ResetGatesForTest()` was **removed** in v0.6.0, so a downstream test suite
  cannot control the gates at all once any test has dialed a connection —
  o11y's own tests had to move the env setup into `TestMain`
  (`nats/conn_test.go`). A per-Conn option makes both production config and
  tests explicit without process-global state.
- **Tracked in**: [#71](https://github.com/flywindy/o11y/issues/71) part 2.
- **Unlocks**: o11y `nats.Connect` can pass `WithTracing(true)` derived from
  the SDK's own toggle instead of requiring every deployment to export two
  env vars (removes the export blocks in `examples/README.md` / `AGENTS.md`
  and a real production footgun); test `TestMain` workaround goes away.
- **Friction**: medium — touches the maintainer's feature-flag design; open
  the design discussion in an issue first.

#### R2. `RequestWithTimeout(ctx, subject, data, timeout)` (name TBD)

- **What**: v0.5.x+ mirrors `nats.Conn`'s API shape, so the primary `Request`
  takes no ctx (its span parents to `context.Background()`) and
  `RequestWithContext` takes no timeout. Add one additive method carrying
  both, as the recommended traced entry point.
- **Tracked in**: [#72](https://github.com/flywindy/o11y/issues/72).
- **Unlocks**: `o11y/nats.Conn.Request` drops its `context.WithTimeout` shim.
  (Low urgency for o11y — the shim fully hides this from SDK users.)
- **Friction**: low-medium — additive, but naming needs the maintainer's
  API-mirror philosophy applied.

#### R3. Deliver spans: explicit opt-in and caller-provider routing

- **What**: when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, `Connect` implicitly
  builds a second, independent `TracerProvider` with its own OTLP exporter
  and **no sampler** (effectively AlwaysSample) for synthetic "deliver"
  spans. Requested: (a) explicit opt-in (e.g. `WithDeliverSpans(...)`)
  decoupled from the app's own exporter env var; (b) route deliver spans
  through the caller-injected TracerProvider, or at least accept a sampler.
- **Evidence**: v0.6.0 `otelnats/conn.go:183-230` (`initNATSProvider`,
  unchanged from v0.2.11).
- **Tracked in**: [#70](https://github.com/flywindy/o11y/issues/70).
- **Unlocks**: removes the sampling-inconsistency and second-exporter caveats
  in ADR 0003's approved-integrations row and ADR 0004; deliver spans become
  adoptable (today o11y deployments must simply avoid the trigger env var).
- **Friction**: high — a design change; issue-first, PR only after direction
  is agreed.

#### R4. Caller-attribute hook on the reply-receive span

- **What**: upstream `recordReply` (new in v0.6.0) emits the requester-side
  reply-receive span but offers no way for callers to attach domain
  attributes (request/correlation ID, room/site ID). o11y's pre-0.6.0
  reply-link span had a variadic `attrs` parameter for exactly this; it was
  removed in the upgrade because keeping the facade span would duplicate the
  upstream one. Propose an option or per-call hook (with the SDK-owned keys
  protected from collision, last-write-wins ordering documented).
- **Unlocks**: restores the searchable-domain-identifier capability the
  facade had to drop (documented as a migration note in `nats/conn.go` and
  the guide).
- **Friction**: medium — API-shape discussion.

#### F3. `Consumer.Next`: honor live ctx cancellation, not just the deadline

- **What**: `tracedConsumer.Next` converts a ctx deadline to
  `jetstream.FetchMaxWait` (`applyCtxDeadlineToFetchOpts`) but wires no
  `jetstream.FetchContext`, so cancelling a deadline-less ctx mid-wait does
  not abort the pull — only an up-front `ctx.Err()` check runs. Wire the ctx
  itself (with the same FetchMaxWait-collision care the o11y facade applies
  on Fetch/FetchBytes).
- **Evidence**: v0.6.0 `oteljetstream/consumer_traced.go` (`Next` →
  `applyCtxDeadlineToFetchOpts`).
- **Unlocks**: the facade `Consumer.Next` doc can drop its "deadline yes,
  cancellation no" caveat.
- **Friction**: low-medium — behavior fix, needs the FetchContext/FetchMaxWait
  mutual-exclusion handling.

#### F4. Batch forwarding goroutine: select `done` on the receive side too

- **What**: `newTracedMessageBatch` / `newDirectMessageBatch` check their
  `done` channel only around the send; a goroutine parked on the
  `raw.Messages()` receive (batch still waiting for messages) does not
  observe `Stop()` until the native channel produces or closes. The o11y
  facade fixed the identical pattern in its own wrapper
  (`nats/jetstream.go` `wrapMessageBatch`); the same nested-select fix
  applies upstream.
- **Evidence**: v0.6.0 `oteljetstream/consumer.go` (`for msg := range
  raw.Messages()` with `select` only on the send).
- **Unlocks**: `MessageBatch.Stop()` becomes promptly effective end to end
  (today the upstream leg can lag until the native pull expires).
- **Friction**: low — mechanical, mirrors a fix already proven in the facade.

#### R5. Batch receive-span lifecycle: end at handover

- **What**: `newTracedMessageBatch` (and `MessagesContext.Next`) end message
  N's receive span only when message N+1 is read, so
  `trace.SpanFromContext(m.Ctx).SetAttributes(...)` is a silent no-op for
  most messages in a batch. Ending the span at handover (send on the
  channel / return from `Next`) — or documenting a processing-scope hook —
  would make per-message enrichment reliable.
- **Evidence**: v0.6.0 `oteljetstream/consumer.go` (`lastSpan` pattern).
- **Unlocks**: deletes the span-lifecycle warning blocks in
  `nats/jetstream.go` (`MessageBatch` doc) and `docs/guide.md`.
- **Friction**: medium — changes emitted span durations; needs the
  maintainer's view on what the receive span should measure.

### Discussion items (no code attached yet)

#### D1. CHANGELOG location and semver policy

The module-level `CHANGELOG.md` present in v0.5.1 is absent from the v0.6.0
module zip. Ask where release notes live going forward, and request a short
written semver policy for the 0.x line (breaking changes → minor bump), so
downstream pins can be managed on the CHANGELOG alone. Remainder of
[#73](https://github.com/flywindy/o11y/issues/73).

#### D2. Old-namespace protection after the module-path cutover

`go get` of pre-0.6.0 versions still resolves through the old
`Marz32onE/instrumentation-go` path (GitHub redirect + module-proxy cache).
That redirect holds only while the old namespace is never re-registered.
Confirm the `Marz32onE` account will be retained (never deleted, repo name
never recreated) so the historical import path cannot be squatted — a
supply-chain concern for anyone still pinned below v0.6.0.

#### D3. `Version()` tag-consistency automation

v0.6.0 guards the version constant with a unit test (`version_test.go`), which
prevents const/test drift but not const/tag drift. A small CI step comparing
the constant against the release tag would close the remaining gap from
[#73](https://github.com/flywindy/o11y/issues/73). Offer to contribute it.

---

## Engagement plan

1. **Umbrella issue** on the upstream repo: link this document's items, state
   the intent to contribute, and ask the two blocking preferences (PR
   granularity; R1/R3 design direction).
2. **Bundle A — consumer-path fixes** (F1, F2, F3, F4): low-friction PR with
   tests; can go out immediately after the umbrella issue.
3. **Bundle B — configuration surface** (R1): issue-first; PR after ack.
4. **Bundle C — request/reply API** (R2, R4): issue-first; PR after ack.
5. R3, R5, D1–D3 ride the umbrella issue discussion until a direction exists.

## o11y issue cross-reference

| o11y issue | Status after the v0.6.0 upgrade | Action |
|---|---|---|
| [#69](https://github.com/flywindy/o11y/issues/69) | Parts 1–2 resolved; part 3 open (→ F2) | Re-scope to part 3 or close in favor of a new narrow issue |
| [#70](https://github.com/flywindy/o11y/issues/70) | Open (→ R3); o11y-side ADR documentation done | Keep open |
| [#71](https://github.com/flywindy/o11y/issues/71) | Part 1 resolved; part 2 open (→ R1, with new reset-hook evidence) | Update body, keep open |
| [#72](https://github.com/flywindy/o11y/issues/72) | Upstream unfixed (→ R2); o11y facade shim shipped, SDK users unaffected | Comment with mitigation status, keep open |
| [#73](https://github.com/flywindy/o11y/issues/73) | Mostly resolved in v0.6.0; remainder → D1, D3 | Update body, keep open (narrowed) |
