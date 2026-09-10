# ADR 0028 — NATS and JetStream Consumer Metrics

**Status**: Proposed — recommendations below require acceptance; not implemented
**Date**: 2026-09-07
**Relates to**: ADR 0002 (metrics strategy and cardinality), ADR 0003
(global-state policy), ADR 0004 / 0022 (NATS integration and facade), ADR 0006
(semconv pin), ADR 0008 (instrumentation sourcing), ADR 0024 (telemetry must
not change work lifetimes), ADR 0026 (dependency direction), ADR 0027
(SDK-owned metrics over an existing trace integration)

## Context

The NATS facade wires a TracerProvider and propagator into `otel-nats`, but
does not accept a MeterProvider or record application processing metrics.
The motivating consumer, `hmchangw/newchat`, implements those observations in
`pkg/natsmetrics`: consumer loop state, delivery dispositions, processing
duration, terminal failure evidence, publish failures, and RPC durations.

The reusable problem is larger than counting an Ack. Every integration must
preserve the native message methods, distinguish failed acknowledgment calls
from completed dispositions, observe handlers that return without settling,
and bound all labels before caching measurement options. Centralizing that
contract in the SDK is valuable; moving application scheduling and retry
policy into the SDK is a separate responsibility with a different cost.

### Evidence baseline

This proposal was researched against these remote `main` snapshots:

