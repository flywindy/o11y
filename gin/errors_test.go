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

// TestMiddleware_GinErrorIdentifiesItsException covers the case position
// pairing got wrong: the handler records an exception of its own on the server
// span before pushing a gin error. The gin.error event must still name the gin
// error through gin.error.message, not the handler's exception.
func TestMiddleware_GinErrorIdentifiesItsException(t *testing.T) {
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

	var exceptionMessages []string
	var typed []sdktrace.Event
	for _, event := range result.span.Events() {
		switch event.Name {
		case semconv.ExceptionEventName:
			message, _ := eventAttribute(event, semconv.ExceptionMessageKey)
			exceptionMessages = append(exceptionMessages, message)
		case "gin.error":
			typed = append(typed, event)
		}
	}
	assert.Equal(t, []string{"cache miss", "invalid id"}, exceptionMessages, "the handler's exception, then otelgin's")
	require.Len(t, typed, 1)
	message, _ := eventAttribute(typed[0], "gin.error.message")
	errorType, _ := eventAttribute(typed[0], "gin.error.type")
	assert.Equal(t, "invalid id", message, "gin.error names the gin error, not the first exception on the span")
	assert.Equal(t, "bind", errorType)
}

// TestMiddleware_FilteredRequestLeavesOuterSpanAlone covers a request the
// chain's filter excludes while an outer layer's span is active. otelgin opens
// no span and records nothing; the chain's error handler must do the same
// rather than put a gin.error event, with no exception beside it, on a span
// that is not its own.
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
// gin.error events must still land on otelgin's server span, next to its
// exception events, not on the already-ended child.
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
// group, once with the inner chain tracing and once with it filtering the
// request. Either way every span carries exactly one exception and one
// gin.error event per error: the inner chain never records on the outer
// chain's span.
func TestMiddleware_NestedChains(t *testing.T) {
	for _, innerFiltered := range []bool{false, true} {
		t.Run(fmt.Sprintf("innerFiltered=%t", innerFiltered), func(t *testing.T) {
			env := newTestEnv(t)
			router := ginframework.New()
			router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
			group := router.Group("/")
			group.Use(o11ygin.Middleware("inner", env.tracerProvider, env.meterProvider, propagation.TraceContext{},
				o11ygin.WithFilter(func(*http.Request) bool { return !innerFiltered }),
			)...)
			group.GET(testRoute, func(c *ginframework.Context) {
				c.AbortWithError(http.StatusInternalServerError, errors.New("nested")).SetType(ginframework.ErrorTypePublic) //nolint:errcheck
			})

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))

			spans := env.spanRecorder.Ended()
			if innerFiltered {
				require.Len(t, spans, 1)
			} else {
				require.Len(t, spans, 2)
			}
			for _, span := range spans {
				assertTypedErrorEvents(t, span, typedError{message: "nested", errorType: "public"})
			}
		})
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
					assert.NotEqual(t, "gin.error", event.Name)
					assert.NotEqual(t, semconv.ExceptionEventName, event.Name)
				}
			}
		})
	}
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
