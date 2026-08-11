# ADR 0025 — Application-Defined Baggage Attributes

**Status**: Accepted (implemented)
**Date**: 2026-08-05
**Relates to**: ADR 0016 (user identity — this ADR generalizes the fixed
whitelist that ADR 0016 §Implementation specifics 3 deliberately left closed,
and amends one of its behaviors), ADR 0003 (global-state policy — the whitelist
must become instance state, not package state), ADR 0006 (semconv — application
keys have no semconv constant to source from), ADR 0015 (sampling — span
attributes only record on sampled spans), ADR 0022 §4 (domain identifiers belong
to the application, not the SDK), ADR 0023 (span naming — high-cardinality
identifiers must never enter span names), ADR 0024 (telemetry must never be
load-bearing), `docs/semconv.md` (attribute catalog)

---

## Context

ADR 0016 shipped baggage-driven attribute materialization for exactly one key,
`user.name`, and closed the door behind it:

> **Whitelist** — start with `["user.name"]`; expanding it is a conscious change
> (header-size and PII review), not an open door.
> — ADR 0016, Implementation specifics §3

That conscious change is now being requested. A consuming product (a chat
service split across an HTTP edge, NATS RPC services, and JetStream workers)
wants a room identifier to appear on the spans and log records of every service
that handles a request, having been identified once at the entry point. The same
shape recurs for a site identifier and a request identifier, and the same shape
will recur for other products with other identifiers.

The mechanism ADR 0016 built is already exactly right for this. What is wrong is
that it is welded to a single key:

```go
// internal/baggageattrs/baggageattrs.go:25
var baggageWhitelist = []whitelistedAttribute{
	{baggageKey: UserNameKey, attributeKey: semconv.UserNameKey},
}
```

A package-level `var` with one entry, no configuration surface, and a matching
hard-coded `hasUserNameAttr bool` in the log handler
(`internal/log/handler.go:57`). An application cannot extend it, and cannot
reimplement it either — see "Dependency behavior" below.

### The framing question: whose requirement is this?

The obvious wrong answer is to add a second hard-coded entry for the requesting
product's key. That would put a chat-domain concept into a general-purpose
observability SDK, and would guarantee a third request, and a fourth.

The correct framing is a **mechanism/policy split**:

- **Mechanism — the SDK's.** "Carry an opaque key/value across service
  boundaries, and materialize it onto this service's spans and log records."
  This is key-agnostic. It requires SDK-internal hooks (see below) and is
  therefore *only* implementable inside the SDK.
- **Policy — the application's.** "The key is `chat.room.id`; the value comes
  from the route parameter; it is set after authorization; it is removed rather
  than trusted at the edge." None of this is knowable by the SDK, and none of it
  should be.

This is the same line ADR 0022 §4 already drew for span attributes — *domain
identifiers the SDK has no way to know (a room ID, a site ID, a request ID from
the payload)* — extended from the single-span case to the cross-service case.

`user.name` is the standing exception, and stays one for a stated reason: it has
a pinned semconv constant (`semconv/v1.39.0/attribute_group.go:15500`,
`UserNameKey = attribute.Key("user.name")`), so centralizing it in the SDK is
what makes a future semconv rename a one-place edit (ADR 0006). Application keys
have no such constant and gain nothing from living in the SDK.

---

## Dependency behavior (verified)

Verified against the pinned dependencies at the time of writing
(`go.opentelemetry.io/otel v1.44.0`, `go.opentelemetry.io/otel/sdk v1.44.0`,
`semconv/v1.39.0`) and the SDK tree on this branch. Items 8–12 are the
substantive findings from review; they materially changed this ADR, and §12
changed what it has to deliver.

1. **Registering a SpanProcessor from the application works in some
   configurations and silently stops working in others — it is not a contract.**
   `Init` exposes no `WithSpanProcessor` and `tracerProviderInternal` is
   unexported (`o11y.go:86`), but `SDK.TracerProvider()` returns an
   `oteltrace.TracerProvider` *interface* (`o11y.go:99`) whose dynamic type is
   the concrete `*sdktrace.TracerProvider` — and that type has an exported
   `RegisterSpanProcessor` (`sdk/trace/provider.go:202`). So a type assertion
   succeeds, and the hook is reachable, **whenever profiling is not running**.
   That is the default: `O11Y_PROFILING_ENABLED` parses with a `false` default
   (`o11y.go:581`), and the public provider is replaced by
   `otelpyroscope.NewTracerProvider(tpInternal)` only after `profiling.Start`
   succeeds (`o11y.go:387`).

   The failure mode is what disqualifies it: the assertion returns `ok == false`
   the moment a service turns profiling on *and it starts successfully* — so an
   application built on it loses all span materialization from a change that has
   nothing to do with baggage, with no compile-time signal. (A configured
   profiler that fails to start only warns and leaves the concrete provider in
   place, `o11y.go:376-387`, so the assertion keeps working — which makes the
   breakage depend on whether Pyroscope happened to be reachable at boot, an
   even worse property than breaking outright.) Building a second
   TracerProvider means abandoning the SDK, and reaching its provider through
   `otel.SetTracerProvider` is forbidden by ADR 0003. **Not impossible;
   unsupported as a stable contract**, which is the actual argument for putting
   the hook behind an SDK option.

2. **The application can wrap the log chain, but only from the outside.** The
   chain is assembled and closed over inside `Init` (`o11y.go:262-320`), so
   there is no seam *within* it. But `obs.Logger` is a `*slog.Logger`, and
   `slog.Logger.Handler()` is exported, so an application can build
   `slog.New(myEnricher{obs.Logger.Handler()})` and get log-only baggage
   enrichment today — at the same position the SDK's own BaggageHandler occupies
   (`o11y.go:309-311`, outermost). This is a supported path and this ADR does not
   claim otherwise.

   What it does not give is the span half (§1) or a single switch that keeps
   spans and logs consistent. An application taking the log-only route must also
   keep its enricher's key list in sync with whatever it does for spans, which
   is the coupling `WithBaggageAttributes` removes.

3. **Baggage transport already works wherever the propagator runs — with one
   documented exception.** The composite propagator
   (`propagation.TraceContext{}` + `propagation.Baggage{}`) is built in
   `internal/trace/trace.go` and mirrored on the trace-disabled path
   (`o11y.go:203`), so disabling the *trace pillar* still forwards baggage.
   HTTP via `otelhttp`/`otelgin` carries baggage end to end today with nothing
   to change. NATS carries it **on the wire but drops it at the consumer**, for
   a separate reason — see §12, the finding that most affects this ADR's value.

   The exception is an effectively disabled NATS connection on the upstream
   direct path: no spans, **no propagation injection/extraction**, and
   JetStream wrappers backed by the direct implementation. Since otel-nats
   v0.8.0, `WithTracingEnabled(false)` supplies only the local default; the
   direct path is fixed only when no relay is available and no upstream env
   overrides it. In that state baggage is not even put on the wire, so unlike
   §12 there is nothing for the facade to restore. Recorded as a scope
   limitation in Decision §9: this feature requires the effective traced NATS
   path, and a service on the direct path keeps whatever per-service explicit
   tagging it already does.

4. **The materialization logic is already key-agnostic.**
   `SpanAttributesFromContext` (`baggageattrs.go:59`),
   `LogAttrsFromContext` (`baggageattrs.go:79`), and the `OnStart` de-duplication
   loop all iterate `baggageWhitelist` without knowing what is in it. Only the
   *source* of the list, and the log handler's `hasUserNameAttr` shortcut, are
   key-specific.

5. **There is one supported escape hatch, and it is not good enough.**
   `WithTraceSampler(sdktrace.Sampler)` (`options.go:170`) is passed through to
   `sdktrace.WithSampler` (`internal/trace/trace.go:53-54`) and is not clobbered
   by `configureSampler` (which only acts when `samplingRatioSet`). A custom
   sampler receives `SamplingParameters.ParentContext`
   (`sdk/trace/sampling.go:34`) and may return `SamplingResult.Attributes`
   (`sampling.go:62`), which the SDK applies to the span at creation
   (`sdk/trace/tracer.go:172`). So an application can, today, read baggage in a
   wrapping sampler and stamp every span. This works, and is a legitimate
   short-term bridge, but it is not the answer: it conflates sampling with
   enrichment, occupies the single `WithTraceSampler` slot, runs on every
   span-start hot path in application-owned code, and — decisively — **does
   nothing for logs.**

6. **Baggage is a bounded, shared budget.** `baggage.go:21-22` caps baggage at
   64 members and 8192 bytes total across the whole header, shared by every
   producer in the process.

7. **Sampling asymmetry (ADR 0015).** `span.SetAttributes` is a no-op on a
   non-recording span, and `SpanProcessor.OnStart` only fires for recording
   spans. So span-side materialization tracks the head sampling rate, while
   **baggage propagation and log materialization are sampling-independent** —
   independent of the sampling *ratio*, that is, not of tracing being wired at
   all: the native NATS mode in §3 removes the Inject/Extract that carries
   baggage in the first place. For
   a "find everything about identifier X" workflow this makes the log side the
   more reliable of the two, which is the strongest argument for generalizing
   the mechanism rather than telling applications to stamp spans themselves.

8. **`NewMemberRaw` does not validate the key as a W3C token, and the failure is
   silent and deferred to the wire.** `NewMemberRaw` (`baggage.go:293`) validates
   via `Member.validate()` (`:381`) → `validateBaggageName` (`:893`), which only
   requires a non-empty, valid-UTF-8 string. The W3C token charset is enforced
   elsewhere: `NewMember` checks it up front (`:270`), and `Member.String()`
   returns the **empty string** for a non-token key (`:409-411`), which
   `Baggage.String()` then silently skips (`:692-707`). Measured against
   v1.44.0:

   | key | `NewMemberRaw` | `NewMember` | wire output |
   |---|---|---|---|
   | `chat.room.id` | ok | ok | `chat.room.id=v1` |
   | `chat room.id` | ok | **error** | *(empty)* |
   | `房間.id` | ok | **error** | *(empty)* |
   | `chat@room` | ok | **error** | *(empty)* |

   A key accepted by `NewMemberRaw` can therefore materialize correctly on local
   spans and logs and **never reach any downstream service**, with no error at
   any point. That defeats the entire purpose of this ADR.

   The reverse does not hold for **values**: `NewMemberRaw` percent-encodes on
   serialization, so a Unicode value survives the round trip
   (`user.name=%E6%84%9B%E5%9B%A0%E6%96%AF%E5%9D%A6%20a%20b`). ADR 0016 chose
   `NewMemberRaw` precisely for that, and that choice must be preserved.

