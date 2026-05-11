package http_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	o11yhttp "github.com/flywindy/o11y/http"
)

func TestNewServerHandler_ThreadsProvidersAndPropagator(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.duration"},
			sdkmetric.Stream{
				AttributeFilter: attribute.NewAllowKeysFilter(
					semconv.HTTPRequestMethodKey,
					semconv.HTTPRouteKey,
					semconv.HTTPResponseStatusCodeKey,
				),
			},
		)),
	)
	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	prop := propagation.TraceContext{}

	var handlerTraceID trace.TraceID
	var handlerSpanID trace.SpanID
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		sc := trace.SpanContextFromContext(r.Context())
		handlerTraceID = sc.TraceID()
		handlerSpanID = sc.SpanID()
		w.WriteHeader(http.StatusAccepted)
	})
	handler := o11yhttp.NewServerHandler(mux, tp, mp, prop, "http.server")

	upstreamTraceID := trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	upstreamSpanID := trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
	ctx := trace.ContextWithRemoteSpanContext(t.Context(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    upstreamTraceID,
		SpanID:     upstreamSpanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))
	req := httptest.NewRequest(http.MethodGet, "/orders/123", nil)
	prop.Inject(ctx, propagation.HeaderCarrier(req.Header))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, upstreamTraceID, handlerTraceID)
	assert.NotEqual(t, upstreamSpanID, handlerSpanID)

	spans := spanRecorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET /orders/{id}", spans[0].Name())
	assert.Equal(t, upstreamTraceID, spans[0].SpanContext().TraceID())
	assert.Equal(t, upstreamSpanID, spans[0].Parent().SpanID())

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Len(t, rm.ScopeMetrics, 1)
	require.NotEmpty(t, rm.ScopeMetrics[0].Metrics)
	assertHTTPDurationLabels(t, rm)
}

func assertHTTPDurationLabels(t *testing.T, rm metricdata.ResourceMetrics) {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "http.server.request.duration" {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			require.NotEmpty(t, histogram.DataPoints)
			attrs := histogram.DataPoints[0].Attributes
			assert.Equal(t, 3, attrs.Len())
			assertAttr(t, attrs, semconv.HTTPRequestMethodKey, "GET")
			assertAttr(t, attrs, semconv.HTTPRouteKey, "/orders/{id}")
			assertAttr(t, attrs, semconv.HTTPResponseStatusCodeKey, "202")
			return
		}
	}
	t.Fatal("http.server.request.duration metric not found")
}

func assertAttr(t *testing.T, attrs attribute.Set, key attribute.Key, want string) {
	t.Helper()
	value, ok := attrs.Value(key)
	require.True(t, ok, "missing attribute %s", key)
	assert.Equal(t, want, value.Emit())
}
