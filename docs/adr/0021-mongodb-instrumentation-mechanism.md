# ADR 0021 — MongoDB Instrumentation Mechanism: replace the Marz wrapper with the contrib CommandMonitor

**Status**: Accepted — implemented; ADR 0014 Phase 2 pool lifecycle implemented
as a follow-up.
**Date**: 2026-06-05
**Supersedes parts of**: ADR 0005 (§2 instrumentation mechanism, §4 document
trace propagation)
**Resolves**: the deferred "Decision point for the reviewer" / Q1 Option B in
ADR 0014 (full span migration, conditioned there on document propagation being
confirmed unused)
**Relates to**: ADR 0003 (global-state policy / Approved-integrations table),
ADR 0008 (instrumentation sourcing policy)

---

## Context

ADR 0005 §2 chose to adopt the upstream wrapper
`github.com/Marz32onE/instrumentation-go/otel-mongo/v2` **instead of writing a
native `event.CommandMonitor`**. The load-bearing premise of that choice was a
single capability: **document-level trace propagation** — injecting an
`_oteltrace` field into stored documents so asynchronous readers (change
streams, outbox/relay, delayed jobs) can restore trace context. This is the one
thing a `CommandMonitor` *structurally cannot* provide, because injecting into a
document must happen in the application layer, not at the wire-command layer.
ADR 0005 §4 made that feature opt-in and off by default.

ADR 0014 then added MongoDB metrics by attaching the **official OTel-contrib
`otelmongo` CommandMonitor in metrics-only mode** (real `MeterProvider`, noop
`TracerProvider`) alongside the Marz spans. In doing so it explicitly **deferred**
the cleaner single-lib design:

> "The clean-architecture alternative is to **migrate spans to the contrib
> `otelmongo` too** (single maintained lib, drop the noop-tracer trick) — but
> that **loses `_oteltrace` document propagation** (ADR 0005 §4) and changes
> span shape. **Recommended only if document propagation is confirmed unused.**"
> — ADR 0014, "Decision point for the reviewer"; see also ADR 0014 §Q1.

The new input that this ADR records: **we have decided not to use document-level
trace propagation into business documents at all** (rationale and external
references below), and that asynchronous trace context belongs on an
outbox/message envelope rather than the business entity. That decision supplies
exactly the condition ADR 0014 deferred on — "document propagation confirmed
unused" — and therefore unblocks ADR 0014 Option B.

---

## Decision

1. **Adopt the official contrib `otelmongo` CommandMonitor as the sole MongoDB
   instrumentation mechanism**, emitting both command spans and
   `db.client.operation.duration` from one maintained library, wired via
   `clientOptions.SetMonitor(...)` with a real `TracerProvider` and
   `MeterProvider`. This is ADR 0014 §Q1 **Option B**, now unblocked.
2. **Drop the Marz wrapper.** `mongo.Connect` returns a **plain driver
   `*mongo.Client`** (not a wrapper type), so application code uses **plain
   `go.mongodb.org/mongo-driver/v2` types end-to-end** — no wrapper types
   threaded through signatures, no wrapped result types, and no "unwrap loses
   spans" foot-gun. Any teardown that instrumentation needs (the Phase 2 pool
   event tracker) is handled by a returned cleanup func, **not** by reintroducing a
   wrapper type to host a `Disconnect` override — see point 6 and "Pool-metric
   lifecycle".
3. **Withdraw document trace propagation from this package** (supersedes
   ADR 0005 §4). `_oteltrace` injection, the `WithDocumentTracePropagation`
   option, and the synthetic delivery tracer are removed.
4. **Driver remains v2-only** (ADR 0005 §1 unchanged). Callers still on driver
   v1 must migrate to v2 to adopt this package; a v1-only service that cannot
   migrate yet can use the v1 contrib `otelmongo` `CommandMonitor` as an interim
   measure outside this package (see Consequences).
5. **Asynchronous trace context** is directed to the outbox / message-envelope
   approach (see "Asynchronous tracing direction"); the full design is out of
   scope for this ADR and, if pursued, belongs in a separate ADR.
