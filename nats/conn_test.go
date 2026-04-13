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

func TestConnect(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, _ := newTestProviders()

	conn, err := o11ynats.Connect(url, tp, prop)
	require.NoError(t, err)
	require.NotNil(t, conn)
	conn.Close()
}

func TestConnect_InvalidURL(t *testing.T) {
	tp, prop, _ := newTestProviders()

	// Port 1 is reserved and unreachable; combined with a short timeout and no
	// reconnect attempts this ensures an immediate connection error.
	conn, err := o11ynats.Connect("nats://127.0.0.1:1", tp, prop,
		nats.MaxReconnects(0),
		nats.Timeout(200*time.Millisecond),
	)
	assert.Error(t, err)
	assert.Nil(t, conn)
}

func TestSubscribe_ContextPropagation(t *testing.T) {
	_, url := startTestServer(t)
	tp, prop, sr := newTestProviders()

	pub, err := o11ynats.Connect(url, tp, prop)
	require.NoError(t, err)
	defer pub.Close()

	sub, err := o11ynats.Connect(url, tp, prop)
	require.NoError(t, err)
	defer sub.Close()

	subject := "test.propagation"

	var (
		wg         sync.WaitGroup
		gotTraceID oteltrace.TraceID
	)
	wg.Add(1)

	_, err = sub.Subscribe(subject, func(ctx context.Context, _ *nats.Msg) {
		defer wg.Done()
		gotTraceID = oteltrace.SpanFromContext(ctx).SpanContext().TraceID()
	})
	require.NoError(t, err)

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

	pub, err := o11ynats.Connect(url, tp, prop)
	require.NoError(t, err)
	defer pub.Close()

	sub, err := o11ynats.Connect(url, tp, prop)
	require.NoError(t, err)
	defer sub.Close()

	subject := "test.queue"
	received := make(chan struct{}, 1)

	_, err = sub.QueueSubscribe(subject, "workers", func(_ context.Context, _ *nats.Msg) {
		received <- struct{}{}
	})
	require.NoError(t, err)

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

	conn, err := o11ynats.Connect(url, tp, prop)
	require.NoError(t, err)
	defer conn.Close()

	js, err := conn.JetStream()
	require.NoError(t, err)
	require.NotNil(t, js)
}
