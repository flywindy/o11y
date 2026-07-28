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
// StoreDir is a per-test temp dir: without it the server persists streams,
// durable consumers, and unacked messages in a shared default directory, so a
// failed run's backlog leaks into the next run — Fetch/Next then deliver a
// stale message whose traceparent points at a dead process's trace, failing
// span-link assertions in ways that only reproduce on a machine that has run
// the suite before.
func startJetStreamServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	opts := test.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
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

// Tracing in these tests is enabled by o11ynats.Connect itself (via
// otelnats.WithTracingEnabled(true)), not by process-wide env gates — that is
// the whole point of the enhancement, so the tests deliberately set no
// OTEL_*_ENABLED variables and still observe spans.

func TestConnect(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	require.NotNil(t, conn)
	conn.Close()
}

func TestConnectWithOptions_DisablesTracing(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	pub, err := o11ynats.ConnectWithOptions(context.Background(), url, tp, prop,
		o11ynats.WithTracingEnabled(false),
	)
	require.NoError(t, err)
	defer pub.Close()
	assert.False(t, pub.TracingEnabled())

	sub, err := o11ynats.ConnectWithOptions(context.Background(), url, tp, prop,
		o11ynats.WithTracingEnabled(false),
	)
	require.NoError(t, err)
	defer sub.Close()
	assert.False(t, sub.TracingEnabled())

	subject := "test.tracing.disabled"
	type receivedMessage struct {
		ctx    context.Context
		header nats.Header
	}
	received := make(chan receivedMessage, 1)

	_, err = sub.Subscribe(context.Background(), subject, func(ctx context.Context, msg *nats.Msg) {
		received <- receivedMessage{ctx: ctx, header: msg.Header}
	})
	require.NoError(t, err)
	require.NoError(t, sub.NatsConn().FlushTimeout(2*time.Second))

	tracer := tp.Tracer("test")
	pubCtx, span := tracer.Start(context.Background(), "ambient")
	err = pub.Publish(pubCtx, subject, []byte("hello"))
	require.NoError(t, err)
	span.End()

	var got receivedMessage
	select {
	case got = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive message within timeout")
	}

	assert.False(t, oteltrace.SpanFromContext(got.ctx).SpanContext().IsValid(),
		"disabled NATS tracing should deliver a background handler context")
	assert.Empty(t, got.header.Get("traceparent"),
		"disabled NATS tracing should delegate natively without injecting traceparent")

	spans := sr.Ended()
	require.Len(t, spans, 1, "only the explicit ambient span should be recorded")
	assert.Equal(t, "ambient", spans[0].Name())
}

func TestConnectWithOptions_ForwardsNATSOptions(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.ConnectWithOptions(context.Background(), url, tp, prop,
		o11ynats.WithNATSOptions(nats.Name("o11y-test-client")),
	)
	require.NoError(t, err)
	defer conn.Close()

	assert.True(t, conn.TracingEnabled())
	assert.Equal(t, "o11y-test-client", conn.NatsConn().Opts.Name)
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

// TestRequest_ReplyLink verifies the requester-side half of the request/reply
// round trip. Since otel-nats v0.6.0 the upstream layer records the reply
// receive span itself (recordReply): a CONSUMER-kind span named after the
// reply inbox, linked to — and parented under — the responder's reply-send
// span context whenever the reply carries one (which it does when the
// responder replied via Conn.Respond). This replaces the reply-link span this
// facade created in earlier SDK versions; the topology moved with it: the
// receive span now lands in the responder's trace (remote parent) rather than
// the requester's, with the link still providing the cross-trace correlation.
func TestRequest_ReplyLink(t *testing.T) {
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
	require.NotEmpty(t, reply.Subject, "a valid reply carries the inbox subject it was delivered to")

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "receive "+reply.Subject {
			continue
		}
		assert.Equal(t, oteltrace.SpanKindClient, s.SpanKind(),
			"reply-receive span should be CLIENT-kind (otel-nats v0.7.0 corrected the span kinds)")
		require.Len(t, s.Links(), 1, "reply-receive span should carry exactly one link")
		assert.True(t, s.Links()[0].SpanContext.IsValid(),
			"the link should point at a valid remote span context")
		assert.NotEqual(t, requestTraceID, s.Links()[0].SpanContext.TraceID(),
			"the link should point at the responder's trace, not loop back to the requester's own")
		assert.Equal(t, s.Links()[0].SpanContext.TraceID(), s.SpanContext().TraceID(),
			"upstream v0.6.0 parents the receive span under the responder's remote context, so span and link share the responder's trace")
		found = true
	}
	assert.True(t, found, "a %q consumer span should be recorded", "receive "+reply.Subject)
}

// TestRequest_ReplyLink_ServerAttrs verifies the upstream reply-receive span
// carries server.address (and server.port, since the test server binds a
// random non-default port), matching the "send"/"process"/"receive" spans
// otelnats emits and the NATS attribute set docs/semconv.md documents —
// without this, the reply-receive span would be the one NATS span in a trace
// that can't be filtered by broker.
func TestRequest_ReplyLink_ServerAttrs(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	responder, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer responder.Close()

	requester, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer requester.Close()

	subject := "test.reqreply.serverattrs"

	_, err = responder.Subscribe(context.Background(), subject, func(ctx context.Context, msg *nats.Msg) {
		assert.NoError(t, responder.Respond(ctx, msg, []byte("pong")))
	})
	require.NoError(t, err)
	require.NoError(t, responder.NatsConn().FlushTimeout(2*time.Second))

	reply, err := requester.Request(context.Background(), subject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.NotEmpty(t, reply.Subject)

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "receive "+reply.Subject {
			continue
		}
		var hasServerAddress, hasServerPort bool
		for _, a := range s.Attributes() {
			if a.Key == semconv.ServerAddressKey {
				hasServerAddress = true
				assert.NotEmpty(t, a.Value.AsString())
			}
			if a.Key == semconv.ServerPortKey {
				hasServerPort = true
			}
		}
		assert.True(t, hasServerAddress, "reply-receive span should carry server.address")
		assert.True(t, hasServerPort, "reply-receive span should carry server.port for a non-default port")
		found = true
	}
	assert.True(t, found, "a %q consumer span should be recorded", "receive "+reply.Subject)
}

