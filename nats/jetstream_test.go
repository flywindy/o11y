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
