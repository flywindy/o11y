package nats

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/akira-core/instrumentation-go/otel-nats/oteljetstream"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// This file is the o11y-owned JetStream facade (ADR 0022 Phase 2). It wraps the
// upstream oteljetstream types in o11y interfaces so callers import only
// github.com/flywindy/o11y/nats, never the upstream oteljetstream package,
// while still getting its trace propagation and consumer span-links for free.
//
// Configuration types (StreamConfig, ConsumerConfig, AckExplicitPolicy, PubAck,
// PullConsumeOpt, PullMessagesOpt, NextOpt, FetchOpt, PublishOpt, …) are taken
// directly from github.com/nats-io/nats.go/jetstream — they are plain stdlib
// types, not an upstream-instrumentation dependency.
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
// Header-casing note: the per-message trace-context extraction above is
// performed entirely inside the upstream oteljetstream package, using its own
// otelnats.HeaderCarrier. Since otel-nats v0.7.0 that carrier matches the
// verbatim key first and then falls back to a MIME-canonical and a
// case-folded lookup, a message whose trace header is stored under a
// canonicalized key (e.g. "Traceparent" rather than the literal lowercase
// "traceparent" this SDK's own Publish writes) — written by any producer that
// canonicalizes — is still linked to its producer's trace when consumed via
// Consume/Messages/Fetch/FetchBytes/FetchNoWait. This resolves the header-
// casing gap that earlier versions documented here (ADR 0022 amendment,
// fixed upstream in v0.7.0).
//
// Scope: this facade wraps the JetStream surface o11y consumers use today
// (stream/consumer management, Publish, the pull consume modes Consume /
// Messages / Fetch / FetchBytes / FetchNoWait, and single-message Next —
// wrappable since otel-nats v0.6.0 fixed Next to return the receive-span
// context). Deferred until a consumer needs them: PushConsumer, ordered
// consumers, and the admin surface (Pause/Resume/List/Unpin). Wrapped on
// demand later.

// JetStreamMsgHandler is the Consume callback signature. ctx carries the
// consumer span and baggage extracted from the message headers, so
// slog.InfoContext(ctx, ...) and tracer.Start(ctx, ...) inside the handler are
// correlated with the producer's trace and retain application baggage. msg is
// the native jetstream.Msg, so
// Ack / Nak / Term / InProgress / Metadata are available directly.
type JetStreamMsgHandler func(ctx context.Context, msg jetstream.Msg)

