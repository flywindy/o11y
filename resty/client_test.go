package resty

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

	"github.com/flywindy/o11y/internal/redact"
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

// TestWrapRedactsCredentialsFromURLFull pins that the url.full span attribute
// carries neither the request's userinfo nor a presigned URL's signature.
//
// A span attribute leaves the process exactly as a log record does. otelhttp,
// the SDK's other client facade, already strips userinfo before emitting
// url.full, so the same call used to be redacted through o11yhttp.NewTransport
// and not through this one. semconv v1.39.0 asks for the query parameters too.
func TestWrapRedactsCredentialsFromURLFull(t *testing.T) {
	tp, mp, sr := testProviders()
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "http://")
	client := NewClient(tp, mp, propagation.TraceContext{})

	resp, err := client.R().
		SetQueryParam("Signature", "abc+def").
		SetQueryParam("include", "items").
		Get("http://bob:hunter2@" + host + "/orders")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode())

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	full := attrValue(t, spans[0], semconv.URLFullKey).AsString()

	assert.NotContains(t, full, "hunter2", "the request's password must not reach url.full")
	// The signature is checked in its encoded form: resty escapes the value
	// into the query, so "abc+def" never appears literally and asserting on it
	// would pass whether or not the redaction ran.
	assert.NotContains(t, full, "abc%2Bdef", "nor the signature of a presigned URL")
	assert.Contains(t, full, host, "the server still has to be identifiable")
	assert.Contains(t, full, "include=items", "and the rest of the query is left alone")

	// The redaction is for the attribute only: the request itself still
	// authenticates, so clearing userinfo on the caller's URL would break it.
	assert.NotEmpty(t, gotAuth, "userinfo must still reach the server as Basic auth")
}

// TestRequestTarget_FailsClosedOnUnresolvableURLs pins that the fallbacks
// taken before resty has built a RawRequest never put a raw URL on the span.
//
// finishError reads the target before RawRequest exists, so these paths run in
// practice. Two shapes get past an IsAbs check while still carrying a
// credential: a scheme-relative URL, where url.Parse sets User but reports the
// URL as not absolute, and an opaque one, where the credential lands in Opaque
// and User stays nil.
func TestRequestTarget_FailsClosedOnUnresolvableURLs(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{
			name: "scheme-relative URL carries userinfo past IsAbs",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			rawURL: "//bob:hunter2@api.example.com/orders",
			want:   "//redacted@api.example.com/orders",
		},
		{
			name: "an opaque URL hides the credential from the parser",
			// #nosec G101 -- fabricated fixture URL, not a live credential
			// nosemgrep: gosec.G101-1
			rawURL: "http:bob:hunter2@api.example.com",
			want:   "[endpoint redacted]",
		},
		{
			name:   "an ordinary relative path is left alone",
			rawURL: "/orders/123?include=items",
			want:   "/orders/123?include=items",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A nil client is the no-base-URL fallback; req.RawRequest is nil
			// because resty has not built one yet.
			target := requestTarget(nil, &restyclient.Request{URL: tt.rawURL})

			assert.NotContains(t, target.fullURL, "hunter2")
			assert.Equal(t, tt.want, target.fullURL)
		})
	}
}

// TestRequestTarget_DropsAnUnparseableURL pins that a URL the parser rejects
// contributes nothing: it could be hiding a credential in any position, and
// there is no reading of it that says otherwise.
func TestRequestTarget_DropsAnUnparseableURL(t *testing.T) {
	target := requestTarget(nil, &restyclient.Request{URL: "http://%zz"})

	assert.Empty(t, target.fullURL)
}

