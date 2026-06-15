package nats

import (
	"context"
	"fmt"

	"github.com/Marz32onE/instrumentation-go/otel-nats/oteljetstream"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// This file is the o11y-owned JetStream facade (ADR 0022 Phase 2). It wraps the
// upstream oteljetstream types in o11y interfaces so callers import only
// github.com/flywindy/o11y/nats, never the Marz oteljetstream package, while
// still getting its trace propagation and consumer span-links for free.
//
// Configuration types (StreamConfig, ConsumerConfig, AckExplicitPolicy, PubAck,
// PullConsumeOpt, PullMessagesOpt, NextOpt, FetchOpt, PublishOpt, …) are taken
// directly from github.com/nats-io/nats.go/jetstream — they are plain stdlib
// types, not a Marz dependency.
//
// Consume callbacks and the Messages/Next iterators deliver the native
// jetstream.Msg together with a ctx carrying the consumer span (matching the
// core MsgHandler shape (ctx, msg)); no o11y-owned message type is introduced.
//
// Scope: this facade wraps the JetStream surface o11y consumers use today
// (stream/consumer management, Publish, and the pull consume modes Consume /
// Messages / Next). Deferred until a consumer needs them: PushConsumer, ordered
// consumers, Fetch / FetchBytes / FetchNoWait (the batch path would require an
// o11y-owned carrier type for the channel), and the admin surface
// (Pause/Resume/List/Unpin). They are wrapped on demand in a later change.

// JetStreamMsgHandler is the Consume callback signature. ctx carries the
// consumer span extracted from the message headers by the upstream layer, so
// slog.InfoContext(ctx, ...) and tracer.Start(ctx, ...) inside the handler are
// correlated with the producer's trace. msg is the native jetstream.Msg, so
// Ack / Nak / Term / InProgress / Metadata are available directly.
type JetStreamMsgHandler func(ctx context.Context, msg jetstream.Msg)

// JetStream is a tracing-aware JetStream context. Obtain one from Conn.JetStream.
type JetStream interface {
	// Publish publishes data to subject. The active span context in ctx is
	// injected into the message headers. opts are forwarded verbatim — in
	// particular jetstream.WithMsgID(id) drives JetStream's server-side
	// deduplication and must be passed through.
	Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)
	// PublishMsg is the *nats.Msg variant of Publish (use it to set headers).
	PublishMsg(ctx context.Context, msg *natsgo.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)

	// CreateOrUpdateStream is idempotent: safe to call on every startup.
	CreateOrUpdateStream(ctx context.Context, cfg jetstream.StreamConfig) (Stream, error)
	// Stream looks up an existing stream by name.
	Stream(ctx context.Context, name string) (Stream, error)
	// DeleteStream removes a stream and all its messages.
	DeleteStream(ctx context.Context, name string) error

	// CreateOrUpdateConsumer is idempotent: restarting resumes from the last
	// acknowledged message rather than reprocessing.
	CreateOrUpdateConsumer(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) (Consumer, error)
	// Consumer looks up an existing consumer on stream by name.
	Consumer(ctx context.Context, stream, name string) (Consumer, error)
	// DeleteConsumer removes a consumer.
	DeleteConsumer(ctx context.Context, stream, name string) error
}

// Stream is a tracing-aware handle to a JetStream stream.
type Stream interface {
	Info(ctx context.Context, opts ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error)
	CachedInfo() *jetstream.StreamInfo
	CreateOrUpdateConsumer(ctx context.Context, cfg jetstream.ConsumerConfig) (Consumer, error)
	Consumer(ctx context.Context, name string) (Consumer, error)
	DeleteConsumer(ctx context.Context, name string) error
}

// Consumer is a tracing-aware handle to a JetStream pull consumer.
type Consumer interface {
	// Consume continuously delivers messages to handler on a background
	// goroutine. Call Stop on the returned ConsumeContext to stop.
	//
	// Note: the returned ConsumeContext exposes only Stop (an upstream
	// limitation — see ConsumeContext). If you need drain-and-wait graceful
	// shutdown, use Messages instead, whose MessagesContext also offers Drain.
	Consume(handler JetStreamMsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error)
	// Messages returns a pull iterator. Each Next yields a ctx carrying the
	// consumer span plus the native jetstream.Msg.
	Messages(opts ...jetstream.PullMessagesOpt) (MessagesContext, error)
	// Next fetches a single message, blocking until one is available, the
	// fetch times out, or ctx is done. The returned ctx carries the consumer
	// span for that message.
	Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error)
	Info(ctx context.Context) (*jetstream.ConsumerInfo, error)
	CachedInfo() *jetstream.ConsumerInfo
}

