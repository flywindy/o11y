package nats_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	o11ynats "github.com/flywindy/o11y/nats"
)

// startTestServer starts an embedded NATS server and returns the server and its
// client URL. The server is shut down automatically via t.Cleanup.
func startTestServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	opts := test.DefaultTestOptions
	opts.Port = -1 // pick a random available port
	s := test.RunServer(&opts)
	t.Cleanup(s.Shutdown)
	return s, s.ClientURL()
}

// startJetStreamServer starts an embedded NATS server with JetStream enabled.
func startJetStreamServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	opts := test.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	s := test.RunServer(&opts)
	t.Cleanup(s.Shutdown)
	return s, s.ClientURL()
}

// newTestProviders returns an in-memory TracerProvider, a TraceContext propagator,
// and a SpanRecorder. No OTLP endpoint is required.
func newTestProviders() (oteltrace.TracerProvider, propagation.TextMapPropagator, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	return tp, prop, sr
}

func enableNATSTracing(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", "true")
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "true")
}

func TestConnect(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	require.NotNil(t, conn)
	conn.Close()
}

func TestConnect_InvalidURL(t *testing.T) {
	tp, prop, _ := newTestProviders()

	// "nats://:invalid" is syntactically invalid so the connect always fails
	// immediately at URL parsing, without relying on network reachability.
	conn, err := o11ynats.Connect(context.Background(), "nats://:invalid", tp, prop)
	assert.Error(t, err)
	assert.Nil(t, conn)
}

