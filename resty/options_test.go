package resty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	restyclient "github.com/go-resty/resty/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestWithSpanNameFormatter checks the formatter names client spans, and that
// an empty name from it falls back to the default METHOD naming rather than
// producing an unnamed span.
func TestWithSpanNameFormatter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	for _, tc := range []struct {
		name      string
		formatter func(*restyclient.Request) string
		want      string
	}{
		{
			name:      "formatter names the span",
			formatter: func(r *restyclient.Request) string { return "orders-api " + r.Method },
			want:      "orders-api GET",
		},
		{
			name:      "empty name falls back to the default",
			formatter: func(*restyclient.Request) string { return "" },
			want:      "GET",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An explicit sampler: without one the SDK reads OTEL_TRACES_SAMPLER.
			sr := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(sr))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			client := NewClient(tp, sdkmetric.NewMeterProvider(), propagation.TraceContext{}, WithSpanNameFormatter(tc.formatter))

			resp, err := client.R().Get(ts.URL + "/orders/123")
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode())

			spans := endedClientSpans(sr)
			require.Len(t, spans, 1)
			assert.Equal(t, tc.want, spans[0].Name())
		})
	}
}