// TestWrapRedactsCredentialsFromTheErrorSpan pins that a failed request does
// not export through the span's error fields what url.full had removed.
//
// Go's client error is a *url.Error holding the request URL: net/http masks the
// password there but keeps the username and the whole query. That text reaches
// the span twice — as the status description and as exception.message — so
// redacting only the attribute would move the credential rather than remove it.
func TestWrapRedactsCredentialsFromTheErrorSpan(t *testing.T) {
	tp, mp, sr := testProviders()
	client := NewClient(tp, mp, propagation.TraceContext{})

	// Port 1 refuses immediately, so the failure is a transport error whose
	// *url.Error carries the request URL.
	_, err := client.R().
		SetQueryParam("Signature", "abc+def").
		Get("http://bob:hunter2@127.0.0.1:1/orders")
	require.Error(t, err)

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	span := spans[0]

	assert.NotContains(t, span.Status().Description, "bob",
		"the username survives net/http's masking, so the SDK has to remove it")
	assert.NotContains(t, span.Status().Description, "abc%2Bdef",
		"and so does a presigned URL's signature")
	assert.Contains(t, span.Status().Description, "127.0.0.1:1",
		"the server still has to be identifiable")

	var exception sdktrace.Event
	for _, event := range span.Events() {
		if event.Name == semconv.ExceptionEventName {
			exception = event
		}
	}
	require.Equal(t, semconv.ExceptionEventName, exception.Name, "the error is still recorded as an exception")

	var message, exceptionType string
	for _, attr := range exception.Attributes {
		switch attr.Key {
		case semconv.ExceptionMessageKey:
			message = attr.Value.AsString()
		case semconv.ExceptionTypeKey:
			exceptionType = attr.Value.AsString()
		}
	}
	assert.NotContains(t, message, "bob")
	assert.NotContains(t, message, "abc%2Bdef")
	// The regression to catch is the exception being recorded as a redaction
	// wrapper. The hook is handed resty's own *resty.ResponseError — not the
	// *url.Error that Get returns to the caller — and the redaction still
	// reaches the *url.Error inside it through errors.As.
	assert.NotContains(t, exceptionType, "redact",
		"the exception must not be recorded as the redaction wrapper's type")
	assert.Equal(t, "*resty.ResponseError", exceptionType,
		"it is the error the hook was handed, unchanged")

}

// TestWrapRedactsANestedTransportError pins that a credential inside a URL the
// SDK never saw — one a custom RoundTripper put in its own error — does not
// reach either of the span's error fields.
//
// resty accepts a custom transport, and net/http wraps whatever it returns in
// an outer *url.Error. The inner one carries its own URL, quoted into the same
// message, and a signed query holds its credential with no "@" anywhere, so
// neither substituting the outer URL nor the closed at-sign rule would catch
// it. A URL that cannot be parsed takes the whole message with it; url.full,
// server.address and error.type still identify the request.
func TestWrapRedactsANestedTransportError(t *testing.T) {
	tp, mp, sr := testProviders()
	client := NewClient(tp, mp, propagation.TraceContext{})
	client.SetTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		// #nosec G101 -- fabricated fixture URL, not a live credential
		// nosemgrep: gosec.G101-1
		return nil, &url.Error{Op: "Get", URL: "https://inner/%zz?Signature=s3cret", Err: errors.New("dial")}
	}))

	_, err := client.R().Get("https://api.example.com/orders")
	require.Error(t, err)
	require.Contains(t, err.Error(), "s3cret", "the premise: the credential is in the error resty is handed")

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	span := spans[0]

	assert.NotContains(t, span.Status().Description, "s3cret")
	for _, event := range span.Events() {
		for _, attr := range event.Attributes {
			assert.NotContains(t, attr.Value.AsString(), "s3cret",
				"no span event may carry it either")
		}
	}
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestExceptionType_MatchesTheSDKRendering pins that the exception type this
// package records is the one span.RecordError would have recorded.
//
// Building the event by hand was necessary to keep the type while redacting the
// message, but it put the naming in this package's hands. The OTel SDK reports
// a named type with its full import path, which is what semconv asks for and
// what a backend groups on; %T reports the short package name instead. A
// pointer type renders the same either way, so a named non-pointer error — the
// shape a caller's own OnBeforeRequest hook can return — is what separates them.
func TestExceptionType_MatchesTheSDKRendering(t *testing.T) {
	for _, err := range []error{
		namedError{},          // a named non-pointer type: the case %T gets wrong
		&namedError{},         // a pointer to it
		errors.New("builtin"), // an unexported stdlib type
		&url.Error{Op: "Get"}, // the shape the client path actually produces
	} {
		t.Run(sdkExceptionType(err), func(t *testing.T) {
			assert.Equal(t, sdkExceptionType(err), exceptionType(err))
		})
	}
}

// namedError is a named, non-pointer error type, the shape whose rendering
// differs between %T and the SDK's.
type namedError struct{}

