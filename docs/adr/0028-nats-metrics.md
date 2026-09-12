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
changing the native consumer configuration or preventing uninstrumented use —
except with metrics disabled, where that check does not run (Contract:
Disabled, section 7).

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
// ConsumerObserver binds an observer to an existing consumer. It hangs off
// Conn — a concrete struct — NOT off the Consumer interface; see below.
func (c *Conn) ConsumerObserver(ctx context.Context, consumer Consumer, cfg ConsumerMetricsConfig) (*ConsumerObserver, error)

// ConsumerMetricsConfig supplies application-owned, bounded vocabulary.
type ConsumerMetricsConfig struct {
    Site       string
    EventTypes []string
    // OnConsumeError receives what a jetstream.ConsumeErrHandler would, plus
    // a loop-lifetime context; see the Consume wiring in section 5 for why it
    // is a field rather than an option, and why it carries its own context.
    OnConsumeError func(context.Context, jetstream.ConsumeContext, error)
}

func (o *ConsumerObserver) Observe(ctx context.Context, msg jetstream.Msg, eventType string, handle func(context.Context, jetstream.Msg) error) error
func (o *ConsumerObserver) Consume(ctx context.Context, handler JetStreamMsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error)
func (o *ConsumerObserver) StartLoop(ctx context.Context) *Loop
func (l *Loop) End(ctx context.Context, err error)
func (o *ConsumerObserver) Close(ctx context.Context) error
func MarkTerminalFailure(ctx context.Context, reason TerminalReason)
```

Its configuration contains the optional site label and event-type allowlist.
Stream, durable consumer name, and acknowledgment policy come from the bound
consumer's setup metadata, not message subjects or payloads: `Consumer`
already exposes `Info(ctx)` and `CachedInfo()`, and `jetstream.ConsumerInfo`
carries all three. Any necessary metadata lookup occurs at setup with the
supplied context, never once per delivery.

**Why it hangs off `Conn` and not off `Consumer`.** Two constraints meet here
and only one shape satisfies both.

`Consumer` is an exported interface, so adding a method to it is a breaking
change for every external implementor — and a silent one for the implementors
most likely to exist. An implementor that *embeds* the interface keeps
compiling and gains a nil method that panics when the new one is called;
newchat already has exactly that shape today
(`search-sync-worker/consumer_source_test.go`, a `fakeO11yConsumer` embedding
a nil `o11ynats.Consumer` and overriding only `Fetch`). This is the same
hazard the test requirements below already guard for on `jetstream.Msg` —
"embedding can silently inherit a new method" — and it applies to this ADR's
own change.

But a plain free function taking only the interface cannot work either,
because construction needs state the interface does not carry: the
MeterProvider, the resolved metrics toggle, and the per-connection identity
registry section 6 requires. Reaching them by asserting the argument to the
concrete facade type would fail for exactly the external and decorator
implementors the free function was meant to protect, and a package-global
registry is barred by ADR 0003.

A method on `Conn` satisfies both. `Conn` is a concrete struct, so a new
method on it breaks no one; it already holds the providers and the toggle; and
it is the natural owner of a registry whose bound section 6 states *per
connection*, which until now had no owner at all. The consumer arrives as an
ordinary interface parameter, so any implementation — SDK, decorator or fake —
can be observed.

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

That inertness is the signature's one ergonomic trap: a handler returning
`error` reads as one whose error the caller acts on, and here the caller only
derives a label from it. The doc comment and the worked example must name it
for what it is, and the type must state that returning non-nil settles
nothing — the example settles explicitly before returning for exactly this
reason. Such a callback records `left_pending` under section 4's precedence,
which is an **investigation signal, not proof of a defect**: a handler
deliberately leaving a message to AckWait redelivery is a legitimate pattern
landing in the same state.

For an existing callback with no error result, an adapter can call it and
return nil. That reports only the errors the adapter exposes; it does not
discover swallowed failures. `MarkTerminalFailure` is the explicit way for
such code to report the narrower fact that it has abandoned work.

#### Contract: Loop

This is the authoritative statement of loop liveness. Other sections reference
it and must not restate it.

- **A loop handle is the unit of liveness.** `StartLoop` returns one and adds
  `+1`; `End` is idempotent and subtracts that `+1`. Two loops on one observer
  read 2, and ending one leaves the other counted.
- **Every handle from one observer carries the same labels**, because labels
  come from the bound consumer. The gauge counts loops, it does not
  distinguish them.
- **The handle spans the application's loop, not one delivery call.** A worker
  that repeatedly calls `Fetch` holds one handle across all of its batches: an
  ordinary finite batch closing its channel is not an ending. Ending a handle
  per batch would drop the gauge to zero between pulls and fire the `< 1`
  liveness alert on a healthy worker.
- **`End` is the only way the SDK learns a loop is over.** It never infers that
  from an error, which is what keeps error classification out of the business
  of deciding whether consumption stopped, and removes any ordering hazard
  between an asynchronous failure and a separate "started" call.
- **A handle never ended leaks its `+1` until the observer closes.** `Close`
  ends every handle the observer still holds, as a normal ending with no
  failure sample — the observer cannot know why a loop the application forgot
  was abandoned.

#### Contract: Observer Close

This is the authoritative statement of `Close`. Section 6 references it.

- Idempotent; after it returns the observer records nothing, and later calls on
  it are no-ops rather than errors.
- Ends every loop handle the observer still holds (see the Loop contract).
- Does **not** wait for in-flight `Observe` calls. An `Observe` already running
  completes and records normally; the application owns that ordering, which is
  the same division the rest of this ADR draws.
- Releases **observer-local measurement-option caches only**. The
  per-connection registry keeps the admitted identity tuple and its canonical
  vocabulary for the life of the connection, because section 6 has a
  replacement observer reuse that frozen vocabulary and reject an incompatible
  one. Admission is never returned: the MeterProvider's cumulative aggregators
  retain every attribute set ever recorded and nothing in `Close` can retract
  them, so returning the budget would let a connection cycling observers admit
  past the section 6 ceiling while its old series stay exported.
- Takes a context because it records.

All six delivery paths — `Consume`, `Messages`, `Next`, `Fetch`, `FetchBytes`,
`FetchNoWait` — share this `Observe` contract; where each mode's *ending* comes
from is section 5. `Observe` goes inside the actual worker, after admission,
as the worked example shows.

Calling a delivery API alone measures nothing. The SDK must not finalize a
previous message when the next one is fetched, a batch channel closes, an
iterator stops, or a consume handle drains — none of those proves the previous
worker returned.

An observed callback must finish using the tracked message before returning.
For work that outlives it, the application places `Observe` around that work
instead. Process termination can prevent any completion sample from being
recorded or exported; this is in-process instrumentation, not a durable
exactly-once ledger.

#### Worked example: the ordering the contracts require

The contracts above are about ordering, so one example is worth more than the
prose it replaces. This is a bounded-concurrency batch worker — the shape all
five newchat consumers use — showing admission, `Observe`, the loop boundary
and shutdown in the order the contracts demand.

```go
obs, err := conn.ConsumerObserver(ctx, cons, o11ynats.ConsumerMetricsConfig{
    Site:       cfg.SiteID,
    EventTypes: []string{"created", "updated", "deleted"},
})
if err != nil {
    return err
}
defer obs.Close(context.WithoutCancel(ctx)) // ends any loop still open

