// Package nats provides a tracing-aware NATS connection wrapper that wires
// the o11y SDK's TracerProvider and Propagator into otelnats / oteljetstream.
// All NATS connections in a service should go through this package so that
// trace context propagates across publishers and subscribers without touching
// global OpenTelemetry state.
package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/akira-core/instrumentation-go/otel-nats/otelnats"
	natsgo "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// MsgHandler is the callback signature for NATS subscriptions managed by this package.
// ctx carries the consumer span and W3C baggage extracted from the inbound
// message headers, enabling log correlation, baggage materialization, and child
// span creation within the handler body.
//
// Note: to reply to a message while preserving trace context, use
// conn.Respond(ctx, msg, data) (or conn.Publish(ctx, msg.Reply, data))
// instead of msg.Respond(data). msg.Respond routes through the raw NATS
// connection and does not inject trace headers, breaking the distributed
// trace.
type MsgHandler func(ctx context.Context, msg *natsgo.Msg)

// Conn is a tracing-aware NATS connection. It embeds *otelnats.Conn so all
// core methods (Publish, PublishMsg, Request, Drain, Close) are available
// directly. Subscribe and QueueSubscribe are overridden to expose the
// simplified MsgHandler callback.
type Conn struct {
	*otelnats.Conn
	policy propagationPolicy
}

type propagationPolicy struct {
	propagator     propagation.TextMapPropagator
	tracingEnabled func() bool
}

// restoreBaggage re-attaches W3C baggage from the inbound headers to ctx.
// Upstream starts each handler span from a fresh context, so baggage extracted
// during its own header read is not carried into the handler's ctx; this
// restores it without disturbing the consumer span context already in ctx.
//
// Guard order is deliberate and load-bearing for cost. Since otel-nats v0.8.0
// tracingEnabled resolves the dynamic switch, which in a relay-capable process
// is two OpenFeature evaluations (measured upstream at roughly 12 us and 23
// allocations each) on every call. The two guards above it are a nil check and
// a map length, so a message that can never carry baggage — no headers at all,
// the common case — is rejected for free instead of paying the relay. The
// reordering is behaviour-identical: all three conditions short-circuit to the
// same unchanged ctx.
//
// Known limitation (upstream dispatch is resolved separately): upstream picks
// the traced or direct handler with its own evaluation of the same switch, and
// this call re-resolves it microseconds later. A relay flip landing between the
// two makes them disagree — restoring wire baggage on a message upstream
// delivered untraced, or dropping it on one upstream traced. The window is
// microseconds against a relay poll interval measured in tens of seconds. A
// race-free fix needs the dispatch decision to travel with the message rather
// than being recomputed; that is tracked as R10 in docs/upstream-otel-nats.md.
func (p propagationPolicy) restoreBaggage(ctx context.Context, headers natsgo.Header) context.Context {
	if p.propagator == nil || len(headers) == 0 {
		return ctx
	}
	if p.tracingEnabled == nil || !p.tracingEnabled() {
		return ctx
	}
	extracted := p.propagator.Extract(context.Background(), &otelnats.HeaderCarrier{H: headers})
	bag := baggage.FromContext(extracted)
	if bag.Len() == 0 {
		return ctx
	}
	return baggage.ContextWithBaggage(ctx, bag)
}

// Connect establishes a traced NATS connection.
// ctx is checked before dialing — if it is already canceled, Connect returns
// immediately with ctx.Err(). Note: the underlying NATS client does not
// support context cancellation during an in-progress dial; canceling ctx
// after Connect returns has no effect on an established connection.
//
// tp and prop must be non-nil; they are wired directly into the underlying
// otelnats layer, and supplied this way no global OTel state is read or
// modified. SDK callers pass obs.TracerProvider() / obs.Propagator, which are
// always non-nil (obs.TracerProvider() is a noop provider — never nil — when
// the trace pillar is off). Passing a literal nil would let the upstream layer
// fall back to the process-global provider/propagator (otel.GetTracerProvider /
// otel.GetTextMapPropagator), which is exactly the ambient global state ADR
// 0003 keeps this SDK away from — so don't.
//
// Tracing defaults to enabled via otelnats.WithTracingEnabled(true). Since
// otel-nats v0.8.0, OTEL_NATS_TRACING_ENABLED and a configured feature-flag
// relay outrank that connection-local default and can change the effective
// state, and the process-wide master OTEL_INSTRUMENTATION_GO_TRACING_ENABLED is
// ANDed above all of them — it defaults to enabled, but a falsy value disables
// NATS tracing whatever the rest of the ladder says. Both variables are a
// strict tri-state: an unparseable value (including the empty string) fails
// this call rather than being ignored. When the SDK's trace pillar is disabled
// but the effective NATS switch
// is enabled, obs.TracerProvider() is a no-op provider: the traced code path
// runs, but every span is non-recording and carries no span context. Because
// upstream starts each publish/consume span from a fresh context, a no-op
// provider yields no active trace to inject, so cross-service trace correlation
// is simply inactive while the pillar is off (not broken) and resumes once the
// trace pillar is enabled.
//
// With no relay and no overriding upstream environment variable,
// ConnectWithOptions plus WithTracingEnabled(false) selects the direct/native
// path. A relay-capable process keeps both implementations and evaluates the
// flags per operation even while the effective value is false.
//
// Typical usage with the o11y SDK:
//
//	conn, err := nats.Connect(ctx, url, obs.TracerProvider(), obs.Propagator)
func Connect(ctx context.Context, url string, tp trace.TracerProvider, prop propagation.TextMapPropagator, natsOpts ...natsgo.Option) (*Conn, error) {
	return connect(ctx, url, tp, prop, true, natsOpts)
}