// ConsumeContext controls a Consume call. It exposes only Stop because the
// upstream oteljetstream layer narrows the native jetstream.ConsumeContext
// (Stop/Drain/Closed) down to Stop. For drain-and-wait shutdown, prefer
// Messages, whose MessagesContext exposes both Stop and Drain.
type ConsumeContext interface {
	Stop()
}

// MessagesContext is the pull iterator returned by Consumer.Messages. Next
// yields a per-message ctx (carrying the consumer span) and the native
// jetstream.Msg. Stop halts immediately; Drain stops new pulls but lets
// already-buffered messages be returned by Next before it reports completion.
type MessagesContext interface {
	Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error)
	Stop()
	Drain()
}

// JetStream returns a tracing-aware JetStream context backed by this connection.
// It inherits the TracerProvider and Propagator wired into the Conn, so publish
// and consume operations propagate trace context with no extra configuration.
//
// The returned types are o11y-owned (see the JetStream / Stream / Consumer
// interfaces above); callers never import the upstream oteljetstream package.
func (c *Conn) JetStream() (JetStream, error) {
	js, err := oteljetstream.New(c.Conn)
	if err != nil {
		return nil, fmt.Errorf("nats jetstream: %w", err)
	}
	return &jetStream{js: js}, nil
}

// --- implementations: thin wrappers over the oteljetstream types ---

type jetStream struct{ js oteljetstream.JetStream }

func (j *jetStream) Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return j.js.Publish(ctx, subject, data, opts...)
}

func (j *jetStream) PublishMsg(ctx context.Context, msg *natsgo.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return j.js.PublishMsg(ctx, msg, opts...)
}

func (j *jetStream) CreateOrUpdateStream(ctx context.Context, cfg jetstream.StreamConfig) (Stream, error) {
	s, err := j.js.CreateOrUpdateStream(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &stream{s: s}, nil
}

func (j *jetStream) Stream(ctx context.Context, name string) (Stream, error) {
	s, err := j.js.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	return &stream{s: s}, nil
}

func (j *jetStream) DeleteStream(ctx context.Context, name string) error {
	return j.js.DeleteStream(ctx, name)
}

func (j *jetStream) CreateOrUpdateConsumer(ctx context.Context, streamName string, cfg jetstream.ConsumerConfig) (Consumer, error) {
	c, err := j.js.CreateOrUpdateConsumer(ctx, streamName, cfg)
	if err != nil {
		return nil, err
	}
	return &consumer{c: c}, nil
}

func (j *jetStream) Consumer(ctx context.Context, streamName, name string) (Consumer, error) {
	c, err := j.js.Consumer(ctx, streamName, name)
	if err != nil {
		return nil, err
	}
	return &consumer{c: c}, nil
}

func (j *jetStream) DeleteConsumer(ctx context.Context, streamName, name string) error {
	return j.js.DeleteConsumer(ctx, streamName, name)
}

type stream struct{ s oteljetstream.Stream }

func (s *stream) Info(ctx context.Context, opts ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	return s.s.Info(ctx, opts...)
}

func (s *stream) CachedInfo() *jetstream.StreamInfo { return s.s.CachedInfo() }

func (s *stream) CreateOrUpdateConsumer(ctx context.Context, cfg jetstream.ConsumerConfig) (Consumer, error) {
	c, err := s.s.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &consumer{c: c}, nil
}

func (s *stream) Consumer(ctx context.Context, name string) (Consumer, error) {
	c, err := s.s.Consumer(ctx, name)
	if err != nil {
		return nil, err
	}
	return &consumer{c: c}, nil
}

func (s *stream) DeleteConsumer(ctx context.Context, name string) error {
	return s.s.DeleteConsumer(ctx, name)
}

type consumer struct{ c oteljetstream.Consumer }

func (c *consumer) Consume(handler JetStreamMsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error) {
	if handler == nil {
		return nil, fmt.Errorf("nats jetstream consume: handler must not be nil")
	}
	return c.c.Consume(func(m oteljetstream.Msg) {
		handler(m.Context(), m.Msg)
	}, opts...)
}

func (c *consumer) Messages(opts ...jetstream.PullMessagesOpt) (MessagesContext, error) {
	mc, err := c.c.Messages(opts...)
	if err != nil {
		return nil, err
	}
	return &messagesContext{mc: mc}, nil
}

func (c *consumer) Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error) {
	return c.c.Next(ctx, opts...)
}

func (c *consumer) Info(ctx context.Context) (*jetstream.ConsumerInfo, error) { return c.c.Info(ctx) }

func (c *consumer) CachedInfo() *jetstream.ConsumerInfo { return c.c.CachedInfo() }

type messagesContext struct{ mc oteljetstream.MessagesContext }

func (m *messagesContext) Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error) {
	return m.mc.Next(opts...)
}

func (m *messagesContext) Stop()  { m.mc.Stop() }
func (m *messagesContext) Drain() { m.mc.Drain() }
