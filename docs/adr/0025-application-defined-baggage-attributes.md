# ADR 0025 — Application-Defined Baggage Attributes

**Status**: Proposed
**Date**: 2026-08-05
**Relates to**: ADR 0016 (user identity — this ADR generalizes the fixed
whitelist that ADR 0016 §Implementation specifics 3 deliberately left closed),
ADR 0003 (global-state policy — the whitelist must become instance state, not
package state), ADR 0006 (semconv — application keys have no semconv constant to
source from), ADR 0015 (sampling — span attributes only record on sampled
spans), ADR 0022 §4 (domain identifiers belong to the application, not the SDK),
ADR 0023 (span naming — high-cardinality identifiers must never enter span
names), `docs/semconv.md` (attribute catalog)

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
  from the route parameter; it is set after authorization; it is overwritten
  rather than trusted at the edge." None of this is knowable by the SDK, and
  none of it should be.

This is the same line ADR 0022 §4 already drew for span attributes — *domain
identifiers the SDK has no way to know (a room ID, a site ID, a request ID from
the payload)* — extended from the single-span case to the cross-service case.

`user.name` is the standing exception, and stays one for a stated reason: it has
a pinned semconv constant (`semconv/v1.39.0`), so centralizing it in the SDK is
what makes a future semconv rename a one-place edit (ADR 0006). Application keys
have no such constant and gain nothing from living in the SDK.

---

## Dependency behavior (verified)

Verified against the tree at the time of writing.

1. **The application cannot register a SpanProcessor.** `Init` exposes no
   `WithSpanProcessor`; `SDK.TracerProvider()` returns the
   `oteltrace.TracerProvider` *interface* (`o11y.go:99`); the concrete
   `tracerProviderInternal *sdktrace.TracerProvider` is unexported
   (`o11y.go:86`); and when profiling is enabled the public provider is a
   `otelpyroscope.NewTracerProvider(tpInternal)` wrapper (`o11y.go:387`), so a
   type assertion back to `*sdktrace.TracerProvider` fails on exactly the
   configuration most services run. Building a second TracerProvider to get the
   hook means abandoning the SDK, and reaching the SDK's provider through
   `otel.SetTracerProvider` is forbidden by ADR 0003.

2. **The application cannot wrap the log handler chain.** The handler chain is
   assembled and closed over inside `Init` (`o11y.go:295-320`); the application
   receives the finished `*slog.Logger`. There is no seam to insert an enricher
   into.

3. **Baggage transport already works, unconditionally.** The composite
   propagator (`propagation.TraceContext{}` + `propagation.Baggage{}`) is built
   in `internal/trace/trace.go` and mirrored on the trace-disabled path
   (`o11y.go:203`), and the NATS facade injects with it. A baggage member
   already rides every HTTP and NATS hop today. **Nothing about transport needs
   to change.** The gap is materialization only.

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
   (`sdk@v1.44.0/trace/sampling.go:34`) and may return
   `SamplingResult.Attributes` (`sampling.go:62`), which the SDK applies to the
   span at creation (`sdk@v1.44.0/trace/tracer.go:172`). So an application can,
   today, read baggage in a wrapping sampler and stamp every span. This works,
   and is a legitimate short-term bridge, but it is not the answer: it conflates
   sampling with enrichment, occupies the single `WithTraceSampler` slot, runs
   on every span-start hot path in application-owned code, and — decisively —
   **does nothing for logs.**

6. **Baggage is a bounded budget.** W3C baggage as implemented in
   `otel@v1.44.0/baggage/baggage.go:21-22` allows at most 64 members and 8192
   bytes total across the whole header, shared by every producer in the process.
   An open-ended whitelist is a way to spend a shared budget, so the SDK must
   bound it.

7. **Sampling asymmetry (ADR 0015).** `span.SetAttributes` is a no-op on a
   non-recording span, and `SpanProcessor.OnStart` only fires for recording
   spans. So span-side materialization tracks the head sampling rate, while
   **baggage propagation and log materialization are sampling-independent.** For
   a "find everything about identifier X" workflow this makes the log side the
   more reliable of the two, which is the strongest argument for generalizing
   the mechanism rather than telling applications to stamp spans themselves.

---

## Decision

**Generalize ADR 0016's fixed whitelist into an application-configured list of
opaque baggage keys. Add no application-domain vocabulary to the SDK.**

Five parts:

### 1. The whitelist becomes instance state

`internal/baggageattrs` grows a `Whitelist` value type built from a key list.
The package-level `var baggageWhitelist` is removed. Two SDK instances in one
process must not share or clobber each other's configuration (ADR 0003).
`UserNameKey` continues to map to the pinned `semconv.UserNameKey`; every other
key maps to an `attribute.Key` of the same string, because application keys have
no semconv constant to source from (ADR 0006, and recorded as such in
`docs/semconv.md`).

