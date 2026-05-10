package http_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	o11yhttp "github.com/flywindy/o11y/http"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestNewTransport_InjectsTraceContextAndCreatesClientSpan(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	prop := propagation.TraceContext{}

	var traceparent string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		traceparent = r.Header.Get("traceparent")
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})

	client := &http.Client{
		Transport: o11yhttp.NewTransport(base, tp, mp, prop),
	}
	resp, err := client.Get("http://example.test/orders")
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.NotEmpty(t, traceparent)
	spans := spanRecorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "HTTP GET", spans[0].Name())
	assert.True(t, spans[0].SpanContext().IsValid())
}
