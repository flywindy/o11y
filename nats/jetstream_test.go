package nats_test

import (
	"context"
	"testing"
	"time"

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
	cc, err := cons.Consume(func(msgCtx context.Context, m jetstream.Msg) {
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

	iter, err := cons.Messages()
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

	cc, err := cons.Consume(nil)
	require.Error(t, err)
	assert.Nil(t, cc)
	assert.Contains(t, err.Error(), "handler must not be nil")
}