loop := obs.StartLoop(ctx) // ONE handle for the whole worker, not per batch
sem := make(chan struct{}, cfg.MaxWorkers)
var wg sync.WaitGroup

for {
    batch, err := cons.Fetch(ctx, cfg.BatchSize)
    if err != nil { // setup failed: this loop is over
        loop.End(ctx, err)
        break
    }

    for msg := range batch.Messages() {
        sem <- struct{}{} // admission FIRST — the timer must not include this wait
        wg.Add(1)
        go func(msg jetstream.Msg) {
            defer func() { <-sem; wg.Done() }()

            // Observe wraps the real work, inside the worker, after admission.
            _ = obs.Observe(ctx, msg, classify(msg),
                func(ctx context.Context, msg jetstream.Msg) error {
                    if err := handle(ctx, msg); err != nil {
                        if permanent(err) {
                            o11ynats.MarkTerminalFailure(ctx, o11ynats.TerminalPermanent)
                            return msg.Term() // settle before returning
                        }
                        return msg.NakWithDelay(backoff())
                    }
                    return msg.Ack()
                })
        }(msg)
    }

    // Read the batch error on the OUTER loop. An ordinary finite batch closing
    // its channel is not an ending; only a non-nil error is.
    if err := batch.Error(); err != nil {
        loop.End(ctx, err)
        break
    }
    if ctx.Err() != nil { // normal shutdown: ends the loop, records no failure
        loop.End(ctx, ctx.Err())
        break
    }
}

