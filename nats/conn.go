package nats

import (
	"context"

	natstracing "github.com/Marz32onE/natstrace/natstrace"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Conn wraps the traced NATS connection to provide a simplified, unified API.
type Conn struct {
	*natstracing.Conn
}

// MsgHandler is a function that processes a NATS message within a context.
type MsgHandler func(ctx context.Context, msg *nats.Msg)

// NewTracedConn creates a new traced NATS connection.
// It explicitly ensures the global providers are used.
func NewTracedConn(url string, natsOpts []nats.Option) (*Conn, error) {
	// Explicitly fetch what was set in o11y.Init to ensure synchronization.
	// Since natstracing v0.1.10 Connect signature does not accept tracing options,
	// and its internal newConn() uses otel.GetTracerProvider(),
	// we rely on the global state which o11y.Init has already configured.

	// Connect using the traced wrapper.
	nc, err := natstracing.Connect(url, natsOpts...)
	if err != nil {
		return nil, err
	}

	return &Conn{Conn: nc}, nil
}

// Subscribe wraps the underlying subscription and passes (ctx, msg) to our local MsgHandler.
func (c *Conn) Subscribe(subject string, cb MsgHandler) (*nats.Subscription, error) {
	return c.Conn.Subscribe(subject, func(m natstracing.MsgWithContext) {
		cb(m.Ctx, m.Msg)
	})
}

// QueueSubscribe is the queue-group variant of Subscribe.
func (c *Conn) QueueSubscribe(subject, queue string, cb MsgHandler) (*nats.Subscription, error) {
	return c.Conn.QueueSubscribe(subject, queue, func(m natstracing.MsgWithContext) {
		cb(m.Ctx, m.Msg)
	})
}

// ExtractContext is a helper to manually extract context if needed (though Subscribe handles it).
func ExtractContext(m *nats.Msg) context.Context {
	return otel.GetTextMapPropagator().Extract(context.Background(), propagation.HeaderCarrier(m.Header))
}