9. **Exceeding the baggage budget silently drops members downstream — and the
   two limits fail differently.** `Baggage.SetMember` (`baggage.go:636-660`)
   checks neither member count nor total encoded size; it is only a map insert.
   Both limits are enforced on the *receiving* side, with different blast
   radii. Measured against v1.44.0:

   | Inbound header | `Parse` result | What the downstream service sees |
   |---|---|---|
   | 70 members | error **plus a partial Baggage of 64** (`baggage.go:528`, `break` after `errMemberNumber`) | the first 64 members in header order; the rest vanish |
   | > 8192 bytes | error, **empty** Baggage (`:517`) | the extraction contributes nothing — `user.name` included |
   | a normal header, after either of the above | no error, full Baggage | unaffected |

   `propagation.Baggage.Extract` installs a non-empty partial result and falls
   back to the *parent context* for an empty one, reporting the error through
   the global error handler **once per process** (`propagation/baggage.go:27-29,
   62-77`, `sync.Once`).

   "Falls back to the parent" is why the byte-limit row says *contributes
   nothing* rather than *clears everything*: the context keeps whatever baggage
   it already carried. For a server extracting an inbound request that is
   normally none — the handler's parent context is derived from the server's
   base context — so the practical outcome there is no baggage at all. On a hop
   whose parent context already carries baggage (an in-process consumer loop,
   say), that inherited baggage survives and only the inbound members are lost.

   The `sync.Once` suppresses the *diagnostic*, not the baggage: each request is
   parsed independently, so an oversized header costs that request's baggage and
   does not poison later ones. The failure is therefore per-request rather than
   process-wide, and at the member limit it is truncation rather than total
   loss — but it is still silent after the first occurrence, still loses
   `user.name` outright on the byte limit, and is still reachable using only SDK
   setters. That is what the post-set check in Decision §8 exists to prevent.

10. **Span attributes are last-write-wins.** `recordingSpan.SetAttributes`
    appends without de-duplicating (`sdk/trace/span.go:263-274`); duplicates are
    resolved on read by `dedupeAttrsFromRecord` via `unique[idx] = a`
    (`span.go:790`), and the over-capacity path does the same (`span.go:333`).
    A later `SetAttributes` therefore overwrites a value written by `OnStart`
    for the same key. This is load-bearing for the ingress analysis below.

11. **`service.name`, `traceId`, and `spanId` are applied by handlers *inside*
    the BaggageHandler.** `service.name` and `environment` are attached to the
    innermost JSON handler (`o11y.go:267`, `stdoutBase.WithAttrs(...)`);
    `traceId`/`spanId` are added by `OtelSlogHandler.Handle`
    (`internal/log/handler.go:30-33`). The BaggageHandler is the outermost
    handler (`o11y.go:309-311`), and its de-duplication can only see the record
    itself and `WithAttrs` calls made on *it*. A baggage member named
    `service.name` or `traceId` therefore produces a **duplicate JSON field**,
    whose winner depends on the consuming parser — a viable path for a
    baggage value to impersonate service identity or corrupt log↔trace
    correlation.

12. **The traced NATS consumer path discards extracted baggage — every one of
    them.** This is the most consequential finding in this ADR, and it is a
    defect in **shipped ADR 0016 Phase 2**, not something this ADR introduces.

    All three consumer wrappers in `otel-nats@v0.8.0` follow the same shape:
    extract the headers into a local `msgCtx`, take *only* the span context out
    of it, then start the consumer span from `context.Background()` and hand the
    handler that context.

    ```go
    // otelnats/conn_traced.go:189-204 (Subscribe / QueueSubscribe)
    msgCtx := t.propagator.Extract(context.Background(), &HeaderCarrier{H: msg.Header})
    originSpanCtx := trace.SpanContextFromContext(msgCtx)   // span context only
    ...
    ctx, span := t.tracer.Start(context.Background(), spanName, opts...)  // msgCtx dropped
    handler(Msg{Msg: msg, Ctx: ctx})
    ```

    `oteljetstream/consumer_traced.go:43-70` (`Consumer.Next`), `:151-169`
    (`Consume`) and `:183-208` (`MessagesContext.Next`) are identical in this
    respect. `msgCtx` — the only thing holding the extracted baggage — is
    discarded in all four.

    The producer side is fine: `Inject(ctx, ...)` uses the caller's context
    (`otelnats/conn_traced.go:128,142`, `oteljetstream/jetstream_traced.go:44`),
    so baggage rides the NATS headers correctly. The asymmetry is exact —
    **send works, receive drops.**

    So a `user.name` set at an HTTP edge and published to NATS reaches the
    worker's headers and then vanishes: no `OnStart` materialization, nothing on
    the handler's logs, nothing on child spans, and nothing injected into that
    handler's own downstream publishes. ADR 0016's claim that *"the NATS facade
    carries baggage with zero new code"* is true of the wire and false of the
    handler.

    **This is fixable in o11y's own facade, without touching upstream.**
    `nats/conn.go:131-133` and `:157-159` already wrap the handler and hold both
    `m.Ctx` and `m.Msg.Header`; `nats/jetstream.go:365` and `:542` have the same
    seam, and `otelnats.Conn.TraceContext()` exposes the propagator. See
    Decision §10.

---

## Decision

**Generalize ADR 0016's fixed whitelist into an application-configured list of
opaque baggage keys. Add no application-domain vocabulary to the SDK.**

### 1. The whitelist becomes instance state

`internal/baggageattrs` grows a `Whitelist` value type built from a key list.
The package-level `var baggageWhitelist` is removed. Two SDK instances in one
process must not share or clobber each other's configuration (ADR 0003).
`UserNameKey` continues to map to the pinned `semconv.UserNameKey`; every other
key maps to an `attribute.Key` of the same string, because application keys have
no semconv constant to source from (ADR 0006, and recorded as such in
`docs/semconv.md`).

Immutability has to be real at both ends. `NewWhitelist(keys ...string)` copies
its input rather than retaining the caller's backing array — a variadic call
site such as `NewWhitelist(app.BaggageKeys()...)` hands over a caller-owned
slice, and retaining it would let a later mutation reconfigure an SDK instance
after `Init`. Accessors that expose the key list return a defensive copy for the
same reason: a contract that hands out a mutable slice is not an immutability
contract.

### 2. A new option: `WithBaggageAttributes(keys ...string)`

Enables materialization of the named baggage members onto this service's spans
and SDK log records. Keys are opaque to the SDK. Calls accumulate, and a key
already registered is **silently** ignored — registering the same key twice is
harmless and idempotent, unlike the *collisions*
`WithExtraHTTPServerAttributeKeys` warns about, which would merge two distinct
values into one label. Keys rejected by static validation are dropped with a
startup WARN rather than failing `Init`, matching that option
(`options.go:417`). The one stricter case is a collision with this process's
actual Resource attributes: that is known only after the Resource is built and
causes `Init` to fail, for the security reason in §6.

**`user.name` is rejected by this option**, with a WARN pointing at
`WithUserBaggage()`. The generic path is defined as carrying no contract beyond
"opaque key"; `user.name` carries the ADR 0016 PII contract. If the generic
option accepted it, a service could enable PII materialization without ever
encountering that contract, which is precisely the boundary ADR 0016 exists to
make deliberate.

The same reservation applies to the **setter**, not just this option — see §4.
Reserving one and not the other leaves the boundary open: rejecting
`WithBaggageAttributes("user.name")` while allowing
`ContextWithBaggageValue(ctx, UserNameKey, name)` still lets an application put
PII on the wire through a call site that states no PII contract, which is the
side that actually reaches the network.

### 3. `WithUserBaggage()` is retained, and tracked separately

The option is kept rather than deprecated, and — following from §2 — it is not
merely sugar for `WithBaggageAttributes(baggageattrs.UserNameKey)`. `user.name`
occupies its own configuration slot, and the whitelist is assembled from that
slot plus the application key list.

That separation is what makes the two caps behave sanely.
`MaxBaggageAttributeKeys` bounds **application** keys only; `user.name` is
counted outside it and can never be evicted by them. Expressing
`WithUserBaggage` as another generic call would make the outcome depend on
option ordering — eight application keys registered first would silently push
`user.name` out with nothing but a warning — which is an unacceptable failure
mode for the one key that carries a PII contract.

Its documentation must state the ADR 0016 split accurately: **this option only
materializes `user.name` onto this service's own spans and logs. It does not
create baggage and does not put anything on the wire — `ContextWithUser` is the
source-side step that does that.** Conflating the two misrepresents where the
PII decision is actually made.

### 4. Source-side API: set, and remove

```go
ContextWithBaggageValue(ctx, key, value) (context.Context, error)
ContextWithoutBaggageValues(ctx, keys ...string) context.Context
```

The setter is the key-agnostic sibling of `ContextWithUser`. It deliberately
**does not** validate the key against any whitelist: the producer (an edge
service) and the materializing consumers are different processes with
independent configuration, so a local check would force every producer to carry
every consumer's whitelist. Whitelisting is a materialization-side concern.

It **does** reject `user.name`, mirroring §2. This is the call that puts a value
on the wire, so leaving it open would let an application propagate PII through
an API that states no PII contract — the reservation would be decorative.
`ContextWithUser` keeps working by taking an unexported path into the shared
implementation: the public generic setter refuses the key, the PII-specific
public API allows it. An implementation that routes `ContextWithUser` through
the *public* setter cannot satisfy both halves and is wrong.

The remover is new and is required by the ingress model (§7). OTel offers
`baggage.ContextWithoutBaggage`, which clears *everything* — unusable at an edge
that must drop application keys while preserving internally-trusted ones — and
`Baggage.DeleteMember` (`baggage.go:665`), which is not reachable at the context
level without an explicit `FromContext`/`ContextWithBaggage` round trip. A
setter without a matching remover forces applications into "overwrite" as the
only sanitization tool, which §7 shows is not sufficient.

### 5. Key validation, at both entry points

Every key the SDK accepts — from `WithBaggageAttributes` **and** from
`ContextWithBaggageValue` — is validated as a W3C token before use. Because Q3
keeps the whitelist out of the setter, key validation is the setter's only
guard.

The validation must use `baggage.NewMember(key, "x")` as a **key probe only**.
Members are still constructed with `NewMemberRaw(key, value)`, so that values
remain raw application strings that the library percent-encodes on
serialization. Reversing this would break ADR 0016's Unicode-username contract
(Dependency behavior §8). Getting this distinction wrong in either direction is
the single most likely implementation error in this ADR.

Tests must assert an actual `Inject` → `Extract` round trip. A test that only
checks whether the constructor returned an error passes for all four rows of the
table in Dependency behavior §8, including the three that never reach the wire.

### 6. Reserved keys, and precedence

The SDK rejects keys that would shadow its own identity or correlation fields.
**Where each reservation can be enforced differs, and the difference is
structural, not a choice:**

- The **static** reservations below — the SDK's log fields, `slog`'s record
  fields, the generated semconv set, the namespace prefixes, `user.name` — are
  enforced in *both* places: `WithBaggageAttributes` drops with a startup WARN,
  and `ContextWithBaggageValue` returns an error. `ValidBaggageKey(key)` needs
  nothing but the key.
- The **dynamic** reservation — this process's actual Resource attributes,
  including whatever `OTEL_RESOURCE_ATTRIBUTES` contributed — is enforced **only
  at `Init`**. `ContextWithBaggageValue` is a package-level function with no SDK
  instance and therefore no Resource to compare against; giving it one would
  mean ambient global state, which ADR 0003 forbids. If any key in the effective
  whitelist collides — application keys, plus `user.name` when
  `WithUserBaggage()` is enabled — `Init` returns an error instead of silently
  removing the key. Failing initialization is deliberate here: the documented
  `tag` helper and `SetUser` explicitly write those keys onto the already-started
  entry span, so dropping a collision from the materialization whitelist would
  still let that write shadow the trusted Resource value. Rejecting the
  configuration is the only enforcement point shared by the whitelist and those
  writers without introducing global state.

  The generic setter remains intentionally unaware of dynamic Resource keys.
  Applications must use `tag` only with keys registered through
  `WithBaggageAttributes`; arbitrary direct `Span.SetAttributes` calls are
  outside this API's enforcement boundary and remain application responsibility.

