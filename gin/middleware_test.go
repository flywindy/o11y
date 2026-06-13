package gin_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	ginframework "github.com/gin-gonic/gin"
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

	o11ygin "github.com/flywindy/o11y/gin"
)

const (
	testService     = "gin-test"
	testRoute       = "/items/:id"
	testPath        = "/items/123"
	customMetricKey = attribute.Key("custom.metric")
)

func TestMiddleware_MatrixRow01_HappyPath(t *testing.T) {
	result := runCanonicalRequest(t, defaultRecovery(), func(c *ginframework.Context) {
		c.String(http.StatusOK, "ok")
	})

	assert.Equal(t, http.StatusOK, result.statusCode)
	assertSpanShape(t, result.span, http.MethodGet, testRoute, http.StatusOK)
	assertSpanStatus(t, result.span, codes.Unset, "")
	assertTypedErrorEvents(t, result.span)
	assertMetricStatusCodes(t, result.metrics, "200")
}

func TestMiddleware_MatrixRow02_JSON400WithoutError(t *testing.T) {
	result := runCanonicalRequest(t, defaultRecovery(), func(c *ginframework.Context) {
		c.JSON(http.StatusBadRequest, ginframework.H{"error": "invalid"})
	})

	assert.Equal(t, http.StatusBadRequest, result.statusCode)
	assertSpanShape(t, result.span, http.MethodGet, testRoute, http.StatusBadRequest)
	assertSpanStatus(t, result.span, codes.Unset, "")
	assertTypedErrorEvents(t, result.span)
	assertMetricStatusCodes(t, result.metrics, "400")
}

func TestMiddleware_MatrixRow03_AbortWithError400(t *testing.T) {
	result := runCanonicalRequest(t, defaultRecovery(), func(c *ginframework.Context) {
		c.AbortWithError(http.StatusBadRequest, errors.New("invalid request")).SetType(ginframework.ErrorTypePublic) //nolint:errcheck
	})

	assert.Equal(t, http.StatusBadRequest, result.statusCode)
	assertSpanShape(t, result.span, http.MethodGet, testRoute, http.StatusBadRequest)
	assertSpanStatus(t, result.span, codes.Error, "Error #01: invalid request\n")
	assertTypedErrorEvents(t, result.span, typedError{message: "invalid request", errorType: "public"})
	assertMetricStatusCodes(t, result.metrics, "400")
}

func TestMiddleware_MatrixRow04_AbortWithError500(t *testing.T) {
	result := runCanonicalRequest(t, defaultRecovery(), func(c *ginframework.Context) {
		c.AbortWithError(http.StatusInternalServerError, errors.New("database down")) //nolint:errcheck
	})

	assert.Equal(t, http.StatusInternalServerError, result.statusCode)
	assertSpanShape(t, result.span, http.MethodGet, testRoute, http.StatusInternalServerError)
	assertSpanStatus(t, result.span, codes.Error, "Error #01: database down\n")
	assertTypedErrorEvents(t, result.span, typedError{message: "database down", errorType: "private"})
	assertMetricStatusCodes(t, result.metrics, "500")
}

func TestMiddleware_MatrixRow05_MultipleErrorsThenAbort500(t *testing.T) {
	result := runCanonicalRequest(t, defaultRecovery(), func(c *ginframework.Context) {
		c.Error(errors.New("first failure")).SetType(ginframework.ErrorTypePublic)                                            //nolint:errcheck
		c.AbortWithError(http.StatusInternalServerError, errors.New("second failure")).SetType(ginframework.ErrorTypePrivate) //nolint:errcheck
	})

	assert.Equal(t, http.StatusInternalServerError, result.statusCode)
	assertSpanShape(t, result.span, http.MethodGet, testRoute, http.StatusInternalServerError)
	assertSpanStatus(t, result.span, codes.Error, "Error #01: first failure\nError #02: second failure\n")
	assertTypedErrorEvents(t, result.span,
		typedError{message: "first failure", errorType: "public"},
		typedError{message: "second failure", errorType: "private"},
	)
	assertMetricStatusCodes(t, result.metrics, "500")
}

