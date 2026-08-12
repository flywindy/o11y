package nats_test

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/flywindy/o11y/internal/baggageattrs"
	o11ynats "github.com/flywindy/o11y/nats"
)

func TestMain(m *testing.M) {
	// otel-nats v0.8.0 gives its environment and relay configuration higher
	// precedence than the per-connection option. Keep this package's baseline
	// tests independent of the developer or CI process environment; individual
	// tests set the variables they are exercising explicitly.
	for _, key := range []string{
		"OTEL_INSTRUMENTATION_GO_TRACING_ENABLED",
		"OTEL_NATS_TRACING_ENABLED",
		"OTEL_INSTRUMENTATION_GO_FLAGS_ENDPOINT",
		"OTEL_INSTRUMENTATION_GO_FLAGS_POLL_INTERVAL",
		"OTEL_INSTRUMENTATION_GO_FLAGS_API_KEY",
	} {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

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

// newTestProviders returns an in-memory TracerProvider, the SDK's
// TraceContext+Baggage propagation shape, and a SpanRecorder.
func newTestProviders() (oteltrace.TracerProvider, propagation.TextMapPropagator, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(baggageattrs.NewWhitelist("app.order.id").NewSpanProcessor()),
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	return tp, prop, sr
}

func contextWithTestBaggage(ctx context.Context, t *testing.T, key, value string) context.Context {
	t.Helper()
	member, err := baggage.NewMemberRaw(key, value)
	require.NoError(t, err)
	bag, err := baggage.FromContext(ctx).SetMember(member)
	require.NoError(t, err)
	return baggage.ContextWithBaggage(ctx, bag)
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
	pubCtx = contextWithTestBaggage(pubCtx, t, "app.order.id", "not-injected")
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
	assert.Empty(t, got.header.Get("baggage"),
		"disabled NATS tracing should delegate natively without injecting baggage")
	assert.Empty(t, baggage.FromContext(got.ctx).Member("app.order.id").Key(),
		"disabled NATS tracing should not deliver baggage from the publisher context")

	raw := nats.NewMsg(subject)
	raw.Data = []byte("raw")
	raw.Header.Set("baggage", "app.order.id=forged")
	require.NoError(t, pub.NatsConn().PublishMsg(raw))
	select {
	case got = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive raw message within timeout")
	}
	assert.Equal(t, "app.order.id=forged", got.header.Get("baggage"))
	assert.Empty(t, baggage.FromContext(got.ctx).Member("app.order.id").Key(),
		"disabled NATS tracing must not restore a raw baggage header")

	spans := sr.Ended()
	require.Len(t, spans, 1, "only the explicit ambient span should be recorded")
	assert.Equal(t, "ambient", spans[0].Name())
}

func TestConnectWithOptions_EnvironmentOverridesTracingDefault(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	tests := []struct {
		name             string
		moduleEnv        string
		masterEnv        string
		optionDefault    bool
		wantTracingState bool
	}{
		{
			name:             "module environment enables an option-disabled connection",
			moduleEnv:        "true",
			optionDefault:    false,
			wantTracingState: true,
		},
		{
			name:             "module environment disables an option-enabled connection",
			moduleEnv:        "false",
			optionDefault:    true,
			wantTracingState: false,
		},
		{
			name:             "master environment vetoes an enabled module",
			moduleEnv:        "true",
			masterEnv:        "false",
			optionDefault:    true,
			wantTracingState: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_NATS_TRACING_ENABLED", tt.moduleEnv)
			if tt.masterEnv != "" {
				t.Setenv("OTEL_INSTRUMENTATION_GO_TRACING_ENABLED", tt.masterEnv)
			}

			conn, err := o11ynats.ConnectWithOptions(context.Background(), url, tp, prop,
				o11ynats.WithTracingEnabled(tt.optionDefault),
			)
			require.NoError(t, err)
			defer conn.Close()
			assert.Equal(t, tt.wantTracingState, conn.TracingEnabled())
		})
	}
}

func TestConnectWithOptions_EnvironmentEnableRestoresBaggage(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "true")

	pub, err := o11ynats.ConnectWithOptions(context.Background(), url, tp, prop,
		o11ynats.WithTracingEnabled(false),
	)
	require.NoError(t, err)
	defer pub.Close()

	sub, err := o11ynats.ConnectWithOptions(context.Background(), url, tp, prop,
		o11ynats.WithTracingEnabled(false),
	)
	require.NoError(t, err)
	defer sub.Close()

	received := make(chan context.Context, 1)
	_, err = sub.Subscribe(context.Background(), "test.env.enable.baggage", func(ctx context.Context, _ *nats.Msg) {
		received <- ctx
	})
	require.NoError(t, err)
	require.NoError(t, sub.NatsConn().FlushTimeout(2*time.Second))

	ctx := contextWithTestBaggage(context.Background(), t, "app.order.id", "order-42")
	require.NoError(t, pub.Publish(ctx, "test.env.enable.baggage", []byte("hello")))

	select {
	case got := <-received:
		assert.Equal(t, "order-42", baggage.FromContext(got).Member("app.order.id").Value(),
			"the effective traced path must restore baggage even when the option default is false")
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive message within timeout")
	}
}