The groups:

- **Emitted by handlers inside the BaggageHandler**: `service.name`,
  `environment` (`o11y.go:267`), `traceId`, `spanId`
  (`internal/log/handler.go:30-33`), and `slog.JSONHandler`'s own built-in
  record fields `time`, `level`, `msg`, `source`. Per Dependency behavior §11 no
  de-duplication at the BaggageHandler layer can see any of these, so only
  refusing the key prevents a duplicate JSON field. The `slog` built-ins matter
  as much as the SDK's own: a baggage member named `level` or `msg` lets an
  untrusted caller forge a second severity or message field in every record the
  service emits.
- **Keys the SDK itself emits**, catalogued in `docs/semconv.md`. The
  materializer writes every whitelisted key as an `attribute.String`, but the
  catalog pins types: `http.response.status_code` is `attribute.Int`
  (`docs/semconv.md:74`, `semconv/v1.39.0/attribute_group.go:7973-7984`). A
  baggage member with that key is a valid W3C token, so nothing else would stop
  it, and materializing it would emit a semconv-invalid string *and* shadow real
  HTTP instrumentation on the same span. The rule is therefore: **a key the SDK
  emits anywhere in its own catalog is refused on the generic path.** Type-aware
  materialization is the alternative and is rejected — it would put a type
  system into a mechanism whose entire premise is that keys are opaque.

  **The reserved set is the whole pinned semconv package, not just the SDK's
  own catalog.** An earlier revision limited it to keys the SDK emits today and
  filed the rest as residual risk. That was wrong. `http.response.body.size` is
  an integer in `semconv/v1.39.0` and absent from `docs/semconv.md`, so it would
  pass a catalog-only check and be materialized as a string — the SDK knowingly
  emitting non-conformant telemetry, which contradicts three things this ADR and
  the repository already assert: that application keys have no semconv constant
  (Context, above), that the generic option holds no semantic-convention opinion
  (its own doc comment), and `AGENTS.md`'s requirement that every semconv
  attribute key *and type* match the pinned version.

  Go has no runtime reflection over package constants, but that only rules out
  discovering the set at run time. **The set is generated at build time from the
  pinned `semconv/v1.39.0/attribute_group.go` and checked in**, with a CI gate —
  alongside the existing `scripts/check_integrations.go` — asserting the
  generated file matches the pin.

  Generation must emit **prefix rules for parameterized families, not only exact
  keys.** Some conventions have no per-member constant at all: `HTTPRequestHeader`
  builds `attribute.StringSlice("http.request.header."+key, val)`
  (`attribute_group.go:8037-8038`), so no `attribute.Key` exists for
  `http.request.header.authorization` and an exact-key set would let it through
  — materialized as a `String` where semconv says `StringSlice`, and carrying an
  auth header name into telemetry besides. The generator therefore recognizes
  these `prefix+key` constructors and emits a namespace rule for each; at least
  one such key belongs in the CI test. ADR 0006 already makes a pin bump a single
  atomic change with a defined checklist; regenerating this list joins it.

  The objection that a future semconv version might standardize a key an
  application already uses is real, and it is **ADR 0006's** problem: a pin bump
  that newly collides with a live application key is a migration to plan in that
  upgrade, not a reason to emit the wrong type today.
- **Wildcard-catalogued resource namespaces**: `host.` and `process.`. The
  catalog lists these as `host.*` and `process.*` (`docs/semconv.md:51-52`)
  because `resource.WithHost()` / `resource.WithProcess()` *detect* them at
  runtime — the exact set is not knowable statically. A reserved list built
  literally from the catalog would therefore contain the string `host.*`, which
  matches no key, and let `process.pid` through to be materialized as a string
  even though semconv defines it as `attribute.Int`
  (`semconv/v1.39.0/attribute_group.go:13048-13050`) — wrong type *and*
  shadowing the SDK's own resource value. **Wildcard rows must be enforced as
  namespace-prefix rejections, not literal keys.**
- **Whatever this process's Resource actually carries.** `buildResource` includes
  `resource.WithFromEnv()` (`o11y.go:428`), so `OTEL_RESOURCE_ATTRIBUTES` can add
  keys that appear in no generated set and no fixed list — and a baggage member
  with that key would shadow the trusted resource value on every span, which is
  the exact harm this section exists to prevent. The Resource is built at
  `o11y.go:192`, before the whitelist is assembled, so `Init` **compares the
  effective whitelist — including `user.name` when enabled — against
  `res.Attributes()`** and returns an error naming every collision. A
  WARN-and-drop policy is insufficient: the entry-span `tag` helper and
  `SetUser` write their keys explicitly after initialization and would still
  shadow the Resource value. Without this check, "every SDK-emitted key is
  refused" is false for any deployment that uses the environment variable.
- **Resource-level service identity**: `service.version`, `service.namespace`,
  `deployment.environment.name` (`docs/semconv.md`, Resource Attributes). These
  do not collide on the log side, but as *span* attributes they shadow the
  resource attributes of the same name in trace-backend queries, letting an
  untrusted caller make a span appear to belong to a different version,
  namespace, or environment. That is the same spoofing class as `service.name`
  and is refused for the same reason.

Applications are additionally directed to use a product namespace. That is
guidance rather than an enforced rule — an unnamespaced `request_id` is harmless
and refusing it buys nothing — but it is the reliable way to stay clear of the
**currently pinned** semconv reservation. It is not an absolute guarantee: a
future semconv pin can claim a previously application-owned namespace, and
`OTEL_RESOURCE_ATTRIBUTES` can introduce any key. The generated reservation and
the `Init`-time Resource collision check remain authoritative.

Precedence is defined, not left emergent:

- **Spans**: explicit attributes win at both ends. `OnStart` **skips** a key
  already supplied at span creation via `trace.WithAttributes` — the current
  processor checks `span.Attributes()` before writing, pinned by
  `TestSpanProcessorDoesNotOverrideExplicitStartAttribute`
  (`internal/baggageattrs/baggageattrs_test.go:103-121`) — and a *later*
  `SetAttributes` for the same key overwrites the baggage value by last-write-wins
  (Dependency behavior §10). Both halves must survive the generalization:
  keeping only the second would let inbound baggage replace a trusted start-time
  attribute. Application code can therefore always beat baggage, before or
  after.
- **Logs**: an attribute already on the record, or supplied via `WithAttrs` on
  the BaggageHandler, wins over the baggage value; the baggage value fills in
  only where nothing else did. This preserves current behavior.

  The collision check must **resolve, then recurse into empty-key groups** to
  hold. `slog` inlines them — *"If a group's key is empty, inline the group's
  Attrs"* (`log/slog/handler.go:57`) — so an explicit
  `slog.Group("", slog.String(k, v))` reaches the JSON output as a top-level `k`,
  while a check that looks only at the outer attribute sees an empty key and
  misses it. The baggage value is then added too, producing a duplicate field in
  which the *baggage* value — possibly forged — is the later one.

  Resolution has to come first, because `slog.Any("", v)` for a `v` implementing
  `LogValuer` presents as `KindLogValuer` until resolved and only then becomes
  the group that gets inlined. `appendAttr` does `a.Value = a.Value.Resolve()`
  as its first statement, ahead of every group and empty-key check
  (`log/slog/handler.go:468`), so a check that tests `Kind()` without resolving
  sees a different shape than the handler that ultimately writes the record.
  Both `recordHasAttr` and the `presetKeys` accumulation in `WithAttrs` must
  call `Value.Resolve()` before testing for an empty-key group, and flatten
  recursively.

  **The scan and the delegated record must then see the same resolved values.**
  `slog` resolves again downstream, and a `LogValuer` that is stateful or
  otherwise non-idempotent can answer differently the second time — so the
  suppression decision would no longer match the JSON actually written, either
  emitting a duplicate field or suppressing a value that is no longer there.
  Whenever the scan resolves **any** `LogValuer`, the handler passes the resolved
  form downstream instead of the original, regardless of whether that first
  result is a scalar, a named group, or an empty-key group. Otherwise a
  non-idempotent value can first resolve to a scalar (so baggage is added) and
  then resolve inside `JSONHandler` to an inlined group containing the same key
  (so a duplicate is emitted). This applies to *both* paths, not just records:
  `Handle` rebuilds the record from resolved attributes, and `WithAttrs`
  delegates a resolved copy of the slice rather than the caller's. Fixing only
  `Handle` leaves the identical divergence for a `LogValuer` supplied through
  `Logger.With`. Records and slices containing no `LogValuer` retain the ordinary
  no-rebuild path.

**Known limitation — `slog` groups.** If an application calls `WithGroup` on the
SDK logger, the BaggageHandler's `r.AddAttrs` is nested by the inner handler, so
`user.name` is emitted at `<group>.user.name` instead of top level, and log
queries keyed on the flat path stop matching. This cannot be fixed from the
BaggageHandler's position in the chain — group nesting is applied by the
handlers beneath it. It is documented as a limitation, and applications are
directed not to open groups on the SDK logger.

**`WithGroup` resets the preset-key set — do not "fix" this.** Today's
`hasUserNameAttr` is deliberately dropped in `WithGroup`
(`internal/log/handler.go:101-105`), and
`TestBaggageHandlerWithGroupDoesNotInheritUserNameAttr`
(`internal/log/handler_test.go:170-186`) pins the resulting output: an explicit
`user.name` supplied before the group stays at top level *and* the baggage value
still appears at `audit.user.name`. That is correct, not an oversight — the two
land at different JSON paths, so there is no duplicate to suppress. Carrying
`presetKeys` across `WithGroup` would suppress the grouped baggage value and
change output that an existing test pins, so the generalized handler must keep
the reset. Only same-level collisions are the preset set's business.

### 7. Ingress: remove, then set — overwrite is not enough

An external caller can forge `baggage: <any key>=<any value>`; the upstream
propagator explicitly acknowledges this threat model
(`propagation/baggage.go:27-28`, *"attacker-controlled baggage headers"*).
Extraction and server-span creation both happen upstream of application
middleware, so `OnStart` stamps the forged value onto the entry span before any
application code runs.

Sanitization therefore has to be reasoned about on **two separate planes**, and
middleware can only reach one of them.

**Plane 1 — the context (everything after the middleware).** Child spans, driver
spans, log records, and all downstream services read the context as the
middleware leaves it. Here the required posture is **clear, then set**.

At a **public** boundary, clear *all* inbound baggage
(`baggage.ContextWithoutBaggage`) and rebuild only the members this request has
authenticated and authorized. Clearing only the edge's own key registry is not
enough: the SDK propagator forwards members this service has never heard of, so
during a rolling deployment an attacker can send a key the edge has not learned
yet but a downstream service already materializes — and that service exports it
as genuine. An allowlist of what to *keep* is sound; a denylist of what to
remove is only as current as the slowest-deploying service.