| Repository | Commit | Relevant dependencies |
|---|---|---|
| o11y | [`e4aef2f1`](https://github.com/flywindy/o11y/tree/e4aef2f1fb611504bd1da260956acb86cc533487) | `otel-nats v0.9.1`, `nats.go v1.50.0`, OTel Go `v1.44.0`, semconv `v1.39.0` |
| newchat | [`28b97da1`](https://github.com/hmchangw/newchat/tree/28b97da1eb1554ecdfe12ea76121f23bce446c8c) | `o11y v0.12.0`, `otel-nats v0.9.1`, `nats.go v1.50.0` |

Implementation must refresh the integration baseline and candidate search.
References to ADRs 0026 and 0027 below identify the verified remote documents.
No runtime or benchmark results are claimed by this proposal.

Relevant source evidence:

- [NATS constructors](https://github.com/flywindy/o11y/blob/e4aef2f1fb611504bd1da260956acb86cc533487/nats/conn.go#L122)
  accept tracing dependencies only.
- [JetStream facade](https://github.com/flywindy/o11y/blob/e4aef2f1fb611504bd1da260956acb86cc533487/nats/jetstream.go#L364)
  already owns consumer, iterator, and batch wrappers, including single-message
  `Next`. The older statement that `Next` cannot be wrapped is obsolete.
- [newchat recorders](https://github.com/hmchangw/newchat/blob/28b97da1eb1554ecdfe12ea76121f23bce446c8c/pkg/natsmetrics/metrics.go#L175)
  declare five custom instruments plus two standard RPC histograms.
- [message-worker](https://github.com/hmchangw/newchat/blob/28b97da1eb1554ecdfe12ea76121f23bce446c8c/message-worker/main.go#L288)
  starts tracking **after** semaphore admission, then calls `Finish` when the
  worker returns. Starting a timer in `Next` would change that measurement.
- [optTable](https://github.com/hmchangw/newchat/blob/28b97da1eb1554ecdfe12ea76121f23bce446c8c/pkg/natsmetrics/opttable.go)
  caches normalized label combinations using copy-on-write maps.

Dependency counts from `go list -deps` describe the build graph, not how many
metrics an integration emits. They must not be used as the proof of a signal
gap or as a substitute for a recording/export test.

### Standards correction

Messaging metrics already exist in pinned semconv v1.39.0. The generated Go
source contains `messaging.client.consumed.messages`,
`messaging.client.sent.messages`, `messaging.client.operation.duration`, and
`messaging.process.duration`. The v1.40.0 specification also defines them,
with **Development** stability [S1]. Development means that compatibility
needs an explicit policy; it does not mean that the names are undefined.

In particular, consumed messages counts delivery to the application, while
newchat's existing counter records a disposition at the end of an attempt.
Those instruments cannot be substituted by renaming. Client operation
duration also explicitly excludes processing duration.

## Decision drivers

1. Observe the existing program without changing acknowledgment, retry,
   cancellation, panic propagation, or shutdown behavior.
2. Make the processing boundary explicit across callback, iterator, and batch
   APIs. Receiving a message and completing its handler are different events.
3. Separate protocol disposition, application-declared failure, and broker
   evidence. None alone proves business success or durable business loss.
4. Keep labels and recorder caches bounded before measurements reach OTel.
5. Preserve SDK provider wiring and automatic view installation without adding
   NATS drivers to the root package's import graph.
6. Give newchat a documented migration, including semantic changes, dashboards,
   disabled-mode behavior, and tests that currently inspect application source.

## Proposed decisions

### 1. Scope: consumer observation, with scheduling owned by the application

Adopt the generic consumer observation layer into `nats/`. Keep spans on the
existing T2 facade; implement the missing metric layer as justified T3.

| Capability | Phase 1 owner |
|---|---|
| Observe a synchronous processing invocation and its message methods | SDK |
| Normalize event types and cache bounded measurement options | SDK |
| Record application-reported loop transitions and terminal failure evidence | SDK recorder; application supplies the facts |
| Choose event vocabulary and declare business failure reasons | Application |
| Semaphore, goroutines, WaitGroup, dispatch, retry, and shutdown sequencing | Application |
| Demonstrate a bounded worker loop | SDK examples, without a worker-runner API |
| Publish-failure and RPC recorders | Remain in newchat for phase 1 |

This is the original "A plus a B example" direction, with A narrowed to its
consumer half. It is not a promise to relocate every function currently in
`pkg/natsmetrics`. In particular, delivery metadata used to make retry
decisions is application behavior and must remain available when metrics are
disabled.

The phase-1 per-message disposition contract supports **AckExplicit**
consumers. AckNone and AckAll need separate semantics: one AckAll call can
acknowledge multiple messages, so one method call does not prove one message
settlement. Unsupported policies must be reported at observer setup, without
changing the native consumer configuration or preventing uninstrumented use.

### 2. Sourcing evaluation under ADR 0008

| Candidate | Evaluation for this metric layer |
|---|---|
| Pinned corporate `otel-nats v0.9.1` | Remains the approved tracing integration. Source inspection found no OTel metric instruments or MeterProvider wiring to facade for these observations; the missing metrics and processing-completion seam fail checklist items 4/5 for this layer. |
| OpenTelemetry Go contrib | The official instrumentation inventory inspected during research lists no NATS/JetStream integration to evaluate [S2]. This is a dated search result, not a claim that no future candidate can exist. |
| NATS server monitoring/exporters | Observe broker state and advisories, not application handler boundaries, swallowed application failures, or local recorder labels. They complement this layer and cannot replace its processing seam. |
| newchat `pkg/natsmetrics` | A source of requirements and characterization cases, not an upstream dependency: it imports application vocabulary and includes retry-related context helpers and worker scheduling. |

No new dependency is proposed. Before implementation, refresh the candidate
search and the existing ADR 0003 upstream audit. Re-evaluate this T3 layer on
ADR 0008's annual cadence or when upstream exposes equivalent metric hooks.

Using a Development messaging convention is an explicit, pinned exception
to ADR 0008's wording about stable attribute sets, consistent with the
existing messaging integration. Acceptance must record this qualification;
it must not claim that messaging metrics have reached Stable status.

### 3. Processing lifetime is explicit

The recommended public abstraction is a per-consumer observer with a
synchronous `Observe` operation. The names below are proposed API, not APIs
already provided by the SDK:

```go
// Consumer retains its existing methods and gains observer construction.
type Consumer interface {
    // Existing methods omitted.
    Observer(ctx context.Context, cfg ConsumerMetricsConfig) (*ConsumerObserver, error)
}

// ConsumerMetricsConfig supplies application-owned, bounded vocabulary.
type ConsumerMetricsConfig struct {
    Site       string
    EventTypes []string
}

func (o *ConsumerObserver) Observe(ctx context.Context, msg jetstream.Msg, eventType string, handle func(context.Context, jetstream.Msg) error) error
func (o *ConsumerObserver) LoopStarted(ctx context.Context)
func (o *ConsumerObserver) LoopStopped(ctx context.Context)
func MarkTerminalFailure(ctx context.Context, reason TerminalReason)
```

`Observer` would be implemented on the existing concrete facade. Its
configuration contains the optional site label and event-type allowlist. Stream, durable
consumer name, and acknowledgment policy come from the bound consumer's
setup metadata, not message subjects or payloads. Any necessary metadata
lookup occurs at setup with the supplied context, never once per delivery.

`Observe` invokes the application callback exactly once on the calling
goroutine. Timing starts immediately before invocation and ends on return
or panic unwinding. The timer excludes earlier queue/semaphore wait and
includes work after an early Ack. It does not wait for broker redelivery or
for goroutines that the callback starts and does not join.

The callback receives a message wrapper and a context carrying observation
state. That context retains the caller's cancellation, deadline, and trace
identity; observation does not detach, cancel, or replace its lifetime.
The returned application error is returned unchanged. The SDK never chooses
Ack, Nak, Term, a retry, or a business result based on that error.

That inertness is the callback signature's one ergonomic trap, and it must be
closed in the API rather than left to the migration. A handler that returns
`error` reads, to every Go programmer, as a handler whose error the caller
acts on — and here the caller records a label and nothing else. Fifteen
services convert to this signature at once; each conversion is one plausible
misreading away from a message that is silently never settled because its
author assumed a returned error would Nak. Name the parameter for what it is
in the doc comment and the examples (the observed error, used only to derive
`error.type`), and state on the type that returning non-nil settles nothing.
An `Observe` callback that returns non-nil without having called a terminal
method is recorded `left_pending` under section 4 — the disposition that
proves the trap was hit, so the guard is a query, not only a convention.

For an existing callback with no error result, an adapter can call it and
return nil. That reports only the errors the adapter exposes; it does not
discover swallowed failures. `MarkTerminalFailure` is the explicit way for
such code to report the narrower fact that it has abandoned work.

All six facade delivery paths use the same observation contract:
`Consume`, `Messages`, `Next`, `Fetch`, `FetchBytes`, and `FetchNoWait`.
Applications invoke `Observe` inside their actual worker. A synchronous
`Consume` callback can invoke it directly. A callback that dispatches work
must put it inside the dispatched worker, after admission. Examples must
show both cases without wrapping the same invocation twice.

Calling a delivery API alone does not automatically measure application
processing. The SDK must not finalize a previous message when the next one
is fetched, a batch channel closes, an iterator stops, or a consume handle
drains. None of those events proves that the previous worker has returned.

An observed callback must finish using the tracked message before returning.
For work that outlives it, the application places `Observe` around that work
instead. Process termination can prevent any completion sample from being
recorded or exported; this is in-process instrumentation, not a durable
exactly-once ledger.

### 4. Disposition and failure semantics

Every message method is forwarded with its original arguments, return value,
and call count. The wrapper observes **Ack, DoubleAck, Nak, NakWithDelay,
Term, and TermWithReason**. `InProgress` remains non-terminal and must not
complete a disposition. Free-text TermWithReason text is never a label.

Failed method calls do not lock in the final disposition. The first
successful terminal method establishes the local disposition; later native
calls still execute and return their native results, without incrementing a
second disposition sample. One completed observation produces one sample:

| Recorded disposition | Evidence at observation completion |
|---|---|
| `ack` | Ack or DoubleAck returned nil |
| `nak` | Nak or NakWithDelay returned nil |
| `term` | Term or TermWithReason returned nil |
| `handler_cancelled` | No successful terminal method, and the processing context is canceled or expired |
| `handler_aborted` | No successful terminal method, and the callback did not return normally, such as panic unwinding |
| `left_pending` | No successful terminal method, callback returned normally, and its context is still live |

Precedence is successful terminal method, abnormal completion, cancellation,
then left-pending. An Ack followed by a handler panic therefore remains an
`ack` disposition but is a failed processing invocation. Abnormal unwinding
must not swallow, replace, or convert the application's panic. Goexit and
other unwinding paths must not be mislabeled as normal return.

These are **local observations**. Ordinary Ack success does not confirm that
the broker received it; DoubleAck waits for confirmation, but a timeout can
still leave receipt uncertain [S3]. The labels do not claim that the broker
still has the message pending or that a business side effect succeeded.

In contrast to newchat's current first-attempt `sync.Once`, an Ack failure
followed by a successful retry before callback completion is recorded as
`ack`. A processing duration also continues until callback completion. Both
are deliberate semantic changes, not metric renames.

The proposed terminal-failure counter accepts these closed, application-
declared reasons: `permanent`, `invalid_payload`, `publish_exhausted`,
`max_deliver`, and `internal`. `max_deliver` means the application has
classified the attempt as unsuccessful and has evidence that its configured
budget is exhausted; it is not synthesized merely from a delivery number.
Unknown reasons normalize to `internal`. The first declaration per observed
invocation wins, and one failure sample is emitted at completion.
Declarations after completion are ignored. Nested observation of the same
active wrapper reuses the outer observation and still invokes the nested
callback, without starting another timer or recording another completion.
Concurrent observations of different deliveries remain supported. Do not
deduplicate separate redeliveries by broker message ID: each is an attempt.

Do not infer a terminal business failure from Term, Ack, handler error, or
MaxDeliver metadata alone. newchat itself Ack-drops permanent errors. A
handler may also deliberately discard an irrelevant message without a
business failure. The application must supply the classification.

`consumer_deleted` and `stream_unavailable` are excluded from this per-work
counter. They describe receive-loop failures with an unknown number of
affected messages. Keep their logs and application recovery handling; add a
separate bounded receive-error instrument only with a named reader and an
explicit event-count contract. Broker MaxDeliver and Term advisories remain
independent evidence [S4]. Never sum these sources as a count of lost work.

### 5. Instruments and names

The recommended phase-1 set is **four consumer instruments**, replacing the
initial proposal's five-family migration. Publisher instrumentation is
deferred as a whole, including its vocabulary.

| Name | OTel instrument / unit | Proposed description and recording boundary |
|---|---|---|
| `o11y.nats.consumer.loops` | Int64UpDownCounter / `{loop}` | Number of application-reported active consumer loops. Record idempotent start/stop transitions per observer; publish an initial zero. |
| `o11y.nats.consumer.dispositions` | Int64Counter / `{message}` | Completed observed delivery attempts by local disposition. Record once at callback completion under section 4. |
| `messaging.process.duration` | Float64Histogram / `s` | Duration of processing operation. Record the complete observed callback invocation, including failures. |
| `o11y.nats.consumer.processing.failures` | Int64Counter / `{failure}` | Observed processing invocations explicitly declared terminally unsuccessful by the application. At most one per invocation. |

Loop state is reported by the application because the application owns the
loop. It means neither broker connectivity nor end-to-end readiness. The
application reports stop when it stops accepting work; existing workers can
finish afterwards. That stop does not classify those workers as canceled.
Multiple observers with the same labels contribute an active-loop count,
not a process-wide Boolean. A disappeared process produces missing/stale
telemetry, not a guaranteed final zero.

The processing histogram uses the standard name because its proposed timing
matches the standard processing operation. Its error class is absent when
the callback returns nil without a terminal-failure declaration; otherwise
it is one of `context.Canceled`, `context.DeadlineExceeded`,
`handler_aborted`, or `_OTHER`. The context sentinels apply to the returned
error, not merely a context that became canceled after successful work.
Arbitrary error strings/types are not label values. A terminal declaration
with a nil callback error uses `_OTHER`.

The histogram carries the event type and error class, **not disposition**.
The disposition counter answers a different breakdown and remains useful;
it is not a second copy of the histogram's count with identical labels.

Use the SDK's shared boundaries by default — `o11y.DefaultLatencyBuckets()`,
currently `[0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]` seconds —
not the messaging convention's fourteen [S1]. Set them as an instrument
advisory and in the scope-specific view. An explicit operator
`WithHistogramBuckets` override takes precedence. The implementation must
distinguish an explicit override from the SDK's existing default slice. Keep
one definition in the driver-free view package.

This is the one place this profile deliberately departs from the convention,
and it is the same call the SDK has already made five times. Every existing
integration view — Elasticsearch, MongoDB, Redis, MinIO, Cassandra — passes
the SDK's `histogramBuckets` rather than its convention's own set, for the
reason recorded at the boundary definition itself: *"Standardizing these
boundaries across the company keeps P99 calculations directly comparable
between services"* (`options.go`). Taking the messaging set here would leave
JetStream processing and HTTP with different bucket layouts, so their
percentiles could not be compared and no single recording rule would fit both.
A latency SLO's bound must therefore be chosen from this set.

Neither set carries the authority the argument might seem to rest on, so the
provenance of both is worth stating rather than implying. The SDK's eleven are
`prometheus.DefBuckets` from `client_golang`, copied verbatim (ADR 0002 §9);
that library's own doc comment calls them a broad starting point and says most
users will need to customise. They are a widely-deployed default, not a
standard. The convention's fourteen are those eleven plus `0.075`, `0.75` and
`7.5`, and they live **only in the specification prose**: `ExplicitBucketBoundaries`
appears nowhere under the pinned `semconv/v1.39.0` — not in `messagingconv`, not
anywhere — so `newProcessDurationOpts` sets description and unit and nothing
else. There is no pinned code to contradict, and ADR 0008's semconv requirement
covers instrument names, attribute keys and types rather than boundaries.

With neither set winning on authority, internal consistency decides, and it
points one way. What is given up is also bounded: the deviation is confined to
boundaries, while the instrument name, the `s` unit, the description,
`messaging.operation.name`/`.type` and the conditional `error.type` all still
conform — which is where interoperability actually lives. A generic messaging
dashboard still finds and groups the series; only `histogram_quantile`'s
interpolation points differ.

The concrete thing the fourteen would buy is three more legal SLO bounds — 75 ms,
750 ms and 7.5 s — since a bound between boundaries cannot be read without
interpolating. No current SLO wants one: SLO-4 and SLO-5 already sit on `0.5`
and `0.25`, and the 300 ms that was once drafted falls between boundaries in
**both** sets, so the fourteen would not have rescued it. Should a future SLO
need one of those three, that is an argument to revisit ADR 0002 §9 for every
family at once — never to give this one family a different layout.

Using this instrument does not claim a complete implementation of the
messaging metric suite. Automatic consumed/sent message counts and client
operation duration are outside this phase. Do not rename dispositions to
`messaging.client.consumed.messages`, or failures-only publishes to
`messaging.client.sent.messages`. No complete delivery-rate or publish-rate
denominator is provided by this proposal.

### 6. Attributes, cardinality, and cache limits

The following mapping is an **SDK-proposed JetStream mapping**, not a claim
that OTel defines a NATS-specific mapping. For these consumer metrics,
the stream is the logical broker destination and the durable consumer is
the subscription. Existing upstream spans retain their current subject-based
destination naming; this proposal does not rename spans or promise a direct
equality join between the metric destination and the span destination.

| Attribute | Applies to | Source and bound |
|---|---|---|
| `messaging.system` | All four | Constant `nats`; a documented custom system value already used upstream |
| `messaging.destination.name` | All four | Stream from consumer setup; admitted identity tuple only |
| `messaging.destination.subscription.name` | All four | Durable consumer from setup; ephemeral identities excluded from this phase |
| `o11y.nats.site` | All four, optional | Immutable deployment/site alias supplied at setup; never extracted from payloads |
| `messaging.operation.name` | Processing duration | Constant `process` |
| `messaging.operation.type` | Processing duration | Constant `process` |
| `error.type` | Processing duration, failures only | Four failure classes defined in section 5 |
| `o11y.nats.event.type` | Dispositions, processing duration, processing failures | Application allowlist plus reserved `unknown` |
| `o11y.nats.disposition` | Dispositions | Six values defined in section 4 |
| `o11y.nats.failure.reason` | Processing failures | Five values defined in section 4 |

Server endpoint labels are intentionally omitted in this processing profile:
the observation context describes application work, potentially after a
reconnect, and the current connection address need not be the delivering
server. This is a documented profile limitation against the convention's
conditional endpoint attributes, not evidence that the SDK cannot discover
its current server. Revisit endpoint attribution with client-operation metrics.

Resource identity stays in the SDK Resource. Do not repeat `service.name`,
`service.version`, or `service.namespace` as inline measurement attributes.
Do not copy baggage, raw subjects, message IDs, user IDs, free-text reasons,
or arbitrary `attribute.KeyValue` values into metric labels.

Required bounds:

1. Copy and deduplicate the event allowlist during setup. Reserve `unknown`;
   an empty list means all event values normalize to it. Reject oversized
   configuration rather than admitting a prefix dependent on traffic order.
2. Normalize **before** cache lookup or attribute construction. No runtime
   event may extend the allowlist. Bounded enums remain SDK-owned for
   dispositions, error classes, and failure reasons.
3. Bound admitted `(site, stream, durable)` tuples per connection and reuse
   their immutable label configuration. Do not intern arbitrary names from
   message metadata into a process-global map. Define configuration lifetime
   and release references when the owning connection/observer is released.
   Repeated setup for the same tuple reuses its frozen vocabulary; incompatible
   vocabulary for that tuple is a setup error, not a growing union of values.
4. Proposed defaults are 16 configured event values per identity and 16
   distinct identities per connection. Including `unknown`, the largest
   instrument then has at most `16 * 17 * 6 = 1,632` label combinations per
   connection. These defaults are acceptance item Q3, not measured optima —
   but they are no longer unanchored. Counted at the evidence baseline,
   newchat declares **14 named event types plus `unknown`** and registers
   **5 consumer identities** across its services, so the identity default has
   ample headroom while the event default leaves two spare slots for a
   service that registers the whole vocabulary. That is the intended shape
   rather than a near-miss: an identity's allowlist is meant to carry the
   event types that consumer actually sees, not the application's full
   taxonomy, and a service approaching the limit is evidence of over-
   registration before it is evidence of a low limit. Raising the default is
   not the first response — recalculate item 6 and the fleet budget first.
5. Multiple connections sharing a MeterProvider and multiple process replicas
   multiply the budget. The per-connection cap does not establish a global
   series bound. Retain the OTel aggregation cap, account for all connections
   in deployment budgets, and treat overflow as a loss of breakdown coverage.
6. With the SDK's 11 finite boundaries (section 5), a classic histogram can
   expose 14 series per attribute set (finite buckets, `+Inf`, sum, count).
   Its five success/error states allow up to `16 * 17 * 5 * 14 = 19,040` such
   series per connection before replicas. Re-derive this number if the
   boundary set changes; it is a function of section 5's decision, not an
   independent constant. The theoretical bound is a review ceiling, not a target;
   applications should register only the vocabulary used by each consumer.

An OTel aggregation cap cannot bound an earlier `optTable`, and an export
filter cannot recover memory already retained there. Overflow preserves
aggregated totals while losing attribute detail; a query filtered on an
outcome can therefore undercount [S5]. Keep cache bounds in the recorder and
pipeline bounds in `internal/metrics`, consistent with ADR 0008 section 3.

Invalid startup configuration returns a descriptive observer-setup error.
Instrumentation setup must not close an already usable NATS connection.
Unknown runtime values use the fallback without blocking message handling.
Instrument-creation errors must be surfaced through setup/diagnostics, not
silently presented as healthy zero traffic. The application decides whether
to continue with observation disabled.

### 7. Providers, toggles, scope, and views

Follow ADR 0027's positional-provider precedent:

```go
func Connect(ctx context.Context, url string, tp trace.TracerProvider, mp metric.MeterProvider, prop propagation.TextMapPropagator, opts ...natsgo.Option) (*Conn, error)
func ConnectWithOptions(ctx context.Context, url string, tp trace.TracerProvider, mp metric.MeterProvider, prop propagation.TextMapPropagator, opts ...ConnectOption) (*Conn, error)
func WithMetricsEnabled(enabled bool) ConnectOption
func MetricViews(histogramBuckets []float64) []sdkmetric.View
```

These signatures are proposed breaking changes. `mp` must be non-nil even
when metrics are disabled, matching the explicit-dependency convention.
`Connect` enables metrics by default; `ConnectWithOptions` accepts the SDK's
resolved `sdk.Toggles.Metrics` through `WithMetricsEnabled`. Passing the
provider does not by itself create a processing boundary: the application
still opts into `Observe` around actual work.

Enablement must be passed explicitly; importing the root package does not
give a leaf integration access to an SDK instance's FeatureToggles. Do not
discover enablement through globals, another environment-variable precedence
chain, or type assertions against a particular no-op provider.

When disabled, keep a valid observer whose loop-state contract still works
but skip metric registration, measurement option caches, timers, and
metrics-only message wrapping. Invoke the handler with the original message
and context. Do not remove application metadata used for retry decisions,
change context lifetime, or change native method behavior. Moving any
retry-related context helper must be tested independently of enablement.

The benefit is avoiding the whole recorder path. Do not state that every
no-op `Add` inherently allocates: pinned OTel no-op method bodies are empty;
actual cost depends on wrapper construction, options, interfaces, and compiler
escape decisions. Disabled/real/no-op provider benchmarks must establish the
implementation's actual allocation and latency cost.

Use meter scope `github.com/flywindy/o11y/nats` with SDK instrumentation
version and the pinned semconv schema URL. Metrics are independent of trace
sampling and remain active with a no-op TracerProvider when metrics are on.
Record against the processing context for exemplars. No new processing span
is required; an existing receive span may already be ended, so do not assert
that its duration equals the handler duration or mutate it after completion.

View definitions live in `internal/views/nats.go`, importing only OTel;
`nats/views.go` re-exports them. Root `Init` composes them into ExtraViews,
with per-instrument allow-keys filters and the exact meter scope. Standalone
MeterProvider users register `nats.MetricViews` and aggregation limits.

Scoping the `messaging.process.duration` view to this integration's meter
scope is load-bearing, not tidiness, and for a reason the SDK has already
written down: `mongo/views.go` scopes its `db.client.operation.duration` view
so it "never matches another integration's `db.client.operation.duration`
instrument (e.g. the Redis wrapper's), which would otherwise produce a
duplicate, conflicting stream when both wrappers are active in the same
process." Borrowing a standard instrument name is what creates that exposure,
and a second messaging integration in one process is the case that would hit
it. The same scoping discipline applies here for the same reason.

This applies Option A locally, as accepted ADR 0027 did. ADR 0026 itself is
still Proposed at the evidence baseline; do not mark its broader refactor
accepted as a side effect. Extend the root forbidden dependency prefixes to
`github.com/nats-io/` and `github.com/akira-core/`. The guarantee is no new
root-to-NATS-driver dependency, not byte-for-byte identical binaries or
independent module version selection.

## Semconv and global-state verification

The `verify-semconv-attributes` skill was applied to pinned semconv v1.39.0
in `go.opentelemetry.io/otel@v1.44.0/semconv/v1.39.0/attribute_group.go`:

| Constant | Verified literal key |
|---|---|
| `MessagingSystemKey` | `messaging.system` |
| `MessagingDestinationNameKey` | `messaging.destination.name` |
| `MessagingDestinationSubscriptionNameKey` | `messaging.destination.subscription.name` |
| `MessagingOperationNameKey` | `messaging.operation.name` |
| `MessagingOperationTypeKey` | `messaging.operation.type` |
| `ErrorTypeKey` | `error.type` |
| `ServiceNameKey`, `ServiceVersionKey`, `ServiceNamespaceKey` | `service.name`, `service.version`, `service.namespace` |

The four messaging instrument names were verified in the adjacent
`messagingconv/metric.go`. `nats` is an upstream-used custom system value,
not a generated well-known NATS constant. All `o11y.nats.*` keys and
instruments above are proposed custom definitions and belong in the catalog's
deviations/custom section. There is no proposed deprecated-key migration;
the application-to-SDK label mapping below is a separate migration.

Implementation must use verified constants for standard keys and add tests
for scope, units, attribute types, and the custom mapping. Constructor and
recording paths must never read or mutate a global MeterProvider. The
existing `checkNoGlobalSetters` gate covers SDK setters; a sentinel-global
test must additionally prove explicit provider routing. No new runtime
global-state verification is claimed before the implementation exists.

## Migration and compatibility

| newchat baseline | Proposed replacement | Semantic change |
|---|---|---|
| `chat.nats.consumer.loop.up` | `o11y.nats.consumer.loops` | Application-reported loop count; not connectivity or handler cancellation |
| `chat.nats.consumer.messages` | `o11y.nats.consumer.dispositions` | Completed invocation; failed settlement calls do not freeze a later successful disposition |
| `chat.nats.consumer.processing.duration` | `messaging.process.duration` | Full callback duration and an error breakdown instead of disposition labels. Boundaries are unchanged: newchat already builds this histogram with `o11y.DefaultLatencyBuckets()`, which section 5 keeps, so existing bucket-bound SLO queries stay valid across the rename |
| `chat.nats.terminal.failures` | `o11y.nats.consumer.processing.failures` | Explicit per-invocation failure declarations only; loop failures are excluded |
| `chat.nats.publish.failures` and the two RPC histograms | Keep in newchat | No phase-1 ownership/name change |

Inline `site`, `stream`, `consumer`, `event_type`, `outcome`, and `reason`
map to the scoped/standard attributes in section 6. The processing histogram
does not retain an outcome label. Operator queries must be reviewed against
these mappings, not transformed by a prefix replacement alone.

Implement SDK behavior first with characterization tests, then migrate AP
call sites in a separate change. Keep scheduling, retry policy, panic guards,
and any required delivery-metadata context bridge in the AP. A newchat handler
that swallows a terminal failure must explicitly preserve that declaration.

### What phase 1 leaves behind in the application

Deferring the publisher and RPC recorders means `pkg/natsmetrics` does not
shrink to nothing, and the residue is worth naming so a reviewer does not read
it as an oversight. After phase 1 newchat still owns its subject conventions
and event/operation vocabulary (`subject.go`, the `Operation` and `EventType`
constants), its consume loop and worker scheduling (`loop.go`), the publish
and RPC recorders, and — because those recorders keep their own bounded label
sets — **a second copy of the copy-on-write measurement-option cache**
alongside the SDK's.

That duplication is an accepted cost of splitting the migration, not a
condition to fix inside phase 1. Consolidating it early would mean either
exporting the cache as SDK API for the application's own instruments, which
commits the SDK to a public shape before the publisher contract exists, or
holding the consumer migration until the publisher vocabulary settles behind
newchat's route-declared method change. The duplication ends when the
publisher work lands and the application's cache has no remaining caller;
until then the two must not be partially merged.

The existing [instrument registry guard](https://github.com/hmchangw/newchat/blob/28b97da1eb1554ecdfe12ea76121f23bce446c8c/pkg/obs/instrument_registry_test.go#L50)
scans application source and will no longer see declarations moved into a
dependency. Add SDK instrument contract tests and an AP collect/scrape test;
keep the source guard for remaining AP-owned instruments. Update the AP
contract with a named dashboard, alert, or investigation for each family.

During rolling deployment, old and new duration/failure families are not
interchangeable. Keep version-specific queries or explicit compatibility
recording rules, validate Prometheus-rendered names/units, and avoid adding
old and new observations from the same invocation. Do not silently remove
old alert coverage before all instances and queries have migrated. No dual
recording is enabled by default.

## Required implementation artifacts and validation

At acceptance/implementation, update:

- `nats/doc.go`: T2 tracing facade plus justified-T3 consumer metrics.
- ADR 0008's NATS row and the qualification for pinned Development messaging
  conventions; amendments to ADRs 0004/0022 for the metric/API surface.
- `docs/semconv.md`: instruments, custom keys, stream/subscription mapping,
  and the endpoint-label limitation. Do not list these as emitted before code lands.
- Driver-free views, root wiring, dependency-prefix gate, provider/toggle tests.
- `docs/guide.md`, `examples/README.md`, JetStream examples, and CHANGELOG:
  API migration and the explicit processing-boundary requirement.

Required behavior tests, with a ManualReader and native-message fakes:

1. Every terminal method forwards arguments/results/call counts; InProgress
   does not settle. Guard the pinned interface method inventory as well as
   interface conformance, because embedding can silently inherit a new method.
2. All facade delivery modes can use the same observer. No timer includes
   semaphore wait; an early Ack does not stop the processing timer.
3. Ack/DoubleAck failure followed by success; repeated terminal calls; Term
   failure; handler error without disposition; cancellation; abnormal unwind;
   Ack followed by panic; concurrent state access under `-race`.
4. Drain stops admission while already admitted handlers finish successfully;
   iterator/batch closure does not finalize those handlers. An abandoned
   unobserved message produces no fabricated completion sample.
5. Terminal declarations deduplicate; classifications are not inferred from
   Ack-drop, Term, delivery count alone, or a loop failure. AckNone/AckAll are
   reported as unsupported at setup without broker mutations.
6. Unknown and adversarial values do not grow caches; duplicate/oversized
   configurations behave as documented; multiple connections can exercise
   aggregation overflow without falsely retaining lost breakdown labels.
7. Metric recording survives unsampled/no-op tracing; enablement controls no
   business behavior. Metadata-dependent retry decisions match with metrics
   on/off. A sentinel global MeterProvider receives no samples.
8. Scope-specific views preserve exact labels, units, explicit overrides, and
   default buckets on both Prometheus and OTLP. Root dependency checks keep
   NATS drivers out. AP scrape/registry migration has explicit coverage.

Before an implementation commit, run repository-required formatting/tidy,
ADR gate, lint/vet, normal tests, and race tests. Benchmark raw handling,
disabled observation, no-op-provider observation, and real recording, with
both warm caches and all permitted combinations. Record measured results
instead of copying newchat's historical timings as SDK performance claims.
This documentation-only proposal requires Markdown/link and diff checks;
it does not claim those implementation gates have passed.

## Deferred work and triggers

- **Publish failures and publisher vocabulary:** migrate together when the
  bounded destination/operation contract and readers are agreed. A failures-
  only counter has no acceptance denominator. Stream depth is not cumulative
  accepted publishes; Core NATS local publish success is not delivery. Keep
  newchat's documented blind spots explicit [S6].
- **RPC metrics:** revisit after newchat's route-declared vocabulary change
  ([PR #475](https://github.com/hmchangw/newchat/pull/475), open at research
  time) settles and response-envelope failure classification is specified.
- **Automatic delivery/client-operation instruments:** add when a named
  reader needs them, with handoff/buffer/abandonment boundaries defined for
  all modes. This is where the standard consumed/sent/operation instruments
  belong; it is not a rename of a completion counter.
- **Receive-loop error metric:** add when an operational reader requires
  bounded failure counts beyond logs and loop-state transitions.
- **Worker runner, automatic retry, DLQ, recovery, ordered/push consumers,
  AckNone/AckAll:** separate proposals driven by concrete consumer needs.
- **Upstream metric adoption:** refresh the ADR 0008 evaluation if a maintained
  library exposes equivalent observations without changing application behavior.

## Consequences

Applications gain one tested observation contract for explicit-ack processing
without transferring worker ownership to the SDK. Standard processing timing
and bounded custom disposition/failure dimensions make the measurements
explainable. Root consumers do not acquire a NATS driver dependency merely
because the SDK installs these views.

The costs are a breaking provider argument, an explicit observation call at
the processing boundary, maintained SDK-owned metric code, and a substantive
AP query migration. Automatic instrumentation cannot cover work whose
lifetime the application never exposes. Crash losses, broker acceptance, and
business completion still require their own evidence. The existing four
consumer families change semantics; this proposal is not a mechanical move
of `pkg/natsmetrics`.

## Open decisions before acceptance

| ID | Decision | Recommended position in this draft |
|---|---|---|
| Q1 | May processing observation require an explicit synchronous wrapper inside the actual worker? | Yes. Keep dispatch/lifetime control in AP code and avoid implicit completion guesses. |
| Q2 | Preserve newchat's first-disposition timing or adopt full processing timing and completion-time disposition? | Adopt the definitions above; treat the migration as a semantic change. If preservation wins, use a custom duration name and revise the table before acceptance. |
| Q3 | Are the consumer-only scope, 16-event/16-identity default limits, stream/subscription mapping, and endpoint-label omission acceptable for the first release? | Start with this bounded profile, validate it against real consumer inventories, and explicitly record the limitations. Do not raise limits without recalculating fleet/histogram cost. |
| Q4 | Is a four-instrument consumer migration acceptable while publisher/RPC stay in newchat? | Yes. Keep their vocabulary and cost decisions together in later work. |

### Q2 in detail: what each answer costs

Q2 is the only one of the four that cannot be answered from this document as
first drafted, because it asks an owner to accept a cost the draft describes
but never totals. It bundles three independent changes, and the bundle — not
any single element — is what makes the migration unmechanical:

| | Preserve newchat's semantics | Adopt the definitions in sections 3-5 |
|---|---|---|
| Timing boundary | Starts after semaphore admission, ends at first disposition | Starts at callback entry, ends at callback return; includes work after an early Ack, excludes queue wait |
| Disposition | First terminal call wins (`sync.Once`); an Ack that failed is frozen as `left_pending` | Completion-time; an Ack failure followed by a successful retry records `ack` |
| Duration instrument | Custom name; the standard `messaging.process.duration` is unusable, because its convention *is* full processing timing | `messaging.process.duration`, standard name and unit |
| Dashboard migration | Names change, shapes do not; panels port by substitution | Every duration and failure panel must be re-reasoned; no prefix rewrite is valid |
| Rolling deploy | Old and new series are comparable | Old and new series are **not** interchangeable and must not be summed; needs version-scoped queries for the duration of the rollout |
| What it buys | A cheaper cutover, once | Timing that matches what the histogram's own convention says it measures, and a disposition that stops reporting a recovered Ack as unsettled |

The asymmetry is that preservation is cheap exactly once and then permanent:
keeping first-disposition timing forecloses the standard instrument name for
the life of the integration, because the convention defines that instrument as
the full processing operation. Adopting costs one reviewed migration of four
families across fifteen services, four documents and one dashboard.

This draft recommends adopting. The recommendation is not free and should not
be accepted as though it were: whoever accepts Q2 is accepting that the
newchat migration PR is a re-reasoning of every consumer query, not a rename.

API names and the exact consumer-binding option layout should be reviewed
with the first implementation, while preserving the contracts in sections
3/7. This draft does not mark any of Q1-Q4 as owner-approved.

## References

- [S1: OTel messaging metrics, pinned version 1.39.0](https://github.com/open-telemetry/semantic-conventions/blob/v1.39.0/docs/messaging/messaging-metrics.md).
  Defines status, timing, and bucket recommendations; the
  [v1.40.0 document](https://github.com/open-telemetry/semantic-conventions/blob/v1.40.0/docs/messaging/messaging-metrics.md)
  also contains the metrics. Exact names and keys were independently checked
  in pinned v1.39.0 Go source above.
- [S2: OpenTelemetry Go contrib instrumentation inventory](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation).
- [S3: NATS confirmed message acknowledgment example](https://natsbyexample.com/examples/jetstream/ack-ack/go/).
- [S4: NATS acknowledgment and redelivery](https://docs.nats.io/learn/jetstream/acknowledgment).
- [S5: OTel Metrics SDK cardinality limits](https://opentelemetry.io/docs/specs/otel/metrics/sdk/#cardinality-limits),
  [cardinality implications](https://opentelemetry.io/blog/2026/cardinality-limits-in-opentelemetry/),
  and [Prometheus instrumentation practices](https://prometheus.io/docs/practices/instrumentation/).
- [S6: newchat publish-failure evidence boundary](https://github.com/hmchangw/newchat/blob/28b97da1eb1554ecdfe12ea76121f23bce446c8c/pkg/natsmetrics/metrics.go#L589).
- [ADR 0026 at the research baseline](https://github.com/flywindy/o11y/blob/e4aef2f1fb611504bd1da260956acb86cc533487/docs/adr/0026-sdk-integration-dependency-direction.md)
  and [ADR 0027 at the research baseline](https://github.com/flywindy/o11y/blob/e4aef2f1fb611504bd1da260956acb86cc533487/docs/adr/0027-elasticsearch-metrics.md).
