# ADR 0025 — Application-Defined Baggage Attributes

**Status**: Proposed
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
`semconv/v1.39.0`) and the SDK tree on this branch. Items 8–11 are the
substantive findings from review; they materially changed this ADR.

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
   (`o11y.go:203`), so disabling the *trace pillar* still forwards baggage. On
   the usual paths — HTTP via `otelhttp`/`otelgin`, NATS and JetStream via the
   facade's Inject/Extract — a baggage member already rides every hop today, and
   **nothing about transport needs to change.**

   The exception is `nats.ConnectWithOptions(..., WithTracingEnabled(false))`,
   whose own documentation states the contract: *"no spans, **no propagation
   injection/extraction**, and JetStream wrappers backed by the upstream direct
   implementation"* (`nats/options.go:23-29`). This is a supported, deliberate
   native-cost mode, and `AGENTS.md:130-134` recommends driving it from
   `obs.Toggles.Trace`. In it, **baggage stops at every NATS hop** — application
   keys and `user.name` alike — so a downstream service materializes nothing,
   and no SDK-side change to materialization can recover it. Recorded as a
   scope limitation in Decision §9 rather than papered over: this feature
   requires the traced NATS path, and a service that hard-disables it keeps
   whatever per-service explicit tagging it already does.

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
values into one label. Keys that are actually *rejected* are dropped with a
startup WARN rather than failing `Init`, matching that option
(`options.go:417`).

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

The SDK rejects — with a startup WARN, and at the setter with an error — keys
that would shadow its own identity or correlation fields. Two groups:

- **Emitted by handlers inside the BaggageHandler**: `service.name`,
  `environment` (`o11y.go:267`), `traceId`, `spanId`
  (`internal/log/handler.go:30-33`), and `slog.JSONHandler`'s own built-in
  record fields `time`, `level`, `msg`, `source`. Per Dependency behavior §11 no
  de-duplication at the BaggageHandler layer can see any of these, so only
  refusing the key prevents a duplicate JSON field. The `slog` built-ins matter
  as much as the SDK's own: a baggage member named `level` or `msg` lets an
  untrusted caller forge a second severity or message field in every record the
  service emits.
- **Resource-level service identity**: `service.version`, `service.namespace`,
  `deployment.environment.name` (`docs/semconv.md`, Resource Attributes). These
  do not collide on the log side, but as *span* attributes they shadow the
  resource attributes of the same name in trace-backend queries, letting an
  untrusted caller make a span appear to belong to a different version,
  namespace, or environment. That is the same spoofing class as `service.name`
  and is refused for the same reason.

Applications are additionally
directed to use a product namespace, but namespacing is guidance, not an
enforced rule — an unnamespaced `request_id` is harmless and refusing it buys
nothing.

Precedence is defined, not left emergent:

- **Spans**: `OnStart` writes the baggage value first; any later
  `SetAttributes` for the same key wins (Dependency behavior §10). Application
  code can therefore always correct a baggage-derived span attribute.
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
gin.Middleware("edge", obs.TracerProvider(), obs.MeterProvider(),
	propagation.TraceContext{})

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
| Post-set baggage size | 64 members / 8192 bytes | the whole header. Checked **after** the member is added; on breach the setter returns an error and the **original** context. |

The post-set check is what actually prevents Dependency behavior §9. It costs a
`Baggage.String()` per set; with at most 64 members that is accepted knowingly
in exchange for not silently truncating or dropping baggage at every downstream
hop. Do not remove it for performance without replacing it with an equivalent
bound.

### 9. Scope limitation: the traced NATS path is required

This feature carries values over W3C Baggage, which needs the propagator's
Inject/Extract to actually run on each hop. `nats.ConnectWithOptions(...,
WithTracingEnabled(false))` deliberately removes exactly that
(`nats/options.go:23-29`, Dependency behavior §3), so in that mode baggage —
application keys and `user.name` alike — stops at every NATS and JetStream hop.

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
// or slog's own record fields (time, level, msg, source); keys longer than
// MaxBaggageKeyBytes;
// user.name, which is reserved for WithUserBaggage because it carries a PII
// contract this option does not state; and keys beyond MaxBaggageAttributeKeys.
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
// push the baggage past the W3C limits of 64 members / 8192 encoded bytes.
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
| any username, when ctx already holds 64 unrelated members | accepted | rejected (member cap) |
| any username, when the encoded baggage would pass 8192 bytes | accepted | rejected (byte cap) |

The last two are not about the username at all — an ordinary short name is
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

Everything else is preserved: `SetUser`, `UserName`, `ContextWithUser`, and
`WithUserBaggage` keep their signatures, their error messages, and their
Unicode-value behavior. A service that adopts nothing new sees no change — with
an empty key list neither the SpanProcessor nor the log handler is installed,
exactly as today.

"The existing ADR 0016 tests still pass unmodified" is evidence of no
regression, not evidence of no behavior change. A dedicated compatibility test
must pin the one change: a >256-byte username accepted before, rejected after.

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

**Order matters: set the baggage first, and only write the span if that
succeeded.** The reserved-key, key-length, value-length, and total-size checks
all live in the setter. Writing the span first would put a value on the entry
span that the SDK just refused — a reserved key, or a value past the cap —
letting the span silently bypass every guard this ADR defines:

```go
// application helper — log through the SDK logger, not the slog default:
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

		// Authorize before tagging. ContextWithBaggageValue checks syntax and
		// size, never authorization: tagging a raw route parameter would let the
		// caller choose what enters trusted telemetry and downstream baggage.
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
- **Partial rollout is safe**: baggage propagates through services that have not
  enabled the option; those services simply do not show the value on their own
  telemetry.

---

## Consequences

**Positive**

- The requesting product is unblocked without any product vocabulary entering
  the SDK, and the next product needs no SDK change at all.
- Log-side materialization is sampling-independent, so identifier-based lookup
  keeps full coverage on services that sample their traces (ADR 0015).
- Driver spans (Cassandra, Redis, MongoDB, NATS) and third-party-created spans
  (`otel-nats` consumer spans) are enriched without touching, forking, or
  waiting on any of those integrations — the SpanProcessor sits below all of
  them. This is the capability ADR 0022 §4 could not offer.
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
| baggage → span | needs a SpanProcessor at provider construction | impossible: no `WithSpanProcessor`, provider returned as interface and wrapped by pyroscope, ADR 0003 forbids the global-state route |
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
// Public listener: extract trace context, never baggage.
publicMux := gin.Middleware("edge", obs.TracerProvider(), obs.MeterProvider(),
	propagation.TraceContext{})

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
   token grammar, and do not switch value construction to `NewMember`. An empty
   key is invalid; an empty **value** is the documented no-op. The reserved set
   includes `user.name`: the **public** setter refuses it, and `ContextWithUser`
   reaches the shared implementation through an unexported path that allows it
   (Decision §4). Routing `ContextWithUser` through the public setter breaks
   ADR 0016 and is wrong.
3. **Size enforcement** — `ContextWithValue` builds the candidate baggage, then
   rejects and returns the original context if the result exceeds 64 members or
   `len(bag.String()) > 8192`.
4. **`internal/log/handler.go`** — replace `hasUserNameAttr bool` with a
   `presetKeys map[string]struct{}` built clone-on-write in `WithAttrs` (never
   mutate a map shared with a derived handler) and **reset in `WithGroup`**,
   matching today's `hasUserNameAttr` and the output pinned by
   `TestBaggageHandlerWithGroupDoesNotInheritUserNameAttr` (Decision §6). Carry
   the `Whitelist` on the handler. Both `recordHasAttr` and the
   `presetKeys` accumulation must call `Value.Resolve()` and then recurse into
   empty-key groups, which `slog` inlines into the output
   (`log/slog/handler.go:57`, resolved first at `:468`). `NewBaggageHandler` takes the
   whitelist as a second parameter. Preserve the precedence in Decision §6.
5. **`options.go`** — add `baggageKeys []string` to `Config` for application
   keys and **keep `userBaggage bool` as its own slot** (Decision §3), so
   `user.name` is neither settable through the generic option nor evictable by
   `MaxBaggageAttributeKeys`. Add `WithBaggageAttributes` and
   `MaxBaggageAttributeKeys`; update `WithUserBaggage`'s doc comment. Drop-with-WARN
   on empty, invalid, reserved, `user.name`, duplicate, and over-cap keys,
   following `WithExtraHTTPServerAttributeKeys` (`options.go:417`).
6. **`o11y.go`** — assemble the whitelist from both slots (`user.name` when
   `userBaggage`, plus `baggageKeys`) and gate all three sites
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
7. **`attributes.go`** — add `ContextWithBaggageValue`,
   `ContextWithoutBaggageValues`, the exported `UserNameKey` applications need to
   name `user.name` in a sanitization set, and a root-package
   `MaxBaggageValueBytes` / `MaxBaggageKeyBytes` aliases so every limit named in
   the public doc comments is resolvable from package `o11y`.
8. **Tests**
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
     no-op returning no error.
   - **Round trip**: for each of the four keys in the Dependency behavior §8
     table, assert `Inject` → `Extract` actually carries (or refuses) the value.
     A constructor-only assertion is explicitly insufficient.
   - Unicode **value** survives the round trip (ADR 0016 regression guard).
   - Size: setter rejects and preserves the original context at >64 members and
     at >8192 encoded bytes; over-length key and value rejected.
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
   - Compatibility: all three `ContextWithUser` rejections that today's
     implementation accepts — a >256-byte username, an ordinary username into a
     context already holding 64 members, and one that would push the encoded
     baggage past 8192 bytes — each leaving ctx unchanged.
   - Empty key list installs neither the processor nor the handler.
   - **Boundary contract** (integration-level, pinning what Decision §7 Plane 2
     and Q6 now recommend): a server wired with a `TraceContext`-only propagator
     records no application baggage attributes on its entry span even when the
     request carries a forged `baggage` header, while the same server wired with
     `obs.Propagator` does; and a `nats.ConnectWithOptions(...,
     WithTracingEnabled(false))` connection neither injects nor extracts
     baggage, pinning the Decision §9 scope limit so a future upstream change
     that quietly restores propagation is noticed rather than assumed.
   - Existing ADR 0016 tests pass **unmodified**.
9. **Docs** — `README.md`, which is the SDK's option reference and would
   otherwise go stale the moment `WithBaggageAttributes` ships; an
   "Application-defined baggage attributes" section in `docs/semconv.md`
   recording that these keys are *not* SDK-owned, are therefore absent from the
   catalog, and remain bound by the never-a-metric-label (ADR 0016 Q3) and
   never-a-span-name (ADR 0023) rules; a `docs/guide.md` section covering the
   entry-span gap, remove-then-set at ingress and what it does *not* cover
   (Decision §7, Plane 2), the context write-back, and the `slog` group
   limitation; `CHANGELOG.md` with the `ContextWithUser` cap under **Changed**.
10. **Neutral example** — the `app.room_id` illustration in `AGENTS.md:286`
    predates this ADR and models a naming convention an adopting product is
    unlikely to follow, so SDK documentation would compete with the adopter's own
    key registry over the same identifier. Change it to a domain-neutral one, as
    `docs/guide.md:903` already does with `app.order_id`.