`ContextWithoutBaggageValues` (§4) is for **internal** boundaries, where the
surviving members have an established trusted source and blanket clearing would
destroy the `user.name` that ADR 0016 Phase 2 exists to carry.

Either way, removal must be unconditional rather than overwrite-if-present,
because three common paths perform no overwrite at all:

1. **A route that never sets the key.** A forged `chat.room.id` on an endpoint
   with no room parameter is never overwritten.
2. **Empty values are a no-op.** `ContextWithBaggageValue` returns ctx unchanged
   for an empty value, leaving a forged member in place.
3. **The error path preserves the attacker's value.** Returning the original
   context on error — which ADR 0024 requires, since telemetry must not be
   load-bearing — also preserves whatever was already there.

Clearing fully closes this plane.

**Plane 2 — the entry span, which middleware cannot fix.** `OnStart` has already
copied the forged member onto the server or consumer span by the time any
application code runs, and clearing baggage returns a *child context* — it
cannot retract an attribute already written to a running span.
Last-write-wins (Dependency behavior §10) rescues only the keys this request
actually sets, via the explicit entry-span write in Consumer guidance. For every
other key, on every one of paths 1–3, **the forged value remains on the entry
span** — the span an operator queries first.

Plane 2 is closed by keeping the baggage out of the context in the first place,
and the SDK already has the seam for it — **give the public boundary its own
propagator**:

```go
// Public listener: extract trace context, never baggage. Nothing to forge,
// nothing for OnStart to copy onto the entry span.
//
// gin.Middleware returns a []gin.HandlerFunc to be SPREAD into Use
// (gin/middleware.go:13-15) — calling it without installing the result
// registers nothing and silently leaves the boundary unsanitized.
r.Use(gin.Middleware("edge", obs.TracerProvider(), obs.MeterProvider(),
	propagation.TraceContext{})...)

// Internal hops keep obs.Propagator, so ADR 0016's "identify once at the
// source" is unaffected.
nats.Connect(ctx, url, obs.TracerProvider(), obs.Propagator)
```

Every propagator entry point takes it per boundary (`http/server.go:21`,
`http/transport.go:16`, `gin/middleware.go:21`, `nats/conn.go:71`), so trust can
be expressed exactly where it actually differs. See Q6. Two fallbacks, for
deployments that cannot do this:

- **Strip the `baggage` header before OTel extraction** at an edge proxy — the
  same effect, one layer out.
- **Declare entry-span application attributes untrusted at public boundaries**
  and rely on the child spans, which are clean once Plane 1 is sanitized.

What the SDK cannot do is *choose* for the application, because only the
application knows which of its listeners is public. What this ADR can do is stop
letting "clear, then set" read as covering more than it does, and point at the
seam that covers the rest.

### 8. Bounds, split by what they actually protect

The previous draft claimed one cap bounded both the hot path and the wire. It
does not (Dependency behavior §9). Three separate bounds:

| Bound | Value | Protects |
|---|---|---|
| `MaxBaggageAttributeKeys` | 8 | the per-span and per-record enrichment loops. Applies to the **application key list only** — not to the wire, and not to `user.name`, which has its own slot (§3). |
| Key length | 128 bytes | the shared header budget, at the setter. |
| Value length (`MaxBaggageValueBytes`) | 256 bytes | the same, at the setter. |
| Post-set baggage size | 64 wire members / 8192 encoded bytes | the serialized header. Checked **after** the member is added; on breach the setter returns an error and the **original** context. Members that `Baggage.String()` omits do not consume the wire-member cap. |
| Materialized value length | `MaxBaggageValueBytes` | this service's own telemetry volume, at the **materialization** side. |

The last row is not redundant with the setter cap: the setter only governs
values this SDK produces. A member arriving on the wire — from an OTel producer
in another language, a Go service using the raw `baggage` API, or any hand-built
but perfectly legal W3C header — can carry a value approaching the full 8192
bytes, and materialization would copy it onto **every span and every log record**
the service emits. That is telemetry amplification from a single inbound header.
The whitelist enricher therefore skips a member whose value exceeds
`MaxBaggageValueBytes`, and the case — a valid header, an over-cap value — is a
required test.

The post-set check is what actually prevents Dependency behavior §9. It counts
only members whose `Member.String()` is non-empty — the same condition
`Baggage.String()` uses before placing a member on the wire — and checks the
encoded header length. Using `Baggage.Len()` here would reject a valid one-member
header merely because the local context also carries 64 `NewMemberRaw` members
whose keys the W3C serializer omits. The serialization work per set is accepted
knowingly in exchange for neither over-rejecting that case nor silently
truncating or dropping baggage at every downstream hop. Do not remove it for
performance without replacing it with equivalent wire-aware bounds.

### 9. Scope limitation: the traced NATS path is required

This feature carries values over W3C Baggage, which needs the propagator's
Inject/Extract to actually run on each hop. An effectively disabled direct-path
connection removes exactly that (`nats/options.go`, Dependency behavior §3),
so in that mode baggage — application keys and `user.name` alike — stops at
every NATS and JetStream hop. `WithTracingEnabled(false)` reaches that mode only
when no higher-precedence upstream env or relay value enables tracing.

This is **not fixable from the materialization side**, and this ADR does not
attempt to fix it. Preserving baggage independently of span creation would mean
re-adding header inject/extract to a mode whose entire purpose is the native
NATS cost profile, which is a change to ADR 0004/0022 territory and needs its
own decision.

The rule is therefore stated rather than worked around: **a service that
hard-disables NATS tracing does not participate in baggage materialization**,
and keeps whatever explicit per-service tagging it already does. Services mixing
the two modes get a partial picture, which is worse than a uniform one — so the
mode should be chosen per mesh, not per service.

### 10. Restore baggage in the NATS facade

Dependency behavior §12 is the blocking issue for this ADR's stated use case:
on the **traced** path, all three `otel-nats` consumer wrappers extract the
headers, keep only the span context, and start the consumer span from
`context.Background()` — so the handler receives a context with no baggage. Send
works, receive drops. Without a fix, "identify once at the edge and read it in
the NATS worker" does not work, for application keys *or* for the `user.name`
that ADR 0016 Phase 2 already claims to deliver.

**Decision: the `nats` facade re-attaches baggage to the handler context.** It
already wraps every handler and holds both halves it needs — the consumer-span
context and the raw message headers:

```go
// nats/conn.go:131-133 today
return c.Conn.Subscribe(subject, func(m otelnats.Msg) {
	handler(m.Ctx, m.Msg)
})

// with the fix
return c.Conn.Subscribe(subject, func(m otelnats.Msg) {
	handler(restoreBaggage(c, m.Ctx, m.Msg.Header), m.Msg)
})
```

`restoreBaggage` must **honor the connection's configured propagation policy**,
not hard-code a baggage extraction. Extract with the connection's own propagator
into a throwaway context, then graft *only* the baggage onto the handler
context:

```go
extracted := prop.Extract(context.Background(), &otelnats.HeaderCarrier{H: hdr})
bag := baggage.FromContext(extracted)
if bag.Len() == 0 {
	return ctx
}
return baggage.ContextWithBaggage(ctx, bag)
```

Two properties fall out of that shape, and both are required:

- **Grafting only the baggage** leaves the consumer span context alone. Applying
  the composite propagator directly to `ctx` would reapply `TraceContext` and
  overwrite the span context `otel-nats` just established, breaking the topology
  ADR 0022 documents.
- **Using the explicitly supplied connection propagator** means a connection
  given `propagation.TraceContext{}` for public NATS ingress (Q6) extracts no
  baggage and restores nothing. The facade must retain the `prop` argument it
  passed upstream; it must not capture `otelnats.Conn.TraceContext()` at
  construction because v0.8.0 makes that method reflect the current dynamic
  gate and it can return the direct implementation's no-op propagator before a
  later relay enable. Hard-coding `propagation.Baggage{}` here would silently
  defeat the caller's sanitization from inside the SDK.

**The restore must also be skipped entirely on the effective native path.**
That path still routes through the facade's wrapper — `conn_direct.go` calls it
with `Msg{Msg: msg, Ctx: context.Background()}` and an intact `msg.Header` — so
an unconditional restore would extract baggage in the one mode Decision §9
says does not participate. Since v0.8.0 the option value is only a default and
cannot be cached as the answer. The facade must follow the connection's
effective dynamic state (for example through `Conn.TracingEnabled`) so an env
or relay override changes span and baggage propagation together. That query is
another relay resolution on a consume path, so its capacity cost and the tiny
between-evaluations transition window must be covered by the implementation
review and load test.

The same wrap applies to `QueueSubscribe` (`nats/conn.go:157-159`) and to
**every** JetStream delivery path — all four upstream discarding sites from
§12: `Consume` (`nats/jetstream.go:365`), the fetched-message channel (`:542`),
the `Messages(ctx)` pull iterator (`:374`), and single-shot `Consumer.Next`
(`:592-603`), which today returns the upstream context unchanged. Missing one
leaves services on that path silently baggage-free, which is the same failure
this decision exists to remove.

Four things this deliberately does **not** claim:

1. **The consumer span itself stays clean of the restored values.** `OnStart`
   already ran, against a context with no baggage. The consumer span is the
   NATS-side equivalent of the HTTP entry span (Decision §7, Plane 2).
   Everything *after* — child spans, driver spans, log records, and this
   handler's own downstream publishes — gets the values.

   **The remedy differs by delivery path, and prescribing one for both is
   wrong.**

   - *Push paths* — Core `Subscribe`/`QueueSubscribe` and JetStream `Consume` —
     hold the consumer span open for the duration of the handler, so
     `trace.SpanFromContext(ctx).SetAttributes(...)` works. This is ADR 0022 §4
     as written.
   - *Pull paths* — `Consumer.Next`, `MessagesContext.Next`, and every `Fetch*`
     batch — end the receive span **before** handing over the context, so
     `SetAttributes` on it is a **deterministic silent no-op**. `nats/jetstream.go:103-115`
     already documents this and names the correct pattern: start your own child
     span with `tracer.Start(ctx, ...)` and enrich that, as
     `examples/jetstream/fetch-worker` does.

   The pull paths are in fact the *easier* case once restoration lands: a child
   span started from the restored context is enriched automatically by
   `OnStart`, with no explicit `SetAttributes` at all. The guidance must say so
   rather than sending callers at an ended span.
2. **It does not fix `otel-nats`.** Upstream still discards `msgCtx`; the facade
   compensates. If upstream later preserves it, the facade's restore becomes a
   no-op rather than a conflict, because `SetMember` on an equal key is
   idempotent.
3. **It does not apply to the native mode** (§9), where nothing was put on the
   wire to restore.
4. **It does not add channel-subscription APIs.** Neither the upstream
   `otelnats.Conn` nor this facade exposes `ChanSubscribe` or
   `ChanQueueSubscribe`; those native APIs deliver raw `*nats.Msg` values with no
   context-first handler seam on which the facade can restore baggage. Expanding
   the facade with a second, context-less delivery model is outside this ADR.
   Applications that need automatic restoration use `Subscribe` or
   `QueueSubscribe`; callers that deliberately reach the raw native connection
   own extraction themselves.