### 2. A new option: `WithBaggageAttributes(keys ...string)`

Enables materialization of the named baggage members onto this service's spans
and SDK log records. Keys are opaque to the SDK. Calls accumulate and
de-duplicate. Invalid keys are dropped with a startup WARN rather than failing
`Init`, matching the established behavior of
`WithExtraHTTPServerAttributeKeys` (`options.go:417`).

### 3. `WithUserBaggage()` is retained, re-expressed, and unchanged in behavior

It becomes `WithBaggageAttributes(baggageattrs.UserNameKey)` plus the ADR 0016
PII contract in its doc comment. Existing callers are unaffected; the option is
kept rather than deprecated because `user.name` carries a PII contract that a
generic key list cannot express.

### 4. A new source-side setter: `ContextWithBaggageValue(ctx, key, value)`

The key-agnostic sibling of `ContextWithUser`. It deliberately **does not**
validate the key against any whitelist: the producer (an edge service) and the
materializing consumers are different processes with independent configuration,
so a local check would force every producer to also carry every consumer's
whitelist. Whitelisting is a materialization-side concern. This preserves ADR
0016's existing `ContextWithUser` behavior rather than changing it.

### 5. Bounds, enforced by the SDK

- At most **8** materialized keys per SDK instance (`MaxBaggageAttributeKeys`).
- At most **256 bytes** per value (`MaxBaggageValueBytes`), returned as an error
  from the setter.

Both exist to protect the shared 64-member / 8192-byte header budget from a
single caller, and to keep the per-span and per-record enrichment loops short.
They are deliberately low: raising either is a conscious change, in the same
spirit as the clause this ADR supersedes.

### What is explicitly NOT decided here

- **No application key enters the SDK.** No `chat.*`, no `SetRoom`, no
  `RoomID`. A reviewer should be able to reject this ADR's implementation PR on
  sight if a product-domain word appears in the diff.
- **Metric labels are untouched.** The whitelist bounds *baggage*, not metric
  dimensions. Keeping high-cardinality identifiers off `internal/metrics` remains
  an enforced-by-convention rule (ADR 0016 Q3), and this ADR widens the set of
  keys to which it applies.
- **Span names are untouched** (ADR 0023).

---

## API change

```go
// options.go

// MaxBaggageAttributeKeys bounds how many baggage keys one SDK instance
// materializes.
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
// Calls accumulate and de-duplicate. Keys that are not valid W3C baggage tokens,
// and keys beyond MaxBaggageAttributeKeys, are dropped with a startup WARN.
// Never route a materialized key into a metric label.
func WithBaggageAttributes(keys ...string) Option

// WithUserBaggage is WithBaggageAttributes for the pinned semconv user.name key,
// plus the ADR 0016 PII contract: user.name is personal data, so enabling this
// puts PII on the wire at every hop.
func WithUserBaggage() Option

// attributes.go

// ContextWithBaggageValue returns a child context carrying key=value as a W3C
// baggage member. Services that enabled WithBaggageAttributes(key) then show it
// on their spans and log records with no per-call-site code.
//
// The key is not checked against any whitelist — the whitelist is a
// materialization-side setting and the producer is usually a different process.
// Values over MaxBaggageValueBytes are rejected. An empty key or value leaves
// ctx unchanged.
//
// Because the SDK propagator includes W3C Baggage, the value travels on HTTP and
// NATS headers. Set it only from data this service has already validated; never
// trust an inbound baggage value from an untrusted caller; strip baggage before
// calling third parties; never route the key into a metric label.
func ContextWithBaggageValue(ctx context.Context, key, value string) (context.Context, error)

// internal/baggageattrs

// Whitelist is an immutable, per-instance set of baggage keys. It is a value
// type, not package state, so two SDK instances never share configuration.
type Whitelist struct{ /* ... */ }

func NewWhitelist(keys ...string) Whitelist
func (w Whitelist) Len() int
func (w Whitelist) Keys() []string
func (w Whitelist) SpanAttributesFromContext(ctx context.Context) []attribute.KeyValue
func (w Whitelist) LogAttrsFromContext(ctx context.Context) []slog.Attr
func (w Whitelist) NewSpanProcessor() sdktrace.SpanProcessor

const MaxBaggageValueBytes = 256

func ContextWithValue(ctx context.Context, key, value string) (context.Context, error)
```