func TestMiddleware_MatrixRow06_PanicWithDefaultRecovery(t *testing.T) {
	result := runCanonicalRequest(t, defaultRecovery(), func(*ginframework.Context) {
		panic("boom")
	})

	assert.Equal(t, http.StatusInternalServerError, result.statusCode)
	assertSpanShape(t, result.span, http.MethodGet, testRoute, http.StatusInternalServerError)
	assertSpanStatus(t, result.span, codes.Error, "")
	assertTypedErrorEvents(t, result.span)
	assertMetricStatusCodes(t, result.metrics, "500")
}

func TestMiddleware_MatrixRow07_PanicWithCustomRecoveryPushingError(t *testing.T) {
	result := runCanonicalRequest(t, customRecovery(true, http.StatusInternalServerError), func(*ginframework.Context) {
		panic("boom")
	})

	assert.Equal(t, http.StatusInternalServerError, result.statusCode)
	assertSpanShape(t, result.span, http.MethodGet, testRoute, http.StatusInternalServerError)
	assertSpanStatus(t, result.span, codes.Error, "Error #01: panic: boom\n")
	assertTypedErrorEvents(t, result.span, typedError{message: "panic: boom", errorType: "private"})
	assertMetricStatusCodes(t, result.metrics, "500")
}

func TestMiddleware_MatrixRow08_PanicWithCustomRecoverySwallowingAs200(t *testing.T) {
	result := runCanonicalRequest(t, customRecovery(false, http.StatusOK), func(*ginframework.Context) {
		panic("boom")
	})

	assert.Equal(t, http.StatusOK, result.statusCode)
	assertSpanShape(t, result.span, http.MethodGet, testRoute, http.StatusOK)
	assertSpanStatus(t, result.span, codes.Unset, "")
	assertTypedErrorEvents(t, result.span)
	assertMetricStatusCodes(t, result.metrics, "200")
}

func TestMiddleware_MatrixRow09_AbortWithoutError204(t *testing.T) {
	result := runCanonicalRequest(t, defaultRecovery(), func(c *ginframework.Context) {
		c.Status(http.StatusNoContent)
		c.Abort()
	})

	assert.Equal(t, http.StatusNoContent, result.statusCode)
	assertSpanShape(t, result.span, http.MethodGet, testRoute, http.StatusNoContent)
	assertSpanStatus(t, result.span, codes.Unset, "")
	assertTypedErrorEvents(t, result.span)
	assertMetricStatusCodes(t, result.metrics, "204")
}

func TestMiddleware_MatrixRow10_InvertedRecoveryOrderProducesIncompleteSpan(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(defaultRecovery())
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
	router.GET(testRoute, func(*ginframework.Context) {
		panic("boom")
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))

	result := env.finish(t, rec)
	assert.Equal(t, http.StatusInternalServerError, result.statusCode)
	assertSpanAttr(t, result.span, semconv.HTTPRequestMethodKey, http.MethodGet)
	assertSpanAttr(t, result.span, semconv.HTTPRouteKey, testRoute)
	assertSpanAttrMissing(t, result.span, semconv.HTTPResponseStatusCodeKey)
	assertSpanStatus(t, result.span, codes.Unset, "")
	assertTypedErrorEvents(t, result.span)
	assertMetricStatusCodes(t, result.metrics)
}

func TestWithSkipProbes_ExactPaths(t *testing.T) {
	probePaths := []string{
		"/health", "/healthz", "/livez", "/readyz",
		"/metrics", "/ping", "/ready", "/live",
	}
	for _, path := range probePaths {
		t.Run(path, func(t *testing.T) {
			env := newTestEnv(t)
			router := ginframework.New()
			router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{},
				o11ygin.WithSkipProbes(),
			)...)
			router.GET(path, func(c *ginframework.Context) { c.Status(http.StatusOK) })
			router.GET(testRoute, func(c *ginframework.Context) { c.Status(http.StatusOK) })

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, rec.Code)
			require.Empty(t, env.spanRecorder.Ended(), "probe path %s should produce no span", path)

			rec2 := httptest.NewRecorder()
			router.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, testPath, nil))
			require.Len(t, env.spanRecorder.Ended(), 1, "non-probe path should still produce a span")
		})
	}
}