wg.Wait() // in-flight Observe calls finish and record; Close does not wait for them
```

Four things this pins that prose kept getting wrong:

1. `StartLoop` is outside the `for`. Inside it, the gauge would drop to zero
   between batches and fire the `< 1` liveness alert on a healthy worker.
2. `sem <- struct{}{}` precedes `Observe`, so admission wait is outside the
   timer — the boundary section 4 defines.
3. `batch.Error()` is read on the outer loop, and only a non-nil value ends
   the loop.
4. `wg.Wait()` is the application's, not `Close`'s. `Close` ends loops and
   releases caches; waiting for in-flight work is the application's ordering.

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
affected messages, so counting them as work would put a number on the metric
that nothing in the process knows.

They are not dropped, though — they move to their own instrument in the same
phase (section 5). An earlier draft deferred that instrument until it had "a
named reader and an explicit event-count contract"; both already exist and the
draft simply did not check. The reader is newchat's live dashboard panel
`sum by (reason) (rate(chat_nats_terminal_failures_total[$__rate_interval]))`,
whose description enumerates `stream_unavailable` among the reasons it exists
to show, and the producer is `LoopFailed` in `pkg/natsmetrics`. The
event-count contract is what excluding them from the per-work counter already
implies: **one sample per loop-failure event, never per affected message.**
Deferring would therefore not have postponed a speculative instrument; it
would have deleted a signal that is wired end to end today, and replaced it
with logs — which is a downgrade for alerting, not an equivalent.

Broker MaxDeliver and Term advisories remain independent evidence [S4]. Never
sum these sources, or the receive-error counter, as a count of lost work.

### 5. Instruments and names

The recommended phase-1 set is **five consumer instruments**. Publisher
instrumentation is deferred as a whole, including its vocabulary.

| Name | OTel instrument / unit | Proposed description and recording boundary |
|---|---|---|
| `o11y.nats.consumer.loops` | Int64UpDownCounter / `{loop}` | Active delivery loops, one `+1` per open loop handle (section 3). Publish an initial zero. |
| `o11y.nats.consumer.dispositions` | Int64Counter / `{message}` | Completed observed delivery attempts by local disposition. Record once at callback completion under section 4. |
| `messaging.process.duration` | Float64Histogram / `s` | Duration of processing operation. Record the complete observed callback invocation, including failures. |
| `o11y.nats.consumer.processing.failures` | Int64Counter / `{failure}` | Observed processing invocations explicitly declared terminally unsuccessful by the application. At most one per invocation. |
| `o11y.nats.consumer.receive.errors` | Int64Counter / `{error}` | Receive-loop failures reported by the application, by bounded cause. One sample per event, never per affected message (section 4). |

Loop boundaries are the Loop contract's (section 3); this section covers only
what `End` records. It emits at most one receive-error sample. The `loops`
gauge means neither broker connectivity nor end-to-end readiness, and a
disappeared process produces stale telemetry rather than a guaranteed final
zero.

**The reason is best effort, and deliberately not a contract.** A nil error,
`context.Canceled` or `context.DeadlineExceeded` is a normal ending and
records nothing — a shutdown, or a bounded fetch reaching its deadline, is how
a loop is *supposed* to end, and counting it would put a fleet-wide spike on
the one instrument whose purpose is to show that consumption broke. Otherwise
the SDK recognises what it can:

| Recognised error | Reason |
|---|---|
| `jetstream.ErrConsumerDeleted`, `ErrConsumerNotFound` | `consumer_deleted` |
| `jetstream.ErrStreamNotFound`, `ErrNoStreamResponse`, `ErrServerShutdown`, `ErrConnectionClosed`, `nats.ErrConnectionClosed`, `nats.ErrDisconnected`, `nats.ErrNoResponders` | `stream_unavailable` |
| everything else | `internal` |

**This is a refinement, not an enumeration.** An earlier draft promised a total
mapping over the client's error surface; that promise cannot be kept, because
the surface is not enumerable from outside and moves with every `nats.go`
release. `internal` is the honest default, adding a row is not a breaking
change, and nothing depends on the table being complete — the loop already
ended, because the caller said so. Classification is the opposite of
processing failures, where only the application knows whether work was
abandoned and so the application declares the reason.

**Where each mode's ending comes from:**

| Mode | The ending | Cause available? |
|---|---|---|
| `Next`, `Messages` | the returned error; a routine `nats.ErrTimeout` from an idle poll is not an ending, so the loop continues and `End` is not called | yes — the error *is* the ending |
| `Fetch`, `FetchBytes`, `FetchNoWait` | `batch.Error()` once the messages channel closes, read on the **application's outer loop**, not per batch; the direct return covers setup only. Read the facade's `messageBatch.Error()`, which already suppresses the `context.Canceled` an explicit `Stop` produces | yes |
| `Consume` | `ConsumeContext.Closed()` | **usually no** — see the contract below |

#### Contract: Consume ending

This is the authoritative statement for `Consume`. It is deliberately weaker
than the other two modes, and the weakness is the contract rather than a gap
to be closed later.

1. **`Closed()` proves this `Consume` ended. It does not prove it failed.**
   The pinned channel is a bare `<-chan struct{}` and carries no cause.
2. **The observer retains evidence of an application-initiated stop.** It owns
   the `ConsumeContext` it returns, so `Stop`/`Drain` called by the
   application is known. The handle handed to `OnConsumeError` must be the
   same wrapper — the native `ConsumeErrHandlerFunc` receives a full
   `ConsumeContext`, so an unwrapped one would let a stop from inside the
   callback bypass this evidence.
3. **The last observed error is diagnostic only.** It is never automatically
   the cause of the closure. The client reports non-terminal errors through
   the same callback, and stops itself on paths that carry no error at all —
   `StopAfter` reaching its limit is a normal ending with no application call
   and no error. Pairing a close with whatever error came before it would
   report that as a failure.
4. **An ending without sufficient evidence is unknown, not a failure.** It
   records no `receive.errors` sample. Unknown endings are not stuffed into a
   failure count to make the instrument look complete in every mode.

The operational consequence, which Q4 must be decided on: in `Consume` mode
`o11y.nats.consumer.receive.errors` will usually record **nothing**, even when
consumption genuinely broke. The authoritative signal that consumption stopped
in that mode is the `loops` gauge falling without an application-initiated
stop — which the liveness expressions in the migration section already cover.
Do not read the absence of receive-error samples as evidence that a `Consume`
loop is healthy.

The observer owns the `Consume` call for a second reason too: the handler
cannot be composed through options. `PullConsumeOpt` declares one unexported
method, so an opaque option cannot be inspected, and
`ConsumeErrHandler.configureConsume` is a plain assignment — options do not
merge, and no ordering preserves both the SDK's handler and the caller's.

```go
// ConsumerMetricsConfig gains:
//   OnConsumeError func(context.Context, jetstream.ConsumeContext, error)
func (o *ConsumerObserver) Consume(ctx context.Context, handler JetStreamMsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error)
```

`OnConsumeError` carries its own context because it fires long after
registration; capturing a request-scoped registration context would be
cancelled before the callback runs, so the observer supplies one detached from
it (`obsctx.Detach`, ADR 0024). A `jetstream.ConsumeErrHandler` passed in
`opts` to this `Consume` is silently overwritten — undetectable by the API, so
it is a documented rule with a lint rule behind it: use `OnConsumeError`.

Receive errors and processing failures answer different questions and must
never be summed: one counts work that will get no further attempt, the other
counts times a loop stopped being able to receive, with the number of affected
messages unknown by construction.

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

#### What this does not measure, and where that evidence lives

These five instruments answer "is processing working". They are half of a
consumer's operational picture, and the ADR states the other half rather than
leaving an operator to discover it during an incident. Nothing here proposes
moving broker monitoring into the SDK.

| Question | Evidence | Blind spot |
|---|---|---|
| Is the loop alive? | `o11y.nats.consumer.loops` (+ `absent()` / `up == 0`) | A `Consume` loop that ended without an application stop shows here and usually *only* here |
| Are messages being settled, and how? | `o11y.nats.consumer.dispositions` by disposition | Counts attempts, not distinct messages |
| How long does processing take? | `messaging.process.duration` | Excludes queue and admission wait, by design |
| Did the application abandon work? | `o11y.nats.consumer.processing.failures` | Only what the application declares |
| Did receiving break? | `o11y.nats.consumer.receive.errors` | Usually silent under `Consume` (Contract: Consume ending) |
| **Am I falling behind?** | **Broker exporter** — JetStream consumer pending and redelivered gauges | Not emitted by this layer at all |
| **Is a message stuck in redelivery?** | **Broker exporter**, cross-checked with `dispositions{disposition="nak"}` | The disposition counter counts attempts; it cannot say how many distinct messages are stuck |
| **Is the worker pool saturated?** | **Not measured.** Scheduling belongs to the application (section 1), so neither this layer nor the broker sees it | Inferable from rising duration against a flat disposition rate, which is an inference, not a signal |

The first five rows are this ADR. The last three are why an operator must not
read this set as the whole picture: a falling `dispositions` rate means
"nothing arriving" or "50k pending", and only the broker exporter can tell
those apart.

### 6. Attributes, cardinality, and cache limits

The following mapping is an **SDK-proposed JetStream mapping**, not a claim
that OTel defines a NATS-specific mapping. For these consumer metrics,
the stream is the logical broker destination and the durable consumer is
the subscription. Existing upstream spans retain their current subject-based
destination naming; this proposal does not rename spans or promise a direct
equality join between the metric destination and the span destination.

| Attribute | Applies to | Source and bound |
|---|---|---|
| `messaging.system` | All five | Constant `nats`; a documented custom system value already used upstream |
| `messaging.destination.name` | All five | Stream from consumer setup; admitted identity tuple only |
| `messaging.destination.subscription.name` | All five | Durable consumer from setup; ephemeral identities excluded from this phase |
| `o11y.nats.site` | All five, optional | Immutable deployment/site alias supplied at setup; never extracted from payloads |
| `messaging.operation.name` | Processing duration | Constant `process` |
| `messaging.operation.type` | Processing duration | Constant `process` |
| `error.type` | Processing duration, failures only | Four failure classes defined in section 5 |
| `o11y.nats.event.type` | Dispositions, processing duration, processing failures | Application allowlist plus reserved `unknown` |
| `o11y.nats.disposition` | Dispositions | Six values defined in section 4 |
| `o11y.nats.failure.reason` | Processing failures | Five values defined in section 4 |
| `o11y.nats.receive.reason` | Receive errors | Three values defined in section 5; SDK-classified, not application-declared |

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
   message metadata into a process-global map. Admission and the frozen
   vocabulary last for the connection; `Close` releases observer-local caches
   only (Contract: Observer Close, section 3). Repeated setup for the same tuple reuses its frozen vocabulary; incompatible
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

#### Contract: Disabled

This is the authoritative statement of disabled mode. Section 1 references it.

- **Still runs:** configuration validation that would be wrong under any
  setting — a malformed event allowlist, an oversized configuration, an
  identity that cannot be resolved from the consumer. These are programming
  errors, and hiding them behind a toggle would make the toggle change which
  bugs are reachable.
- **Skipped:** metric registration, measurement-option caches, timers, and
  metrics-only message wrapping.
- **Skipped, and this is the precedence rule:** validation that exists only
  because metrics exist. **Unsupported acknowledgment policies are the case
  this decides** — section 1 requires `AckNone`/`AckAll` to be rejected at
  observer setup, but that rejection exists solely because per-message
  disposition cannot be recorded for them. With metrics disabled there is
  nothing to record, so turning observability off must not fail a startup that
  would otherwise succeed. Disabled mode therefore skips it.
- **Guaranteed either way:** the handler is invoked with the original message
  and context; application metadata used for retry decisions stays available;
  context lifetime and native method behavior are unchanged. Moving any
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
| `chat.nats.consumer.processing.duration` | `messaging.process.duration` | Full callback duration and an error breakdown instead of disposition labels. Boundaries are unchanged — newchat already builds this histogram with `o11y.DefaultLatencyBuckets()`, which section 5 keeps — so a bucket-bound query keeps *parsing*, and that is the trap: what lands in those buckets now measures a different span of time (Q2). Re-evaluate every SLO over this family rather than porting it, and scope queries by version through the rollout |
| `chat.nats.terminal.failures` | `o11y.nats.consumer.processing.failures` **and** `o11y.nats.consumer.receive.errors` | The one family splits in two — see the reason-by-reason mapping below. A panel that today breaks the single family down `by (reason)` must query both and must not sum them |
| `chat.nats.publish.failures` and the two RPC histograms | Keep in newchat | No phase-1 ownership/name change |

Where each `reason` value goes, because the split is not a clean partition:

| `chat.nats.terminal.failures{reason=…}` | Lands on | As |
|---|---|---|
| `permanent`, `invalid_payload`, `publish_exhausted`, `max_deliver` | `…processing.failures` | `o11y.nats.failure.reason`, same values |
| `consumer_deleted`, `stream_unavailable` | `…receive.errors` | `o11y.nats.receive.reason`, same values |
| `internal` | **both** | `o11y.nats.failure.reason="internal"` *and* `o11y.nats.receive.reason="internal"` |

`internal` is the row that will bite a mechanical migration, and it is worth
being precise about why: it is already two things today. `MarkTerminal` records
it when a handler declares an unclassified terminal failure, and `LoopFailed`
records it as the default arm for a receive error matching none of its
sentinels — both onto the same metric under the same label value. The split
separates them, which is the improvement; the cost is that a dashboard that
merely renames the metric keeps one source and silently drops the other, and
one that queries both and adds them double-counts nothing today but will
conflate a handler bug with a broker outage tomorrow. Point each existing
`internal` panel at whichever source it was actually watching. The two
instruments carry different attribute *keys*, so a query can always tell them
apart even though the value spelling is shared.

Inline `site`, `stream`, `consumer`, `event_type`, `outcome`, and `reason`
map to the scoped/standard attributes in section 6. The processing histogram
does not retain an outcome label. Operator queries must be reviewed against
these mappings, not transformed by a prefix replacement alone.

**Phase-1 inventory, counted at the evidence baseline.** Three numbers, because
they are different sizes and the ADR previously quoted only the largest:

| | Count |
|---|---|
| Services with a consumer to migrate | **5** — `message-gatekeeper`, `broadcast-worker`, `message-worker`, `notification-worker`, `room-worker` |
| Consumer registrations (`ConsumerConfig` sites) | **5** |
| Shared entry points to modify | **1** — `pkg/natsmetrics/loop.go`, which all five already go through |

The "fifteen services" figure quoted elsewhere in earlier drafts is the whole
`pkg/natsmetrics` import surface, which includes the publish and RPC recorders
this phase defers. The shared loop is what makes the migration small: the
observer calls land in one helper rather than in five hand-written loops.

That lowers the **cost**; it is not what makes the SDK/application split
correct. The split rests on which side can see what — the SDK holds the
`jetstream.Msg` interface and its own pinned error surface, the application
holds business failure, event vocabulary and scheduling — and that argument
does not change with the number of call sites. A larger N would make this
migration more expensive without making the boundary wrong.

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

### Per-durable liveness alerting has to be redesigned, not relabelled

The loop family is the one migration item that cannot be handled by mapping
labels, because two things change at once and the second is not a rename.

newchat's dashboard already documents the trap in the *existing* metric: a
hard process crash cannot emit a zero — the series goes stale and then
disappears — so `chat_nats_consumer_loop_up` alone reports a crashed consumer
as green. Catching it needs one `absent()` per expected durable, fully
qualified, backed by a maintained inventory of what should be running:

```promql
absent(chat_nats_consumer_loop_up{
  service_name="broadcast-worker", site="site-a",
  stream="MESSAGES-CANONICAL-site-a", consumer="broadcast-worker"})