// FetchedMessage pairs a message delivered through Consumer.Fetch /
// FetchBytes / FetchNoWait with the consumer-span ctx and restored baggage from
// its headers, mirroring the (ctx, msg) shape Consume and Messages already
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
// best-effort fixed size. Callers that abandon a FetchBytes batch early
// should call Stop: it releases this facade's forwarding goroutine and the
// upstream one (otel-nats v0.6.0 added the matching upstream escape hatch),
// discarding undelivered messages. Stop is idempotent and safe to call after
// the channel has closed.
//
// Each FetchedMessage's receive span (m.Ctx) is already ended by the time your
// loop body runs: since otel-nats v0.7.0 the upstream oteljetstream library
// ends every message's receive span BEFORE handing it to the channel (a
// deterministic, spec-compliant handover — IsRecording() == false at delivery,
// matching single-shot Consumer.Next), rather than the earlier
// end-when-N+1-is-read behavior. So trace.SpanFromContext(m.Ctx).
// SetAttributes(...) is a silent no-op for every delivered message, not just
// most of a multi-message batch. Two things are unaffected by this and
// remain safe: obs.Logger.*Context(m.Ctx, ...) log correlation (it only reads
// the immutable trace/span IDs off ctx, not the span's recording state), and
// starting your own child span via tracer.Start(m.Ctx, ...) for per-message
// work — see examples/jetstream/fetch-worker, which uses exactly that
// pattern rather than enriching the receive span directly.
type MessageBatch interface {
	Messages() <-chan FetchedMessage
	// Error returns the batch's terminal error after the Messages channel
	// closes, matching the native jetstream.MessageBatch contract. A batch
	// abandoned via Stop reports no error for the fetch-context cancellation
	// Stop itself triggers (that is expected teardown, not a fetch failure);
	// a context.Canceled originating from the caller's own ctx, without a
	// Stop, is still surfaced.
	Error() error
	// Stop abandons the batch: undelivered messages are discarded and the
	// Messages channel closes once any in-flight send completes. It releases
	// both this facade's forwarding goroutine and — by cancelling the native
	// pull's fetch context — the upstream one and its NATS pull subscription,
	// so an abandoned still-waiting batch is not left parked until the pull
	// expires. The upstream release is immediate for Fetch/FetchBytes calls
	// whose fetch context was wired (the default); when the caller supplied
	// their own jetstream.FetchMaxWait the fetch context is not wired, so the
	// upstream leg instead unwinds when that FetchMaxWait elapses. Reading the
	// channel to exhaustion instead of calling Stop remains fully supported.
	Stop()
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
	// downstream and does NOT stop a running loop — use the returned
	// ConsumeContext (Stop for immediate teardown, Drain for drain-and-wait,
	// Closed to await completion). Per-message trace context arrives via the
	// handler's ctx argument, extracted from the message headers.
	Consume(ctx context.Context, handler JetStreamMsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error)
	// Messages returns a pull iterator. Each Next yields a ctx carrying the
	// consumer span and restored baggage plus the native jetstream.Msg. ctx is a registration-time
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
	// in the native API. Any other invalid combination (e.g. a ctx deadline
	// too tight for an explicit jetstream.FetchHeartbeat) is returned as an
	// error rather than silently falling back to an unbounded fetch. Passing
	// your own jetstream.FetchContext in opts is unsupported and not
	// detected: the native API has no double-set guard for that option (only
	// for FetchMaxWait), so it silently overrides this ctx parameter's
	// cancellation wiring with no error — pass the ctx you want to govern
	// cancellation as this method's ctx argument, not as an opt.
	Fetch(ctx context.Context, batch int, opts ...jetstream.FetchOpt) (MessageBatch, error)
	// FetchBytes is the byte-budgeted variant of Fetch: the server stops
	// delivering once maxBytes is reached rather than once batch messages
	// have arrived. ctx is plumbed into the native call exactly as for Fetch.
	FetchBytes(ctx context.Context, maxBytes int, opts ...jetstream.FetchOpt) (MessageBatch, error)
	// FetchNoWait requests up to batch currently-available messages and
	// returns without waiting for more to arrive (no jetstream.FetchOpt —
	// matches the native jetstream.Consumer.FetchNoWait signature).
	FetchNoWait(ctx context.Context, batch int) (MessageBatch, error)
	// Next fetches a single message. The returned ctx carries the consumer
	// receive span (linked to the producer's trace when the message headers
	// carry one), matching the (ctx, msg) shape of Messages().Next — the
	// receive span is already ended by the time Next returns, so start your
	// own child span via tracer.Start(ctx, ...) for per-message work rather
	// than enriching the receive span.
	//
	// ctx semantics (otel-nats v0.7.0): a genuinely cancelable ctx
	// (WithCancel/WithTimeout/WithDeadline) is wired into the native pull
	// via jetstream.FetchContext, so BOTH a ctx deadline AND live
	// cancellation abort an in-flight Next promptly — cancelling a
	// deadline-less ctx mid-wait now unblocks it, unlike earlier versions
	// (which only checked ctx before the native call). A non-cancelable ctx
	// (context.Background/TODO, whose Done channel never fires) skips that
	// wiring, so a caller-supplied jetstream.FetchMaxWait opt still bounds
	// the wait. Passing a cancelable ctx AND your own jetstream.FetchMaxWait
	// is rejected upstream with jetstream.ErrInvalidOption (the two are
	// mutually exclusive in the native API) — express the bound via the ctx
	// deadline instead of a separate FetchMaxWait opt when you also need
	// cancellation.
	Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error)
	Info(ctx context.Context) (*jetstream.ConsumerInfo, error)
	CachedInfo() *jetstream.ConsumerInfo
}

// ConsumeContext controls a Consume call. Since otel-nats v0.6.0 the
// upstream layer mirrors the native jetstream.ConsumeContext in full, so all
// three teardown primitives are available: Stop halts immediately and
// discards buffered messages; Drain stops new pulls but lets already-buffered
// messages be handled before shutdown completes; Closed returns a channel
// that closes once consuming has fully stopped or drained.
type ConsumeContext interface {
	Stop()
	Drain()
	Closed() <-chan struct{}
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
	return &jetStream{js: js, policy: c.policy}, nil
}

