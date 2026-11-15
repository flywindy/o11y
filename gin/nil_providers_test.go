package gin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	ginframework "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	o11ygin "github.com/flywindy/o11y/gin"
)

// installRecordingGlobals sets a recording tracer provider, a recording meter
// provider and the W3C propagator as the OpenTelemetry globals, and restores
// the previous ones when the test ends.
//
// It stands in for a third-party library that installs globals in the same
// process. Only test code may do this; SDK code never sets a global (ADR 0003).
// A test that calls it must not run in parallel.
func installRecordingGlobals(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	prevTP, prevMP, prevProp := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(spans),
	))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})
	return spans, reader
}

// TestMiddleware_NilProvidersNeverUseTheGlobals pins that a nil tracer
// provider, meter provider or propagator is replaced by one that does nothing.
//
// otelgin reads a nil option value as "not set" and falls back to the
// OpenTelemetry globals, so handing the nils through made the middleware
// record on whatever another library had installed process-wide — while this
// function's documentation said that never happens.
func TestMiddleware_NilProvidersNeverUseTheGlobals(t *testing.T) {
	globalSpans, globalReader := installRecordingGlobals(t)

	ginframework.SetMode(ginframework.TestMode)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, nil, nil, nil)...)
	var seen trace.SpanContext
	router.GET(testRoute, func(c *ginframework.Context) {
		seen = trace.SpanContextFromContext(c.Request.Context())
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, testPath, nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "the request is still served")
	assert.Empty(t, globalSpans.Started(), "no span may be started on the global tracer provider")
	var rm metricdata.ResourceMetrics
	require.NoError(t, globalReader.Collect(context.Background(), &rm))
	assert.Empty(t, rm.ScopeMetrics, "no instrument may be created on the global meter provider")
	assert.False(t, seen.IsValid(),
		"the global propagator must not be used to read the caller's trace context either")
}

// TestMiddleware_NilPropagatorExtractsNothing pins the case a disabled-signal
// code path produces: real providers, no propagator. The server span is still
// recorded on the provider that was passed, as a root span — the caller's
// trace context is not read, by the global propagator or any other.
func TestMiddleware_NilPropagatorExtractsNothing(t *testing.T) {
	globalSpans, _ := installRecordingGlobals(t)

	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))

	ginframework.SetMode(ginframework.TestMode)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, tp, mp, nil)...)
	router.GET(testRoute, func(c *ginframework.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, testPath, nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	router.ServeHTTP(httptest.NewRecorder(), req)

	ended := spans.Ended()
	require.Len(t, ended, 1, "the server span is on the provider that was passed")
	assert.False(t, ended[0].Parent().IsValid(), "and it has no remote parent")
	assert.Empty(t, globalSpans.Started())
}