This is scoped as part of this ADR because ADR 0025 is what makes the gap
load-bearing, but it repairs an ADR 0016 Phase 2 promise that has been untrue
since it shipped. That is worth calling out separately in the CHANGELOG under
**Fixed**, not folded into the new feature.

### What is explicitly NOT decided here

- **No application key enters the SDK.** No `chat.*`, no `SetRoom`, no
  `RoomID`. A reviewer should be able to reject this ADR's implementation PR on
  sight if a product-domain word appears in the diff.
- **Metric labels are untouched.** The whitelist bounds *baggage*, not metric
  dimensions. Keeping high-cardinality identifiers off `internal/metrics` remains
  enforced by convention (ADR 0016 Q3); this ADR widens the set of keys it
  applies to.
- **Span names are untouched** (ADR 0023).

---

## API change

```go
// options.go

// MaxBaggageAttributeKeys bounds how many application-defined baggage keys one
// SDK instance materializes onto spans and log records. user.name, enabled via
// WithUserBaggage, is tracked separately and does not count against this cap.
const MaxBaggageAttributeKeys = 8

// WithBaggageAttributes enables materialization of the named W3C baggage
// members onto this service's spans and SDK log records.
//
// The keys are application-defined: the SDK does not interpret them and holds no
// semantic-convention opinion about them. Use your own namespace, keep the list
// short, and use the same list in every service that should surface the value.
//
// This option does not create baggage. Use ContextWithBaggageValue where the
// value is known and trusted; this option is what makes spans and logs show it.
//
// Calls accumulate and de-duplicate. Dropped with a startup WARN: the empty
// string; keys that are not valid W3C baggage tokens (they would materialize
// locally but never reach another service); keys that would shadow the SDK's own
// identity or correlation fields (service.name, service.version,
// service.namespace, environment, deployment.environment.name, traceId, spanId)
// or slog's own record fields (time, level, msg, source); ANY key defined by the
// pinned semconv package -- not only the ones this SDK emits, since the
// materializer writes every value as a string and semconv pins types (e.g.
// http.response.body.size is an int) -- including parameterized families with no
// per-key constant, such as anything under http.request.header., and the host.
// and process. namespace prefixes populated by resource detectors; keys longer than
// MaxBaggageKeyBytes;
// and user.name, which is reserved for WithUserBaggage because it carries a PII
// contract this option does not state.
//
// Init returns an error if any key in the effective whitelist collides with an
// attribute on this process's actual Resource, including a key supplied through
// OTEL_RESOURCE_ATTRIBUTES. The effective whitelist includes user.name when
// WithUserBaggage is enabled. Dropping a collision is not safe because
// application code also writes these keys explicitly onto already-started entry
// spans.
// MaxBaggageAttributeKeys is applied at Init after that collision check;
// overflow is reported by the same startup WARN.
// Never route a materialized key into a metric label.
func WithBaggageAttributes(keys ...string) Option

// WithUserBaggage enables materialization of the pinned semconv user.name
// baggage member onto this service's spans and log records.
//
// It does not create baggage and does not put anything on the wire.
// ContextWithUser is the source-side step that places user.name — personal data
// — into baggage, from where the SDK propagator carries it across HTTP and NATS
// hops. See ADR 0016 for the two separate opt-in boundaries.
func WithUserBaggage() Option

// attributes.go

// UserNameKey is the semconv key this SDK uses for the acting user's login name.
// It is exported so applications can name it in a sanitization set without
// hard-coding the string; per docs/semconv.md the value is sourced from the
// pinned semconv package, never written as a literal.
const UserNameKey = baggageattrs.UserNameKey

// MaxBaggageValueBytes is the per-value limit ContextWithBaggageValue and
// ContextWithUser enforce. Exported from the root package so callers can
// resolve the limit named in those doc comments without reaching into internal.
const MaxBaggageValueBytes = baggageattrs.MaxBaggageValueBytes

// MaxBaggageKeyBytes is the per-key limit ContextWithBaggageValue and
// WithBaggageAttributes enforce. Exported for the same reason: a caller whose
// syntactically valid W3C token is refused needs to be able to find out why.
const MaxBaggageKeyBytes = baggageattrs.MaxBaggageKeyBytes

// ContextWithBaggageValue returns a child context carrying key=value as a W3C
// baggage member. Services that enabled WithBaggageAttributes(key) then show it
// on their spans and log records with no per-call-site code.
//
// The key must be a valid W3C baggage token; a key that is merely valid UTF-8
// would materialize on this service's telemetry and then be silently dropped
// during propagation, so it is rejected here instead. It must also not be
// reserved — including user.name, which has its own PII-contract API in
// ContextWithUser and is refused here so PII cannot reach the wire through a
// call site that states no such contract.
//
// The key is NOT checked against any whitelist — the whitelist is a
// materialization-side setting and the producer is usually a different process.
//
// Returns an error, leaving ctx unchanged, when the key is invalid or reserved,
// when the key exceeds MaxBaggageKeyBytes or the value exceeds
// MaxBaggageValueBytes, or when adding the member would
// push the serialized baggage past the W3C limits of 64 wire members / 8192
// encoded bytes. Locally held members that Baggage.String omits do not consume
// the wire-member cap.
// That last check matters: on the receiving side a header past 64 members is
// truncated to the first 64 in header order, and one past 8192 bytes yields no
// baggage at all for that request — user.name included.
//
// An empty key is an error like any other invalid token — the empty string is
// not a valid W3C token, and accepting it silently would contradict the contract
// above. An empty VALUE is the one no-op: it leaves ctx unchanged and returns no
// error, preserving ContextWithUser's existing "unauthenticated is not an error"
// behavior (ADR 0016). Note that an empty value does NOT remove an existing
// member; use ContextWithoutBaggageValues.
//
// Set only from data this service has already validated; never trust an inbound
// value from an untrusted caller; strip baggage before calling third parties;
// never route the key into a metric label.
func ContextWithBaggageValue(ctx context.Context, key, value string) (context.Context, error)

// ContextWithoutBaggageValues returns a child context with the named baggage
// members removed, leaving every other member intact.
//
// This is the primitive for INTERNAL boundaries, where the members it leaves
// behind have an established trusted source. At a PUBLIC boundary use
// baggage.ContextWithoutBaggage instead and rebuild only authenticated members:
// this function removes the keys you name, and the propagator forwards keys
// this service has never heard of, so a denylist is only as current as the
// slowest-deploying service in the mesh (Decision 7).
//
// Removal must be unconditional rather than overwrite-if-present: a forged
// member on a route that never sets that key is never overwritten.
//
// This sanitizes the context, and therefore every later span, log record, and
// downstream service. It cannot retract an attribute already written to the
// entry span, which OnStart stamped before any application code ran; see
// Decision 7, Plane 2.
//
// Unlike baggage.ContextWithoutBaggage, this preserves members it was not asked
// to remove, so an internal hop can drop application keys while keeping the
// user.name it trusts from a peer service.
func ContextWithoutBaggageValues(ctx context.Context, keys ...string) context.Context

// internal/baggageattrs

// Whitelist is an immutable, per-instance set of baggage keys. It is a value
// type, not package state, so two SDK instances never share configuration.
type Whitelist struct{ /* ... */ }

func NewWhitelist(keys ...string) Whitelist
func (w Whitelist) Len() int
func (w Whitelist) Keys() []string // defensive copy
func (w Whitelist) SpanAttributesFromContext(ctx context.Context) []attribute.KeyValue
func (w Whitelist) LogAttrsFromContext(ctx context.Context) []slog.Attr
func (w Whitelist) NewSpanProcessor() sdktrace.SpanProcessor

const (
	MaxBaggageValueBytes = 256
	MaxBaggageKeyBytes   = 128
)

func ValidBaggageKey(key string) error
func ContextWithValue(ctx context.Context, key, value string) (context.Context, error)
func ContextWithoutValues(ctx context.Context, keys ...string) context.Context
```

### Compatibility

The change is additive with **intentional behavior changes to
`ContextWithUser`**, which is reimplemented on the shared `ContextWithValue`
logic and therefore inherits its syntax, length, and baggage-budget guards — but
not the generic `user.name` reservation, which it reaches past through the
unexported path (Decision §4). Today it only calls `SetMember` and accepts
whatever results. After this change it rejects, leaving ctx unchanged:

| Input | Today | After |
|---|---|---|
| username > 256 bytes | accepted | rejected (`MaxBaggageValueBytes`) |
| any username, when ctx already holds 64 unrelated wire-serializable members | accepted | rejected (member cap) |
| any username, when the encoded baggage would exceed 8192 bytes | accepted | rejected (byte cap) |
| an **inbound** `user.name` over 256 bytes, from an older SDK, another language, or a raw baggage producer | materialized | skipped at materialization (Decision §8) |

The fourth row is not a source-side rejection at all: it is the materialization
cap, and it means an upgraded service stops showing an over-cap `user.name` that
today's `WithUserBaggage` displays — visible during a rolling upgrade as the
value appearing on old pods and not new ones. It needs its own `user.name`
regression test, and belongs in the CHANGELOG **Changed** entry alongside the
other three.

Rows two and three are not about the username either — an ordinary short name is
refused because of what other producers already put in the context. That is the
point of the check (Dependency behavior §9: the alternative is the *downstream*
service silently losing the whole baggage), but it is a behavior change to a
shipped API and is documented as one rather than folded into "just a value cap".

`user.name` is deliberately **not** exempted. Dependency behavior §9 shows that
an unbounded value is what pushes a header past the byte limit, costing a
downstream service its whole baggage for that request — `user.name` first among
it. There is no argument for `user.name` being the one key allowed to do that.
The affected input — a >256-byte username — is not a realistic legitimate value,
but the budget-driven rejections are reachable with entirely ordinary input.
All three are recorded as **Changed**, not **Added**, in the CHANGELOG.

Everything else about the *materialization* surface is preserved: `SetUser`,
`UserName`, `ContextWithUser`, and `WithUserBaggage` keep their signatures,
their error messages, and their Unicode-value behavior, and when the
**effective** whitelist is empty neither the SpanProcessor nor the log handler
is installed.

"Effective" matters because §3 gives `user.name` its own slot: the whitelist is
the application key list **plus** `user.name` when `WithUserBaggage` is set. An
implementation that gated installation on `len(baggageKeys) == 0` would disable
`user.name` materialization for every service that has it today — a regression
of shipped ADR 0016 behavior, caused by generalizing around it.

**But "adopts nothing new, sees no change" is not true for traced NATS
services.** The Decision §10 restoration is deliberately *unconditional* — it is
a bug fix for ADR 0016, not a feature of the whitelist — so a traced NATS
service that upgrades without enabling either option still changes: handler
contexts now carry the baggage that arrived on the wire, and anything that
handler publishes downstream now forwards it, where today both are dropped.

That is the intended repair, and for `user.name` it is what ADR 0016 promised
all along. It is still a propagation-behavior change reaching services that
opted into nothing, so it belongs in the CHANGELOG under **Fixed** with that
scope stated plainly. Services on the native path (§9) are unaffected.