// --- implementations: thin wrappers over the oteljetstream types ---

type jetStream struct {
	js     oteljetstream.JetStream
	policy propagationPolicy
}

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
	return &stream{s: s, policy: j.policy}, nil
}

func (j *jetStream) Stream(ctx context.Context, name string) (Stream, error) {
	s, err := j.js.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	return &stream{s: s, policy: j.policy}, nil
}

func (j *jetStream) DeleteStream(ctx context.Context, name string) error {
	return j.js.DeleteStream(ctx, name)
}

func (j *jetStream) CreateOrUpdateConsumer(ctx context.Context, streamName string, cfg jetstream.ConsumerConfig) (Consumer, error) {
	c, err := j.js.CreateOrUpdateConsumer(ctx, streamName, cfg)
	if err != nil {
		return nil, err
	}
	return &consumer{c: c, policy: j.policy}, nil
}

func (j *jetStream) Consumer(ctx context.Context, streamName, name string) (Consumer, error) {
	c, err := j.js.Consumer(ctx, streamName, name)
	if err != nil {
		return nil, err
	}
	return &consumer{c: c, policy: j.policy}, nil
}

func (j *jetStream) DeleteConsumer(ctx context.Context, streamName, name string) error {
	return j.js.DeleteConsumer(ctx, streamName, name)
}

type stream struct {
	s      oteljetstream.Stream
	policy propagationPolicy
}

func (s *stream) Info(ctx context.Context, opts ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	return s.s.Info(ctx, opts...)
}

func (s *stream) CachedInfo() *jetstream.StreamInfo { return s.s.CachedInfo() }

func (s *stream) CreateOrUpdateConsumer(ctx context.Context, cfg jetstream.ConsumerConfig) (Consumer, error) {
	c, err := s.s.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &consumer{c: c, policy: s.policy}, nil
}

func (s *stream) Consumer(ctx context.Context, name string) (Consumer, error) {
	c, err := s.s.Consumer(ctx, name)
	if err != nil {
		return nil, err
	}
	return &consumer{c: c, policy: s.policy}, nil
}

func (s *stream) DeleteConsumer(ctx context.Context, name string) error {
	return s.s.DeleteConsumer(ctx, name)
}

type consumer struct {
	c      oteljetstream.Consumer
	policy propagationPolicy
}

func (c *consumer) Consume(ctx context.Context, handler JetStreamMsgHandler, opts ...jetstream.PullConsumeOpt) (ConsumeContext, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats jetstream consume: %w", err)
	}
	if handler == nil {
		return nil, fmt.Errorf("nats jetstream consume: handler must not be nil")
	}
	cc, err := c.c.Consume(func(m oteljetstream.Msg) {
		handler(c.policy.restoreBaggage(m.Context(), m.Msg.Headers()), m.Msg)
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
	return &messagesContext{mc: mc, policy: c.policy}, nil
}

func (c *consumer) Fetch(ctx context.Context, batch int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats jetstream fetch: %w", err)
	}
	// Derive a cancelable fetch ctx so Stop can abort the in-flight native
	// pull, not just the facade's own forwarding goroutine — see
	// wrapMessageBatch and messageBatch.Stop.
	fetchCtx, cancel := context.WithCancel(ctx)
	mb, err := fetchWithCtxFallback(fetchCtx, opts, func(o []jetstream.FetchOpt) (oteljetstream.MessageBatch, error) {
		return c.c.Fetch(batch, o...)
	})
	if err != nil {
		cancel()
		return nil, err
	}
	// batch is Fetch's own hard cap on message count, so a same-sized buffer
	// guarantees the forwarding goroutine below can drain the entire batch
	// without blocking, even if the caller never reads Messages() at all —
	// see wrapMessageBatch.
	return wrapMessageBatch(mb, batch, cancel, c.policy), nil
}

func (c *consumer) FetchBytes(ctx context.Context, maxBytes int, opts ...jetstream.FetchOpt) (MessageBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats jetstream fetch-bytes: %w", err)
	}
	fetchCtx, cancel := context.WithCancel(ctx)
	mb, err := fetchWithCtxFallback(fetchCtx, opts, func(o []jetstream.FetchOpt) (oteljetstream.MessageBatch, error) {
		return c.c.FetchBytes(maxBytes, o...)
	})
	if err != nil {
		cancel()
		return nil, err
	}
	// FetchBytes has no message-count cap (only a byte budget, which a
	// single small message could satisfy many times over), so unlike Fetch
	// there is no buffer size that provably fits the whole batch — see the
	// fetchBytesBatchBuf doc comment.
	return wrapMessageBatch(mb, fetchBytesBatchBuf, cancel, c.policy), nil
}