6. **Expose an instrumentation entry point for application-built clients.**
   Beyond a URI-only `Connect`, the package MUST let an application that builds
   its own `*options.ClientOptions` (for `SetAuth`, pool sizing, read/write
   concerns, …) attach o11y instrumentation **without surrendering client
   construction** — via an `Instrument(opts, tp, mp, prop) (func(context.Context) error, error)`
   decorator and/or a `NewMonitor(tp, mp) *event.CommandMonitor` builder. This is
   required because a URI-only `Connect` cannot express credentials passed through
   `options.Credential` (the common case), and it keeps integration a one-line
   change for services that already centralize client construction. Two
   semantics are normative:
   - **Monitor composition, not overwrite.** `*options.ClientOptions` holds
     exactly one `CommandMonitor` slot and `SetMonitor` replaces it. `Instrument`
     MUST detect an existing monitor and **fan out** (compose into one monitor
     that dispatches `Started`/`Succeeded`/`Failed` to both) rather than clobber
     it. The docs MUST also warn that calling `SetMonitor` *after* `Instrument`
     reverses the problem and drops o11y's monitor.
   - **Returned cleanup func (Option A lifecycle).** `Instrument` returns a
     cleanup `func` the caller invokes at shutdown. Phase 1 returned a no-op
     because the `CommandMonitor` is a passive struct with nothing to
     unregister; after ADR 0014 Phase 2 it disables SDK-owned pool event
     handling. See "Pool-metric lifecycle".

   See "Adoption surface" below.
7. **Command spans are always-on, governed solely by the sampler.** The two
   Marz env gates — `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` and
   `OTEL_MONGO_TRACING_ENABLED` — are **dropped**, not reproduced. Command spans
   are emitted unconditionally and their volume is controlled by the
   `TracerProvider`'s sampler (ADR 0015), exactly like every other span in the
   SDK. Rationale: gating telemetry behind bespoke env vars is an upstream
   Marz-ism, inconsistent with the always-on metric (ADR 0014) and with how o11y
   centralizes sampling/export in `Init`. This is a behaviour change for any
   caller that relied on the gate to suppress spans; acceptable pre-1.0 and
   called out in the CHANGELOG.

---

## Decision driver (root cause)

ADR 0005's choice of the Marz wrapper over a command monitor rested on a single
premise: that we wanted document-level trace propagation — the one capability a
`CommandMonitor` structurally cannot provide. We have since decided that the
**automatic, hidden mutation of business entities by the SDK** is an
anti-pattern we will not adopt (it mutates the system-of-record; the forced
`$set._oteltrace` on every update corrupts update semantics; and trace and
document lifetimes diverge), and that asynchronous trace context belongs on an
**outbox / message envelope**, not the business entity. (To be precise about
scope: the rejected thing is the SDK silently stamping arbitrary business
documents — *not* persisting trace context in general. A deliberately modelled
outbox/event record may itself be a MongoDB document or sub-document and is
explicitly **not** what we reject; see "Asynchronous tracing direction".)

With that premise removed, the wrapper provides **no capability the official
contrib `otelmongo` CommandMonitor does not** — so we are paying the wrapper's
price (wrapper-type coupling across call sites, an "unwrap loses spans"
foot-gun, command-span coverage limited to the CRUD methods Marz overrides, and
a per-version global-state/semconv/propagation audit mandated by ADR 0005
itself) for a feature we have decided never to use.

**The reversal is triggered by the collapse of that premise, not by the
wrapper's ergonomic cost.** ADR 0005 was a reasonable decision under its own
premise; this ADR records that the premise no longer holds.

### Why the alternative is also positively better (supporting, not load-bearing)

- **Coverage.** The CommandMonitor instruments *every* wire command — including
  non-CRUD operations and any method Marz never wrapped — whereas Marz spans
  exist only for the CRUD methods it explicitly overrode.
- **Ergonomics / types.** Plain driver types throughout; nothing to unwrap; the
  v2 driver migration the package already requires gets *lighter*, not heavier
  (no wrapper tax layered on top).
- **Maintenance / supply chain.** The contrib monitor is OTel-maintained and is
  **already a dependency** (ADR 0014), and was verified global-state SAFE for
  ADR 0003. Marz requires a fresh global-state / semconv / propagation /
  delivery-tracer audit on every version bump (ADR 0005 §2).

### What this ADR explicitly does **not** claim

- **Not** that Marz is buggy. Its design is internally coherent and correct for
  its intended feature; method-wrapping is in fact the *only* way to inject into
  documents. We decline the feature, not the implementation.
