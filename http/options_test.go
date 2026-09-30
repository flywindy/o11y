package http_test

import (
	"context"
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
	"go.opentelemetry.io/otel/trace"

	o11yhttp "github.com/flywindy/o11y/http"
)

// serveOnce sends one GET /orders/1 through a server handler built with opts
// and returns the spans it ended. upstream, when valid, is injected as the
// incoming trace context.
func serveOnce(t *testing.T, mp *sdkmetric.MeterProvider, upstream trace.SpanContext, opts ...o11yhttp.Option) []sdktrace.ReadOnlySpan {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	// An explicit sampler: without one the SDK reads OTEL_TRACES_SAMPLER.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	if mp == nil {
		mp = sdkmetric.NewMeterProvider()
		t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	}
	prop := propagation.TraceContext{}

	served := false
	handler := o11yhttp.NewServerHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}), tp, mp, prop, opts...)

	req := httptest.NewRequest(http.MethodGet, "/orders/1", nil)
	if upstream.IsValid() {
		prop.Inject(trace.ContextWithRemoteSpanContext(t.Context(), upstream), propagation.HeaderCarrier(req.Header))
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)
	require.True(t, served, "the wrapped handler must run whatever the options")
	return recorder.Ended()
}

func TestWithSpanNameFormatter(t *testing.T) {
	spans := serveOnce(t, nil, trace.SpanContext{},
		o11yhttp.WithSpanNameFormatter(func(operation string, r *http.Request) string {
			return "custom " + operation + " " + r.URL.Path
		}),
	)
	require.Len(t, spans, 1)
	assert.Equal(t, "custom http.server /orders/1", spans[0].Name(),
		"the caller's formatter replaces the facade's default, which would name this span GET")
}

func TestWithFilter(t *testing.T) {
	t.Run("excluded", func(t *testing.T) {
		spans := serveOnce(t, nil, trace.SpanContext{},
			o11yhttp.WithFilter(func(*http.Request) bool { return false }),
		)
		assert.Empty(t, spans, "a request the filter rejects gets no span")
	})
	t.Run("included", func(t *testing.T) {
		spans := serveOnce(t, nil, trace.SpanContext{},
			o11yhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path == "/orders/1" }),
		)
		assert.Len(t, spans, 1)
	})
}

// TestWithPublicEndpoint checks that an inbound trace context is not trusted
// as the parent: the server span starts a new trace and links to the caller's
// span instead.
func TestWithPublicEndpoint(t *testing.T) {
	upstream := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})

	t.Run("default trusts the caller", func(t *testing.T) {
		spans := serveOnce(t, nil, upstream)
		require.Len(t, spans, 1)
		assert.Equal(t, upstream.TraceID(), spans[0].SpanContext().TraceID())
	})
	t.Run("public endpoint starts a new trace", func(t *testing.T) {
		spans := serveOnce(t, nil, upstream, o11yhttp.WithPublicEndpoint())
		require.Len(t, spans, 1)
		assert.NotEqual(t, upstream.TraceID(), spans[0].SpanContext().TraceID())
		assert.False(t, spans[0].Parent().IsValid(), "the caller's span must not be the parent")
		require.Len(t, spans[0].Links(), 1)
		assert.Equal(t, upstream.SpanID(), spans[0].Links()[0].SpanContext.SpanID(),
			"the caller's span is kept as a link")
	})
}

func TestWithMetricAttributesFn(t *testing.T) {
	const key = attribute.Key("app.tenant")
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	serveOnce(t, mp, trace.SpanContext{},
		o11yhttp.WithMetricAttributesFn(func(*http.Request) []attribute.KeyValue { //nolint:staticcheck // the deprecated option is what is under test
			return []attribute.KeyValue{key.String("acme")}
		}),
	)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			for _, dp := range hist.DataPoints {
				if v, ok := dp.Attributes.Value(key); ok {
					assert.Equal(t, "acme", v.AsString())
					found = true
				}
			}
		}
	}
	assert.True(t, found, "the request-derived attribute must reach http.server.request.duration")
}