func TestConnectWithOptions_EnvironmentDisableSkipsBaggage(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "false")

	pub, err := o11ynats.ConnectWithOptions(context.Background(), url, tp, prop,
		o11ynats.WithTracingEnabled(true),
	)
	require.NoError(t, err)
	defer pub.Close()

	sub, err := o11ynats.ConnectWithOptions(context.Background(), url, tp, prop,
		o11ynats.WithTracingEnabled(true),
	)
	require.NoError(t, err)
	defer sub.Close()

	received := make(chan context.Context, 1)
	_, err = sub.Subscribe(context.Background(), "test.env.disable.baggage", func(ctx context.Context, _ *nats.Msg) {
		received <- ctx
	})
	require.NoError(t, err)
	require.NoError(t, sub.NatsConn().FlushTimeout(2*time.Second))

	raw := nats.NewMsg("test.env.disable.baggage")
	raw.Header.Set("baggage", "app.order.id=forged")
	require.NoError(t, pub.NatsConn().PublishMsg(raw))

	select {
	case got := <-received:
		assert.Empty(t, baggage.FromContext(got).Member("app.order.id").Key(),
			"the effective direct path must skip baggage even when the option default is true")
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive message within timeout")
	}
}

func TestConnectWithOptions_InvalidUpstreamFlagFailsConstruction(t *testing.T) {
	tp, prop, _ := newTestProviders()
	t.Setenv("OTEL_NATS_TRACING_ENABLED", "enabled")

	conn, err := o11ynats.ConnectWithOptions(context.Background(), "nats://127.0.0.1:1", tp, prop,
		o11ynats.WithTracingEnabled(true),
	)

	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorContains(t, err, "OTEL_NATS_TRACING_ENABLED")
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

// TestConnect_RejectsNilProviders pins the non-nil precondition the package
// comment states. It is enforced rather than documented because otel-nats
// v0.8.0 made a nil propagator silently harmful: the facade now holds the
// caller's propagator directly (v0.8.0's directConn.TraceContext returns a
// throwaway empty propagator, so reading it back is no longer viable), and a
// nil one would disable baggage restoration for the connection's whole life
// while upstream kept propagating through the OTel globals. The URL is never
// dialed — validation happens before the connect attempt.
func TestConnect_RejectsNilProviders(t *testing.T) {
	tp, prop, _ := newTestProviders()

	t.Run("nil tracer provider", func(t *testing.T) {
		conn, err := o11ynats.Connect(context.Background(), "nats://127.0.0.1:1", nil, prop)
		assert.Nil(t, conn)
		require.Error(t, err)
		assert.ErrorContains(t, err, "tracer provider must not be nil")
	})

	t.Run("nil propagator", func(t *testing.T) {
		conn, err := o11ynats.Connect(context.Background(), "nats://127.0.0.1:1", tp, nil)
		assert.Nil(t, conn)
		require.Error(t, err)
		assert.ErrorContains(t, err, "propagator must not be nil")
	})

	t.Run("ConnectWithOptions applies the same validation", func(t *testing.T) {
		conn, err := o11ynats.ConnectWithOptions(context.Background(), "nats://127.0.0.1:1", tp, nil,
			o11ynats.WithTracingEnabled(false),
		)
		assert.Nil(t, conn)
		require.Error(t, err)
		assert.ErrorContains(t, err, "propagator must not be nil")
	})
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
		gotOrderID string
	)
	wg.Add(1)

	_, err = sub.Subscribe(context.Background(), subject, func(ctx context.Context, _ *nats.Msg) {
		defer wg.Done()
		gotTraceID = oteltrace.SpanFromContext(ctx).SpanContext().TraceID()
		gotOrderID = baggage.FromContext(ctx).Member("app.order.id").Value()
	})
	require.NoError(t, err)
	require.NoError(t, sub.NatsConn().FlushTimeout(2*time.Second))

	// Start a root span on the publisher side so there is a valid trace ID to
	// propagate through the message headers.
	tracer := tp.Tracer("test")
	pubCtx, span := tracer.Start(context.Background(), "test-publish")
	pubCtx = contextWithTestBaggage(pubCtx, t, "app.order.id", "order-42")
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
	assert.NotEqual(t, pubTraceID, gotTraceID, "baggage restoration must preserve the consumer span context")
	assert.Equal(t, "order-42", gotOrderID, "subscriber ctx should retain propagated baggage")

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

func TestSubscribe_UsesConfiguredPropagatorForBaggageRestoration(t *testing.T) {
	_, url := startTestServer(t)
	tp, _, _ := newTestProviders()
	traceOnly := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})

	pub, err := o11ynats.Connect(context.Background(), url, tp, traceOnly)
	require.NoError(t, err)
	defer pub.Close()
	sub, err := o11ynats.Connect(context.Background(), url, tp, traceOnly)
	require.NoError(t, err)
	defer sub.Close()

	received := make(chan context.Context, 1)
	_, err = sub.Subscribe(context.Background(), "test.trace-only", func(ctx context.Context, _ *nats.Msg) {
		received <- ctx
	})
	require.NoError(t, err)
	require.NoError(t, sub.NatsConn().FlushTimeout(2*time.Second))

	msg := nats.NewMsg("test.trace-only")
	msg.Header.Set("baggage", "app.order.id=forged")
	require.NoError(t, pub.NatsConn().PublishMsg(msg))

	select {
	case ctx := <-received:
		assert.Empty(t, baggage.FromContext(ctx).Member("app.order.id").Key(),
			"a TraceContext-only policy must not be bypassed by the facade")
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive message within timeout")
	}
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
	received := make(chan string, 1)

	_, err = sub.QueueSubscribe(context.Background(), subject, "workers", func(ctx context.Context, _ *nats.Msg) {
		received <- baggage.FromContext(ctx).Member("app.order.id").Value()
	})
	require.NoError(t, err)
	require.NoError(t, sub.NatsConn().FlushTimeout(2*time.Second))

	pubCtx := contextWithTestBaggage(context.Background(), t, "app.order.id", "queue-order")
	err = pub.Publish(pubCtx, subject, []byte("ping"))
	require.NoError(t, err)

	select {
	case got := <-received:
		assert.Equal(t, "queue-order", got)
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

// replyReceiveSpanName is the name of the upstream reply-receive span. It is
// the bare operation name with no destination segment: the reply inbox is
// auto-generated and single-use, and semconv v1.39.0 directs omitting
// {destination} when no low-cardinality value is available. otel-nats v0.9.0
// implemented that (the span was `receive {inbox}` up to v0.8.0), so the inbox
// now lives only in the span's attributes — see
// TestRequest_ReplyReceive_DestinationIsInbox.
const replyReceiveSpanName = "receive"

// assertNoSpanNameCarriesInbox is the regression guard behind that rename: no
// recorded span may put the inbox in its NAME, whichever path produced it.
// Names are a dimension in trace backends (Jaeger's operation list, the
// spanmetrics connector), so one leaked inbox is unbounded cardinality — a
// weaker per-span assertion would not catch a new path that reintroduces it.
func assertNoSpanNameCarriesInbox(t *testing.T, sr *tracetest.SpanRecorder, inbox string) {
	t.Helper()
	require.NotEmpty(t, inbox)
	for _, s := range sr.Ended() {
		assert.NotContains(t, s.Name(), inbox,
			"span %q must not carry the reply inbox in its name", s.Name())
	}
}

// TestRequest_ReplyLink verifies the requester-side half of the request/reply
// round trip. Since otel-nats v0.6.0 the upstream layer records the reply
// receive span itself (recordReply): a CLIENT-kind span, linked to — and
// parented under — the responder's reply-send
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
		if s.Name() != replyReceiveSpanName {
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
	assert.True(t, found, "a %q client span should be recorded", replyReceiveSpanName)
	assertNoSpanNameCarriesInbox(t, sr, reply.Subject)
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
		if s.Name() != replyReceiveSpanName {
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
	assert.True(t, found, "a %q client span should be recorded", replyReceiveSpanName)
}

// TestRequest_ReplyReceive_DestinationIsInbox verifies the upstream
// reply-receive span can still be correlated by inbox now that its name no
// longer carries one. The inbox lives on four attributes:
// messaging.destination.name (the reply's own Subject, i.e. the inbox NATS
// generated for this call), messaging.message.conversation_id (the same value,
// also set late on the requester's send span), and the pair
// messaging.destination.temporary / .anonymous, which are what classify the
// destination as one whose name must be omitted. otel-nats v0.9.0 added the
// last three; without them the rename would have made the inbox unqueryable
// rather than merely unnamed.
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
		if s.Name() != replyReceiveSpanName {
			continue
		}
		assert.Contains(t, s.Attributes(), semconv.MessagingDestinationNameKey.String(reply.Subject),
			"reply-receive span should carry messaging.destination.name set to the reply's inbox subject")
		assert.Contains(t, s.Attributes(), semconv.MessagingMessageConversationID(reply.Subject),
			"reply-receive span should carry the inbox as messaging.message.conversation_id")
		assert.Contains(t, s.Attributes(), semconv.MessagingDestinationTemporary(true),
			"an inbox destination is temporary, which is why the span name omits it")
		assert.Contains(t, s.Attributes(), semconv.MessagingDestinationAnonymous(true),
			"an inbox destination is anonymous (auto-generated name)")
		found = true
	}
	assert.True(t, found, "a %q client span should be recorded", replyReceiveSpanName)
	assertNoSpanNameCarriesInbox(t, sr, reply.Subject)
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
		if s.Name() != replyReceiveSpanName {
			continue
		}
		assert.Empty(t, s.Links(),
			"the reply-receive span must carry no link when the reply has no trace context")
		found = true
	}
	assert.True(t, found, "upstream records the reply-receive span even for an untraced reply")
}

