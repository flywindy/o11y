// Package nats provides a tracing-aware NATS connection wrapper that wires
// the o11y SDK's TracerProvider and Propagator into otelnats / oteljetstream.
// All NATS connections in a service should go through this package so that
// trace context propagates across publishers and subscribers without touching
// global OpenTelemetry state.
package nats

import (
	"context"
	"fmt"

	"github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream"
	"github.com/Marz32onE/instrumentation-go/otel-nats/otelnats"
	natsgo "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// MsgHandler is the callback signature for NATS subscriptions managed by this package.
// ctx carries the trace context extracted from the inbound message headers,
// enabling log correlation and child span creation within the handler body.
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
}

// Connect establishes a traced NATS connection.
// ctx is checked before dialing — if it is already canceled, Connect returns
// immediately with ctx.Err(). Note: the underlying NATS client does not
// support context cancellation during an in-progress dial; canceling ctx
// after Connect returns has no effect on an established connection.
//
// tp and prop are wired directly into the underlying otelnats layer;
// no global OTel state is read or modified.
//
// Typical usage with the o11y SDK:
//
//	conn, err := nats.Connect(ctx, url, obs.TracerProvider(), obs.Propagator)
func Connect(ctx context.Context, url string, tp trace.TracerProvider, prop propagation.TextMapPropagator, natsOpts ...natsgo.Option) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats connect: context already canceled: %w", err)
	}
	nc, err := otelnats.ConnectWithOptions(url, natsOpts,
		otelnats.WithTracerProvider(tp),
		otelnats.WithPropagators(prop),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", url, err)
	}
	return &Conn{Conn: nc}, nil
}

// Subscribe subscribes to subject and invokes handler for each inbound message.
// The handler's ctx carries a consumer span created by the otelnats layer. That
// consumer span holds a span link to the publisher's trace, enabling correlation
// across services in Grafana Tempo. Calls to slog.InfoContext(ctx, ...) will
// include the consumer's traceId and spanId; calls to tracer.Start(ctx, ...)
// produce child spans of the consumer span.
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
		handler(m.Ctx, m.Msg)
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
		handler(m.Ctx, m.Msg)
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
// Respond guarantees the reply carries the trace context; it does not create a
// requester-side span that links back to the responder. That requester-side
// linkage is out of scope (see ADR 0022).
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

// JetStream returns a tracing-aware JetStream interface backed by this connection.
// The returned JetStream inherits the TracerProvider and Propagator from this Conn
// via otelnats.Conn.TraceContext — no additional configuration is required.
//
// Use the returned interface to create streams, consumers, and publish with
// full OTel trace propagation across JetStream publish and consume operations.
func (c *Conn) JetStream() (oteljetstream.JetStream, error) {
	return oteljetstream.New(c.Conn)
}
