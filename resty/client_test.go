package resty

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	restyclient "github.com/go-resty/resty/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

type routeKey struct{}

func TestWrapInjectsTraceContextAndCreatesClientSpan(t *testing.T) {
	tp, mp, sr := testProviders()
	prop := propagation.TraceContext{}

	var traceparent string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	client := NewClient(tp, mp, prop)
	resp, err := client.R().Get(ts.URL + "/orders/123")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode())

	require.NotEmpty(t, traceparent)
	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assert.Equal(t, "GET", spans[0].Name())
	assertAttr(t, spans[0], semconv.HTTPRequestMethodKey, "GET")
	assertAttr(t, spans[0], semconv.HTTPResponseStatusCodeKey, int64(http.StatusCreated))
	assertNoAttr(t, spans[0], semconv.HTTPRequestResendCountKey)
	assertNoAttr(t, spans[0], restyErrorKindKey)
}

func TestWrapIsIdempotent(t *testing.T) {
	tp, mp, sr := testProviders()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := restyclient.New()
	Wrap(client, tp, mp, propagation.TraceContext{})
	Wrap(client, tp, mp, propagation.TraceContext{})

	resp, err := client.R().Get(ts.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode())
	assert.Len(t, endedClientSpans(sr), 1)
}

func TestRetrySuccessCreatesSiblingSpansAndInjectsPerAttemptTraceparent(t *testing.T) {
	tp, mp, sr := testProviders()
	prop := propagation.TraceContext{}
	var count atomic.Int32
	var headers []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("traceparent"))
		if count.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(tp, mp, prop)
	client.SetRetryCount(1).
		SetRetryWaitTime(time.Millisecond).
		SetRetryMaxWaitTime(time.Millisecond).
		AddRetryCondition(func(resp *restyclient.Response, _ error) bool {
			return resp != nil && resp.StatusCode() == http.StatusServiceUnavailable
		})

	parentCtx, parent := tp.Tracer("test").Start(context.Background(), "caller")
	resp, err := client.R().SetContext(parentCtx).Get(ts.URL + "/orders/123")
	parent.End()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode())

	spans := endedClientSpans(sr)
	require.Len(t, spans, 2)
	assert.Equal(t, parent.SpanContext().SpanID(), spans[0].Parent().SpanID())
	assert.Equal(t, parent.SpanContext().SpanID(), spans[1].Parent().SpanID())
	assertAttr(t, spans[0], semconv.HTTPResponseStatusCodeKey, int64(http.StatusServiceUnavailable))
	assertNoAttr(t, spans[0], semconv.HTTPRequestResendCountKey)
	assertAttr(t, spans[1], semconv.HTTPResponseStatusCodeKey, int64(http.StatusOK))
	assertAttr(t, spans[1], semconv.HTTPRequestResendCountKey, int64(1))

	require.Len(t, headers, 2)
	first := traceparentParts(t, headers[0])
	second := traceparentParts(t, headers[1])
	assert.Equal(t, parent.SpanContext().TraceID().String(), first.traceID)
	assert.Equal(t, parent.SpanContext().TraceID().String(), second.traceID)
	assert.NotEqual(t, first.spanID, second.spanID)
	assert.Equal(t, spans[0].SpanContext().SpanID().String(), first.spanID)
	assert.Equal(t, spans[1].SpanContext().SpanID().String(), second.spanID)
}

func TestTransportRetryExhaustedMarksLastAttempt(t *testing.T) {
	tp, mp, sr := testProviders()
	addr := unusedTCPAddr(t)
	client := NewClient(tp, mp, propagation.TraceContext{})
	client.SetRetryCount(2).
		SetRetryWaitTime(time.Millisecond).
		SetRetryMaxWaitTime(time.Millisecond)

	_, err := client.R().Get("http://" + addr + "/unreachable")
	require.Error(t, err)

	spans := endedClientSpans(sr)
	require.Len(t, spans, 3)
	for i, span := range spans {
		assertAttr(t, span, restyErrorKindKey, "transport")
		assertAttr(t, span, semconv.ErrorTypeKey, "*net.OpError")
		if i == len(spans)-1 {
			assertAttr(t, span, restyRetryExhaustedKey, true)
			continue
		}
		assertNoAttr(t, span, restyRetryExhaustedKey)
	}
}

