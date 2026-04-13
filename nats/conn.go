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
// conn.Publish(ctx, msg.Reply, data) instead of msg.Respond(data).
// msg.Respond routes through the raw NATS connection and does not inject
// trace headers, breaking the distributed trace.
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
		return nil, err
	}
	return &Conn{Conn: nc}, nil
}

// Subscribe subscribes to subject and invokes handler for each inbound message.
// The handler's ctx carries a consumer span created by the otelnats layer. That
// consumer span holds a span link to the publisher's trace, enabling correlation
// across services in Grafana Tempo. Calls to slog.InfoContext(ctx, ...) will
// include the consumer's trace_id and span_id; calls to tracer.Start(ctx, ...)
// produce child spans of the consumer span.
func (c *Conn) Subscribe(subject string, handler MsgHandler) (*natsgo.Subscription, error) {
	if handler == nil {
		return nil, fmt.Errorf("nats subscribe %q: handler must not be nil", subject)
	}
	return c.Conn.Subscribe(subject, func(m otelnats.Msg) {
		handler(m.Ctx, m.Msg)
	})
}

// QueueSubscribe is the queue-group variant of Subscribe. All members of the
// same queue group share message delivery round-robin, providing load balancing
// across multiple subscriber instances.
func (c *Conn) QueueSubscribe(subject, queue string, handler MsgHandler) (*natsgo.Subscription, error) {
	if handler == nil {
		return nil, fmt.Errorf("nats queue-subscribe %q/%q: handler must not be nil", subject, queue)
	}
	return c.Conn.QueueSubscribe(subject, queue, func(m otelnats.Msg) {
		handler(m.Ctx, m.Msg)
	})
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