// ConnectWithOptions establishes a NATS connection with o11y-owned connection
// options.
//
// It defaults to the same tracing behavior as Connect. WithTracingEnabled is a
// connection-local default: the upstream environment and relay can override it.
// With no relay or override, false selects the direct/native path. Use
// WithNATSOptions to pass ordinary nats.go connection options such as nats.Name
// or nats.UserCredentials.
func ConnectWithOptions(ctx context.Context, url string, tp trace.TracerProvider, prop propagation.TextMapPropagator, opts ...ConnectOption) (*Conn, error) {
	cfg := newConnectConfig(opts)
	return connect(ctx, url, tp, prop, cfg.tracingDefault, cfg.natsOpts)
}

func connect(ctx context.Context, url string, tp trace.TracerProvider, prop propagation.TextMapPropagator, tracingDefault bool, natsOpts []natsgo.Option) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats connect: context already canceled: %w", err)
	}
	// The non-nil requirement is now enforced rather than only documented.
	// Before otel-nats v0.8.0 a nil propagator was survivable here because the
	// facade read its propagator back from nc.TraceContext(), which supplied a
	// usable value on both paths. v0.8.0 made that accessor return a throwaway
	// empty propagator on the direct path, so the facade holds the caller's
	// value directly — and a nil one would silently disable baggage restoration
	// for the life of the connection while upstream carried on propagating via
	// the OTel globals. Failing the call keeps that divergence from shipping,
	// and keeps ADR 0003's no-ambient-globals contract enforceable rather than
	// advisory. SDK callers pass obs.TracerProvider() / obs.Propagator, which
	// are never nil, so this cannot fire on the supported path.
	if tp == nil {
		return nil, fmt.Errorf("nats connect %s: tracer provider must not be nil (pass obs.TracerProvider())", url)
	}
	if prop == nil {
		return nil, fmt.Errorf("nats connect %s: propagator must not be nil (pass obs.Propagator)", url)
	}
	nc, err := otelnats.ConnectWithOptions(url, natsOpts,
		otelnats.WithTracerProvider(tp),
		otelnats.WithPropagators(prop),
		otelnats.WithTracingEnabled(tracingDefault),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", url, err)
	}
	return &Conn{
		Conn: nc,
		policy: propagationPolicy{
			propagator:     prop,
			tracingEnabled: nc.TracingEnabled,
		},
	}, nil
}

// Subscribe subscribes to subject and invokes handler for each inbound message.
// The handler's ctx carries a consumer span created by the otelnats layer. That
// consumer span holds a span link to the publisher's trace, enabling correlation
// across services in Grafana Tempo. Calls to slog.InfoContext(ctx, ...) will
// include the consumer's traceId and spanId; calls to tracer.Start(ctx, ...)
// produce child spans of the consumer span. On the traced path the facade also
// restores W3C baggage using this connection's configured propagator without
// replacing that consumer span context.
//
// ctx is checked at registration time only. If ctx is already cancelled or
// past its deadline, Subscribe returns ctx.Err() without registering the
// subscription. Subsequent ctx cancellation does NOT stop the subscription —
// long-running subscriptions are torn down via the returned
// *natsgo.Subscription's Unsubscribe / Drain methods. Each delivered message
// gets its own consumer-side context derived from the inbound headers, so
// the caller's ctx is intentionally not retained for handler invocation.
//
// Subscribe rejects an empty subject up-front: an empty subject silently
// matches no messages on the NATS server and is almost always a programming
// error.
func (c *Conn) Subscribe(ctx context.Context, subject string, handler MsgHandler) (*natsgo.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats subscribe %q: %w", subject, err)
	}
	if subject == "" {
		return nil, fmt.Errorf("nats subscribe: subject must not be empty")
	}
	if handler == nil {
		return nil, fmt.Errorf("nats subscribe %q: handler must not be nil", subject)
	}
	return c.Conn.Subscribe(subject, func(m otelnats.Msg) {
		handler(c.policy.restoreBaggage(m.Ctx, m.Msg.Header), m.Msg)
	})
}

