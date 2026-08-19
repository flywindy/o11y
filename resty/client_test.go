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
	"go.opentelemetry.io/otel/baggage"
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

func TestWrapNilPropagatorDefaultsToTraceContext(t *testing.T) {
	tp, mp, _ := testProviders()
	var traceparent string
	var baggageHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("traceparent")
		baggageHeader = r.Header.Get("baggage")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	member, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	bag, err := baggage.New(member)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(context.Background(), bag)
	parentCtx, parent := tp.Tracer("test").Start(ctx, "caller")
	defer parent.End()

	client := NewClient(tp, mp, nil)
	resp, err := client.R().SetContext(parentCtx).Get(ts.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode())

	parts := traceparentParts(t, traceparent)
	assert.Equal(t, parent.SpanContext().TraceID().String(), parts.traceID)
	assert.Empty(t, baggageHeader)
}

func TestWrapRecordsComposedRestyURL(t *testing.T) {
	tp, mp, sr := testProviders()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(tp, mp, propagation.TraceContext{})
	client.SetBaseURL(ts.URL)

	resp, err := client.R().
		SetPathParam("id", "123").
		SetQueryParam("include", "items").
		Get("/orders/{id}")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode())

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assertAttr(t, spans[0], semconv.URLFullKey, ts.URL+"/orders/123?include=items")
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
	assertAttr(t, spans[0], semconv.ErrorTypeKey, "503")
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
	assertAttr(t, spans[0], semconv.ErrorTypeKey, "504")
	assert.Equal(t, codes.Error, spans[0].Status().Code)
}

func TestNilRouteContextKeyDoesNotPanic(t *testing.T) {
	tp, mp, sr := testProviders()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(tp, mp, propagation.TraceContext{},
		WithRouteFromContext(nil),
		WithMetricRouteEnabled(true),
	)
	resp, err := client.R().Get(ts.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode())

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assertNoAttr(t, spans[0], semconv.HTTPRouteKey)
}

func TestResponseErrorPreservesHTTPStatusCode(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("too large"))
	}))
	defer ts.Close()

	client := NewClient(tp, mp, propagation.TraceContext{})
	client.SetResponseBodyLimit(1)

	_, err := client.R().Get(ts.URL + "/too-large")
	require.Error(t, err)

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assertAttr(t, spans[0], semconv.HTTPResponseStatusCodeKey, int64(http.StatusInternalServerError))
	assertAttr(t, spans[0], semconv.URLFullKey, ts.URL+"/too-large")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	point := findClientDurationPoint(t, rm)
	statusCode, ok := point.Attributes.Value(semconv.HTTPResponseStatusCodeKey)
	require.True(t, ok)
	assert.Equal(t, int64(http.StatusInternalServerError), statusCode.AsInt64())
}

func TestResponseMiddlewareErrorFinishesSpanAsError(t *testing.T) {
	tp, mp, sr := testProviders()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(tp, mp, propagation.TraceContext{})
	client.OnAfterResponse(func(_ *restyclient.Client, _ *restyclient.Response) error {
		return errors.New("response middleware failed")
	})

	_, err := client.R().Get(ts.URL + "/middleware")
	require.Error(t, err)

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assertAttr(t, spans[0], semconv.HTTPResponseStatusCodeKey, int64(http.StatusOK))
	assertAttr(t, spans[0], semconv.URLFullKey, ts.URL+"/middleware")
	assertAttr(t, spans[0], semconv.ErrorTypeKey, "*resty.ResponseError")
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

// testProvidersWithReader mirrors testProviders but also returns the reader, so
// a test can assert on recorded duration samples.
func testProvidersWithReader() (trace.TracerProvider, *sdkmetric.MeterProvider, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return tp, mp, sr, reader
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

// ---------------------------------------------------------------------------
// Retry-driven span completion
//
// resty owns the retry decision and evaluates the request's own conditions
// appended to the client's. Request.AddRetryCondition writes to an unexported
// field, so the wrapper cannot reproduce that decision and must consume resty's
// retry hook instead of re-deriving it.
// ---------------------------------------------------------------------------

// retryConditionStyles registers the same 503-retry rule the two ways resty
// supports. Telemetry must not depend on which one a caller picked.
var retryConditionStyles = []struct {
	name  string
	apply func(*restyclient.Client, *restyclient.Request)
}{
	{"client-level", func(c *restyclient.Client, _ *restyclient.Request) {
		c.AddRetryCondition(func(resp *restyclient.Response, _ error) bool {
			return resp != nil && resp.StatusCode() == http.StatusServiceUnavailable
		})
	}},
	{"request-level", func(_ *restyclient.Client, r *restyclient.Request) {
		r.AddRetryCondition(func(resp *restyclient.Response, _ error) bool {
			return resp != nil && resp.StatusCode() == http.StatusServiceUnavailable
		})
	}},
}

// TestRetryEndsEverySpanForBothConditionStyles is the regression test: a
// request that retried on a condition registered with req.AddRetryCondition
// used to leave every attempt span but the last unended, and its duration
// sample unrecorded, because the wrapper asked the client for conditions the
// request held privately.
func TestRetryEndsEverySpanForBothConditionStyles(t *testing.T) {
	for _, style := range retryConditionStyles {
		t.Run(style.name, func(t *testing.T) {
			tp, mp, sr, reader := testProvidersWithReader()
			var count atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if count.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer ts.Close()

			client := NewClient(tp, mp, propagation.TraceContext{})
			client.SetRetryCount(1).
				SetRetryWaitTime(time.Millisecond).
				SetRetryMaxWaitTime(time.Millisecond)
			req := client.R()
			style.apply(client, req)

			resp, err := req.Get(ts.URL)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode())
			require.EqualValues(t, 2, count.Load(), "the server must have seen both attempts")

			spans := endedClientSpans(sr)
			require.Len(t, spans, 2, "every attempt must produce an ended span")
			assertAttr(t, spans[0], semconv.HTTPResponseStatusCodeKey, int64(http.StatusServiceUnavailable))
			assertAttr(t, spans[1], semconv.HTTPResponseStatusCodeKey, int64(http.StatusOK))
			assertAttr(t, spans[1], semconv.HTTPRequestResendCountKey, int64(1))

			assert.EqualValues(t, 2, totalClientDurationSamples(t, reader),
				"each attempt must record an http.client.request.duration sample")
		})
	}
}

// TestRetryExhaustedMarkedForBothConditionStyles pins the attribute that
// depended on the same blind re-derivation: the wrapper now takes "resty
// decided to retry" from the hook firing rather than recomputing it, so the
// marker survives on the request-level path too.
func TestRetryExhaustedMarkedForBothConditionStyles(t *testing.T) {
	for _, style := range retryConditionStyles {
		t.Run(style.name, func(t *testing.T) {
			tp, mp, sr, reader := testProvidersWithReader()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer ts.Close()

			client := NewClient(tp, mp, propagation.TraceContext{})
			client.SetRetryCount(1).
				SetRetryWaitTime(time.Millisecond).
				SetRetryMaxWaitTime(time.Millisecond)
			req := client.R()
			style.apply(client, req)

			resp, err := req.Get(ts.URL)
			require.NoError(t, err)
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode())

			spans := endedClientSpans(sr)
			require.Len(t, spans, 2)
			assertNoAttr(t, spans[0], restyRetryExhaustedKey)
			assertAttr(t, spans[1], restyRetryExhaustedKey, true)
			assert.EqualValues(t, 2, totalClientDurationSamples(t, reader))
		})
	}
}

