package nats

import (
	"context"
	"errors"
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
// The Consume callback and the Messages iterator deliver the native
// jetstream.Msg together with a ctx carrying the consumer span (matching the
// core MsgHandler shape (ctx, msg)). Fetch / FetchBytes / FetchNoWait deliver
// the same (ctx, msg) pairing over a channel, via the o11y-owned
// FetchedMessage type — required because a channel, unlike a callback or
// iterator method, cannot return two separate values per delivery.
//
// Consume, Messages, and Fetch/FetchBytes/FetchNoWait also take a ctx. For
// Consume and Messages it is a registration-time guard only, consistent with
// the core Subscribe / QueueSubscribe facade: checked once up front (an
// already-cancelled ctx is rejected) but NOT plumbed into the upstream call —
// it does NOT cancel a running consume — use ConsumeContext.Stop /
// MessagesContext.Stop|Drain for that. Fetch and FetchBytes go further: ctx
// is also passed to the native call via jetstream.FetchContext, so cancelling
// or timing out ctx after the call returns actually cancels the in-flight
// pull and closes the batch channel early (falling back to the caller's own
// FetchMaxWait, unmodified, when one is explicitly set — the two are mutually
// exclusive upstream). FetchNoWait takes no FetchOpt in the native API, so
// there is nothing to plumb ctx into; it remains a registration-time guard
// only. Per-message trace context flows from the message headers, not from
// this registration ctx (see ADR 0022 amendment).
//
// Scope: this facade wraps the JetStream surface o11y consumers use today
// (stream/consumer management, Publish, and the pull consume modes Consume /
// Messages / Fetch / FetchBytes / FetchNoWait). Deferred until a consumer
// needs them: single-message Consumer.Next (upstream v0.2.11 returns the
// producer's remote context rather than the local receive span — use
// Messages with jetstream.PullMaxMessages(1) for single fetch), PushConsumer,
// ordered consumers, and the admin surface (Pause/Resume/List/Unpin). Wrapped
// on demand later.

// JetStreamMsgHandler is the Consume callback signature. ctx carries the
// consumer span extracted from the message headers by the upstream layer, so
// slog.InfoContext(ctx, ...) and tracer.Start(ctx, ...) inside the handler are
// correlated with the producer's trace. msg is the native jetstream.Msg, so
// Ack / Nak / Term / InProgress / Metadata are available directly.
type JetStreamMsgHandler func(ctx context.Context, msg jetstream.Msg)

// FetchedMessage pairs a message delivered through Consumer.Fetch /
// FetchBytes / FetchNoWait with the consumer-span ctx extracted from its
// headers, mirroring the (ctx, msg) shape Consume and Messages already
// deliver. An o11y-owned type is needed here (rather than re-exporting the
// upstream oteljetstream.Msg) because the batch is delivered over a channel,
// not a callback/iterator method — see ADR 0022 amendment (2026-07-01).
type FetchedMessage struct {
	Ctx context.Context
	Msg jetstream.Msg
}

// MessageBatch is the result of Consumer.Fetch / FetchBytes / FetchNoWait.
// Messages yields each delivered message paired with its consumer-span ctx;
// range over the channel until it closes, then call Error for the terminal
// batch error (matching the native jetstream.MessageBatch contract).
//
// Each message is forwarded onto the channel by a background goroutine, into
// a buffer sized to the request's own message-count bound where one exists.
// For Fetch and FetchNoWait, batch is that bound, so the goroutine can always
// drain the entire batch into the buffer and exit cleanly — abandoning
// Messages() before reading everything is safe, no goroutine or message is
// leaked. FetchBytes has no message-count bound (only a byte budget, which
// an unbounded number of small messages could satisfy), so its buffer is a
// best-effort fixed size: a batch with more messages than that can still
// leave the forwarding goroutine blocked on a send with nothing left to
// receive it, until the caller resumes reading or the process exits — there
// is no Stop/cancel escape hatch on MessageBatch. Prefer Fetch/FetchNoWait
// over FetchBytes when early abandonment is a realistic caller pattern.
type MessageBatch interface {
	Messages() <-chan FetchedMessage
	Error() error
}

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
	// ctx is a registration-time guard only (consistent with Subscribe): an
	// already-cancelled ctx is rejected up front, but ctx is not plumbed
	// downstream and does NOT stop a running loop — use ConsumeContext.Stop for
	// that. Per-message trace context arrives via the handler's ctx argument,
	// extracted from the message headers.
	//
	// Note: the returned ConsumeContext exposes only Stop (an upstream
	// limitation — see ConsumeContext). If you need drain-and-wait graceful
	// shutdown, use Messages instead, whose MessagesContext also offers Drain.
	Consume(ctx context.Context, handler JetStreamMsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error)
	// Messages returns a pull iterator. Each Next yields a ctx carrying the
	// consumer span plus the native jetstream.Msg. ctx is a registration-time
	// guard only, with the same semantics as Consume's ctx.
	Messages(ctx context.Context, opts ...jetstream.PullMessagesOpt) (MessagesContext, error)
	// Fetch requests up to batch messages and returns immediately with a
	// MessageBatch; messages (each paired with its consumer-span ctx) arrive
	// on the returned channel as the server delivers them. Unlike Consume /
	// Messages, ctx is plumbed into the native pull request (via
	// jetstream.FetchContext): cancelling or timing out ctx after Fetch
	// returns ends the in-flight pull early and closes the batch channel,
	// rather than only being checked once up front. If opts already contains
	// jetstream.FetchMaxWait, that takes precedence and ctx reverts to a
	// registration-time guard only — the two options are mutually exclusive
	// in the native API.
	Fetch(ctx context.Context, batch int, opts ...jetstream.FetchOpt) (MessageBatch, error)
	// FetchBytes is the byte-budgeted variant of Fetch: the server stops
	// delivering once maxBytes is reached rather than once batch messages
	// have arrived. ctx is plumbed into the native call exactly as for Fetch.
	FetchBytes(ctx context.Context, maxBytes int, opts ...jetstream.FetchOpt) (MessageBatch, error)
	// FetchNoWait requests up to batch currently-available messages and
	// returns without waiting for more to arrive (no jetstream.FetchOpt —
	// matches the native jetstream.Consumer.FetchNoWait signature).
	FetchNoWait(ctx context.Context, batch int) (MessageBatch, error)
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

func (c *consumer) Consume(ctx context.Context, handler JetStreamMsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats jetstream consume: %w", err)
	}
	if handler == nil {
		return nil, fmt.Errorf("nats jetstream consume: handler must not be nil")
	}
	cc, err := c.c.Consume(func(m oteljetstream.Msg) {
		handler(m.Context(), m.Msg)
	}, opts...)
	if err != nil {
		return nil, err
	}
	return cc, nil
}

