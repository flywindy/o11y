package nats_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	oteltrace "go.opentelemetry.io/otel/trace"

	o11ynats "github.com/flywindy/o11y/nats"
)

// TestJetStream_Consume_TracePropagation publishes inside a root span and
// asserts that the Consume handler receives a ctx carrying a valid consumer
// span, and that a recorded consumer span links back to the publisher's trace.
func TestJetStream_Consume_TracePropagation(t *testing.T) {
	enableNATSTracing(t)
	_, url := startJetStreamServer(t)
	tp, prop, sr := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS", "events.created", "consume-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)

	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	ctxCh := make(chan context.Context, 1)
	cc, err := cons.Consume(context.Background(), func(msgCtx context.Context, m jetstream.Msg) {
		_ = m.Ack()
		select {
		case ctxCh <- msgCtx:
		default:
		}
	})
	require.NoError(t, err)
	defer cc.Stop()

	tracer := tp.Tracer("test")
	pubCtx, span := tracer.Start(context.Background(), "publish-event")
	pubTraceID := span.SpanContext().TraceID()
	_, err = js.Publish(pubCtx, subject, []byte("hello"))
	require.NoError(t, err)
	span.End()

	var msgCtx context.Context
	select {
	case msgCtx = <-ctxCh:
	case <-time.After(3 * time.Second):
		t.Fatal("consumer did not receive message within timeout")
	}

	gotTraceID := oteltrace.SpanFromContext(msgCtx).SpanContext().TraceID()
	assert.True(t, gotTraceID.IsValid(), "handler ctx should carry a valid trace ID")

	// otelnats follows OTel messaging semantics: the consumer span starts a new
	// trace and links back to the producer rather than parenting under it.
	assert.Eventually(t, func() bool {
		for _, s := range sr.Ended() {
			for _, link := range s.Links() {
				if link.SpanContext.TraceID() == pubTraceID {
					return true
				}
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond,
		"a consumer span should link back to the publisher's trace")
}

// TestJetStream_Consume_DrainClosed covers the teardown surface added when
// the facade ConsumeContext widened to mirror the native interface (otel-nats
// v0.6.0): Drain lets an in-flight message finish processing, and Closed
// signals once the consume loop has fully shut down.
func TestJetStream_Consume_DrainClosed(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_DC", "events.dc.created", "drain-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	received := make(chan struct{}, 1)
	cc, err := cons.Consume(context.Background(), func(_ context.Context, m jetstream.Msg) {
		_ = m.Ack()
		select {
		case received <- struct{}{}:
		default:
		}
	})
	require.NoError(t, err)

	_, err = js.Publish(context.Background(), subject, []byte("hello"))
	require.NoError(t, err)

	// The message published before Drain must still be delivered and handled.
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("consumer did not receive message within timeout")
	}

	cc.Drain()

	// Closed must signal completion after a drain — this is the graceful
	// drain-and-wait shutdown that was impossible when the facade exposed
	// only Stop().
	select {
	case <-cc.Closed():
	case <-time.After(3 * time.Second):
		t.Fatal("ConsumeContext.Closed did not signal within timeout after Drain")
	}
}

// TestJetStream_FetchBytes_StopUnblocksWaitingBatch is the regression test
// for MessageBatch.Stop releasing a batch that is still WAITING for messages
// (empty stream, pull request open): the facade's forwarding goroutine parks
// on the upstream channel receive in that state, so Stop must be selected on
// the receive side too — without that, Messages() would not close until the
// native pull expires (tens of seconds), despite Stop's contract.
func TestJetStream_FetchBytes_StopUnblocksWaitingBatch(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_SU", "events.su.created", "stop-unblock-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	// Nothing has been published: the batch stays open waiting for messages.
	batch, err := cons.FetchBytes(context.Background(), 1024)
	require.NoError(t, err)

	// Give the forwarding goroutine a moment to park on the upstream receive,
	// so Stop is exercised against the waiting state, not the setup window.
	time.Sleep(100 * time.Millisecond)
	batch.Stop()

	select {
	case _, ok := <-batch.Messages():
		assert.False(t, ok, "Messages channel should close after Stop, not deliver")
	case <-time.After(2 * time.Second):
		t.Fatal("Messages channel did not close promptly after Stop on a waiting batch")
	}
}

// TestJetStream_Messages_TracePropagation exercises the pull-iterator path
// (the one chat uses): Messages().Next() must yield a ctx carrying the consumer
// span and the native jetstream.Msg.
func TestJetStream_Messages_TracePropagation(t *testing.T) {
	enableNATSTracing(t)
	_, url := startJetStreamServer(t)
	tp, prop, sr := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_M", "events.m.created", "messages-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	tracer := tp.Tracer("test")
	pubCtx, span := tracer.Start(context.Background(), "publish-event")
	pubTraceID := span.SpanContext().TraceID()
	_, err = js.Publish(pubCtx, subject, []byte("hi"))
	require.NoError(t, err)
	span.End()

	iter, err := cons.Messages(context.Background())
	require.NoError(t, err)
	defer iter.Stop()

	// Run Next in a goroutine so the test fails fast instead of blocking if no
	// message arrives.
	type result struct {
		ctx context.Context
		msg jetstream.Msg
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		c, m, e := iter.Next()
		resCh <- result{c, m, e}
	}()

	var res result
	select {
	case res = <-resCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Messages().Next did not return within timeout")
	}
	require.NoError(t, res.err)
	require.NotNil(t, res.msg)
	require.NoError(t, res.msg.Ack())

	gotTraceID := oteltrace.SpanFromContext(res.ctx).SpanContext().TraceID()
	assert.True(t, gotTraceID.IsValid(), "Next ctx should carry a valid trace ID")
	// The pull-iterator receive span is ended on the *next* Next() call (it
	// spans until the caller asks for the next message), so assert against
	// Started() — the link is set at span start.
	assert.Eventually(t, func() bool {
		for _, s := range sr.Started() {
			for _, link := range s.Links() {
				if link.SpanContext.TraceID() == pubTraceID {
					return true
				}
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond,
		"a consumer receive span should link back to the publisher's trace")
}

// TestJetStream_Fetch_TracePropagation exercises the batch pull path (Fetch):
// each FetchedMessage delivered on the MessageBatch channel must carry a ctx
// with a valid consumer span that links back to the publisher's trace.
func TestJetStream_Fetch_TracePropagation(t *testing.T) {
	enableNATSTracing(t)
	_, url := startJetStreamServer(t)
	tp, prop, sr := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_F", "events.f.created", "fetch-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	tracer := tp.Tracer("test")
	pubCtx, span := tracer.Start(context.Background(), "publish-event")
	pubTraceID := span.SpanContext().TraceID()
	_, err = js.Publish(pubCtx, subject, []byte("hi"))
	require.NoError(t, err)
	span.End()

	batch, err := cons.Fetch(context.Background(), 1)
	require.NoError(t, err)

	var fetched o11ynats.FetchedMessage
	var got int
	timeout := time.After(3 * time.Second)
drain:
	for {
		select {
		case m, ok := <-batch.Messages():
			if !ok {
				break drain
			}
			fetched = m
			got++
		case <-timeout:
			t.Fatal("Fetch did not deliver a message within timeout")
		}
	}
	require.Equal(t, 1, got, "Fetch(ctx, 1) should deliver exactly one message")
	require.NotNil(t, fetched.Msg)
	require.NoError(t, fetched.Msg.Ack())
	require.NoError(t, batch.Error(), "batch should complete without a terminal error")

	gotTraceID := oteltrace.SpanFromContext(fetched.Ctx).SpanContext().TraceID()
	assert.True(t, gotTraceID.IsValid(), "fetched message ctx should carry a valid trace ID")
	// Like the Messages() iterator, the batch's per-message receive span isn't
	// ended until the next message arrives or the underlying pull request
	// completes (which can take up to its expiry) — so assert against
	// Started(), where the link is already set at span creation, rather than
	// Ended().
	assert.Eventually(t, func() bool {
		for _, s := range sr.Started() {
			for _, link := range s.Links() {
				if link.SpanContext.TraceID() == pubTraceID {
					return true
				}
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond,
		"a consumer span should link back to the publisher's trace")
}

// TestJetStream_FetchBytes_Deliver is a lighter smoke test for FetchBytes:
// trace propagation is exercised in full by TestJetStream_Fetch_TracePropagation
// (all three batch modes share wrapMessageBatch), so this only asserts
// delivery and drains the batch fully. FetchMaxWait bounds the pull request
// so it closes once the byte budget's wait expires rather than staying open
// waiting for more bytes.
func TestJetStream_FetchBytes_Deliver(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_FB", "events.fb.created", "fetch-bytes-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, err = js.Publish(context.Background(), subject, []byte("a"))
	require.NoError(t, err)

	fbBatch, err := cons.FetchBytes(context.Background(), 1024, jetstream.FetchMaxWait(500*time.Millisecond))
	require.NoError(t, err)

	var got int
	timeout := time.After(3 * time.Second)
drain:
	for {
		select {
		case m, ok := <-fbBatch.Messages():
			if !ok {
				break drain
			}
			require.NoError(t, m.Msg.Ack())
			got++
		case <-timeout:
			t.Fatal("FetchBytes did not close within timeout")
		}
	}
	require.Equal(t, 1, got, "FetchBytes should deliver exactly the one published message")
	require.NoError(t, fbBatch.Error())
}

// TestJetStream_FetchNoWait_Deliver mirrors TestJetStream_FetchBytes_Deliver
// for FetchNoWait: the message is published before the call so it is already
// available server-side, matching FetchNoWait's "no waiting for new messages"
// contract.
func TestJetStream_FetchNoWait_Deliver(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_FNW", "events.fnw.created", "fetch-nowait-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	_, err = js.Publish(context.Background(), subject, []byte("a"))
	require.NoError(t, err)
	require.NoError(t, conn.NatsConn().FlushTimeout(2*time.Second))

	// The publish ack round trip above guarantees the message is committed
	// server-side before FetchNoWait asks for it.
	batch, err := cons.FetchNoWait(context.Background(), 1)
	require.NoError(t, err)

	select {
	case m, ok := <-batch.Messages():
		require.True(t, ok, "FetchNoWait should deliver the already-published message")
		require.NoError(t, m.Msg.Ack())
	case <-time.After(3 * time.Second):
		t.Fatal("FetchNoWait did not deliver within timeout")
	}
}

// TestJetStream_Fetch_ContextGuard locks down the one validation guard the
// batch wrappers add: an already-cancelled registration ctx is rejected up
// front by Fetch / FetchBytes / FetchNoWait, consistent with Consume/Messages.
func TestJetStream_Fetch_ContextGuard(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_FG", "events.fg.created", "fetch-guard-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = cons.Fetch(canceled, 1)
	require.Error(t, err)
	_, err = cons.FetchBytes(canceled, 1024)
	require.Error(t, err)
	_, err = cons.FetchNoWait(canceled, 1)
	require.Error(t, err)
}

// TestJetStream_Fetch_CtxCancellation_MidFetch locks down the fetchOptsWithCtx
// fix: Fetch plumbs ctx into the native pull request via jetstream.FetchContext,
// so cancelling ctx after Fetch has already returned ends the in-flight wait
// early instead of running to the (multi-second, in this test's case) default
// FetchMaxWait. No message is ever published, so without the fix this would
// block for the full default wait; the assertion budget is well under that.
func TestJetStream_Fetch_CtxCancellation_MidFetch(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_CTXCANCEL", "events.ctxcancel.created", "ctx-cancel-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	start := time.Now()
	batch, err := cons.Fetch(ctx, 1)
	require.NoError(t, err)

	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	select {
	case _, ok := <-batch.Messages():
		require.False(t, ok, "no message was ever published, the channel should close without delivering one")
	case <-time.After(5 * time.Second):
		t.Fatal("batch channel did not close within the cancellation budget — ctx is not reaching the native fetch")
	}
	elapsed := time.Since(start)

	assert.ErrorIs(t, batch.Error(), context.Canceled)
	assert.Less(t, elapsed, 5*time.Second, "cancelling ctx should end the fetch well under the default FetchMaxWait")
	assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond, "fetch should not resolve before ctx was actually canceled")
}

// TestJetStream_Fetch_CtxOptionCollision_NotSwallowed locks down
// isFetchMaxWaitCollision's precision: fetchWithCtxFallback must only retry
// without ctx when the native rejection is specifically the
// FetchContext+FetchMaxWait collision, not any jetstream.ErrInvalidOption.
// Pairing a short ctx deadline with an explicit FetchHeartbeat triggers a
// different native rejection ("expiry time should be at least 2 times the
// heartbeat", also ErrInvalidOption) — a blind fallback would silently drop
// ctx here and run the fetch to the 30s default wait instead of surfacing
// this error, defeating the caller's own cancellation/deadline.
func TestJetStream_Fetch_CtxOptionCollision_NotSwallowed(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_HBCOLLIDE", "events.hbcollide.created", "hb-collide-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	// A 1s ctx deadline combined with a 2s heartbeat: the ctx-derived expiry
	// (under 1s once FetchContext reserves its buffer) is under 2x the
	// heartbeat, so the native call rejects the combination outright.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	_, err = cons.Fetch(ctx, 1, jetstream.FetchHeartbeat(2*time.Second))
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, jetstream.ErrInvalidOption)
	assert.NotContains(t, err.Error(), "cannot specify both FetchContext and FetchMaxWait",
		"this should be the heartbeat/expiry rejection, not the FetchMaxWait collision")
	assert.Less(t, elapsed, 5*time.Second,
		"the error should return immediately during option validation, not after falling back to a blocking fetch")
}

// TestJetStream_Fetch_FetchMaxWaitCollision_Retries locks down the actual
// positive-path collision fetchWithCtxFallback exists for: a caller passing
// both a live ctx and their own explicit jetstream.FetchMaxWait. Injecting
// jetstream.FetchContext(ctx) unconditionally would make every such call
// fail outright with jetstream.ErrInvalidOption ("cannot specify both
// FetchContext and FetchMaxWait") — this asserts the retry recovers
// transparently instead, deferring to the caller's FetchMaxWait, against an
// empty consumer so the only thing that can end the batch is that FetchMaxWait
// actually expiring (proving it's in effect, not merely that no error leaked).
func TestJetStream_Fetch_FetchMaxWaitCollision_Retries(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_MAXWAITCOLLIDE", "events.maxwaitcollide.created", "maxwait-collide-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	// ctx is live (no deadline, never cancelled) so FetchContext(ctx) is
	// always injected first; jetstream.FetchMaxWait below is what collides
	// with it and must win.
	start := time.Now()
	batch, err := cons.Fetch(context.Background(), 1, jetstream.FetchMaxWait(300*time.Millisecond))
	require.NoError(t, err, "the FetchMaxWait collision should be retried transparently, not surfaced to the caller")

	_, ok := <-batch.Messages()
	elapsed := time.Since(start)
	require.False(t, ok, "no message was ever published, the channel should close without delivering one")
	require.NoError(t, batch.Error())

	assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond,
		"should not resolve before the caller's own FetchMaxWait")
	assert.Less(t, elapsed, 5*time.Second,
		"should resolve close to the caller's 300ms FetchMaxWait, not the 30s default")
}

// TestJetStream_Fetch_ReceiveSpanEndsBeforeConsumption documents a known
// trade-off of the goroutine-leak fix (see MessageBatch's doc comment):
// buffering the forwarding channel to bufSize lets the forwarding goroutine
// race ahead through the whole upstream batch — and upstream ends message N's
// receive span as soon as it reads message N+1 off its own channel — well
// ahead of the caller's own processing pace. So by the time a caller's loop
// gets to later messages in a batch, those receive spans are typically
// already ended, and trace.SpanFromContext(m.Ctx).SetAttributes(...) on them
// is a silent no-op; log correlation and child spans (see
// examples/jetstream/fetch-worker) are unaffected and remain the supported
// pattern. If upstream oteljetstream ever changes this span-lifecycle
// behavior, this test breaks as a prompt to update the doc comments that
// describe it.
func TestJetStream_Fetch_ReceiveSpanEndsBeforeConsumption(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_SPANLIFECYCLE", "events.spanlifecycle.created", "span-lifecycle-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	const batchSize = 5
	for i := 0; i < batchSize; i++ {
		_, err = js.Publish(context.Background(), subject, []byte("m"))
		require.NoError(t, err)
	}
	require.NoError(t, conn.NatsConn().FlushTimeout(2*time.Second))

	batch, err := cons.Fetch(context.Background(), batchSize)
	require.NoError(t, err)

	var last o11ynats.FetchedMessage
	var got int
drain:
	for {
		select {
		case m, ok := <-batch.Messages():
			if !ok {
				break drain
			}
			last = m
			got++
			// Simulate realistic per-message processing time, giving the
			// forwarding goroutine room to race ahead through the rest of
			// the already-buffered batch before this loop reads the next
			// message — the condition under which spans end early.
			time.Sleep(20 * time.Millisecond)
		case <-time.After(3 * time.Second):
			t.Fatal("Fetch did not deliver the full batch within timeout")
		}
	}
	require.Equal(t, batchSize, got)
	require.NoError(t, batch.Error())

	assert.False(t, oteltrace.SpanFromContext(last.Ctx).IsRecording(),
		"the last message's receive span should already be ended by the time this loop reads it, per the documented trade-off")
}

// TestJetStream_Fetch_NoGoroutineLeakOnEarlyAbandon locks down the
// wrapMessageBatch buffering fix: batch is Fetch's own hard cap on message
// count, so sizing the forwarding channel's buffer to batch lets the
// background goroutine drain the entire upstream batch and exit on its own,
// even when the caller reads only part of it and abandons the rest. Before
// the fix, abandoning early left that goroutine blocked forever trying to
// send the next message to nobody.
func TestJetStream_Fetch_NoGoroutineLeakOnEarlyAbandon(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_LEAK", "events.leak.created", "leak-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	const batchSize = 5
	for i := 0; i < batchSize; i++ {
		_, err = js.Publish(context.Background(), subject, []byte("m"))
		require.NoError(t, err)
	}
	require.NoError(t, conn.NatsConn().FlushTimeout(2*time.Second))

	batch, err := cons.Fetch(context.Background(), batchSize)
	require.NoError(t, err)

	// Read only the first message, then deliberately abandon the rest —
	// nobody ever reads batch.Messages() again after this.
	select {
	case m, ok := <-batch.Messages():
		require.True(t, ok, "batch should deliver at least one message")
		require.NoError(t, m.Msg.Ack())
	case <-time.After(3 * time.Second):
		t.Fatal("Fetch did not deliver the first message within timeout")
	}

	// The forwarding goroutine must still finish and exit on its own: with
	// the buffer sized to batchSize, it can drain the remaining messages
	// into the buffer without a reader and then return. Check for its stack
	// frame directly (rather than a raw runtime.NumGoroutine() count, which
	// is too noisy here — the embedded NATS server's own background timers
	// and expiration loops churn independently of anything this test does)
	// so the assertion targets exactly the goroutine this fix is about.
	assert.Eventually(t, func() bool {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		return !strings.Contains(string(buf[:n]), "nats.wrapMessageBatch")
	}, 3*time.Second, 20*time.Millisecond,
		"wrapMessageBatch's forwarding goroutine should exit on its own after the batch is abandoned early, not leak")
}

// TestJetStream_Consume_NilHandler locks down the one validation guard the
// wrapper adds: a nil Consume handler returns an error rather than panicking.
func TestJetStream_Consume_NilHandler(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	const streamName, subject, consumerName = "EVENTS_N", "events.n.created", "nil-handler-test"
	stream, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	cons, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	cc, err := cons.Consume(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, cc)
	assert.Contains(t, err.Error(), "handler must not be nil")
}

// TestJetStream_ManagementRoundTrip exercises the thin passthrough surface that
// the trace-propagation tests above don't reach directly: stream/consumer
// lookup, Info / CachedInfo, PublishMsg, the single-message Consumer.Next, the
// Messages Drain path, and DeleteConsumer / DeleteStream.
func TestJetStream_ManagementRoundTrip(t *testing.T) {
	enableNATSTracing(t)
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)

	ctx := context.Background()
	const streamName, subject, consumerName = "EVENTS_MGMT", "events.mgmt.created", "mgmt-consumer"

	// Stream: create, look up, Info, CachedInfo.
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{subject},
	})
	require.NoError(t, err)

	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)
	info, err := stream.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, streamName, info.Config.Name)
	assert.NotNil(t, stream.CachedInfo())

	// Consumer: create, look up, Info, CachedInfo.
	_, err = js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	cons, err := js.Consumer(ctx, streamName, consumerName)
	require.NoError(t, err)
	cinfo, err := cons.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, consumerName, cinfo.Name)
	assert.NotNil(t, cons.CachedInfo())

	// Publish + PublishMsg, then fetch one via the Messages iterator (the
	// single-message Consumer.Next is intentionally not wrapped — ADR 0022
	// amendment; use Messages with PullMaxMessages(1) for single fetch).
	_, err = js.Publish(ctx, subject, []byte("a"))
	require.NoError(t, err)
	_, err = js.PublishMsg(ctx, &nats.Msg{Subject: subject, Data: []byte("b")})
	require.NoError(t, err)

	iter, err := cons.Messages(ctx, jetstream.PullMaxMessages(1))
	require.NoError(t, err)

	type result struct {
		msg jetstream.Msg
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		_, m, e := iter.Next()
		resCh <- result{m, e}
	}()
	select {
	case res := <-resCh:
		require.NoError(t, res.err)
		require.NotNil(t, res.msg)
		require.NoError(t, res.msg.Ack())
	case <-time.After(3 * time.Second):
		t.Fatal("Messages().Next did not return within timeout")
	}

	// Drain path must not panic.
	iter.Drain()

	// Delete consumer and stream.
	require.NoError(t, js.DeleteConsumer(ctx, streamName, consumerName))
	require.NoError(t, js.DeleteStream(ctx, streamName))
}