The change is **additive and non-breaking**. `SetUser`, `UserName`,
`ContextWithUser`, and `WithUserBaggage` keep their signatures and behavior. A
service that adopts nothing new sees no change: with an empty key list, neither
the SpanProcessor nor the log handler is installed, exactly as today.

---

## Consumer guidance

Normative for applications; recorded here because two of these are non-obvious
failure modes that the SDK cannot prevent.

### One key registry per product

Define the keys once, in one application package, and have every service pass
the same list. Two services with different lists produce a dataset where an
identifier is present on some hops and absent on others, which is worse than
absent everywhere.

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
**not the entry span itself**, which is usually the first span an operator
queries.

Applications should therefore do both at the point of identification:

```go
// application helper
func tag(ctx context.Context, key, value string) context.Context {
	if value == "" {
		return ctx
	}
	// (a) the span that is already running never saw this baggage
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(key, value))

	// (b) every span, log record, and downstream service after this point
	next, err := o11y.ContextWithBaggageValue(ctx, key, value)
	if err != nil {
		slog.WarnContext(ctx, "tag baggage failed",
			slog.String("key", key), slog.Any("error", err))
		return ctx // telemetry must never be load-bearing (ADR 0024)
	}
	return next
}
```

The SDK deliberately does not fuse (a) and (b) into one call, preserving ADR
0016's orthogonal split between the span setter (`SetUser`) and the baggage
setter (`ContextWithUser`). See Q4.

### The returned context must be stored back

`ContextWithBaggageValue` returns a child context. A middleware that calls it
without writing the result back into the framework (`c.Request =
c.Request.WithContext(ctx)` for gin, the equivalent setter for a NATS router)
silently does nothing.

### Trust boundaries

- **Ingress**: an external caller can forge `baggage: <your key>=<any value>`.
  Set application keys from data this service has already validated, overwriting
  any inbound member of the same key. `baggage.SetMember` overwrites by key, so
  setting unconditionally is the correct posture at an edge.
- **Egress**: strip baggage (`baggage.ContextWithoutBaggage`) before calling
  external third parties.
- **Metrics**: never promote a materialized key to a metric label.
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
  ignore the identity of the keys. The change is plumbing.
- `ContextWithUser` / `SetUser` / `UserName` and their tests are untouched, so
  ADR 0016's surface stays stable while its constraint is lifted.

**Negative / trade-offs**

- **More things can now be put on the wire.** ADR 0016 bounded PII exposure by
  making the whitelist un-extendable; this ADR trades that for the numeric
  bounds in Decision §5 plus documented guidance. A team can now put something
  sensitive into baggage without an SDK change gating it. This is the real cost
  of the decision and it is accepted knowingly: the alternative — an SDK release
  per application identifier — is not a security control, only friction.
- Header budget becomes a shared, application-managed resource. The 8-key and
  256-byte caps bound the SDK's own contribution but cannot stop a service from
  writing raw baggage members outside the SDK.
- Two enrichment paths now coexist per key (baggage materialization and explicit
  `SetAttributes`), and a reader at a call site cannot tell which produced a
  given attribute. This is ADR 0016's "Phase 2 is magic" trade-off, now applying
  to more keys.
- The entry-span gap (see Consumer guidance) is a genuine sharp edge that the
  SDK cannot close, only document.

---

## Decision analysis

### Q1 — Where does this capability belong?

| | **SDK (chosen)** | Application |
|---|---|---|
| baggage → span | needs a SpanProcessor at provider construction | impossible: no `WithSpanProcessor`, provider returned as interface and wrapped by pyroscope, ADR 0003 forbids the global-state route |
| baggage → log | needs to wrap the handler chain | impossible: chain closed over inside `Init` |
| key naming, value source, trust rules | must not know | the only party that can know |

- **Chosen: mechanism in the SDK, policy in the application.** The two halves
  are not merely separable, they are each *only* implementable on their
  respective side.

### Q2 — Generalize, or add a second hard-coded key?

| | Second hard-coded entry | **Configurable list (chosen)** |
|---|---|---|
| SDK carries product vocabulary | yes | no |
| Next identifier | another SDK release | zero SDK change |
| Review burden | every key is an SDK PR | bounded by numeric caps + guidance |
| PII gating | an SDK PR per key (friction, not control) | documented contract |

- **Chosen: configurable.** Per-key SDK releases make the SDK a bottleneck on
  every consuming product's schema, and the gate they provide is friction rather
  than an actual control.

### Q3 — Should the setter validate the key against the whitelist?

| | Validate in setter | **No validation (chosen)** |
|---|---|---|
| Producer configuration | must mirror every consumer's whitelist | none needed |
| Failure mode | edge silently drops values consumers wanted | unmaterialized member rides one hop |
| Consistency with ADR 0016 | changes `ContextWithUser` behavior | preserves it |