func TestSubscribe_ContextPropagation(t *testing.T) {
	enableNATSTracing(t)

	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	pub, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer pub.Close()

	sub, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer sub.Close()

	subject := "test.propagation"

	var (
		wg         sync.WaitGroup
		gotTraceID oteltrace.TraceID
	)
	wg.Add(1)

	_, err = sub.Subscribe(context.Background(), subject, func(ctx context.Context, _ *nats.Msg) {
		defer wg.Done()
		gotTraceID = oteltrace.SpanFromContext(ctx).SpanContext().TraceID()
	})
	require.NoError(t, err)
	require.NoError(t, sub.NatsConn().FlushTimeout(2*time.Second))

	// Start a root span on the publisher side so there is a valid trace ID to
	// propagate through the message headers.
	tracer := tp.Tracer("test")
	pubCtx, span := tracer.Start(context.Background(), "test-publish")
	pubTraceID := span.SpanContext().TraceID()

	err = pub.Publish(pubCtx, subject, []byte("hello"))
	require.NoError(t, err)
	span.End()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive message within timeout")
	}

	// otelnats follows OTel messaging semantics: the consumer span starts a new
	// trace and links to the producer span rather than parenting under it.
	// Therefore gotTraceID will differ from pubTraceID, but must still be valid.
	assert.True(t, gotTraceID.IsValid(), "subscriber ctx should carry a valid trace ID")
	assert.NotEqual(t, oteltrace.TraceID{}, gotTraceID, "subscriber trace ID must not be zero")

	// The consumer span is ended by a defer inside wrapHandler after our callback
	// returns. Poll the SpanRecorder until that span appears, then verify it
	// carries a link back to the publisher's trace.
	assert.Eventually(t, func() bool {
		for _, s := range sr.Ended() {
			for _, link := range s.Links() {
				if link.SpanContext.TraceID() == pubTraceID {
					return true
				}
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond,
		"consumer span should have a span link back to the publisher's trace ID")
}

func TestQueueSubscribe(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	pub, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer pub.Close()

	sub, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer sub.Close()

	subject := "test.queue"
	received := make(chan struct{}, 1)

	_, err = sub.QueueSubscribe(context.Background(), subject, "workers", func(_ context.Context, _ *nats.Msg) {
		received <- struct{}{}
	})
	require.NoError(t, err)
	require.NoError(t, sub.NatsConn().FlushTimeout(2*time.Second))

	err = pub.Publish(context.Background(), subject, []byte("ping"))
	require.NoError(t, err)

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("queue subscriber did not receive message within timeout")
	}
}

// TestRespond_TracePropagation verifies the headline guarantee of Conn.Respond:
// a reply sent from inside a handler carries the W3C trace context, unlike a
// raw msg.Respond. The responder replies via conn.Respond and the requester
// asserts the reply message headers contain a traceparent.
func TestRespond_TracePropagation(t *testing.T) {
	enableNATSTracing(t)

	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	responder, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer responder.Close()

	requester, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer requester.Close()

	subject := "test.reqreply"

	// The responder publishes the reply to msg.Reply (a dynamic inbox). Capture
	// that subject so the test can locate the reply's producer span below.
	replySubjectCh := make(chan string, 1)

	_, err = responder.Subscribe(context.Background(), subject, func(ctx context.Context, msg *nats.Msg) {
		replySubjectCh <- msg.Reply
		// Reply over the traced path so the response carries trace context.
		// Use assert (not require): this runs on a subscription goroutine, where
		// FailNow must not be called, but a reply failure should still surface.
		assert.NoError(t, responder.Respond(ctx, msg, []byte("pong")))
	})
	require.NoError(t, err)
	require.NoError(t, responder.NatsConn().FlushTimeout(2*time.Second))

	// Start a root span so there is a valid trace ID to propagate.
	tracer := tp.Tracer("test")
	reqCtx, span := tracer.Start(context.Background(), "test-request")
	defer span.End()

	reply, err := requester.Request(reqCtx, subject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Equal(t, "pong", string(reply.Data))

	// The reply must carry the responder's trace context in its headers; this is
	// exactly what raw msg.Respond would drop.
	require.NotNil(t, reply.Header, "reply must carry headers")
	assert.NotEmpty(t, reply.Header.Get("traceparent"),
		"reply should carry a traceparent header injected by Respond")

	// Span-level proof that Respond routed through the traced publish path (not a
	// raw msg.Respond): a producer "send" span must be recorded for the reply,
	// addressed to the dynamic reply inbox.
	var replySubject string
	select {
	case replySubject = <-replySubjectCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run")
	}
	require.NotEmpty(t, replySubject, "reply subject (inbox) must be set")

	assert.Eventually(t, func() bool {
		for _, s := range sr.Ended() {
			var destMatch, isSend bool
			for _, a := range s.Attributes() {
				switch a.Key {
				case semconv.MessagingDestinationNameKey:
					destMatch = a.Value.AsString() == replySubject
				case semconv.MessagingOperationTypeKey:
					isSend = a.Value.AsString() == semconv.MessagingOperationTypeSend.Value.AsString()
				}
			}
			if destMatch && isSend {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond,
		"a producer send span should be recorded for the reply publish, proving Respond uses the traced Publish path")
}

// TestRequest_ReplyLink verifies the requester-side gap closed by
// Conn.Request (ADR 0022 amendment, 2026-07-01): once the reply carrying the
// responder's trace context (injected by Conn.Respond) comes back, Request
// must start a CONSUMER-kind span that links to that context, closing the
// handler → requester leg of the round trip in the trace view.
func TestRequest_ReplyLink(t *testing.T) {
	enableNATSTracing(t)

	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	responder, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer responder.Close()

	requester, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer requester.Close()

	subject := "test.reqreply.link"

	_, err = responder.Subscribe(context.Background(), subject, func(ctx context.Context, msg *nats.Msg) {
		assert.NoError(t, responder.Respond(ctx, msg, []byte("pong")))
	})
	require.NoError(t, err)
	require.NoError(t, responder.NatsConn().FlushTimeout(2*time.Second))

	tracer := tp.Tracer("test")
	reqCtx, span := tracer.Start(context.Background(), "test-request")
	requestTraceID := span.SpanContext().TraceID()

	reply, err := requester.Request(reqCtx, subject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)
	span.End()

	// The reply-receive span must be a child of the request's own trace (it
	// runs synchronously right after Request returns, in the same local flow)
	// while also carrying a link to the responder's reply-send span context —
	// the cross-service correlation that makes the round trip visible.
	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "receive "+subject {
			continue
		}
		assert.Equal(t, requestTraceID, s.SpanContext().TraceID(),
			"reply-receive span should stay in the requester's own trace")
		require.Len(t, s.Links(), 1, "reply-receive span should carry exactly one link")
		assert.True(t, s.Links()[0].SpanContext.IsValid(),
			"the link should point at a valid remote span context")
		assert.NotEqual(t, requestTraceID, s.Links()[0].SpanContext.TraceID(),
			"the link should point at the responder's trace, not loop back to the requester's own")
		found = true
	}
	assert.True(t, found, "a %q consumer span should be recorded", "receive "+subject)
}

// TestRequest_ReplyLink_CustomAttrs verifies the variadic attrs on Request
// land on the reply-link span. This is the hook applications use to make the
// span searchable on domain identifiers the SDK cannot infer on its own
// (request/correlation ID, room ID, site ID, …).
func TestRequest_ReplyLink_CustomAttrs(t *testing.T) {
	enableNATSTracing(t)

	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	responder, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer responder.Close()

	requester, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer requester.Close()

	subject := "test.reqreply.attrs"

	_, err = responder.Subscribe(context.Background(), subject, func(ctx context.Context, msg *nats.Msg) {
		assert.NoError(t, responder.Respond(ctx, msg, []byte("pong")))
	})
	require.NoError(t, err)
	require.NoError(t, responder.NatsConn().FlushTimeout(2*time.Second))

	reply, err := requester.Request(context.Background(), subject, []byte("ping"), 2*time.Second,
		attribute.String("app.request_id", "req-123"),
		attribute.String("app.room_id", "room-42"),
	)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "receive "+subject {
			continue
		}
		attrs := s.Attributes()
		assert.Contains(t, attrs, attribute.String("app.request_id", "req-123"))
		assert.Contains(t, attrs, attribute.String("app.room_id", "room-42"))
		found = true
	}
	assert.True(t, found, "a %q consumer span should be recorded", "receive "+subject)
}

// TestRequest_ReplyLink_BaseAttrsWinCollision verifies that a caller-supplied
// attr reusing one of the span's own semconv keys cannot override it — the
// same "built-in wins" guarantee redis.WithAttributes / cassandra.WithAttributes
// document elsewhere in this SDK. Without this, a caller could silently
// corrupt messaging.system/destination.name/operation.type/operation.name on
// the reply-link span.
func TestRequest_ReplyLink_BaseAttrsWinCollision(t *testing.T) {
	enableNATSTracing(t)

	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	responder, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer responder.Close()

	requester, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer requester.Close()

	subject := "test.reqreply.collision"

	_, err = responder.Subscribe(context.Background(), subject, func(ctx context.Context, msg *nats.Msg) {
		assert.NoError(t, responder.Respond(ctx, msg, []byte("pong")))
	})
	require.NoError(t, err)
	require.NoError(t, responder.NatsConn().FlushTimeout(2*time.Second))

	reply, err := requester.Request(context.Background(), subject, []byte("ping"), 2*time.Second,
		semconv.MessagingSystemKey.String("not-nats"),
		attribute.String("app.request_id", "req-123"),
	)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "receive "+subject {
			continue
		}
		attrs := s.Attributes()
		assert.Contains(t, attrs, semconv.MessagingSystemKey.String("nats"),
			"the SDK's own messaging.system value must win over a caller-supplied collision")
		assert.Contains(t, attrs, attribute.String("app.request_id", "req-123"),
			"a non-colliding caller attr must still be attached")
		found = true
	}
	assert.True(t, found, "a %q consumer span should be recorded", "receive "+subject)
}

// TestRequest_NoReplyHeader_NoLinkSpan verifies Request degrades cleanly when
// the reply carries no trace context — e.g. an untraced responder, or one
// that replied with the raw msg.Respond instead of Conn.Respond. No span
// should be created, and Request must still return the reply as-is.
func TestRequest_NoReplyHeader_NoLinkSpan(t *testing.T) {
	enableNATSTracing(t)

	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	responder, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer responder.Close()

	requester, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer requester.Close()

	subject := "test.reqreply.nolink"

	// Reply via the raw NATS msg.Respond, which does not inject trace headers.
	_, err = responder.Subscribe(context.Background(), subject, func(_ context.Context, msg *nats.Msg) {
		assert.NoError(t, msg.Respond([]byte("pong")))
	})
	require.NoError(t, err)
	require.NoError(t, responder.NatsConn().FlushTimeout(2*time.Second))

	reply, err := requester.Request(context.Background(), subject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Equal(t, "pong", string(reply.Data))

	for _, s := range sr.Ended() {
		assert.NotEqual(t, "receive "+subject, s.Name(),
			"no reply-receive span should be recorded when the reply carries no trace context")
	}
}

// TestRespond_Validation locks down the registration-time guards on
// Conn.Respond: a nil message and a message with no reply subject both return
// an error rather than panicking or silently publishing nowhere.
func TestRespond_Validation(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	cases := []struct {
		name    string
		msg     *nats.Msg
		wantErr string
	}{
		{"nil msg", nil, "msg must not be nil"},
		{"empty reply", &nats.Msg{Subject: "test.subject"}, "no reply subject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := conn.Respond(context.Background(), tc.msg, []byte("data"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestJetStream_NotNil(t *testing.T) {
	_, url := startJetStreamServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)
	require.NotNil(t, js)
}

// noopHandler is a do-nothing MsgHandler used as a stand-in by validation
// tests that exercise the registration-time guards on Subscribe /
// QueueSubscribe.
func noopHandler(_ context.Context, _ *nats.Msg) {}

// TestSubscribe_Validation locks down every registration-time guard in
// Conn.Subscribe: empty subject, nil handler, and an already-cancelled ctx.
// Per AGENTS.md every public function must have a unit test, and table-driven
// tests are preferred — this single table covers all three error paths.
func TestSubscribe_Validation(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name    string
		ctx     context.Context
		subject string
		handler o11ynats.MsgHandler
		wantErr string
	}{
		{"canceled ctx", canceled, "test.subject", noopHandler, context.Canceled.Error()},
		{"empty subject", context.Background(), "", noopHandler, "subject must not be empty"},
		{"nil handler", context.Background(), "test.subject", nil, "handler must not be nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := conn.Subscribe(tc.ctx, tc.subject, tc.handler)
			require.Error(t, err)
			assert.Nil(t, sub)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestQueueSubscribe_Validation mirrors TestSubscribe_Validation for the
// queue-group variant. QueueSubscribe has one extra guard (empty queue), so
// the table carries four rows instead of three.
func TestQueueSubscribe_Validation(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name    string
		ctx     context.Context
		subject string
		queue   string
		handler o11ynats.MsgHandler
		wantErr string
	}{
		{"canceled ctx", canceled, "test.subject", "workers", noopHandler, context.Canceled.Error()},
		{"empty subject", context.Background(), "", "workers", noopHandler, "subject must not be empty"},
		{"empty queue", context.Background(), "test.subject", "", noopHandler, "queue must not be empty"},
		{"nil handler", context.Background(), "test.subject", "workers", nil, "handler must not be nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := conn.QueueSubscribe(tc.ctx, tc.subject, tc.queue, tc.handler)
			require.Error(t, err)
			assert.Nil(t, sub)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