func TestServerTimeoutStatusSetsRestyErrorKind(t *testing.T) {
	tp, mp, sr := testProviders()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer ts.Close()

	client := NewClient(tp, mp, propagation.TraceContext{})
	resp, err := client.R().Get(ts.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode())

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assertAttr(t, spans[0], restyErrorKindKey, "server_timeout")
	assert.Equal(t, codes.Error, spans[0].Status().Code)
}

func TestRouteFromContextControlsSpanNameAndMetricRoute(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(tp, mp, propagation.TraceContext{},
		WithRouteFromContext(routeKey{}),
		WithMetricRouteEnabled(true),
	)
	ctx := context.WithValue(context.Background(), routeKey{}, "/orders/{id}")
	resp, err := client.R().SetContext(ctx).Get(ts.URL + "/orders/123")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode())

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assert.Equal(t, "GET /orders/{id}", spans[0].Name())
	assertAttr(t, spans[0], semconv.HTTPRouteKey, "/orders/{id}")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	point := findClientDurationPoint(t, rm)
	got, ok := point.Attributes.Value(semconv.HTTPRouteKey)
	require.True(t, ok)
	assert.Equal(t, "/orders/{id}", got.AsString())
}

func TestRestyErrorKindTaxonomy(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		errType string
		kind    string
	}{
		{
			name:    "client canceled",
			err:     context.Canceled,
			errType: "context.Canceled",
			kind:    "client_canceled",
		},
		{
			name:    "client timeout",
			err:     context.DeadlineExceeded,
			errType: "context.DeadlineExceeded",
			kind:    "client_timeout",
		},
		{
			name:    "tls",
			err:     &tls.CertificateVerificationError{},
			errType: "*tls.CertificateVerificationError",
			kind:    "tls",
		},
		{
			name:    "transport",
			err:     &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			errType: "*net.OpError",
			kind:    "transport",
		},
		{
			name:    "protocol",
			err:     errors.New("http2: stream closed"),
			errType: "*errors.errorString",
			kind:    "protocol",
		},
		{
			name:    "unknown",
			err:     errors.New("boom"),
			errType: "*errors.errorString",
			kind:    "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.errType, errorType(tt.err))
			assert.Equal(t, tt.kind, restyErrorKind(tt.err))
		})
	}
}

func testProviders() (trace.TracerProvider, *sdkmetric.MeterProvider, *tracetest.SpanRecorder) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return tp, mp, sr
}

func endedClientSpans(sr *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	all := sr.Ended()
	out := make([]sdktrace.ReadOnlySpan, 0, len(all))
	for _, span := range all {
		if span.InstrumentationScope().Name == instrumentationName {
			out = append(out, span)
		}
	}
	return out
}

func assertAttr(t *testing.T, span sdktrace.ReadOnlySpan, key attribute.Key, want any) {
	t.Helper()
	for _, attr := range span.Attributes() {
		if attr.Key != key {
			continue
		}
		switch w := want.(type) {
		case string:
			assert.Equal(t, w, attr.Value.AsString())
		case int64:
			assert.Equal(t, w, attr.Value.AsInt64())
		case bool:
			assert.Equal(t, w, attr.Value.AsBool())
		default:
			t.Fatalf("unsupported attr assertion type %T", want)
		}
		return
	}
	t.Fatalf("attribute %s not found on span %s", key, span.Name())
}

func assertNoAttr(t *testing.T, span sdktrace.ReadOnlySpan, key attribute.Key) {
	t.Helper()
	for _, attr := range span.Attributes() {
		if attr.Key == key {
			t.Fatalf("attribute %s unexpectedly found on span %s", key, span.Name())
		}
	}
}

func unusedTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

type traceparent struct {
	traceID string
	spanID  string
}

func traceparentParts(t *testing.T, header string) traceparent {
	t.Helper()
	parts := strings.Split(header, "-")
	require.Len(t, parts, 4, fmt.Sprintf("invalid traceparent %q", header))
	return traceparent{traceID: parts[1], spanID: parts[2]}
}

func findClientDurationPoint(t *testing.T, rm metricdata.ResourceMetrics) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "http.client.request.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			require.NotEmpty(t, hist.DataPoints)
			return hist.DataPoints[0]
		}
	}
	t.Fatal("http.client.request.duration metric not found")
	return metricdata.HistogramDataPoint[float64]{}
}
