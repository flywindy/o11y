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

func ctxWithSpan(t *testing.T, traceHex, spanHex string) context.Context {
	t.Helper()
	tid, err := trace.TraceIDFromHex(traceHex)
	require.NoError(t, err, "invalid trace ID hex %q", traceHex)
	sid, err := trace.SpanIDFromHex(spanHex)
	require.NoError(t, err, "invalid span ID hex %q", spanHex)
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
	ctx := ctxWithSpan(t, "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")

	msg := &gonnats.Msg{}
	o11ynats.Inject(ctx, prop, msg)

	require.NotNil(t, msg.Header)
	// nats.Header.Get/Set is case-sensitive (unlike http.Header); Inject uses
	// headerCarrier, which writes the literal-case key the W3C propagator
	// passes in ("traceparent", lowercase per the W3C spec) with no MIME
	// canonicalization, matching how otel-nats itself writes NATS headers.
	assert.NotEmpty(t, msg.Header["traceparent"], "traceparent header must be set")
}

// TestInject_InitializesNilHeader verifies that Inject handles a nil Header map.
func TestInject_InitializesNilHeader(t *testing.T) {
	prop := newProp()
	ctx := ctxWithSpan(t, "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
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

// TestExtract_LiteralCaseHeaderKey locks down the interop fix at the heart of
// this package: Extract must read a header keyed by the literal lowercase
// "traceparent" nats.go's case-sensitive Header uses — the exact form
// otel-nats itself writes, and the form a W3C-compliant non-Go client (e.g.
// the nats.ws browser example) would also use. Before this fix, Extract went
// through propagation.HeaderCarrier, which is backed by http.Header and
// canonicalizes lookups to "Traceparent", silently missing this header and
// returning ctx unchanged instead of the extracted span context.
func TestExtract_LiteralCaseHeaderKey(t *testing.T) {
	prop := newProp()
	msg := &gonnats.Msg{
		Header: gonnats.Header{
			"traceparent": []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		},
	}

	extracted := o11ynats.Extract(context.Background(), prop, msg)

	got := trace.SpanContextFromContext(extracted)
	require.True(t, got.IsValid(), "Extract must read a literal-case lowercase traceparent header")
	wantTraceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	assert.Equal(t, wantTraceID, got.TraceID())
}

// TestExtract_CanonicalCaseHeaderKeyFallback locks down backward compatibility
// with messages written by a pre-fix SDK version: before this package's
// Inject switched to the case-sensitive headerCarrier, it went through
// propagation.HeaderCarrier (http.Header-backed), which canonicalizes
// "traceparent" to "Traceparent" on write. A JetStream message sitting
// unconsumed in a durable stream across an SDK upgrade would carry that
// canonicalized key; Extract must still find it via the MIME-canonical
// fallback rather than silently losing the trace link.
func TestExtract_CanonicalCaseHeaderKeyFallback(t *testing.T) {
	prop := newProp()
	msg := &gonnats.Msg{
		Header: gonnats.Header{
			"Traceparent": []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		},
	}

	extracted := o11ynats.Extract(context.Background(), prop, msg)

	got := trace.SpanContextFromContext(extracted)
	require.True(t, got.IsValid(), "Extract must fall back to the MIME-canonical Traceparent key")
	wantTraceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	assert.Equal(t, wantTraceID, got.TraceID())
}