// QueueSubscribe is the queue-group variant of Subscribe. All members of the
// same queue group share message delivery round-robin, providing load balancing
// across multiple subscriber instances. Both subject and queue must be
// non-empty.
//
// ctx semantics match Subscribe: it is consulted at registration only;
// cancel the returned *natsgo.Subscription via Unsubscribe / Drain to stop
// delivery.
func (c *Conn) QueueSubscribe(ctx context.Context, subject, queue string, handler MsgHandler) (*natsgo.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats queue-subscribe %q/%q: %w", subject, queue, err)
	}
	if subject == "" {
		return nil, fmt.Errorf("nats queue-subscribe: subject must not be empty")
	}
	if queue == "" {
		return nil, fmt.Errorf("nats queue-subscribe %q: queue must not be empty", subject)
	}
	if handler == nil {
		return nil, fmt.Errorf("nats queue-subscribe %q/%q: handler must not be nil", subject, queue)
	}
	return c.Conn.QueueSubscribe(subject, queue, func(m otelnats.Msg) {
		handler(c.policy.restoreBaggage(m.Ctx, m.Msg.Header), m.Msg)
	})
}

// Respond replies to msg over the traced publish path, preserving trace
// context end to end. It injects the active span context carried by ctx into
// the reply headers using the same producer instrumentation as Publish, so the
// reply links into the distributed trace.
//
// Use this instead of msg.Respond(data), which routes through the raw NATS
// connection and skips header injection, breaking the trace (ADR 0004 §5).
// Respond is the recommended way to reply from inside a Subscribe /
// QueueSubscribe handler.
//
// It returns an error if msg is nil or if msg.Reply is empty, matching the
// up-front validation Subscribe and QueueSubscribe perform — an empty reply
// subject means there is nowhere to send the response and is almost always a
// programming error.
//
// Respond itself does not create a requester-side span; the reply is simply
// injected with ctx's trace context via the traced Publish path. The
// requester-side receive span that links back to this reply is created by
// Conn.Request (ADR 0022 amendment, 2026-07-01).
func (c *Conn) Respond(ctx context.Context, msg *natsgo.Msg, data []byte) error {
	if msg == nil {
		return fmt.Errorf("nats respond: msg must not be nil")
	}
	if msg.Reply == "" {
		return fmt.Errorf("nats respond: msg has no reply subject")
	}
	if err := c.Publish(ctx, msg.Reply, data); err != nil {
		return fmt.Errorf("nats respond to %q: %w", msg.Reply, err)
	}
	return nil
}

// Request sends subject/data and waits for a reply. ctx carries the trace
// context and cancellation; timeout bounds the wait. The upstream otel-nats
// layer (v0.6.0+) emits the producer "send" span, injects W3C headers, and
// emits the requester-side CLIENT "receive" span for the reply (otel-nats
// v0.7.0 corrected the kind from CONSUMER) — linked to
// the responder's trace whenever the responder replied via Conn.Respond (or
// any traced publish path). Earlier SDK versions created that reply span in
// this facade; it is now delegated to upstream so the round trip is recorded
// exactly once.
//
// The upstream primary Request method mirrors nats.Conn.Request and takes no
// ctx (its producer span would parent to context.Background()), so this
// facade routes through RequestWithContext with a timeout-derived deadline,
// preserving the o11y ctx-first contract: the send span parents to ctx and
// cancelling ctx aborts the wait early.
//
// Known limitation (unchanged): the request-side spans land in the caller's
// trace only when ctx already carries an active span. Callers without an
// ambient span at the call site (e.g. a background worker's own top-level
// request loop) should open one first:
//
//	ctx, span := tracer.Start(ctx, "request "+subject)
//	defer span.End()
//	reply, err := conn.Request(ctx, subject, payload, timeout)
//
// Migration note (pre-1.0 API change, otel-nats v0.6.0 upgrade): the
// variadic attrs parameter was removed. The reply receive span is now
// created inside otel-nats, which currently offers no caller-attribute
// injection point; attach domain identifiers (request/correlation IDs,
// room/site IDs) to your own ambient span instead.
func (c *Conn) Request(ctx context.Context, subject string, data []byte, timeout time.Duration) (*natsgo.Msg, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.RequestWithContext(reqCtx, subject, data)
}

// RequestMsg sends a pre-built request message — use it to set headers on the
// request — and waits for a reply, with the same ctx-first tracing contract as
// Request: ctx carries the trace context and cancellation, and timeout bounds
// the wait. The producer "send" span parents to ctx; as with Request, the
// upstream layer records the reply "receive" span under the responder's trace,
// linked back to the request when the reply carries trace context and recorded
// without a link when it does not.
//
// This shadows the embedded otelnats.Conn.RequestMsg(msg, timeout), whose
// ctx-less signature parents its producer span to context.Background() and so
// orphans the trace (the issue #72 footgun the facade's ctx-first Request
// shim exists to prevent). Always use this ctx-first form; the embedded
// ctx-less method is intentionally shadowed and unreachable through the facade.
func (c *Conn) RequestMsg(ctx context.Context, msg *natsgo.Msg, timeout time.Duration) (*natsgo.Msg, error) {
	if msg == nil {
		return nil, fmt.Errorf("nats request-msg: msg must not be nil")
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.RequestMsgWithContext(reqCtx, msg)
}
