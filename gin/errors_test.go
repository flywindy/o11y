package gin_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	ginframework "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	o11ygin "github.com/flywindy/o11y/gin"
)

func TestErrorRecorder_NoopsWithoutActiveSpan(t *testing.T) {
	ginframework.SetMode(ginframework.TestMode)
	router := ginframework.New()
	router.Use(o11ygin.ErrorRecorder())
	router.GET("/fail", func(c *ginframework.Context) {
		c.AbortWithError(http.StatusInternalServerError, errors.New("no span")) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	require.NotPanics(t, func() {
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestErrorRecorder_RecordsTypedGinErrors(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(spanStarter(env.tracerProvider))
	router.Use(o11ygin.ErrorRecorder())
	router.GET("/fail", func(c *ginframework.Context) {
		c.AbortWithError(http.StatusInternalServerError, errors.New("typed")).SetType(ginframework.ErrorTypeBind) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))

	spans := env.spanRecorder.Ended()
	require.Len(t, spans, 1)
	assertSpanStatus(t, spans[0], codes.Error, "typed")
	assertStandaloneErrorEvents(t, spans[0], typedError{message: "typed", errorType: "bind"})
}

// TestMiddleware_LeavesApplicationExceptionsAlone has the handler record an
// exception of its own on the server span before pushing a gin error. The
// application's exception is left as recorded; only the gin error gets
// gin.error.type, and each is recorded once.
func TestMiddleware_LeavesApplicationExceptionsAlone(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
	router.GET(testRoute, func(c *ginframework.Context) {
		trace.SpanFromContext(c.Request.Context()).RecordError(errors.New("cache miss"))
		c.AbortWithError(http.StatusBadRequest, errors.New("invalid id")).SetType(ginframework.ErrorTypeBind) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))
	result := env.finish(t, rec)

	events := result.span.Events()
	require.Len(t, events, 2)
	message, _ := eventAttribute(events[0], semconv.ExceptionMessageKey)
	_, typed := eventAttribute(events[0], "gin.error.type")
	assert.Equal(t, "cache miss", message)
	assert.False(t, typed)
	message, _ = eventAttribute(events[1], semconv.ExceptionMessageKey)
	errorType, _ := eventAttribute(events[1], "gin.error.type")
	assert.Equal(t, "invalid id", message)
	assert.Equal(t, "bind", errorType)
	assertSpanStatus(t, result.span, codes.Error, "Error #01: invalid id\n")
}

// TestMiddleware_OuterMiddlewareSeesErrors checks that keeping c.Errors from
// otelgin is invisible outside the chain: a middleware registered before
// Middleware, like gin's logger or an error-response middleware, still finds
// every error the handlers pushed, in order.
func TestMiddleware_OuterMiddlewareSeesErrors(t *testing.T) {
	env := newTestEnv(t)
	var seen []string
	router := ginframework.New()
	router.Use(func(c *ginframework.Context) {
		c.Next()
		for _, ge := range c.Errors {
			seen = append(seen, ge.Error())
		}
	})
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
	router.GET(testRoute, func(c *ginframework.Context) {
		c.Error(errors.New("first"))                                           //nolint:errcheck
		c.AbortWithError(http.StatusInternalServerError, errors.New("second")) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))
	result := env.finish(t, rec)

	assert.Equal(t, []string{"first", "second"}, seen)
	assertTypedErrorEvents(t, result.span,
		typedError{message: "first", errorType: "private"},
		typedError{message: "second", errorType: "private"},
	)
}

// TestMiddleware_FilterOptionsCombine passes WithSkipPaths and a WithFilter of
// its own. Every filter from every option must pass for a request to be
// traced, and an excluded request gets no event from the chain.
func TestMiddleware_FilterOptionsCombine(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{},
		o11ygin.WithSkipPaths(),
		o11ygin.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/internal" }),
	)...)
	for _, path := range []string{"/healthz", "/internal", testRoute} {
		router.GET(path, func(c *ginframework.Context) {
			c.AbortWithError(http.StatusBadRequest, errors.New("bad")).SetType(ginframework.ErrorTypeBind) //nolint:errcheck
		})
	}

	for _, path := range []string{"/healthz", "/internal", testPath} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}

	spans := env.spanRecorder.Ended()
	require.Len(t, spans, 1, "only the path neither filter excludes is traced")
	assertTypedErrorEvents(t, spans[0], typedError{message: "bad", errorType: "bind"})
}

// TestMiddleware_FilteredRequestLeavesOuterSpanAlone covers a request the
// chain's filter excludes while an outer layer's span is active. otelgin opens
// no span and records nothing; the chain's error handler must do the same
// rather than record on a span that is not its own.
func TestMiddleware_FilteredRequestLeavesOuterSpanAlone(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(spanStarter(env.tracerProvider))
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{},
		o11ygin.WithSkipPaths(),
	)...)
	router.GET("/healthz", func(c *ginframework.Context) {
		c.AbortWithError(http.StatusServiceUnavailable, errors.New("not ready")).SetType(ginframework.ErrorTypePrivate) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	spans := env.spanRecorder.Ended()
	require.Len(t, spans, 1, "only the outer span: the chain skipped the request")
	assert.Equal(t, "manual", spans[0].Name())
	assert.Empty(t, spans[0].Events())
}

// TestMiddleware_ContextSwapDownstream puts a middleware after the chain that
// replaces c.Request's context with a child span's and never restores it. The
// exception events must still land on otelgin's server span, not on the
// already-ended child.
func TestMiddleware_ContextSwapDownstream(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
	router.Use(func(c *ginframework.Context) {
		ctx, child := env.tracerProvider.Tracer("test").Start(c.Request.Context(), "child")
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		child.End()
	})
	router.GET(testRoute, func(c *ginframework.Context) {
		c.AbortWithError(http.StatusInternalServerError, errors.New("swapped")).SetType(ginframework.ErrorTypePrivate) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))

	var server, child sdktrace.ReadOnlySpan
	for _, span := range env.spanRecorder.Ended() {
		if span.Name() == "child" {
			child = span
		} else {
			server = span
		}
	}
	require.NotNil(t, server)
	require.NotNil(t, child)
	assert.Empty(t, child.Events())
	assertTypedErrorEvents(t, server, typedError{message: "swapped", errorType: "private"})
}

// TestMiddleware_NestedChains installs Middleware on the engine and again on a
// group, with each chain tracing or filtering the request. Every span carries
// exactly one typed exception event per error, and a chain that filtered the
// request adds nothing: neither chain acts on the other's span, filter answer
// or hidden errors.
func TestMiddleware_NestedChains(t *testing.T) {
	for _, tc := range []struct {
		outerFiltered, innerFiltered bool
		spans                        int
	}{
		{false, false, 2},
		{false, true, 1},
		{true, false, 1},
		{true, true, 0},
	} {
		t.Run(fmt.Sprintf("outerFiltered=%t/innerFiltered=%t", tc.outerFiltered, tc.innerFiltered), func(t *testing.T) {
			env := newTestEnv(t)
			router := ginframework.New()
			router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{},
				o11ygin.WithFilter(func(*http.Request) bool { return !tc.outerFiltered }),
			)...)
			group := router.Group("/")
			group.Use(o11ygin.Middleware("inner", env.tracerProvider, env.meterProvider, propagation.TraceContext{},
				o11ygin.WithFilter(func(*http.Request) bool { return !tc.innerFiltered }),
			)...)
			group.GET(testRoute, func(c *ginframework.Context) {
				c.AbortWithError(http.StatusInternalServerError, errors.New("nested")).SetType(ginframework.ErrorTypePublic) //nolint:errcheck
			})

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))

			spans := env.spanRecorder.Ended()
			require.Len(t, spans, tc.spans)
			for _, span := range spans {
				assertTypedErrorEvents(t, span, typedError{message: "nested", errorType: "public"})
			}
		})
	}
}

