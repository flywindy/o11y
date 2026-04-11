package nats_test

import (
	"context"
	"testing"

	gonnats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	o11ynats "github.com/flywindy/o11y/nats"
)

func newProp() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func ctxWithSpan(traceHex, spanHex string) context.Context {
	tid, _ := trace.TraceIDFromHex(traceHex)
	sid, _ := trace.SpanIDFromHex(spanHex)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// TestInject_SetsTraceparentHeader verifies that Inject writes the W3C
// traceparent header into the message.
func TestInject_SetsTraceparentHeader(t *testing.T) {
	prop := newProp()
	ctx := ctxWithSpan("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")

	msg := &gonnats.Msg{}
	o11ynats.Inject(ctx, prop, msg)

	require.NotNil(t, msg.Header)
	// nats.Header.Get is case-sensitive; the W3C propagator stores the key in
	// MIME canonical form ("Traceparent") via propagation.HeaderCarrier.
	assert.NotEmpty(t, msg.Header["Traceparent"], "traceparent header must be set")
}

// TestInject_InitializesNilHeader verifies that Inject handles a nil Header map.
func TestInject_InitializesNilHeader(t *testing.T) {
	prop := newProp()
	ctx := ctxWithSpan("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	msg := &gonnats.Msg{Header: nil}

	o11ynats.Inject(ctx, prop, msg)

	assert.NotNil(t, msg.Header, "nil Header must be initialized by Inject")
}

// TestExtract_NilHeaderReturnsOriginalCtx verifies that Extract with a nil
// Header returns the original context unchanged.
func TestExtract_NilHeaderReturnsOriginalCtx(t *testing.T) {
	prop := newProp()
	ctx := context.Background()
	msg := &gonnats.Msg{Header: nil}

	extracted := o11ynats.Extract(ctx, prop, msg)

	assert.Equal(t, ctx, extracted, "Extract with nil Header must return original context")
}

// TestInjectExtract_RoundTrip verifies that a span context survives an
// Inject → Extract round trip over NATS message headers.
func TestInjectExtract_RoundTrip(t *testing.T) {
	prop := newProp()

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	msg := &gonnats.Msg{}
	o11ynats.Inject(ctx, prop, msg)
	extractedCtx := o11ynats.Extract(context.Background(), prop, msg)

	got := trace.SpanContextFromContext(extractedCtx)
	assert.True(t, got.IsValid(), "extracted span context must be valid")
	assert.Equal(t, traceID, got.TraceID(), "TraceID must survive round trip")
	assert.Equal(t, spanID, got.SpanID(), "SpanID must survive round trip")
}