- **Chosen: no validation.** Whitelisting is a materialization-side concern; the
  producer is a different process. The 256-byte value cap is retained because it
  protects the shared header budget, which *is* a producer-side concern.

### Q4 — Should `ContextWithBaggageValue` also write the current span?

| | Fuse both | **Baggage only (chosen)** |
|---|---|---|
| Entry-span gap | closed by the SDK | closed by two lines of application code |
| ADR 0016 symmetry | breaks it (`ContextWithUser` would differ) | preserved |
| Surprise | a "context" function mutates a span | none |
| `user.name` parity | would need `ContextWithUser` changed too | untouched |

- **Chosen: baggage only.** Keeping the setter orthogonal matches ADR 0016's
  existing `SetUser` / `ContextWithUser` split. The cost is a documented sharp
  edge; the alternative is an asymmetric API where one key behaves differently
  from the rest. Revisit if the two-line application helper proves to be a
  recurring source of bugs across adopters.

### Q5 — Why not the sampler escape hatch (Dependency behavior §5)?

It works today and needs no SDK change, but it enriches spans only — no log
records — and log materialization is the sampling-independent half of the
capability. It also mixes enrichment into the sampling decision and occupies the
single `WithTraceSampler` slot. **Recorded as a legitimate interim bridge for an
application that cannot wait for this ADR to ship, and as a non-goal for the
SDK.**

---

## Implementation specifics (settle in the implementing PR)

1. **`internal/baggageattrs`** — introduce `Whitelist` with `NewWhitelist`,
   `Len`, `Keys`, and the three existing functions converted to methods; delete
   the package-level `var baggageWhitelist`. `attributeKeyFor` maps
   `UserNameKey` to `semconv.UserNameKey` and any other key to
   `attribute.Key(key)`. Add `ContextWithValue` and `MaxBaggageValueBytes`;
   reimplement `ContextWithUser` on top of it, keeping its current error
   messages so ADR 0016's tests stay green unchanged.
2. **`internal/log/handler.go`** — replace `hasUserNameAttr bool` with a
   `presetKeys map[string]struct{}` accumulated in `WithAttrs`, and carry the
   `Whitelist` on the handler. `NewBaggageHandler` takes the whitelist as a
   second parameter. Preserve the existing precedence: an attribute supplied via
   `WithAttrs` or already on the record wins over the baggage value.
3. **`options.go`** — add `baggageKeys []string` to `Config`, replacing
   `userBaggage bool`; add `WithBaggageAttributes` and
   `MaxBaggageAttributeKeys`; re-express `WithUserBaggage`. Validate keys with
   `baggage.NewMemberRaw(key, "x")` rather than a hand-written token grammar.
   Drop-with-WARN on invalid, duplicate, and over-cap keys, following
   `WithExtraHTTPServerAttributeKeys` (`options.go:417`).
4. **`o11y.go`** — build the whitelist once and gate all three sites
   (`o11y.go:208`, `:310`, `:316`) on `whitelist.Len() > 0`. Generalize
   `appendUserBaggageWarnings` (`o11y.go:496`) to warn on the trace-disabled
   combination and to log the effective key list at startup — the effective list
   is the first thing an operator needs when an expected attribute is missing.
5. **`attributes.go`** — add `ContextWithBaggageValue`.
6. **Tests** — whitelist construction (dedup, order, empty, over-cap, invalid
   token); per-instance isolation (two whitelists in one test, no cross-talk);
   span and log materialization for a non-semconv key; `user.name` still maps to
   the semconv attribute key; `WithAttrs` precedence for an arbitrary key;
   value-length rejection; empty-list means no processor and no handler
   installed. Existing ADR 0016 tests must pass **unmodified** — that is the
   non-breaking proof.
7. **Docs** — a "Application-defined baggage attributes" section in
   `docs/semconv.md` recording that these keys are *not* SDK-owned, are
   therefore absent from the catalog, and are still bound by the
   never-a-metric-label (ADR 0016 Q3) and never-a-span-name (ADR 0023) rules; a
   `docs/guide.md` section covering the entry-span gap, the context write-back,
   and the trust boundaries; `CHANGELOG.md` under `[Unreleased]`.
8. **Neutral example** — the `app.room_id` illustration in `AGENTS.md:286`
   predates this ADR and models a naming convention an adopting product is
   unlikely to follow, so SDK documentation would be competing with the
   adopter's own key registry over the same identifier. Change it to a
   domain-neutral one, as `docs/guide.md:903` already does with `app.order_id`.
