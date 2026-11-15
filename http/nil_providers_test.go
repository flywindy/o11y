package http_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	o11yhttp "github.com/flywindy/o11y/http"
)

// inboundTraceparent is a valid W3C header for a sampled remote parent.
const inboundTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

// recordingGlobals is what installRecordingGlobals put behind the process-wide
// OpenTelemetry accessors, so a test can see whether anything reached them.
type recordingGlobals struct {
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

// installRecordingGlobals sets a recording tracer provider, a recording meter
// provider and the W3C propagator as the OpenTelemetry globals, and restores
// the previous ones when the test ends.
//
// It stands in for a third-party library that installs globals in the same
// process. Only test code may do this; SDK code never sets a global (ADR 0003).
// A test that calls it must not run in parallel.
func installRecordingGlobals(t *testing.T) recordingGlobals {
	t.Helper()
	g := recordingGlobals{
		spans:  tracetest.NewSpanRecorder(),
		reader: sdkmetric.NewManualReader(),
	}
	prevTP, prevMP, prevProp := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(g.spans),
	))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(g.reader)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})
	return g
}

// assertUntouched fails when a span or a metric reached the globals.
func (g recordingGlobals) assertUntouched(t *testing.T) {
	t.Helper()
	assert.Empty(t, g.spans.Started(), "no span may be started on the global tracer provider")
	var rm metricdata.ResourceMetrics
	require.NoError(t, g.reader.Collect(context.Background(), &rm))
	assert.Empty(t, rm.ScopeMetrics, "no instrument may be created on the global meter provider")
}

// TestNewServerHandler_NilProvidersNeverUseTheGlobals pins that a nil tracer
// provider, meter provider or propagator is replaced by one that does nothing.
//
// otelhttp reads a nil option value as "not set" and falls back to the
// OpenTelemetry globals, so handing the nils through made the handler record
// on whatever another library had installed process-wide — while this
// function's documentation said that never happens.
func TestNewServerHandler_NilProvidersNeverUseTheGlobals(t *testing.T) {
	globals := installRecordingGlobals(t)

	var seen trace.SpanContext
	handler := o11yhttp.NewServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = trace.SpanContextFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}), nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("traceparent", inboundTraceparent)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, "the request is still served")
	globals.assertUntouched(t)
	assert.False(t, seen.IsValid(),
		"the global propagator must not be used to read the caller's trace context either")
}

// TestNewTransport_NilProvidersNeverUseTheGlobals is the client-side
// counterpart: no span or metric on the globals, and no header written by the
// global propagator.
func TestNewTransport_NilProvidersNeverUseTheGlobals(t *testing.T) {
	globals := installRecordingGlobals(t)

	var sent http.Header
	transport := o11yhttp.NewTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = r.Header.Clone()
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
	}), nil, nil, nil)

	// The caller is inside a sampled span, so a propagator that was consulted
	// would have something to write.
	ctx := trace.ContextWithSpanContext(context.Background(), sampledSpanContext())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test/x", nil)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	globals.assertUntouched(t)
	assert.Empty(t, sent.Get("traceparent"), "the global propagator must not write a header")
}

// TestNewTransport_NilPropagatorInjectsNothing pins the case a disabled-signal
// code path produces: real providers, no propagator. The client span is still
// recorded on the provider that was passed, and the request carries no
// traceparent — not the SDK's, and not one written by a global propagator.
func TestNewTransport_NilPropagatorInjectsNothing(t *testing.T) {
	globals := installRecordingGlobals(t)

	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))

	var sent http.Header
	transport := o11yhttp.NewTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = r.Header.Clone()
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
	}), tp, mp, nil)

	ctx, parent := tp.Tracer("test").Start(context.Background(), "caller")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test/x", nil)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	parent.End()

	assert.Empty(t, sent.Get("traceparent"), "no propagator was given, so no header is written")
	assert.Len(t, spans.Ended(), 2, "the caller's span and the client span are on the provider that was passed")
	globals.assertUntouched(t)
}

// sampledSpanContext returns a valid, sampled span context for a caller that
// has no SDK span of its own.
func sampledSpanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c},
		SpanID:     trace.SpanID{0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31},
		TraceFlags: trace.FlagsSampled,
	})
}