// Error implements error.
func (namedError) Error() string { return "named" }

// sdkExceptionType reproduces the pinned OTel SDK's typeStr
// (sdk/trace/span.go), so the assertion is against the upstream rule rather
// than against a copy of this package's own implementation.
func sdkExceptionType(i any) string {
	t := reflect.TypeOf(i)
	if t.PkgPath() == "" && t.Name() == "" {
		return t.String()
	}
	return fmt.Sprintf("%s.%s", t.PkgPath(), t.Name())
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

// attrValue returns the value span recorded for key, failing the test when the
// attribute is absent.
func attrValue(t *testing.T, span sdktrace.ReadOnlySpan, key attribute.Key) attribute.Value {
	t.Helper()
	for _, attr := range span.Attributes() {
		if attr.Key == key {
			return attr.Value
		}
	}
	t.Fatalf("span %q has no %s attribute", span.Name(), key)
	return attribute.Value{}
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

// TestClonedClientResolvesAgainstItsOwnBaseURL pins the reason the target is
// captured in the before-request hook rather than from a client stored at Wrap
// time. Client.Clone() shallow-copies the hook slice, so a clone shares the
// wrapper's hooks; a captured client reference would still point at the
// original and resolve a relative URL against the wrong base.
func TestClonedClientResolvesAgainstItsOwnBaseURL(t *testing.T) {
	tp, mp, sr, _ := testProvidersWithReader()
	original := NewClient(tp, mp, propagation.TraceContext{})
	original.SetBaseURL("http://original.internal:1111")

	clone := original.Clone()
	clone.SetBaseURL("http://cloned.internal:2222")
	clone.OnBeforeRequest(func(_ *restyclient.Client, _ *restyclient.Request) error {
		// Fail before resty builds RawRequest, so only the client's base URL
		// can resolve the relative path.
		return errors.New("middleware refused the request")
	})

	_, err := clone.R().Get("/orders")
	require.Error(t, err)

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assertAttr(t, spans[0], semconv.ServerAddressKey, "cloned.internal")
	assertAttr(t, spans[0], semconv.ServerPortKey, int64(2222))
}

// authReportingTransport is the shape the review named: a RoundTripper that
// names the Authorization header it was handed. A proxy wrapper, an auth
// middleware or a retry logger can all do this.
type authReportingTransport struct{}

func (authReportingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	//nolint:err113 // the point is an error naming the header, not a sentinel
	return nil, fmt.Errorf("upstream refused Authorization: %s", r.Header.Get("Authorization"))
}

// TestWrapRedactsTheURLDerivedBasicHeaderFromTheErrorSpan pins the credential
// a transport sees in place of the URL's userinfo.
//
// http.Client converts "user:pass@host" into "Authorization: Basic
// base64(user:pass)" before RoundTrip is called, so a transport never sees the
// userinfo — it sees an opaque token containing neither half as a substring.
// No URL redaction can recognise it, which is why it has to be named as a
// secret rather than found.
func TestWrapRedactsTheURLDerivedBasicHeaderFromTheErrorSpan(t *testing.T) {
	tp, mp, sr := testProviders()
	client := NewClient(tp, mp, propagation.TraceContext{})
	client.SetTransport(authReportingTransport{})

	// #nosec G101 -- fabricated fixture credential, not a live one
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const password = "hunter2Secret"
	token := base64.StdEncoding.EncodeToString([]byte("bob:" + password))
	require.NotContains(t, token, password, "the premise: the wire form hides the password")

	_, err := client.R().Get("http://bob:" + password + "@127.0.0.1:1/orders")
	require.Error(t, err)

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	span := spans[0]

	var message string
	for _, event := range span.Events() {
		if event.Name != semconv.ExceptionEventName {
			continue
		}
		for _, attr := range event.Attributes {
			if attr.Key == semconv.ExceptionMessageKey {
				message = attr.Value.AsString()
			}
		}
	}
	require.NotEmpty(t, message, "the error is recorded as an exception")

	for name, text := range map[string]string{
		"status description": span.Status().Description,
		"exception.message":  message,
	} {
		assert.NotContains(t, text, token, name+" must not carry the derived credential")
		assert.NotContains(t, text, password, name+" must not carry the password either")
	}
}

// TestWrapRedactsTheConfiguredRequestCredentials pins that a credential the
// caller configured — rather than one this SDK derived from the URL — is
// removed from both of the span's error fields.
//
// urlDerivedSecrets covered only the Basic header http.Client builds out of a
// URL's userinfo. A caller who uses SetAuthToken or sets Authorization directly
// hands the token to a transport this package does not own, and a proxy
// wrapper, an auth middleware or a retry logger that names the header it was
// given puts it in an error. It is not a URL, so nothing in the URL redaction
// has anything to act on: it has to be named.
func TestWrapRedactsTheConfiguredRequestCredentials(t *testing.T) {
	tp, mp, sr := testProviders()
	client := NewClient(tp, mp, propagation.TraceContext{})
	client.SetTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("proxy rejected header %q and cookie %q",
			r.Header.Get("Authorization"), r.Header.Get("Cookie"))
	}))

	// #nosec G101 -- fabricated fixture credentials, not live ones
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token, cookie = "s3cret-bearer-token", "c00kie-value"

	_, err := client.R().
		SetAuthToken(token).
		SetCookie(&http.Cookie{Name: "sess", Value: cookie}).
		Get("https://api.example.com/orders")
	require.Error(t, err)
	require.Contains(t, err.Error(), token, "the premise: the token is in the error the hook is handed")
	require.Contains(t, err.Error(), cookie, "and so is the cookie")

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	span := spans[0]

	assert.NotContains(t, span.Status().Description, token)
	assert.NotContains(t, span.Status().Description, cookie)
	assert.Contains(t, span.Status().Description, "api.example.com",
		"the server still has to be identifiable")
	for _, event := range span.Events() {
		for _, attr := range event.Attributes {
			assert.NotContains(t, attr.Value.AsString(), token, "no span event may carry it either")
			assert.NotContains(t, attr.Value.AsString(), cookie)
		}
	}
}