// fetchWithCtxFallback calls call with jetstream.FetchContext(ctx) prepended
// to opts, so Fetch/FetchBytes actually honor ctx cancellation and deadlines
// mid-fetch instead of only checking ctx.Err() once up front (see the
// package doc comment for how that differs from Consume/Messages/
// FetchNoWait, which have no native mechanism to plumb ctx into at all).
//
// The native API rejects combining FetchContext with an explicit
// FetchMaxWait outright (regardless of option order — jetstream.pullConsumer
// checks for both being set only after applying every opt), so when the
// caller already supplied their own FetchMaxWait, call is retried with opts
// unchanged (no FetchContext), deferring to that FetchMaxWait exactly as
// before this change. The retry is scoped precisely to that collision via
// isFetchMaxWaitCollision: jetstream.ErrInvalidOption also covers unrelated
// rejections (e.g. a ctx-derived expiry too small for an explicit
// FetchHeartbeat, or a ctx whose deadline has already passed), and blindly
// retrying on any of those would silently drop the caller's real
// cancellation/deadline instead of surfacing the actual problem — so those
// are returned to the caller as-is.
//
// Not defended against: opts also containing the caller's own
// jetstream.FetchContext. fetchOptsWithCtx always applies this package's
// FetchContext(ctx) first, but unlike FetchMaxWait, the native pullRequest
// has no "already set" guard for FetchContext — a later FetchContext in opts
// just overwrites req.ctx with no error, silently taking over cancellation
// from the ctx parameter Fetch/FetchBytes actually document as authoritative.
// Callers should pass the ctx they want to govern cancellation as Fetch's/
// FetchBytes' own ctx argument, never as a FetchContext opt.
func fetchWithCtxFallback(ctx context.Context, opts []jetstream.FetchOpt, call func([]jetstream.FetchOpt) (oteljetstream.MessageBatch, error)) (oteljetstream.MessageBatch, error) {
	mb, err := call(fetchOptsWithCtx(ctx, opts))
	if isFetchMaxWaitCollision(err) {
		mb, err = call(opts)
	}
	return mb, err
}

func fetchOptsWithCtx(ctx context.Context, opts []jetstream.FetchOpt) []jetstream.FetchOpt {
	return append([]jetstream.FetchOpt{jetstream.FetchContext(ctx)}, opts...)
}

// isFetchMaxWaitCollision reports whether err is specifically nats.go's
// rejection of combining FetchContext with an explicit FetchMaxWait in the
// same call ("cannot specify both FetchContext and FetchMaxWait"), the one
// jetstream.ErrInvalidOption case fetchWithCtxFallback should retry around.
// Matching the message, not just the errors.Is(err, jetstream.ErrInvalidOption)
// sentinel, is deliberate: that sentinel is shared by several unrelated
// rejections (heartbeat/expiry mismatch, an already-expired ctx deadline,
// invalid priority-group settings, ...), and only this specific one means
// "the caller's own FetchMaxWait should win, ctx is still fine to drop."
func isFetchMaxWaitCollision(err error) bool {
	return errors.Is(err, jetstream.ErrInvalidOption) &&
		strings.Contains(err.Error(), "cannot specify both FetchContext and FetchMaxWait")
}