- **Not** a metrics change. `db.client.operation.duration` already comes from
  the contrib monitor (ADR 0014); this ADR only adds command spans to the same
  monitor (by giving it a real `TracerProvider` instead of the noop one) and
  removes the now-redundant Marz span source.

### Correction to a framing in ADR 0005

ADR 0005 §2 framed the rejected alternative as writing "a native
`event.CommandMonitor`" — i.e. self-authored and self-maintained. The mechanism
this ADR adopts is the **official OTel-contrib** monitor: neither self-authored
nor Marz, and maintained upstream. The "don't write/maintain our own monitor"
concern in ADR 0005 is therefore partly moot — a maintained monitor is available
off the shelf, which ADR 0014 already relies on for metrics.

---

## Acceptance criteria — audit before this ADR is Accepted

This reversal hinges on the premise that document propagation is unused, so that
premise must be verified, not assumed. The following MUST hold before flipping
this ADR to Accepted:

1. **No consumer reads/writes `_oteltrace`.** Grep every consumer service (and
   this repo) for `_oteltrace` / `TraceMetadataKey` / `WithDocumentTracePropagation`
   — no production dependency. (In-tree, only `examples/mongodb/main.go` and
   `mongo/client_test.go` reference it; both are ours and are removed by the
   implementing PR. As one downstream data point, `github.com/hmchangw/chat`
   uses no change streams and no `_oteltrace`.)
2. **No consumer depends on Marz wrapper types** (`marzmongo.*` / the wrapped
   `Database`/`Collection`/result types).
3. **No consumer relies on the Marz env-gate semantics** being the thing that
   turns command spans on/off (relevant to Decision point 7).
4. **Named owner sign-off** for removing the public `WithDocumentTracePropagation`
   option and changing span shape (breaking, pre-1.0).

If any of (1)–(3) fails for a real consumer, that consumer's async needs are
re-evaluated against the outbox/envelope direction before removal, or the
removal is staged behind a deprecation.

**Audit outcome (2026-06-06): satisfied.** The owner confirmed there are no
other consumers — nothing outside this repo depends on `_oteltrace`, the Marz
wrapper types, or the env-gate semantics (criteria 1–3). With owner sign-off
(criterion 4), the gate is met and the ADR is Accepted. The only in-tree
references (`examples/mongodb/main.go`, `mongo/client_test.go`) are ours and are
removed by the implementing PR.

---

## Why document propagation into business documents is an anti-pattern

This is **not** an internal stylistic preference; it diverges from established
async-tracing practice across every normative and reference source.

- **OpenTelemetry messaging conventions** make span **links** — not parent-child
  — the default producer/consumer correlation: *"These conventions use spans
  links as the default mechanism to correlate producers and consumer(s)
  because: It is the only consistent trace structure that can be guaranteed,
  given the many different messaging systems models available. It is the only
  option to correlate producer and consumer(s) in batch scenarios as a span can
  only have a single parent."* They require the carrier to be a *message
  creation context* and that *"A producer SHOULD attach a message creation
  context to each message. If possible, the message creation context SHOULD be
  attached in such a way that it cannot be changed by intermediaries."* A
  business document fails that test: it is mutable by arbitrary writers, and
  Marz's forced `$set._oteltrace` is itself an intermediary mutation. [OTel]
- **CloudEvents** places `traceparent` / `tracestate` on the **event envelope**
  and defines them as *"historical data of the parent trace, in order to
  diagnose eventual failures of the system"*, adding that the extension *"is not
  intended to replace the protocol specific headers for tracing, like the ones
  described in W3C Trace Context for HTTP."* The sanctioned carrier is the
  envelope, used for links/diagnosis — not the business payload. [CloudEvents]
- For the specific DB→broker gap, the canonical **Transactional Outbox** pattern
  and its reference implementation **Debezium** both carry trace metadata in a
  *dedicated outbox-record column* and the *message headers*, and resume the
  trace from there — never in the business entity. [Richardson] [Debezium]

Across every source, the approved carrier is the message envelope / outbox event
record / transport headers. None is the business entity. Marz's
`_oteltrace`-in-document mechanism is therefore a deviation from established
practice, which is why we **decline** it rather than merely disable it.