func (c *consumer) Messages(ctx context.Context, opts ...jetstream.PullMessagesOpt) (MessagesContext, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats jetstream messages: %w", err)
	}
	mc, err := c.c.Messages(opts...)
	if err != nil {
		return nil, err
	}
	return &messagesContext{mc: mc}, nil
}

func (c *consumer) Fetch(ctx context.Context, batch int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats jetstream fetch: %w", err)
	}
	mb, err := c.c.Fetch(batch, fetchOptsWithCtx(ctx, opts)...)
	if errors.Is(err, jetstream.ErrInvalidOption) {
		// The native API rejects combining FetchContext with an explicit
		// FetchMaxWait (see fetchOptsWithCtx) — the caller supplied their
		// own FetchMaxWait, so defer to it exactly as before rather than
		// erroring out because of our own ctx wiring.
		mb, err = c.c.Fetch(batch, opts...)
	}
	if err != nil {
		return nil, err
	}
	// batch is Fetch's own hard cap on message count, so a same-sized buffer
	// guarantees the forwarding goroutine below can drain the entire batch
	// without blocking, even if the caller never reads Messages() at all —
	// see wrapMessageBatch.
	return wrapMessageBatch(mb, batch), nil
}

func (c *consumer) FetchBytes(ctx context.Context, maxBytes int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats jetstream fetch-bytes: %w", err)
	}
	mb, err := c.c.FetchBytes(maxBytes, fetchOptsWithCtx(ctx, opts)...)
	if errors.Is(err, jetstream.ErrInvalidOption) {
		mb, err = c.c.FetchBytes(maxBytes, opts...)
	}
	if err != nil {
		return nil, err
	}
	// FetchBytes has no message-count cap (only a byte budget, which a
	// single small message could satisfy many times over), so unlike Fetch
	// there is no buffer size that provably fits the whole batch — see the
	// fetchBytesBatchBuf doc comment.
	return wrapMessageBatch(mb, fetchBytesBatchBuf), nil
}

