# ADR 0021 — MongoDB Instrumentation Mechanism: replace the Marz wrapper with the contrib CommandMonitor

**Status**: Proposed
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
2. **Drop the Marz wrapper.** `mongo.Connect` returns a plain driver
   `*mongo.Client` (or a thin struct embedding it that adds nothing to the call
   surface), so application code uses **plain `go.mongodb.org/mongo-driver/v2`
   types end-to-end** — no wrapper types threaded through signatures, no
   wrapped result types, and no "unwrap loses spans" foot-gun.
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

---

## Decision driver (root cause)

ADR 0005's choice of the Marz wrapper over a command monitor rested on a single
premise: that we wanted document-level trace propagation — the one capability a
`CommandMonitor` structurally cannot provide. We have since decided that
propagating trace context **into business documents is an anti-pattern we will
not adopt** (it mutates the system-of-record; the forced `$set._oteltrace` on
every update corrupts update semantics; and trace and document lifetimes
diverge), and that asynchronous trace context belongs on an **outbox / message
envelope**, not the business entity.

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
record / transport headers. None is the business document. Marz's
`_oteltrace`-in-document mechanism is therefore a deviation from established
practice, which is why we **decline** it rather than merely disable it.

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
- **Span shape changes** (e.g. `network.peer.*` attributes, formatter-based span
  names, per-wire-command granularity). ADR 0014 §Q1 already classified this as
  a "trace rewrite / high" blast-radius change: existing Tempo queries,
  dashboards, and alerts that depend on Marz span names/attributes must be
  migrated. This is the principal cost of the reversal.
- **Command-span gating changes.** Marz gated spans behind
  `OTEL_INSTRUMENTATION_GO_TRACING_ENABLED` + `OTEL_MONGO_TRACING_ENABLED`
  (ADR 0005); the contrib monitor reads no such env vars (ADR 0014). If gated
  command spans are still wanted, the gate must be reproduced in this package
  (e.g. swap in a noop tracer when disabled — the same mechanism ADR 0014
  already uses), otherwise spans become always-on.
- **Parent-span linkage** now relies on the v2 driver propagating the operation
  `context.Context` into the monitor callbacks (the contrib monitor opens the
  span from that ctx). This is the expected v2 behavior but should be verified
  with a real end-to-end trace during implementation.
- **v1 callers still must migrate to v2** (unchanged from ADR 0005 §1); a v1-only
  service may use the v1 contrib `otelmongo` monitor as an interim, outside this
  package.

---

## Follow-up (not part of this Proposed ADR)

Tracked here for the implementing PR once this ADR is Accepted; **no code or
other ADR is modified by this document**:

- `mongo/client.go`: replace the Marz wrapper with the contrib monitor emitting
  spans + metrics; return a plain `*mongo.Client`; drop
  `WithDocumentTracePropagation`.
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