```

`o11y.nats.consumer.loops` keeps that trap and adds two changes, neither of
which is a relabel.

First, the label *names* move, and the names an alert must use are the
**Prometheus-rendered** ones, not the OTLP attribute keys in section 6. This
repo's default metrics path is Prometheus pull (AGENTS.md), and the exporter
renders dots as underscores, so the selector above becomes:

```promql
absent(o11y_nats_consumer_loops{
  service_name="broadcast-worker", o11y_nats_site="site-a",
  messaging_destination_name="MESSAGES-CANONICAL-site-a",
  messaging_destination_subscription_name="broadcast-worker"})
```

Writing `messaging.destination.subscription.name` in a PromQL selector matches
nothing and fails open — the alert simply never fires. Section 6's table is the
OTLP contract; this is its exposition rendering, and alerting needs the second.

Second, the value is no longer a per-durable boolean. Section 5 makes it a
count to which every observer with the same labels contributes, so a process
running two observers over one durable reads 2 and an expression testing
`== 1` silently stops firing. The live-but-idle case needs its own expression
beside the `absent()` one, because a reachable process whose loop has stopped
still exports the series — at zero:

```promql
o11y_nats_consumer_loops{
  service_name="broadcast-worker", o11y_nats_site="site-a",
  messaging_destination_name="MESSAGES-CANONICAL-site-a",
  messaging_destination_subscription_name="broadcast-worker"} < 1