func TestWithSkipProbes_PrefixOpt(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{},
		o11ygin.WithSkipProbes(o11ygin.WithSkipProbePrefixes("/internal/")),
	)...)
	router.GET("/internal/debug", func(c *ginframework.Context) { c.Status(http.StatusOK) })
	router.GET(testRoute, func(c *ginframework.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/debug", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, env.spanRecorder.Ended(), "prefix-matched path should produce no span")

	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, testPath, nil))
	require.Len(t, env.spanRecorder.Ended(), 1, "non-prefix path should still produce a span")
}

func TestWithSkipProbes_ExactNotMatchedByPrefixOpt(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	// No prefix opt — /healthcheck should NOT be skipped (it's not in the exact list).
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{},
		o11ygin.WithSkipProbes(),
	)...)
	router.GET("/healthcheck", func(c *ginframework.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthcheck", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, env.spanRecorder.Ended(), 1, "/healthcheck is not in exact list and should be traced")
}

func TestMiddleware_OptionPassThroughs(t *testing.T) {
	env := newTestEnv(t, customMetricKey)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(
		testService,
		env.tracerProvider,
		env.meterProvider,
		propagation.TraceContext{},
		o11ygin.WithSpanNameFormatter(func(c *ginframework.Context) string {
			return "custom " + c.FullPath()
		}),
		o11ygin.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/skip"
		}),
		o11ygin.WithMetricAttributesFn(func(*http.Request) []attribute.KeyValue {
			return []attribute.KeyValue{customMetricKey.String("kept")}
		}),
	)...)
	router.GET(testRoute, func(c *ginframework.Context) {
		c.Status(http.StatusAccepted)
	})
	router.GET("/skip", func(c *ginframework.Context) {
		c.Status(http.StatusNoContent)
	})

	skipped := httptest.NewRecorder()
	router.ServeHTTP(skipped, httptest.NewRequest(http.MethodGet, "/skip", nil))
	require.Equal(t, http.StatusNoContent, skipped.Code)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))
	result := env.finish(t, rec)

	require.Equal(t, http.StatusAccepted, result.statusCode)
	assert.Equal(t, "custom "+testRoute, result.span.Name())
	assertMetricStatusCodes(t, result.metrics, "202")
	assertMetricAttr(t, result.metrics, customMetricKey, "kept")
}

type testEnv struct {
	reader         *sdkmetric.ManualReader
	spanRecorder   *tracetest.SpanRecorder
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
}

type requestResult struct {
	statusCode int
	span       sdktrace.ReadOnlySpan
	metrics    metricdata.ResourceMetrics
}

type typedError struct {
	message   string
	errorType string
}

func runCanonicalRequest(t *testing.T, recovery ginframework.HandlerFunc, handler ginframework.HandlerFunc) requestResult {
	t.Helper()
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
	if recovery != nil {
		router.Use(recovery)
	}
	router.GET(testRoute, handler)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))
	return env.finish(t, rec)
}

func newTestEnv(t *testing.T, extraMetricKeys ...attribute.Key) *testEnv {
	t.Helper()
	ginframework.SetMode(ginframework.TestMode)
	reader := sdkmetric.NewManualReader()
	metricKeys := append([]attribute.Key{
		semconv.HTTPRequestMethodKey,
		semconv.HTTPRouteKey,
		semconv.HTTPResponseStatusCodeKey,
	}, extraMetricKeys...)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.duration"},
			sdkmetric.Stream{
				AttributeFilter: attribute.NewAllowKeysFilter(metricKeys...),
			},
		)),
	)
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	return &testEnv{
		reader:         reader,
		spanRecorder:   spanRecorder,
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
	}
}