"The existing ADR 0016 tests still pass unmodified" cannot be the criterion,
and earlier revisions of this ADR were wrong to lean on it. Implementation §1
turns `NewSpanProcessor()` into a `Whitelist` method and §4 gives
`NewBaggageHandler` a second parameter, so eight existing call sites — two in
`internal/baggageattrs/baggageattrs_test.go`, six in
`internal/log/handler_test.go` — stop compiling. No compatibility shim is
warranted: both are `internal/` packages with no external consumers, and adding
one purely to avoid touching tests would preserve a signature nothing needs.

**The criterion is therefore: those tests are updated at their call sites only,
and every assertion in them is preserved byte for byte.** A diff that changes an
`assert`/`require` line in either file is a red flag that needs justifying, not
a mechanical port. That is the real non-regression proof; "unmodified" was never
achievable.

Separately, unmodified tests would in any case be evidence of no regression, not
evidence of no behavior change. Dedicated compatibility tests
must pin **all three** rows above — the overlong username, the 64-member
context, and the 8192-byte budget — each accepted today and rejected after.

---

## Consumer guidance

Normative for applications. These are failure modes the SDK cannot prevent.

### One key registry per product

Define the keys once, in one application package, and have every service pass
the same list. Two services with different lists produce a dataset where an
identifier is present on some hops and absent on others — worse than absent
everywhere.

```go
// application code — not the SDK
const (
	KeyRoomID = "chat.room.id"
	KeySiteID = "chat.site.id"
)

func BaggageKeys() []string { return []string{KeyRoomID, KeySiteID} }
```

```go
obs, err := o11y.Init(ctx,
	o11y.WithServiceName("room-service"),
	// ...
	o11y.WithUserBaggage(),                            // user.name — semconv, PII
	o11y.WithBaggageAttributes(app.BaggageKeys()...),  // application-defined
)
```

### The entry span needs an explicit write

`SpanProcessor.OnStart` reads the context **as it was when the span started**.
Middleware always runs *after* the framework's server or consumer span exists
(`otelgin`, `otelhttp`, and the `otel-nats` consumer span are all created
upstream of application middleware). So setting baggage in middleware enriches
every *subsequent* span — child spans, driver spans, downstream services — but
not the entry span itself, which is usually the first span an operator queries.

Applications should therefore do both at the point of identification. Because
span attributes are last-write-wins, the explicit write also overwrites anything
a forged inbound member put on the entry span *for this key*.

The helper below accepts **only keys returned by the application's
`BaggageKeys` registry** and passed to `WithBaggageAttributes` at startup. `Init`
checks that complete registered set against the built Resource and fails on a
collision before any request can reach the explicit span write. Passing an
arbitrary or caller-controlled key to `tag` is outside this contract; the
package-level setter cannot discover process Resource attributes without the
global state ADR 0003 forbids.

**Order matters: set the baggage first, and only write the span if that
succeeded.** The reserved-key, key-length, value-length, and total-size checks
all live in the setter. Writing the span first would put a value on the entry
span that the SDK just refused — a reserved key, or a value past the cap —
letting the span silently bypass every guard this ADR defines:

```go
// application helper for keys from app.BaggageKeys only — log through the SDK
// logger, not the slog default:
// Init deliberately does not call slog.SetDefault (ADR 0003), so a bare
// slog.WarnContext here would bypass the SDK's JSON/OTLP chain and lose the
// traceId/spanId that make this diagnostic findable.
func tag(ctx context.Context, log *slog.Logger, key, value string) context.Context {
	if value == "" {
		return ctx
	}
	// (a) validated: rejects reserved keys, non-token keys, over-cap values,
	//     and members that would push the baggage past the W3C limits
	next, err := o11y.ContextWithBaggageValue(ctx, key, value)
	if err != nil {
		log.WarnContext(ctx, "tag baggage failed",
			slog.String("key", key), slog.Any("error", err))
		return ctx // telemetry must never be load-bearing (ADR 0024)
	}

	// (b) only now the entry span, which started before this baggage existed
	trace.SpanFromContext(next).SetAttributes(attribute.String(key, value))
	return next
}
```

### Public ingress: clear everything, then rebuild

The error and empty-value paths above both return the incoming context
unchanged, so neither removes a forged member. At a public boundary, clear
before setting — and set only values this request has *authorized*, not values
it merely supplied:

```go
func Ingress(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Clear ALL inbound baggage, not just the keys this service knows.
		// The propagator forwards members we have never heard of, so a
		// key a downstream service already materializes but this edge has
		// not learned yet would sail through a per-key removal untouched.
		ctx := baggage.ContextWithoutBaggage(c.Request.Context())

		account := auth.AccountFrom(c) // authenticated
		if account != "" {
			next, err := o11y.ContextWithUser(ctx, account)
			if err != nil {
				// Do not swallow, and do not fall through to SetUser: the
				// setter just refused this value, so writing it to the entry
				// span anyway would bypass the same length and budget checks
				// tag() is careful to run first.
				log.WarnContext(ctx, "set user baggage failed", slog.Any("error", err))
			} else {
				ctx = next
				o11y.SetUser(ctx, account)
			}
		}

		// Authorize before tagging. ContextWithBaggageValue checks syntax,
		// reservations, key/value limits, and total size, never authorization:
		// tagging a raw route parameter would let the caller choose what enters
		// trusted telemetry and downstream baggage.
		if room, ok := authz.ResolveRoom(c, account, c.Param("roomID")); ok {
			ctx = tag(ctx, log, app.KeyRoomID, room.CanonicalID)
		}

		c.Request = c.Request.WithContext(ctx) // without this, none of the above applies
		c.Next()
	}
}
```

An **internal** hop that trusts its callers uses
`o11y.ContextWithoutBaggageValues(ctx, app.BaggageKeys()...)` instead, dropping
application keys it intends to re-derive while keeping the `user.name` a peer
service propagated.

This sanitizes Plane 1 completely. It does **not** clean the entry span: by the
time this middleware runs, `OnStart` has already copied any forged inbound
member onto the server span, and only the keys reaching `tag` above are
overwritten. Close Plane 2 by stripping the `baggage` header ahead of
`otelgin`/`otelhttp` (transport middleware or edge proxy), or accept that
application attributes on public entry spans are untrusted — Decision §7.

Internal service-to-service hops may trust inbound baggage and skip the removal
step — that trust is what makes "identify once at the source" work at all.

### The returned context must be stored back

`ContextWithBaggageValue` and `ContextWithoutBaggageValues` return child
contexts. A middleware that calls them without writing the result back into the
framework (`c.Request = c.Request.WithContext(ctx)` for gin, the equivalent
setter for a NATS router) silently does nothing.

### Other boundaries

- **Egress**: `baggage.ContextWithoutBaggage` alone is **not** sufficient.
  `propagation.Baggage.Inject` calls `carrier.Set` only when the context holds
  non-empty baggage (`propagation/baggage.go:40-45`), so with a cleared context
  it writes nothing — and a `baggage` header already present on the carrier
  (typically copied from the inbound request) survives untouched and reaches the
  third party. Clear the context **and** either start from a fresh carrier or
  delete the `baggage` header from the outbound one.
- **Metrics**: never promote a materialized key to a metric label.
- **`slog` groups**: do not call `WithGroup` on the SDK logger; see Decision §6.
- **Rollout has a required order, and "partial rollout is safe" applies only
  after step 2.** Because the NATS restoration is unconditional (Decision §10)
  and today's consumers drop baggage (§12), a rolling deployment has new pods
  restoring and forwarding while old pods still drop — the same message path
  intermittently carries the identifier, which is worse to debug than not having
  it. Order:
  1. Deploy the SDK version containing the NATS restoration to **every** service
     that consumes from the mesh, including intermediaries and all replicas.
  2. Confirm no pre-restoration consumer remains on any hop.
  3. *Then* enable application baggage keys, which can safely be done service by
     service — a service without the option still forwards baggage and simply
     does not show it on its own telemetry.

---

## Consequences

**Positive**

- The requesting product is unblocked without any product vocabulary entering
  the SDK, and the next product needs no SDK change at all.
- Log-side materialization is sampling-independent, so identifier-based lookup
  keeps full coverage on services that sample their traces (ADR 0015).
- Driver spans (Cassandra, Redis, MongoDB, NATS producer spans) are enriched
  without touching, forking, or waiting on any of those integrations — the
  SpanProcessor sits below all of them. This is the capability ADR 0022 §4 could
  not offer. **The `otel-nats` consumer span is the exception**: restoration
  happens after its `OnStart` (Decision §10). On push paths it needs the
  explicit `SetAttributes` ADR 0022 §4 prescribes, like the HTTP entry span; on
  pull paths that span is already ended and the caller enriches its own child
  span instead. Everything started *within* the handler is covered either way.
- The core logic is unchanged: the iteration and de-duplication in
  `SpanAttributesFromContext`, `LogAttrsFromContext`, and `OnStart` already
  ignore the identity of the keys.
- The post-set size check closes a hazard that exists in ADR 0016 today: nothing
  currently stops a service from building an oversized baggage and silently
  killing `user.name` propagation for every downstream service.

**Negative / trade-offs**

- **More things can now be put on the wire.** ADR 0016 bounded PII exposure by
  making the whitelist un-extendable; this ADR trades that for numeric bounds,
  key validation, and documented guidance. A team can now put something
  sensitive into baggage without an SDK change gating it. This is the real cost
  and it is accepted knowingly: the alternative — an SDK release per application
  identifier — is friction, not a security control.
- **The ingress model depends on application discipline, and is not complete at
  the SDK layer.** The removal primitive fully sanitizes the context, but the
  entry span is stamped by `OnStart` before any application code runs and cannot
  be retracted from a child context (Decision §7, Plane 2). Closing that
  requires stripping the `baggage` header ahead of span creation, which only a
  layer that knows the listener is public can do. A service that skips both
  steps exports forged identifiers on its entry span, and the telemetry looks
  entirely normal.
- **One behavior change** to `ContextWithUser` (see Compatibility).
- The post-set size check adds a `Baggage.String()` per setter call.
- Two enrichment paths now coexist per key (baggage materialization and explicit
  `SetAttributes`), and a reader at a call site cannot tell which produced a
  given attribute — ADR 0016's "Phase 2 is magic" trade-off, now applying to
  more keys.
- The `slog` group limitation is real and unfixable from this layer.

---

## Decision analysis

### Q1 — Where does this capability belong?

| | **SDK (chosen)** | Application |
|---|---|---|
| baggage → span | needs a SpanProcessor at provider construction | reachable but unsupported: a type assertion works only while profiling is not running (Dependency behavior §1), and ADR 0003 forbids the global-state route |
| baggage → log | wraps the handler chain | **possible** via `obs.Logger.Handler()` (Dependency behavior §2) — but log-only, and the key list must then be kept in sync with the span side by hand |
| key naming, value source, trust rules | must not know | the only party that can know |

- **Chosen: mechanism in the SDK, policy in the application.** The span half is
  only implementable in the SDK; the log half is implementable on either side,
  but splitting them puts the burden of keeping one key list consistent across
  two mechanisms on every adopter. Policy is only knowable by the application.

### Q2 — Generalize, or add a second hard-coded key?