// fetchOptsWithCtx prepends jetstream.FetchContext(ctx) to opts so Fetch /
// FetchBytes actually honor ctx cancellation and deadlines mid-fetch,
// instead of only checking ctx.Err() once up front (see the package doc
// comment for how that differs from Consume/Messages/FetchNoWait, which have
// no native mechanism to plumb ctx into at all).
//
// The native API rejects combining FetchContext with an explicit
// FetchMaxWait outright (regardless of option order — jetstream.pullConsumer
// checks for both being set only after applying every opt), so this alone
// isn't sufficient: each call site above must retry without this injection
// (falling back to the caller's opts as-is) when that specific collision
// error comes back, deferring to the caller's explicit FetchMaxWait exactly
// as before this change.
func fetchOptsWithCtx(ctx context.Context, opts []jetstream.FetchOpt) []jetstream.FetchOpt {
	return append([]jetstream.FetchOpt{jetstream.FetchContext(ctx)}, opts...)
}

func (c *consumer) FetchNoWait(ctx context.Context, batch int) (MessageBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats jetstream fetch-no-wait: %w", err)
	}
	mb, err := c.c.FetchNoWait(batch)
	if err != nil {
		return nil, err
	}
	// Same reasoning as Fetch: batch is a hard cap here too.
	return wrapMessageBatch(mb, batch), nil
}

// fetchBytesBatchBuf is the best-effort forwarding buffer size for
// FetchBytes (see wrapMessageBatch): unlike Fetch/FetchNoWait, FetchBytes has
// no message-count bound to size the buffer against exactly, so this is a
// judgment call sized for the common case (many small-to-medium messages
// under a byte budget), not a guarantee. A batch with more messages than
// this can still leave the forwarding goroutine blocked if the caller
// abandons Messages() early — see the MessageBatch doc comment.
const fetchBytesBatchBuf = 256

// wrapMessageBatch adapts the upstream oteljetstream.MessageBatch (a channel
// of oteljetstream.Msg) to the o11y MessageBatch (a channel of
// FetchedMessage), forwarding each message's consumer-span ctx unchanged.
//
// out is buffered to bufSize so the forwarding goroutine can drain the
// upstream channel into the buffer and exit cleanly on its own, rather than
// blocking forever on a send once the caller stops reading Messages() —
// see the MessageBatch doc comment for the goroutine-leak this closes (fully,
// when bufSize is a true upper bound on message count; best-effort
// otherwise).
func wrapMessageBatch(mb oteljetstream.MessageBatch, bufSize int) MessageBatch {
	out := make(chan FetchedMessage, bufSize)
	go func() {
		defer close(out)
		for m := range mb.Messages() {
			out <- FetchedMessage{Ctx: m.Ctx, Msg: m.Msg}
		}
	}()
	return &messageBatch{ch: out, mb: mb}
}

type messageBatch struct {
	ch chan FetchedMessage
	mb oteljetstream.MessageBatch
}

func (m *messageBatch) Messages() <-chan FetchedMessage { return m.ch }
func (m *messageBatch) Error() error                    { return m.mb.Error() }

func (c *consumer) Info(ctx context.Context) (*jetstream.ConsumerInfo, error) { return c.c.Info(ctx) }

func (c *consumer) CachedInfo() *jetstream.ConsumerInfo { return c.c.CachedInfo() }

type messagesContext struct{ mc oteljetstream.MessagesContext }

func (m *messagesContext) Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error) {
	return m.mc.Next(opts...)
}

func (m *messagesContext) Stop()  { m.mc.Stop() }
func (m *messagesContext) Drain() { m.mc.Drain() }