func (c *consumer) FetchNoWait(ctx context.Context, batch int) (MessageBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("nats jetstream fetch-no-wait: %w", err)
	}
	mb, err := c.c.FetchNoWait(batch)
	if err != nil {
		return nil, err
	}
	// FetchNoWait takes no FetchOpt, so there is no cancelable fetch ctx to
	// wire — but it never blocks waiting for new messages (it returns only
	// what is already buffered), so the upstream forwarding goroutine drains
	// and exits on its own without a pull to abort. A no-op cancel keeps the
	// wrapMessageBatch contract uniform.
	// Same reasoning as Fetch: batch is a hard cap here too.
	return wrapMessageBatch(mb, batch, func() {}, c.policy), nil
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
// FetchedMessage), preserving each consumer span while restoring baggage with
// the connection's configured propagation policy.
//
// out is buffered to bufSize so the forwarding goroutine can drain the
// upstream channel into the buffer and exit cleanly on its own, rather than
// blocking forever on a send once the caller stops reading Messages() —
// see the MessageBatch doc comment for the goroutine-leak this closes (fully,
// when bufSize is a true upper bound on message count; best-effort
// otherwise).
//
// cancel aborts the native pull that backs this batch. Stop invokes it so an
// abandoned, still-waiting Fetch/FetchBytes batch does not leave the upstream
// forwarding goroutine (and its NATS pull subscription) parked until the pull
// expires: cancelling the fetch ctx closes the native message channel, which
// drains upstream. It is also invoked when this goroutine exits on its own
// (normal drain) so the fetch ctx is not leaked. cancel is a no-op on the
// FetchMaxWait-collision path (where the fetch ctx was not wired) and for
// FetchNoWait (which never blocks); Stop still releases this facade's
// goroutine via done in those cases.
func wrapMessageBatch(mb oteljetstream.MessageBatch, bufSize int, cancel context.CancelFunc, policy propagationPolicy) MessageBatch {
	out := make(chan FetchedMessage, bufSize)
	done := make(chan struct{})
	go func() {
		defer close(out)
		defer cancel()
		for {
			// Select on the upstream receive as well as the send: a batch
			// still waiting for messages parks this goroutine on the receive,
			// and Stop must release it (and close out) promptly from there
			// too, not only when it is blocked on a full out buffer.
			select {
			case m, ok := <-mb.Messages():
				if !ok {
					return
				}
				select {
				case out <- FetchedMessage{Ctx: policy.restoreBaggage(m.Ctx, m.Msg.Headers()), Msg: m.Msg}:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()
	return &messageBatch{ch: out, mb: mb, done: done, cancel: cancel}
}

type messageBatch struct {
	ch       chan FetchedMessage
	mb       oteljetstream.MessageBatch
	done     chan struct{}
	cancel   context.CancelFunc
	stopOnce sync.Once
}

func (m *messageBatch) Messages() <-chan FetchedMessage { return m.ch }

func (m *messageBatch) Error() error {
	err := m.mb.Error()
	// Stop cancels the fetch ctx, which the native pull surfaces as
	// context.Canceled. After an explicit Stop that is expected teardown, not
	// a batch failure, so it is not reported. A context.Canceled seen without
	// a Stop (the caller's own ctx was canceled) is a real terminal condition
	// and is returned unchanged.
	if err != nil && errors.Is(err, context.Canceled) {
		select {
		case <-m.done:
			return nil
		default:
		}
	}
	return err
}

func (m *messageBatch) Stop() {
	m.stopOnce.Do(func() {
		close(m.done)
		// Abort the native pull first so the upstream forwarding goroutine's
		// blocking receive on raw.Messages() unblocks (its channel closes),
		// then call the upstream Stop to close its own done channel.
		m.cancel()
		m.mb.Stop()
	})
}

func (c *consumer) Next(ctx context.Context, opts ...jetstream.FetchOpt) (context.Context, jetstream.Msg, error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, fmt.Errorf("nats jetstream next: %w", err)
	}
	// Never return a nil context: the upstream Next returns (nil, nil, err) on
	// failure, which risks nil-dereference in callers that touch ctx before
	// checking err. Substitute the caller's own ctx on the error path.
	msgCtx, msg, err := c.c.Next(ctx, opts...)
	if err != nil {
		return ctx, nil, err
	}
	return c.policy.restoreBaggage(msgCtx, msg.Headers()), msg, nil
}

func (c *consumer) Info(ctx context.Context) (*jetstream.ConsumerInfo, error) { return c.c.Info(ctx) }

func (c *consumer) CachedInfo() *jetstream.ConsumerInfo { return c.c.CachedInfo() }

type messagesContext struct {
	mc     oteljetstream.MessagesContext
	policy propagationPolicy
}

func (m *messagesContext) Next(opts ...jetstream.NextOpt) (context.Context, jetstream.Msg, error) {
	ctx, msg, err := m.mc.Next(opts...)
	if err != nil {
		return ctx, msg, err
	}
	return m.policy.restoreBaggage(ctx, msg.Headers()), msg, nil
}

func (m *messagesContext) Stop()  { m.mc.Stop() }
func (m *messagesContext) Drain() { m.mc.Drain() }
