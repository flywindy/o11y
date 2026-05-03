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