| | Second hard-coded entry | **Configurable list (chosen)** |
|---|---|---|
| SDK carries product vocabulary | yes | no |
| Next identifier | another SDK release | zero SDK change |
| Review burden | every key is an SDK PR | bounded by validation + numeric caps |
| PII gating | an SDK PR per key (friction, not control) | documented contract |

- **Chosen: configurable.** Per-key SDK releases make the SDK a bottleneck on
  every consuming product's schema, and the gate they provide is friction rather
  than an actual control.

### Q3 — Should the setter validate the key against the whitelist?

| | Validate against whitelist | **Token validation only (chosen)** |
|---|---|---|
| Producer configuration | must mirror every consumer's whitelist | none needed |
| Failure mode | edge silently drops values consumers wanted | unmaterialized member rides one hop |
| Consistency with ADR 0016 | changes `ContextWithUser` behavior | preserves it |

- **Chosen: no whitelist check, but mandatory W3C token, length, reserved-key,
  and total-size checks.** Whitelisting is a materialization-side concern and the
  producer is a different process. The other checks stay because they protect
  the shared header, which *is* a producer-side concern — and because with the
  whitelist out of the picture they are the setter's only guard.

### Q4 — Should `ContextWithBaggageValue` also write the current span?

| | Fuse both | **Baggage only (chosen)** |
|---|---|---|
| Entry-span gap | closed by the SDK | closed by two lines of application code |
| ADR 0016 symmetry | breaks it (`ContextWithUser` would differ) | preserved |
| Surprise | a "context" function mutates a span | none |
| `user.name` parity | would need `ContextWithUser` changed too | untouched |

- **Chosen: baggage only.** Keeping the setter orthogonal matches ADR 0016's
  existing `SetUser` / `ContextWithUser` split. Revisit if the two-line helper
  proves a recurring source of bugs across adopters.

### Q5 — Why not the sampler escape hatch (Dependency behavior §5)?

It works today and needs no SDK change, but it enriches spans only — no log
records — and log materialization is the sampling-independent half of the
capability. It also mixes enrichment into the sampling decision and occupies the
single `WithTraceSampler` slot. **Recorded as a legitimate interim bridge for an
application that cannot wait for this ADR to ship, and as a non-goal for the
SDK.**

### Q6 — Should the SDK ship a baggage-filtering propagator?

**No — because the application already has this seam, and using it is the
recommended way to close Plane 2.**

An earlier revision of this ADR rejected propagator-level filtering on the
grounds that "the propagator is configured once per SDK instance" and so cannot
tell a public caller from an internal one. **That premise was wrong.** Every
propagator entry point in this SDK takes the propagator as an explicit
per-boundary argument:

```go
http.NewServerHandler(next, tp, mp, prop, ...)   // http/server.go:21
http.NewTransport(base, tp, mp, prop, ...)       // http/transport.go:16
gin.Middleware(service, tp, mp, prop, ...)       // gin/middleware.go:21
nats.Connect(ctx, url, tp, prop, ...)            // nats/conn.go:71
```

Trust is a property of the caller, and the SDK already lets the application
express exactly that — by wiring a different propagator per boundary:

```go
// Public listener: extract trace context, never baggage. The slice must be
// spread into Use — see gin/middleware.go:13-15.
r.Use(gin.Middleware("edge", obs.TracerProvider(), obs.MeterProvider(),
	propagation.TraceContext{})...)

// Internal hops: the SDK propagator, baggage included.
conn, _ := nats.Connect(ctx, url, obs.TracerProvider(), obs.Propagator)
```

This is **strictly better than the removal primitive for public boundaries**,
because it acts *before* `Extract` puts anything in the context and therefore
before `OnStart` stamps the entry span — the one thing Decision §7 Plane 2
identifies as unreachable from middleware. It composes exactly with §7's model:
a public boundary is supposed to discard all inbound baggage and rebuild, and a
TraceContext-only propagator is the cleanest expression of that.

| | SDK-shipped filtering propagator | **Application-composed per boundary (chosen)** |
|---|---|---|
| Granularity | per SDK instance | per boundary, which is where trust actually differs |
| New SDK surface | a propagator type plus its configuration | none — `propagation.TraceContext{}` is upstream |
| Closes Plane 2 | yes | yes |
| Effect on internal hops | needs opting out per hop | untouched; they keep `obs.Propagator` |

**The seam is per boundary object, and for NATS that means per connection, not
per direction.** HTTP splits cleanly — `NewServerHandler` and `NewTransport` are
separate objects, so inbound and outbound are configured independently. A
`nats.Conn` is bidirectional and takes one propagator for the whole connection
(`nats/conn.go:92-95`), so giving it `propagation.TraceContext{}` also stops
newly authenticated baggage from being **injected** into that connection's
outbound publishes. A NATS listener exposed to untrusted callers therefore needs
a separate connection from the one it publishes trusted work on. In practice
NATS is normally an internal transport and this does not arise; where it does,
the two-connection split is the answer, and it should be a deliberate choice
rather than a surprise.

So the SDK adds nothing here. `ContextWithoutBaggageValues` (Decision §4)
remains the tool for **internal** boundaries that want to drop specific keys
while keeping others — a case the propagator swap cannot express, since it is
all-or-nothing on baggage.

---

## Implementation specifics (settle in the implementing PR)

1. **`internal/baggageattrs`** — introduce `Whitelist` with `NewWhitelist`,
   `Len`, `Keys` (defensive copy), and the three existing functions converted to
   methods; delete the package-level `var baggageWhitelist`. `attributeKeyFor`
   maps `UserNameKey` to `semconv.UserNameKey` and any other key to
   `attribute.Key(key)`. Add `ValidBaggageKey`, `ContextWithValue`,
   `ContextWithoutValues`, `MaxBaggageValueBytes`, `MaxBaggageKeyBytes`.
   Reimplement `ContextWithUser` on `ContextWithValue`, keeping its current error
   messages. `NewWhitelist` must copy its variadic input, and `Keys` must return
   a copy.
2. **Key validation** — `ValidBaggageKey` uses `baggage.NewMember(key, "x")` as a
   token probe, plus the length and reserved-key checks. Member construction
   continues to use `NewMemberRaw(key, value)`. Do not hand-roll the RFC 7230
   token grammar, and do not switch value construction to `NewMember`.

   **Order matters: validate the key — syntax, length, reserved — *before*
   applying the empty-value no-op.** Otherwise the two rules collide silently on
   the cases where both apply: `ContextWithBaggageValue(ctx, "", "")` and
   `ContextWithBaggageValue(ctx, UserNameKey, "")` must both return an error, not
   the quiet no-op an empty value would otherwise earn them. An empty key is
   invalid; an empty **value** is the documented no-op, but only once the key has
   passed. Both cases are required tests. The reserved set
   includes `user.name`: the **public** setter refuses it, and `ContextWithUser`
   reaches the shared implementation through an unexported path that allows it
   (Decision §4). Routing `ContextWithUser` through the public setter breaks
   ADR 0016 and is wrong.
3. **Size enforcement** — `ContextWithValue` builds the candidate baggage, then
   serializes it and rejects with the original context if more than 64 members
   have a non-empty `Member.String()` or `len(bag.String()) > 8192`. Do not use
   `bag.Len()` for the first check: it counts local `NewMemberRaw` entries whose
   invalid W3C keys `Baggage.String()` omits, and those entries consume no header
   budget. Separately, the **materialization** side
   (`SpanAttributesFromContext` / `LogAttrsFromContext`) skips any whitelisted
   member whose value exceeds `MaxBaggageValueBytes`: the setter cap governs only
   what this SDK produces, and an inbound member can be far larger (Decision §8).
4. **`internal/log/handler.go`** — replace `hasUserNameAttr bool` with a
   `presetKeys map[string]struct{}` built clone-on-write in `WithAttrs` (never
   mutate a map shared with a derived handler) and **reset in `WithGroup`**,
   matching today's `hasUserNameAttr` and the output pinned by
   `TestBaggageHandlerWithGroupDoesNotInheritUserNameAttr` (Decision §6). Carry
   the `Whitelist` on the handler. Both `recordHasAttr` and the
   `presetKeys` accumulation must call `Value.Resolve()` and then recurse into
   empty-key groups, which `slog` inlines into the output
   (`log/slog/handler.go:57`, resolved first at `:468`). If that scan encounters
   any `LogValuer`, delegate its resolved form — for scalars and named groups as
   well as empty-key groups — by rebuilding the record in `Handle` and passing a
   resolved slice from `WithAttrs`. Otherwise a non-idempotent value is resolved
   again by the inner handler and can change the collision decision. Keep the
   no-rebuild path when no `LogValuer` was resolved. `NewBaggageHandler` takes
   the whitelist as a second parameter. Preserve the precedence in Decision §6.
5. **`options.go`** — add `baggageKeys []string` to `Config` for application
   keys and **keep `userBaggage bool` as its own slot** (Decision §3), so
   `user.name` is neither settable through the generic option nor evictable by
   `MaxBaggageAttributeKeys`. Add `WithBaggageAttributes` and
   `MaxBaggageAttributeKeys`; update `WithUserBaggage`'s doc comment. Drop with a
   WARN on empty, invalid, statically reserved, and `user.name` keys, following
   `WithExtraHTTPServerAttributeKeys` (`options.go:417`). Accumulate valid unique
   keys without truncating; the cap is applied at `Init` after its Resource
   collision check.
6. **`o11y.go`** — form the effective whitelist from both slots (`user.name`
   when `userBaggage`, plus `baggageKeys`) and compare **every effective key**
   with the built Resource's own attributes (`res.Attributes()`, available from
   `o11y.go:192`) before constructing the `Whitelist` — the
   `OTEL_RESOURCE_ATTRIBUTES` case in Decision §6 that no static list can cover.
   This includes `user.name`: `WithUserBaggage` makes it materializable and
   `SetUser` writes it explicitly, so treating it differently would preserve the
   same shadowing gap under another option. Collect all colliding keys and return
   one deterministic `Init` error listing them; do not WARN-and-drop them.

   **`MaxBaggageAttributeKeys` is enforced here, after that collision check, not
   in the option.** If a collision exists initialization has already failed, so
   no colliding key can consume a slot in a partially configured SDK. Otherwise
   truncate the validated unique list to the cap, warn on the overflow, and gate all three sites
   (`o11y.go:208`, `:310`, `:316`) on `whitelist.Len() > 0`. Generalize
   `appendUserBaggageWarnings` (`o11y.go:496`) to log the effective key list at
   startup — the first thing an operator needs when an expected attribute is
   missing — and to warn on the trace-disabled combination.

   Scope that warning precisely: with the **trace pillar** off the propagator is
   still built (`o11y.go:203`), so baggage still propagates and still
   materializes on log records; only spans are lost. It must not be worded as
   though baggage stops. The native NATS case (Decision §9) *does* stop
   propagation, but `Init` cannot detect it — the connection is created later,
   by a separate call — so it stays documentation-only rather than a startup
   warning the SDK is not in a position to emit.