// TestRetrySpanTargetAttributesSurviveTheRetryPath guards the client reference
// the retry hook now carries: resty's hook signature passes no client, and
// resolving the target without one loses server.address exactly when a host is
// misbehaving.
func TestRetrySpanTargetAttributesSurviveTheRetryPath(t *testing.T) {
	tp, mp, sr := testProviders()
	var count atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if count.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(tp, mp, propagation.TraceContext{})
	client.SetRetryCount(1).
		SetRetryWaitTime(time.Millisecond).
		SetRetryMaxWaitTime(time.Millisecond).
		SetBaseURL(ts.URL)
	req := client.R()
	req.AddRetryCondition(func(resp *restyclient.Response, _ error) bool {
		return resp != nil && resp.StatusCode() == http.StatusServiceUnavailable
	})

	// A relative path: only the client's base URL can resolve it to a host.
	_, err := req.Get("/orders")
	require.NoError(t, err)

	spans := endedClientSpans(sr)
	require.Len(t, spans, 2)
	host, _, splitErr := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	require.NoError(t, splitErr)
	assertAttr(t, spans[0], semconv.ServerAddressKey, host)
}

// TestPanicDuringRequestEndsSpan covers the last path that could leave a span
// open: resty unwinds a panic through its panic hooks only.
func TestPanicDuringRequestEndsSpan(t *testing.T) {
	tp, mp, sr, reader := testProvidersWithReader()
	client := NewClient(tp, mp, propagation.TraceContext{})
	// Registered after the wrapper's own before-request hook, so the span
	// exists by the time this runs.
	client.OnBeforeRequest(func(_ *restyclient.Client, _ *restyclient.Request) error {
		panic("middleware exploded")
	})

	require.Panics(t, func() {
		_, _ = client.R().Get("http://127.0.0.1:1/")
	})

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1, "the attempt span must be ended even when the request panics")
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assert.EqualValues(t, 1, totalClientDurationSamples(t, reader))
}

// TestPanicBeforeRawRequestKeepsServerAddress pins that the early-failure path
// still resolves its target. resty runs before-request hooks before it builds
// RawRequest, so a relative URL has nothing to resolve against except the
// client's base URL — and the hook, not the request, is what holds the client.
func TestPanicBeforeRawRequestKeepsServerAddress(t *testing.T) {
	tp, mp, sr, _ := testProvidersWithReader()
	client := NewClient(tp, mp, propagation.TraceContext{})
	client.SetBaseURL("http://orders.internal:8080")
	client.OnBeforeRequest(func(_ *restyclient.Client, _ *restyclient.Request) error {
		panic("middleware exploded")
	})

	require.Panics(t, func() {
		_, _ = client.R().Get("/orders")
	})

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assertAttr(t, spans[0], semconv.ServerAddressKey, "orders.internal")
	assertAttr(t, spans[0], semconv.ServerPortKey, int64(8080))
}

// totalClientDurationSamples sums http.client.request.duration counts across
// every data point, since attempts differ in their attribute sets.
func totalClientDurationSamples(t *testing.T, reader *sdkmetric.ManualReader) uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var total uint64
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "http.client.request.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			for _, dp := range hist.DataPoints {
				total += dp.Count
			}
		}
	}
	return total
}