// TestRequest_ReplyReceive_DestinationIsInbox verifies the upstream
// reply-receive span can be correlated by inbox: its
// messaging.destination.name is the reply's own Subject (the request/reply
// inbox NATS generated for this call), the same value the requester's send
// span emits as messaging.message.conversation_id via msg.Reply. (Earlier SDK
// versions put the inbox on a conversation_id attribute of a facade-created
// span; upstream v0.6.0's recordReply uses the destination attribute of its
// receive span instead.)
func TestRequest_ReplyReceive_DestinationIsInbox(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	responder, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer responder.Close()

	requester, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer requester.Close()

	subject := "test.reqreply.conversationid"

	_, err = responder.Subscribe(context.Background(), subject, func(ctx context.Context, msg *nats.Msg) {
		assert.NoError(t, responder.Respond(ctx, msg, []byte("pong")))
	})
	require.NoError(t, err)
	require.NoError(t, responder.NatsConn().FlushTimeout(2*time.Second))

	reply, err := requester.Request(context.Background(), subject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.NotEmpty(t, reply.Subject, "a valid reply always carries the inbox subject it was delivered to")

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "receive "+reply.Subject {
			continue
		}
		assert.Contains(t, s.Attributes(), semconv.MessagingDestinationNameKey.String(reply.Subject),
			"reply-receive span should carry messaging.destination.name set to the reply's inbox subject")
		found = true
	}
	assert.True(t, found, "a %q consumer span should be recorded", "receive "+reply.Subject)
}

// TestRequest_NoReplyHeader_NoLinkSpan verifies Request degrades cleanly when
// the reply carries no trace context — e.g. an untraced responder, or one
// that replied with the raw msg.Respond instead of Conn.Respond. Upstream
// v0.6.0 still records the reply-receive span (recordReply is unconditional)
// but it must carry no link, and Request must return the reply as-is.
func TestRequest_NoReplyHeader_NoLinkSpan(t *testing.T) {
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
	require.NotEmpty(t, reply.Subject)

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() != "receive "+reply.Subject {
			continue
		}
		assert.Empty(t, s.Links(),
			"the reply-receive span must carry no link when the reply has no trace context")
		found = true
	}
	assert.True(t, found, "upstream records the reply-receive span even for an untraced reply")
}

// TestRequestMsg_CtxFirstTracing verifies the RequestMsg shim routes through
// the ctx-first path: the producer send span parents to the caller's ctx
// trace, not context.Background() (the embedded ctx-less RequestMsg footgun
// this shadow method exists to prevent). It also confirms a header set on the
// pre-built request message reaches the responder — proving RequestMsg sends
// the caller's *nats.Msg rather than a fresh one.
func TestRequestMsg_CtxFirstTracing(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	responder, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer responder.Close()

	requester, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer requester.Close()

	subject := "test.requestmsg"

	gotHeaderCh := make(chan string, 1)
	_, err = responder.Subscribe(context.Background(), subject, func(ctx context.Context, msg *nats.Msg) {
		gotHeaderCh <- msg.Header.Get("X-Custom")
		assert.NoError(t, responder.Respond(ctx, msg, []byte("pong")))
	})
	require.NoError(t, err)
	require.NoError(t, responder.NatsConn().FlushTimeout(2*time.Second))

	tracer := tp.Tracer("test")
	reqCtx, span := tracer.Start(context.Background(), "test-request-msg")
	reqTraceID := span.SpanContext().TraceID()

	msg := &nats.Msg{Subject: subject, Data: []byte("ping"), Header: nats.Header{}}
	msg.Header.Set("X-Custom", "hdr-val")
	reply, err := requester.RequestMsg(reqCtx, msg, 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)
	span.End()
	assert.Equal(t, "pong", string(reply.Data))

	select {
	case got := <-gotHeaderCh:
		assert.Equal(t, "hdr-val", got, "the custom request header must reach the responder")
	case <-time.After(2 * time.Second):
		t.Fatal("responder did not run")
	}

	// The CLIENT send span (name "{subject} request", per upstream
	// startRequestSpan) must live in the requester's own trace (ctx-first
	// path), not a disconnected background root trace (the ctx-less footgun).
	var found bool
	for _, s := range sr.Ended() {
		if s.SpanKind() == oteltrace.SpanKindClient && s.SpanContext().TraceID() == reqTraceID {
			found = true
		}
	}
	assert.True(t, found,
		"RequestMsg send span should parent to the caller's ctx trace, not context.Background()")
}

// TestRequestMsg_NilMsg locks down the nil-message guard on Conn.RequestMsg,
// matching Conn.Respond's guard rather than delegating a nil-deref to upstream.
func TestRequestMsg_NilMsg(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	reply, err := conn.RequestMsg(context.Background(), nil, time.Second)
	require.Error(t, err)
	assert.Nil(t, reply)
	assert.Contains(t, err.Error(), "msg must not be nil")
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