// TestMiddleware_HandleContextKeepsTracedFlag re-dispatches the request with
// engine.HandleContext to a path the chain's filter excludes, and pushes the
// error there. HandleContext clears c.Keys, but the outer chain read whether it
// traced the request before c.Next, so the error is still recorded once, typed,
// on the outer span.
func TestMiddleware_HandleContextKeepsTracedFlag(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{},
		o11ygin.WithSkipPaths(),
	)...)
	router.GET("/healthz", func(c *ginframework.Context) {
		c.AbortWithError(http.StatusServiceUnavailable, errors.New("not ready")).SetType(ginframework.ErrorTypePublic) //nolint:errcheck
	})
	router.GET(testRoute, func(c *ginframework.Context) {
		c.Request.URL.Path = "/healthz"
		router.HandleContext(c)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))

	spans := env.spanRecorder.Ended()
	require.Len(t, spans, 1, "only the outer request is traced")
	assertTypedErrorEvents(t, spans[0], typedError{message: "not ready", errorType: "public"})
}

// TestMiddleware_NilErrOnlyLeavesStatusUnset pushes only a nil-Err entry on a
// 4xx response. There is no error to record, so the chain neither records an
// exception nor marks the span Error, as ErrorRecorder does.
func TestMiddleware_NilErrOnlyLeavesStatusUnset(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
	router.GET(testRoute, func(c *ginframework.Context) {
		c.Error(&ginframework.Error{Type: ginframework.ErrorTypeBind}) //nolint:errcheck
		c.Status(http.StatusBadRequest)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))
	result := env.finish(t, rec)

	assertSpanStatus(t, result.span, codes.Unset, "")
	assert.Empty(t, result.span.Events())
}