7. **`nats/conn.go` and `nats/jetstream.go`** (Decision §10) — wrap the handler
   invocation at `conn.go:131-133`, `:157-159`, `jetstream.go:365`, and `:542`
   — plus the `Messages(ctx)` iterator's `Next` (`jetstream.go:374`) and
   single-shot `Consumer.Next` (`jetstream.go:592-603`), which today returns the
   upstream context unchanged — so the handler context carries the message's
   baggage. Extract with the **connection's own** propagator **into a throwaway
   `context.Background()`** — never into the handler context — and graft only
   `baggage.FromContext` of that result onto the handler context.

   Both halves of that are load-bearing. Applying the propagator to the handler
   context directly would overwrite the consumer span context `otel-nats` just
   set. Passing the handler context as `Extract`'s *parent* is subtler: `Extract`
   falls back to the parent when the message carries no `baggage` header
   (Dependency behavior §9), so whatever baggage the handler context already held
   would be read back out and re-grafted — laundering ambient or subscription-level
   baggage into a message that carried none. A `context.Background()` parent makes
   "no header" mean exactly "no baggage restored". Skip the
   restore entirely when tracing is disabled for the connection (Decision §9;
   `conn_direct.go:57-61` still invokes the facade wrapper with intact headers).
   **The JetStream wrapper graph cannot reach that policy today, and must be
   changed to carry it.** `Conn` embeds `*otelnats.Conn` (`nats/conn.go:34-36`)
   so Core NATS can reach `TraceContext()`, but the reference is dropped at the
   very first JetStream hop: `JetStream()` returns `&jetStream{js: js}`
   (`nats/jetstream.go:274`), and `stream` (`:328`), `consumer` (`:356`) and
   `messagesContext` (`:610`) each hold only their upstream object. By the time
   `Consume`, `Next`, `Messages().Next` or the fetched-message forwarder runs,
   the connection's propagator and `tracingEnabled` are unreachable.

   So the implementation must:

   - give `jetStream`, `stream`, `consumer` and `messagesContext` either the
     `*otelnats.Conn` or an immutable `{propagator, tracingEnabled}` policy
     value;
   - thread it through **every** constructor that mints one of those wrappers —
     `JetStream()`, `CreateOrUpdateStream()` (`nats/jetstream.go:288-294`,
     which returns its own fresh `&stream{s: s}`), `Stream()`,
     `CreateOrUpdateConsumer()`, `Consumer()`, `Messages()`, and the
     fetched-batch forwarder — not only the ones on the delivery path being
     fixed at the time;
   - be tested per **constructor path**, not only per delivery method. A
     consumer obtained through `CreateOrUpdateConsumer` and one obtained through
     `Consumer` must both restore, and so must one reached through a `Stream`
     returned by `CreateOrUpdateStream` rather than `Stream()`.

   Without this the tempting shortcut is to hard-code a propagator in the
   JetStream paths — which is precisely the defect this decision already had to
   correct once.

   Re-audit these call sites on every `otel-nats` bump — if upstream starts
   preserving `msgCtx`, the restore becomes redundant rather than wrong, but the
   ADR's §12 evidence would need updating.
8. **`attributes.go`** — add `ContextWithBaggageValue`,
   `ContextWithoutBaggageValues`, the exported `UserNameKey` applications need to
   name `user.name` in a sanitization set, and a root-package
   `MaxBaggageValueBytes` / `MaxBaggageKeyBytes` aliases so every limit named in
   the public doc comments is resolvable from package `o11y`.
9. **Tests**
   - Whitelist: dedup, order, empty, over-cap, invalid token, reserved key;
     per-instance isolation (two whitelists, no cross-talk); mutating the slice
     passed to `NewWhitelist` after construction does not change the whitelist;
     mutating the slice returned by `Keys()` does not either.
   - Cap independence: eight application keys plus `WithUserBaggage` yields nine
     materialized keys, in either option order; a ninth application key is
     dropped with a warning and `user.name` is not.
   - `WithBaggageAttributes("user.name")` is refused and warns; the public
     `ContextWithBaggageValue(ctx, UserNameKey, ...)` returns an error while
     `ContextWithUser` still succeeds.
   - Empty-key groups: an explicit `slog.Group("", slog.String(k, v))` suppresses
     the baggage value for `k`, in both the record and the `WithAttrs` paths;
     and the same for `slog.Any("", v)` where `v` is a `LogValuer` resolving to
     such a group, which only presents as a group after `Resolve()`.
   - `WithGroup` resets the preset set:
     `TestBaggageHandlerWithGroupDoesNotInheritUserNameAttr` passes unmodified,
     with the explicit value at top level and the baggage value under the group.
   - Empty key rejected at both the option and the setter; empty value is a
     no-op returning no error — **but** `("", "")` and `(UserNameKey, "")` both
     return errors, pinning that key validation runs before the no-op.
   - **Round trip**: for each of the four keys in the Dependency behavior §8
     table, assert `Inject` → `Extract` actually carries (or refuses) the value.
     A constructor-only assertion is explicitly insufficient.
   - Unicode **value** survives the round trip (ADR 0016 regression guard).
   - Size: setter rejects and preserves the original context at >64
     **serializable** members and at >8192 encoded bytes; over-length key and
     value rejected. A mixed context containing 64 locally created members whose
     invalid W3C keys serialize to empty plus one ordinary valid member is
     accepted and injects exactly that one member, proving `Baggage.Len()` is not
     used as the wire cap.
   - Removal: `ContextWithoutBaggageValues` drops the named keys and preserves
     members it was not asked to remove, including `user.name` when it is not
     named — and drops it when it is.
   - Collision: baggage members named `service.name`, `service.version`,
     `service.namespace`, `environment`, `deployment.environment.name`,
     `traceId`, `spanId`, `time`, `level`, `msg`, `source` are refused at both
     the option and the setter; a test asserts no duplicate JSON field is
     emitted.
   - Budget: a header of 70 members extracts as 64 (truncation, not total loss)
     and one over 8192 bytes extracts as empty — pinning the Dependency
     behavior §9 semantics so a future OTel bump that changes them is caught.
   - Log handler: `WithAttrs` precedence for an arbitrary key; `WithAttrs` +
     `WithGroup` combinations; a `-race` test exercising a handler derived
     concurrently.
   - Span precedence: `OnStart` value overwritten by a later `SetAttributes`.
   - Compatibility: an **inbound** `user.name` over `MaxBaggageValueBytes` is
     materialized today and skipped after, plus all three `ContextWithUser`
     rejections that today's implementation accepts — a >256-byte username, an ordinary username into a
     context already holding 64 wire-serializable members, and one that would push the encoded
     baggage past 8192 bytes — each leaving ctx unchanged.
   - An empty **effective whitelist** installs neither the processor nor the
     handler. An empty application key list with `WithUserBaggage()` enabled is
     not empty and still installs both integrations.
   - **NATS baggage restoration** (Decision §10): publish with baggage set,
     receive through `Subscribe`, `QueueSubscribe`, JetStream `Consume`, the
     fetched-message channel, the `Messages(ctx)` iterator, and single-shot
     `Consumer.Next` — all four upstream discarding sites plus both facade
     wrappers; assert the handler context carries the members and that the
     consumer span context is unchanged by the restore. Cover each **constructor
     path** too, not just each delivery method: a consumer reached via
     `CreateOrUpdateConsumer` and one via `Consumer` must both restore, which is
     what catches a policy field threaded through only some of the wrappers. On the pull paths also
     assert that a child span started from the restored context carries the
     attributes, since the receive span itself is ended and cannot. A regression
     here silently returns the SDK to the §12 behavior, so this is the test that
     matters most for the stated use case. `ChanSubscribe` and
     `ChanQueueSubscribe` are intentionally excluded per Decision §10: neither
     facade exposes those context-less native delivery APIs. The native-mode
     no-inject/no-extract case is pinned separately by the Boundary contract
     test below.
   - Semconv keys are refused at both the option and the setter, covering all
     three shapes: one the SDK emits (`http.response.status_code`), one it does
     **not** emit but semconv defines as an int (`http.response.body.size`), and
     a wildcard-catalogued one that only a namespace-prefix check catches
     (`process.pid`), and one from a **parameterized family** with no per-member
     constant at all (`http.request.header.authorization`) — the case that
     passes an exact-key-only generator and is exactly why prefix rules exist.
     An application-namespaced key is still accepted.
   - The generated semconv key set matches the pinned package — the CI gate's
     own regression test.
   - A key supplied through `OTEL_RESOURCE_ATTRIBUTES` causes `Init` to fail when
     it also appears in the effective whitelist, with the colliding key in the
     error; no SDK, whitelist, or entry-span writer is made available in that
     configuration. Cover both an application key and `user.name` enabled via
     `WithUserBaggage()`. Multiple collisions are reported together in
     deterministic order.
   - Cap ordering remains deterministic after the Resource check: with no
     collisions, the first eight validated unique application keys materialize
     and the ninth is warned and dropped. A colliding key never consumes a slot
     because its configuration fails as a whole.
   - A non-idempotent `LogValuer` supplied through `Logger.With` (the `WithAttrs`
     path) suppresses correctly, not only one supplied on the record.
   - A non-idempotent `LogValuer` resolving first to an empty-key group produces
     exactly one field: the scan's decision and the emitted JSON agree. Also
     cover the inverse-shape case from this review: first resolution is a scalar
     or named group and the second would be an empty-key group containing a
     whitelisted key; delegation must use the first resolved form, so the valuer
     is called once and no duplicate field appears. Run both cases through
     record attributes and `Logger.With`.
   - Materialization skips an inbound member whose value exceeds
     `MaxBaggageValueBytes`, built from a legal W3C header the setter never saw.
   - **Boundary contract** (integration-level, pinning what Decision §7 Plane 2
     and Q6 now recommend): a server wired with a `TraceContext`-only propagator
     records no application baggage attributes on its entry span even when the
     request carries a forged `baggage` header, while the same server wired with
     `obs.Propagator` does; and an effectively disabled NATS connection with no
     relay or upstream env override neither injects nor extracts baggage,
     pinning the Decision §9 scope limit so a future upstream change that
     quietly restores propagation is noticed rather than assumed.
   - Existing ADR 0016 tests are ported at their **call sites only** — the two
     `NewSpanProcessor()` sites and the six `NewBaggageHandler(base)` sites —
     with every assertion preserved verbatim (Compatibility).
10. **Docs** — `README.md`, which is the SDK's option reference and would
   otherwise go stale the moment `WithBaggageAttributes` ships; an
   "Application-defined baggage attributes" section in `docs/semconv.md`
   recording that these keys are *not* SDK-owned, are therefore absent from the
   catalog, and remain bound by the never-a-metric-label (ADR 0016 Q3) and
   never-a-span-name (ADR 0023) rules; a `docs/guide.md` section covering the
   entry-span gap, remove-then-set at ingress and what it does *not* cover
   (Decision §7, Plane 2), the context write-back, and the `slog` group
   limitation; `CHANGELOG.md` with the `ContextWithUser` cap under **Changed**.
11. **Neutral example** — the `app.room_id` illustration in `AGENTS.md:286`
    predates this ADR and models a naming convention an adopting product is
    unlikely to follow, so SDK documentation would compete with the adopter's own
    key registry over the same identifier. Change it to a domain-neutral one, as
    `docs/guide.md:903` already does with `app.order_id`.