// spanNames returns the names of every recorded span, for the span-name shape
// assertions below.
func spanNames(sr *tracetest.SpanRecorder) []string {
	spans := sr.Ended()
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	return names
}

// TestSpanNames_SemconvShape pins the span names the NATS integration emits, in
// the semconv v1.39.0 messaging shape "{messaging.operation.name} {destination}"
// — operation first, and the destination only when a low-cardinality one exists.
// The names are entirely upstream-owned (otel-nats exposes no span-name
// formatter), so this test is the SDK's baseline for detecting an upstream
// rename: v0.9.0 changed all three of these (from "send {subject}",
// "{subject} request", and a concrete-subject "process"), and nothing in this
// repo caught it because nothing pinned them. docs/semconv.md's NATS span-name
// table is the prose half of this assertion.
//
// The process span additionally proves the destination is the *registered*
// subscription subject, not the concrete delivered one: a wildcard subscription
// is what bounds this name, and the concrete subject stays available on
// messaging.destination.name with the wildcard on messaging.destination.template.
func TestSpanNames_SemconvShape(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	const (
		filter    = "test.spannames.*"
		published = "test.spannames.created"
		rpc       = "test.spannames.rpc"
	)

	_, err = conn.Subscribe(context.Background(), filter, func(ctx context.Context, msg *nats.Msg) {
		if msg.Reply != "" {
			assert.NoError(t, conn.Respond(ctx, msg, []byte("pong")))
		}
	})
	require.NoError(t, err)
	require.NoError(t, conn.NatsConn().FlushTimeout(2*time.Second))

	require.NoError(t, conn.Publish(context.Background(), published, []byte("event")))
	reply, err := conn.Request(context.Background(), rpc, []byte("ping"), 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, reply)

	assert.Eventually(t, func() bool {
		names := spanNames(sr)
		for _, want := range []string{
			"publish " + published, // was "send {subject}" up to v0.8.0
			"request " + rpc,       // was "{subject} request" up to v0.8.0
			"process " + filter,    // the subscription subject, not the delivered one
			replyReceiveSpanName,   // bare "receive": the inbox is not nameable
		} {
			if !slices.Contains(names, want) {
				return false
			}
		}
		return true
	}, 2*time.Second, 10*time.Millisecond, "recorded span names: %v", spanNames(sr))

	// The one wildcard subscription delivers both the published event and the
	// request, so both deliveries share a span name while differing in
	// destination.name. Wait for both rather than snapshotting: Request returns
	// as soon as the reply arrives, and the handler sends that reply before it
	// returns, so the RPC delivery's process span can still be open here while
	// the earlier publish delivery has already satisfied the name assertion
	// above.
	var concrete []string
	require.Eventually(t, func() bool {
		concrete = nil
		for _, s := range sr.Ended() {
			if s.Name() != "process "+filter {
				continue
			}
			for _, a := range s.Attributes() {
				if a.Key == semconv.MessagingDestinationNameKey {
					concrete = append(concrete, a.Value.AsString())
				}
			}
		}
		return len(concrete) == 2
	}, 2*time.Second, 10*time.Millisecond, "recorded span names: %v", spanNames(sr))
	assert.ElementsMatch(t, []string{published, rpc}, concrete,
		"messaging.destination.name stays the concrete delivered subject on each process span")

	// The bounded name and the concrete subject coexist on every such span, so
	// assert the template over all of them rather than the first one recorded.
	for _, s := range sr.Ended() {
		if s.Name() != "process "+filter {
			continue
		}
		assert.Contains(t, s.Attributes(), semconv.MessagingDestinationTemplate(filter),
			"a wildcard subscription records its pattern as messaging.destination.template")
	}
}