**What is in scope.** The objection is specifically the SDK *automatically and
invisibly mutating the business entity*. It is not a ban on persisting trace
context: an outbox/event record is itself a stored document (often in the same
MongoDB), and putting `traceparent` there is correct — because that record is a
deliberately modelled envelope, not the business entity, and the application,
not a hidden SDK hook, decides to write it.

**Honest limit of these citations.** No specification literally says "do not
write `traceparent` into a business MongoDB document"; that is the contrapositive
of the positive guidance above. The references establish the *recommended
carrier*; the anti-pattern conclusion follows because the business document is
none of those carriers and additionally violates the "not changeable by
intermediaries" property OTel calls for.

---

## Asynchronous tracing direction (pointer, not a design)

When asynchronous MongoDB-sourced eventing needs trace continuity, the approved
direction is the **Transactional Outbox**: within the same transaction as the
business write, append an event to a dedicated `outbox` collection whose record
**is** the message envelope and legitimately carries `traceparent`; a separate
relay (or CDC such as Debezium's outbox event router) publishes it to the broker
copying `traceparent` into the **message headers**; the consumer extracts from
the headers and **links** its span to the producer (new trace per consumed
message). Marz's *consumer-side* model — detach parent, new TraceID, link to
origin, model the broker as a deliver span — is a reasonable pattern to borrow;
only its **carrier** (the business document) is wrong. A full design is out of
scope here and, if pursued, warrants its own ADR.

---

## Adoption surface — instrumenting an application-built client

The mechanism must support the common downstream shape: a service that already
centralizes MongoDB client construction in one helper and types all of its
repositories against the plain `go.mongodb.org/mongo-driver/v2` types.

A representative public example is **`github.com/hmchangw/chat`** (a Go
microservice monorepo, already on driver v2 v2.5.0, **no change streams**):
every service obtains its client from a single
`pkg/mongoutil.Connect(...)` that builds `*options.ClientOptions` (including
`SetAuth`) and returns a plain `*mongo.Client`; all repositories
(`store_mongo.go`, `history-service/internal/mongorepo/*`, …) are typed on
`*mongo.Database` / `*mongo.Collection`.

Under this ADR such a project integrates with **no repository or business-code
changes**:

- The single client-builder calls `o11ymongo.Instrument(opts, tp, mp, prop)`
  before the MongoDB driver's `mongo.Connect(opts)` — one line; auth and pool
  options untouched.
- The returned `*mongo.Client` is unchanged in type, so every repository keeps
  compiling as-is. This is exactly the property the wrapper approach could
  **not** provide: Marz's `Database()` returns a wrapped type that does not
  match a `*mongo.Database` parameter, which would force either signature
  churn across every repo or an unwrap that loses spans.
- `o11ymongo.MetricViews()` is composed into the MeterProvider once at startup.

Net effect for `chat`: edits confined to `pkg/mongoutil` (one line) and
provider sourcing at each `main.go`; the ~dozen repositories and all query code
are untouched. Document propagation is irrelevant here (no change streams), so
nothing is lost by dropping it.

The only non-MongoDB friction is provider sourcing: such projects often rely on
OpenTelemetry **globals** (e.g. `chat`'s `pkg/otelutil` calls
`otel.SetTracerProvider`), whereas o11y supplies **explicit** providers
(ADR 0003). Bridging that — passing `obs.TracerProvider()` /
`obs.MeterProvider()` / `obs.Propagator`, or setting the globals once at
bootstrap — is a cross-cutting integration step, not a MongoDB-specific one.

This case is the motivating reason Decision point 6 requires an
instrument/monitor entry point rather than a URI-only `Connect`.

## Span-shape migration (Marz → contrib)

Span shape changes; this is the principal operational cost. Dashboards, Tempo
queries, and alerts keyed on the Marz shapes must be migrated. Verified against
the pinned contrib version (`config.go` / `mongo.go`) and Marz `semconv.go`;
re-verify if the contrib pin is bumped.

| Aspect | Marz (current) | Contrib `otelmongo` (0021) |
|---|---|---|
| Span name | `"<op> <collection>"` (space, *logical* op): `insert messages`, `find messages`, `aggregate messages` (Watch→`aggregate`) | upstream default is `"<collection>.<command>"` (dot, *wire command*), but the o11y facade overrides it via `WithSpanNameFormatter` to `"mongodb.<command> <collection>"` per the cross-package convention (ADR 0023): `mongodb.insert messages`, `mongodb.find messages`, `mongodb.getMore`; `mongodb.<command>` if no collection |
| Operation vocabulary | logical: insert/find/update/delete/aggregate/distinct/bulkWrite | wire commands, incl. `getMore`/`createIndexes`/`listIndexes`/`ping`/… |
| Granularity | one span per application call | one span per wire command (Find+getMore → multiple; bulkWrite / transactions split) |
| Span kind | Client (+ optional Consumer "deliver" spans) | Client only |
| Address attrs | `server.address` / `server.port` | `network.peer.address` / `network.peer.port` |
| Other attrs | `db.system.name`, `db.namespace`, `db.collection.name`, `db.operation.name` | same + `network.transport=tcp` |
| `db.query.text` | not emitted | **not emitted by default** (`CommandAttributeDisabled` defaults `true` in the pin); opt-in via `WithCommandAttributeDisabled(false)` |
| Error | `RecordError` + `SetStatus(Error)` | `SetStatus(Error, msg)` on `Failed`; `error.type` is on the **metric**, not the span |
| Env gate | `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` + `OTEL_MONGO_TRACING_ENABLED` | none — sampler-governed (Decision point 7) |

Migration checklist for consumers: update span-name queries
(`insert messages` → `messages.insert`), swap `server.*` → `network.peer.*` in
attribute filters, and expect additional `getMore` spans on cursor-heavy reads.

---

## Pool-metric lifecycle (ADR 0014 Phase 2)

ADR 0014 Phase 2 originally planned to unregister pool metric handling via a `Disconnect`
override on the **wrapper** `*mongo.Client`. Returning a plain `*mongo.Client`
removes that host, so this ADR fixes the lifecycle as **Option A**:

- `Instrument(...)` attaches the `CommandMonitor` (Phase 1) and, in Phase 2, the
  SDK-owned `PoolMonitor` that emits synchronous pool-metric deltas through the
  provided `MeterProvider`. It returns a **cleanup func** that disables
  SDK-owned pool event handling after the application's final metrics flush.
- `ConnectionPoolClosed` zeroes and removes the closed pool state but does not
  disable the tracker. MongoDB can close and recreate pools during topology
  changes, and the same instrumented client must keep reporting the recreated
  pool.
- **Rejected — tie to `MeterProvider` shutdown:** zero app code, but it couples
  each client's pool state to the provider's lifetime and **leaks callbacks** for
  short-lived/repeated clients (tests, reconnect loops) until `obs.Shutdown()`.
  May be offered later as an opt-in convenience for single-long-lived-client
  apps, but not the default.
- **Rejected — thin wrapper struct with `Disconnect()`:** reintroduces a
  non-plain return type, undoing the core ergonomic win of Decision point 2
  (`func(*mongo.Client)` parameters would not accept it).
- `ConnectionPoolCleared` does not immediately zero counts or sizing. The driver
  closes affected connections asynchronously; checked-out work stays visible
  until the corresponding `ConnectionClosed` events reconcile the counters.

---

## Alternatives considered

- **Keep Marz + add `type Database = …` aliases in `o11y/mongo`** (so callers
  name o11y types instead of importing upstream). Rejected: aliases remove only
  the upstream-import leak, not the wrapper tax — call sites must still thread
  wrapper types (or unwrap and lose spans), result types stay wrapped, and we
  keep auditing a dependency for a feature we do not use.
- **Write our own `event.CommandMonitor`.** Rejected: reinvents the contrib
  monitor, which is maintained upstream and already a dependency; contradicts
  ADR 0008's "prefer a maintained library" default.
- **Status quo (Marz spans + contrib metric, ADR 0014 Option A).** Rejected for
  this package's needs now that document propagation is declined: it retains the
  wrapper tax and the Marz env-gate foot-gun for no remaining benefit.

---

## Consequences

**Positive**

- One maintained instrumentation library for both signals; the noop-tracer trick
  from ADR 0014 Phase 1 disappears.
- Application code uses plain v2 driver types — the ergonomics issue that
  motivated this investigation is removed at the source.
- Broader command-span coverage (all wire commands, not only wrapped CRUD).
- Fewer dependencies: the Marz module leaves `go.mod` and its ADR 0003 row is
  removed.

**Negative / trade-offs**

- **We give up the ability to enable document propagation through this package.**
  Accepted: we have decided against it; async needs go through the outbox
  envelope instead.
- **Span shape changes** (span names, `server.*` → `network.peer.*`,
  per-wire-command granularity). ADR 0014 §Q1 classified this as a
  "trace rewrite / high" blast-radius change: existing Tempo queries, dashboards,
  and alerts that depend on Marz span names/attributes must be migrated. This is
  the principal cost of the reversal — see "Span-shape migration" for the full
  matrix and consumer checklist.
- **Command spans become always-on** (Decision point 7): the two Marz env gates
  are dropped, so a process that previously left them unset and got no command
  spans will now emit them (subject to the sampler). Documented in the CHANGELOG.
- **Parent-span linkage** now relies on the v2 driver propagating the operation
  `context.Context` into the monitor callbacks (the contrib monitor opens the
  span from that ctx). This is the expected v2 behavior but should be verified
  with a real end-to-end trace during implementation.
- **v1 callers still must migrate to v2** (unchanged from ADR 0005 §1); a v1-only
  service may use the v1 contrib `otelmongo` monitor as an interim, outside this
  package.

---

## Implementation follow-up

Tracked here for the implementing PRs after this ADR was accepted:

- `mongo/client.go`: replace the Marz wrapper with the contrib monitor emitting
  spans + metrics; return a plain `*mongo.Client`; drop
  `WithDocumentTracePropagation`. Add `Instrument(opts, tp, mp, prop) (func(context.Context) error, error)`
  (composing/fanning-out with any existing `CommandMonitor`, not overwriting it;
  returning a cleanup func) and a
  `NewMonitor(tp, mp) *event.CommandMonitor` builder (Decision point 6).
- Give the contrib monitor a real `TracerProvider` (no noop) and **do not**
  reproduce the `OTEL_*_ENABLED` gates (Decision point 7); confirm `error.type`
  lands on the metric and parent-span linkage works end-to-end on driver v2.
- ADR 0014: cross-update the Phase 2 lifecycle from a wrapper `Disconnect`
  override to the cleanup func returned by `Instrument` (Pool-metric lifecycle).
- Tests / `examples/mongodb` / README: drop `_oteltrace` usage
  (`examples/mongodb/main.go`, `mongo/client_test.go`) and simplify call sites.
- `mongo/doc.go`: update the Tier annotation (single maintained-lib T2).
- ADR 0003: remove the Marz `otel-mongo/v2` Approved-integrations row.
- ADR 0005 / ADR 0014: add a one-line "Superseded in part by ADR 0021" pointer
  at the top (only after this ADR is Accepted).
- CHANGELOG: `[Unreleased]` entry for the mechanism change and the removal of
  `WithDocumentTracePropagation` (breaking; span shape change).

---

## References

- [OpenTelemetry — Semantic conventions for messaging spans][OTel] (span links
  as the default correlation; message creation context attached to the message,
  ideally not changeable by intermediaries)
- [CloudEvents — Distributed Tracing Extension v1.0.2][CloudEvents]
  (`traceparent`/`tracestate` on the event envelope as historical/creation-time
  data; not intended to replace transport headers)
- [W3C — Trace Context][W3C] (`traceparent` / `tracestate` format)
- [Chris Richardson — Pattern: Transactional outbox][Richardson] (canonical
  definition)
- [Debezium — Distributed Tracing][Debezium] (trace metadata in a dedicated
  outbox column, resumed by the Event Router SMT, injected into message headers
  for the consumer)
- [Coding Militia — Transactional outbox pattern meets distributed tracing and
  OpenTelemetry][Coding Militia] (practitioner walkthrough of storing serialized
  trace context in the outbox record)

[OTel]: https://opentelemetry.io/docs/specs/semconv/messaging/messaging-spans/
[CloudEvents]: https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/extensions/distributed-tracing.md
[W3C]: https://www.w3.org/TR/trace-context/
[Richardson]: https://microservices.io/patterns/data/transactional-outbox.html
[Debezium]: https://debezium.io/documentation/reference/stable/integrations/tracing.html
[Coding Militia]: https://blog.codingmilitia.com/2024/06/17/transactional-outbox-pattern-meets-distributed-tracing-and-opentelemetry/