// TestWrapRedactsTheClientLevelCredentials pins the half of that set which
// stops being reachable once beforeRequest returns.
//
// resty passes the invoking client to no other hook, so a token or a
// basic-auth pair configured on the client — rather than on the request — is
// only in hand at that one moment. It is resolved there and carried on the
// request state, the same way the target is.
func TestWrapRedactsTheClientLevelCredentials(t *testing.T) {
	tp, mp, sr := testProviders()
	client := NewClient(tp, mp, propagation.TraceContext{})
	// #nosec G101 -- fabricated fixture credential, not a live one
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	client.SetBasicAuth("alice", "hunter2")
	client.SetTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("proxy rejected header %q", r.Header.Get("Authorization"))
	}))

	basic := base64.StdEncoding.EncodeToString([]byte("alice:hunter2"))

	_, err := client.R().Get("https://api.example.com/orders")
	require.Error(t, err)
	require.Contains(t, err.Error(), basic,
		"the premise: what reaches the error is the base64, which holds neither half")

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assert.NotContains(t, spans[0].Status().Description, basic)
	assert.NotContains(t, spans[0].Status().Description, "hunter2")
}

// TestWrapRedactsACallersOwnAuthorizationHeaderKey pins the one case a rule on
// header names cannot reach on its own: resty lets the caller move its token to
// a header of their choosing, and the name they chose need not read like a
// credential. The client knows which header that is, so the hook asks it rather
// than guessing from the name.
func TestWrapRedactsACallersOwnAuthorizationHeaderKey(t *testing.T) {
	tp, mp, sr := testProviders()
	client := NewClient(tp, mp, propagation.TraceContext{})
	client.HeaderAuthorizationKey = "X-Tenant-Hdr"
	client.SetTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("proxy rejected header %q", r.Header.Get("X-Tenant-Hdr"))
	}))

	// #nosec G101 -- fabricated fixture credential, not a live one
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token = "s3cret-bearer-token"
	require.False(t, redact.CredentialHeaderName("X-Tenant-Hdr"),
		"the premise: nothing about this name says it carries one")

	_, err := client.R().SetAuthToken(token).Get("https://api.example.com/orders")
	require.Error(t, err)
	require.Contains(t, err.Error(), token, "and the token is in the error")

	spans := endedClientSpans(sr)
	require.Len(t, spans, 1)
	assert.NotContains(t, spans[0].Status().Description, token)
}