// TestSpanNames_InboxDestinationDropped covers the two manual request/reply
// halves that otel-nats v0.9.0 brought under the same inbox rule as the
// reply-receive span. Both are spans a wrapper could easily assume are named
// after a subject:
//
//   - a reply published with conn.Publish(msg.Reply, …) rather than
//     conn.Respond — the responder half of a hand-rolled exchange; and
//   - a handler subscribed directly on an inbox — the requester half of the
//     callback-style RPC where a peer advertises its own inbox.
//
// Up to v0.8.0 these were "publish {inbox}" and "process {inbox}", i.e. one
// span name per request. Both are now bare, with the inbox on the attributes.
func TestSpanNames_InboxDestinationDropped(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	conn, err := o11ynats.Connect(context.Background(), url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	// Half one: reply to a request with a plain traced Publish to msg.Reply.
	const subject = "test.spannames.manualreply"
	_, err = conn.Subscribe(context.Background(), subject, func(ctx context.Context, msg *nats.Msg) {
		assert.NoError(t, conn.Publish(ctx, msg.Reply, []byte("pong")))
	})
	require.NoError(t, err)

	// Half two: subscribe on an inbox of our own and publish to it.
	inbox := nats.NewInbox()
	delivered := make(chan struct{}, 1)
	_, err = conn.Subscribe(context.Background(), inbox, func(_ context.Context, _ *nats.Msg) {
		delivered <- struct{}{}
	})
	require.NoError(t, err)
	require.NoError(t, conn.NatsConn().FlushTimeout(2*time.Second))

	reply, err := conn.Request(context.Background(), subject, []byte("ping"), 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "pong", string(reply.Data))

	require.NoError(t, conn.Publish(context.Background(), inbox, []byte("callback")))
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("inbox subscription did not receive the message")
	}

	assert.Eventually(t, func() bool {
		names := spanNames(sr)
		return slices.Contains(names, "publish") && slices.Contains(names, "process")
	}, 2*time.Second, 10*time.Millisecond,
		"a bare publish and a bare process span should be recorded; got %v", spanNames(sr))

	assertNoSpanNameCarriesInbox(t, sr, reply.Subject)
	assertNoSpanNameCarriesInbox(t, sr, inbox)
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