func (env *testEnv) finish(t *testing.T, rec *httptest.ResponseRecorder) requestResult {
	t.Helper()
	spans := env.spanRecorder.Ended()
	require.Len(t, spans, 1)
	var metrics metricdata.ResourceMetrics
	require.NoError(t, env.reader.Collect(t.Context(), &metrics))
	return requestResult{
		statusCode: rec.Code,
		span:       spans[0],
		metrics:    metrics,
	}
}

func defaultRecovery() ginframework.HandlerFunc {
	return ginframework.RecoveryWithWriter(io.Discard)
}

func customRecovery(pushError bool, status int) ginframework.HandlerFunc {
	return func(c *ginframework.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if pushError {
				c.Error(fmt.Errorf("panic: %v", recovered)) //nolint:errcheck
			}
			if status == http.StatusOK {
				c.JSON(http.StatusOK, ginframework.H{"fallback": true})
				return
			}
			c.AbortWithStatus(status)
		}()
		c.Next()
	}
}

func assertSpanShape(t *testing.T, span sdktrace.ReadOnlySpan, method, route string, statusCode int) {
	t.Helper()
	assertSpanAttr(t, span, semconv.HTTPRequestMethodKey, method)
	assertSpanAttr(t, span, semconv.HTTPRouteKey, route)
	assertSpanAttr(t, span, semconv.HTTPResponseStatusCodeKey, fmt.Sprintf("%d", statusCode))
}

func assertSpanStatus(t *testing.T, span sdktrace.ReadOnlySpan, code codes.Code, description string) {
	t.Helper()
	assert.Equal(t, code, span.Status().Code)
	assert.Equal(t, description, span.Status().Description)
}

func assertSpanAttr(t *testing.T, span sdktrace.ReadOnlySpan, key attribute.Key, want string) {
	t.Helper()
	for _, attr := range span.Attributes() {
		if attr.Key == key {
			assert.Equal(t, want, attr.Value.String())
			return
		}
	}
	t.Fatalf("missing span attribute %s", key)
}

func assertSpanAttrMissing(t *testing.T, span sdktrace.ReadOnlySpan, key attribute.Key) {
	t.Helper()
	for _, attr := range span.Attributes() {
		if attr.Key == key {
			t.Fatalf("unexpected span attribute %s=%s", key, attr.Value.String())
		}
	}
}

func assertTypedErrorEvents(t *testing.T, span sdktrace.ReadOnlySpan, want ...typedError) {
	t.Helper()
	var got []typedError
	for _, event := range span.Events() {
		errorType, ok := eventAttribute(event, "gin.error.type")
		if !ok {
			continue
		}
		message, ok := eventAttribute(event, "exception.message")
		require.True(t, ok, "typed error event missing exception.message")
		got = append(got, typedError{message: message, errorType: errorType})
	}
	assert.Equal(t, want, got)
}

func eventAttribute(event sdktrace.Event, key attribute.Key) (string, bool) {
	for _, attr := range event.Attributes {
		if attr.Key == key {
			return attr.Value.String(), true
		}
	}
	return "", false
}

func assertMetricStatusCodes(t *testing.T, rm metricdata.ResourceMetrics, want ...string) {
	t.Helper()
	var got []string
	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "http.server.request.duration" {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			for _, point := range histogram.DataPoints {
				value, ok := point.Attributes.Value(semconv.HTTPResponseStatusCodeKey)
				if ok {
					got = append(got, value.String())
				}
			}
		}
	}
	assert.ElementsMatch(t, want, got)
}

func assertMetricAttr(t *testing.T, rm metricdata.ResourceMetrics, key attribute.Key, want string) {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "http.server.request.duration" {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			for _, point := range histogram.DataPoints {
				value, ok := point.Attributes.Value(key)
				if ok {
					assert.Equal(t, want, value.String())
					return
				}
			}
		}
	}
	t.Fatalf("missing metric attribute %s", key)
}