// TestMiddleware_HiddenErrorsLeaveNoKey checks the chain removes the entry it
// used to set c.Errors aside once it puts them back, so c.Keys holds nothing
// of the chain's after the request.
func TestMiddleware_HiddenErrorsLeaveNoKey(t *testing.T) {
	env := newTestEnv(t)
	var keys map[any]any
	router := ginframework.New()
	router.Use(func(c *ginframework.Context) {
		c.Next()
		keys = c.Keys
	})
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
	router.GET(testRoute, func(c *ginframework.Context) {
		c.AbortWithError(http.StatusBadRequest, errors.New("bad")) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))

	for key := range keys {
		_, isString := key.(string)
		assert.True(t, isString, "unexpected non-string key %#v left in c.Keys", key)
	}
}

// TestNilErrEntryDoesNotPanic pushes a *gin.Error with no Err, which gin
// appends as is, on a 5xx response. There is nothing to classify or record;
// neither the canonical chain nor ErrorRecorder on its own may dereference it.
func TestNilErrEntryDoesNotPanic(t *testing.T) {
	for name, install := range map[string]func(*ginframework.Engine, *testEnv){
		"middleware": func(r *ginframework.Engine, env *testEnv) {
			r.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
		},
		"error recorder": func(r *ginframework.Engine, env *testEnv) {
			r.Use(spanStarter(env.tracerProvider), o11ygin.ErrorRecorder())
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			router := ginframework.New()
			install(router, env)
			router.GET(testRoute, func(c *ginframework.Context) {
				c.Error(&ginframework.Error{Type: ginframework.ErrorTypeBind}) //nolint:errcheck
				c.Status(http.StatusInternalServerError)
			})

			rec := httptest.NewRecorder()
			require.NotPanics(t, func() {
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))
			})
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			for _, span := range env.spanRecorder.Ended() {
				for _, event := range span.Events() {
					assert.NotEqual(t, semconv.ExceptionEventName, event.Name)
				}
			}
		})
	}
}

// TestMiddleware_FilterEvaluatedOncePerRequest uses a filter whose answer
// changes on every call, like a sampling filter. The chain must ask it once
// per request, so otelgin and the error handler act on the same answer: a
// traced request gets its typed exception, an excluded one nothing.
func TestMiddleware_FilterEvaluatedOncePerRequest(t *testing.T) {
	env := newTestEnv(t)
	calls := 0
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{},
		o11ygin.WithFilter(func(*http.Request) bool {
			calls++
			return calls%2 == 1 // true, false, true, … — a second call would flip it
		}),
	)...)
	router.GET(testRoute, func(c *ginframework.Context) {
		c.AbortWithError(http.StatusInternalServerError, errors.New("sampled")).SetType(ginframework.ErrorTypePrivate) //nolint:errcheck
	})

	for range 2 {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))
	}

	assert.Equal(t, 2, calls, "one filter call per request")
	spans := env.spanRecorder.Ended()
	require.Len(t, spans, 1, "the first request is traced, the second excluded")
	assertTypedErrorEvents(t, spans[0], typedError{message: "sampled", errorType: "private"})
}

// TestErrorRecorder_StatusSkipsTrailingNilErr pushes a real error and then a
// nil-Err entry on a 5xx response. The status must come from the last error
// there is, not be dropped because the last entry has none.
func TestErrorRecorder_StatusSkipsTrailingNilErr(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(spanStarter(env.tracerProvider), o11ygin.ErrorRecorder())
	router.GET("/fail", func(c *ginframework.Context) {
		c.Error(errors.New("db down")).SetType(ginframework.ErrorTypePrivate) //nolint:errcheck
		c.Error(&ginframework.Error{Type: ginframework.ErrorTypePrivate})     //nolint:errcheck
		c.Status(http.StatusInternalServerError)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))

	spans := env.spanRecorder.Ended()
	require.Len(t, spans, 1)
	assertSpanStatus(t, spans[0], codes.Error, "db down")
	assertStandaloneErrorEvents(t, spans[0], typedError{message: "db down", errorType: "private"})
}

// assertStandaloneErrorEvents checks that ErrorRecorder used without
// Middleware turns each error into exactly one exception event carrying
// gin.error.type, and adds nothing else.
func assertStandaloneErrorEvents(t *testing.T, span sdktrace.ReadOnlySpan, want ...typedError) {
	t.Helper()
	var got []typedError
	for _, event := range span.Events() {
		require.Equal(t, semconv.ExceptionEventName, event.Name)
		errorType, ok := eventAttribute(event, "gin.error.type")
		require.True(t, ok, "exception event missing gin.error.type")
		message, ok := eventAttribute(event, semconv.ExceptionMessageKey)
		require.True(t, ok, "exception event missing exception.message")
		got = append(got, typedError{message: message, errorType: errorType})
	}
	assert.Equal(t, want, got)
}

func spanStarter(tp trace.TracerProvider) ginframework.HandlerFunc {
	tracer := tp.Tracer("test")
	return func(c *ginframework.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "manual")
		defer span.End()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