```

Three conditions, three different failures: `absent()` catches the durable
whose series vanished, `< 1` catches the one still scraped with no live loop,
and `up == 0` catches the whole target going away. None substitutes for
another, and only the third is unchanged by this migration.

Treat this as its own migration task with the inventory owner, ahead of the
cutover: regenerate all three per durable against the rendered names. None of
it is derivable from the label mapping table above, which is why it is called
out separately.

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
- `README.md`, `docs/guide.md`, `examples/README.md`, JetStream examples, and
  CHANGELOG: the breaking `Connect` signature, `WithMetricsEnabled`, and the
  explicit processing-boundary requirement.

Required behavior tests, with a ManualReader and native-message fakes:

1. Every terminal method forwards arguments/results/call counts; InProgress
   does not settle. Guard the pinned interface method inventory as well as
   interface conformance, because embedding can silently inherit a new method.
2. All facade delivery modes can use the same observer. No timer includes
   semaphore wait; an early Ack does not stop the processing timer.
3. Ack/DoubleAck failure followed by success; repeated terminal calls; Term
   failure; handler error without disposition; cancellation; abnormal unwind;
   Ack followed by panic; concurrent state access under `-race`.
4. Shutdown ordering, stated per mode because `Drain` means different things.
   Native `ConsumeContext.Drain` (the `Consume` mode only) stops admission
   while already-admitted deliveries finish; it does **not** wait for
   goroutines the callback dispatched, which is the application's `wg.Wait()`
   in the worked example. For `Messages`, `Next` and the batch modes there is
   no native drain at all — the application stops fetching. In every mode,
   iterator or batch closure does not finalize handlers, `Close` does not wait
   for in-flight `Observe` calls (Contract: Observer Close), and an abandoned
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
9. The receive-error classifier is total and pinned: each sentinel in section
   5's table maps to its stated reason, an unrecognised error is `internal`,
   and `context.Canceled`/`context.DeadlineExceeded` end the loop while
   recording no sample. An unrecognised error must not change any behaviour
   beyond its label, so the suite proves the table is refinement rather than
   contract.
10. Every delivery mode reaches `End` by its own route: `Next`/`Messages`
    from the returned error, with a routine `nats.ErrTimeout` leaving the
    loop open; the batch modes from `batch.Error()` after the channel closes,
    with a `Stop`-induced `context.Canceled` producing no sample; `Consume`
    from `ConsumeContext.Closed()`. Two loops on one observer read 2, and
    ending one leaves the other counted.
11. `OnConsumeError` fires for terminal and non-terminal errors alike, with a
    context that outlives a cancelled registration context, and a
    `jetstream.ConsumeErrHandler` passed in `opts` does not suppress the
    observer's own recording.
12. The Loop and Observer Close contracts, with their counter-examples: one
    handle survives many batches and the gauge does not dip between them; two
    loops on one observer read 2; a handle never ended is closed by `Close` as
    a normal ending with no failure sample; `Close` is idempotent, records
    nothing afterwards, leaves the identity admitted and its vocabulary intact
    so a replacement still gets the frozen vocabulary and still rejects an
    incompatible one, and cycling observers does not raise the
    admitted-identity count.
13. The Consume ending contract's four rules, each with its counter-example:
    an application `Stop` records no failure; a `StopAfter` limit reached
    after an earlier recoverable error records **no** failure, and in
    particular not that error; a stop called from inside `OnConsumeError`
    still counts as application-initiated; and a close with no evidence
    records nothing rather than a synthetic reason.
14. The Disabled contract: an `AckNone` consumer constructs successfully with
    metrics off and fails setup with metrics on, while a malformed event
    allowlist fails in both.

Before an implementation commit, run repository-required formatting/tidy,
ADR gate, lint/vet, normal tests, and race tests. Benchmark raw handling,
disabled observation, no-op-provider observation, and real recording, with
both warm caches and all permitted combinations. Record measured results
instead of copying newchat's historical timings as SDK performance claims.
This documentation-only proposal requires Markdown/link and diff checks;
it does not claim those implementation gates have passed.

## Deferred work and triggers

- **Publish failures and publisher vocabulary:** migrate together when the
  bounded destination/operation contract and readers are agreed. That trigger
  needs a named owner at acceptance; without one the phase-1 duplication below
  has no one to end it. A failures-
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
| Q4 | Is the consumer migration acceptable given the failure signal it actually preserves, while publisher/RPC stay in newchat? | Qualified yes. Five instruments, but decide on what they carry, not that they exist: `receive.errors` is strong for `Next`/`Messages`/batch and **usually records nothing under `Consume`**, where the ending is authoritative but its cause is not (Contract: Consume ending, section 5). If that degradation is unacceptable, the honest alternative is to declare `receive.errors` unsupported for `Consume` rather than to fill it with unknown endings. |

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
the full processing operation. Adopting costs one reviewed migration, whose
real size is in the inventory below rather than the whole `pkg/natsmetrics`
surface.

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
