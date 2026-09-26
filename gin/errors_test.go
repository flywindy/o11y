package gin_test

import (
	"errors"
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

// validationError is a named, non-pointer error type, so exception.type takes
// the package-qualified branch of the SDK's rendering rather than the builtin
// one errors.New produces.
type validationError struct{ field string }

func (e validationError) Error() string { return "invalid " + e.field }

// TestMiddleware_GinErrorIdentifiesItsException covers the case position
// pairing got wrong: the handler records an exception of its own on the server
// span before pushing a gin error. The gin.error event must still name the gin
// error, through exception.type and exception.message, not the handler's.
func TestMiddleware_GinErrorIdentifiesItsException(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
	router.GET(testRoute, func(c *ginframework.Context) {
		trace.SpanFromContext(c.Request.Context()).RecordError(errors.New("cache miss"))
		c.AbortWithError(http.StatusBadRequest, validationError{field: "id"}).SetType(ginframework.ErrorTypeBind) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))
	result := env.finish(t, rec)

	var exceptionTypes []string
	var typed []sdktrace.Event
	for _, event := range result.span.Events() {
		switch event.Name {
		case semconv.ExceptionEventName:
			errType, _ := eventAttribute(event, semconv.ExceptionTypeKey)
			exceptionTypes = append(exceptionTypes, errType)
		case "gin.error":
			typed = append(typed, event)
		}
	}
	require.Len(t, exceptionTypes, 2, "the handler's exception and otelgin's")
	require.Len(t, typed, 1)
	message, _ := eventAttribute(typed[0], semconv.ExceptionMessageKey)
	errType, _ := eventAttribute(typed[0], semconv.ExceptionTypeKey)
	errorType, _ := eventAttribute(typed[0], "gin.error.type")
	assert.Equal(t, "invalid id", message)
	assert.Equal(t, "github.com/flywindy/o11y/gin_test.validationError", errType)
	assert.Contains(t, exceptionTypes, errType, "exception.type must match the SDK's rendering on otelgin's event")
	assert.Equal(t, "bind", errorType)
}

// TestMiddleware_FilteredRequestRecordsExceptions covers a request otelgin
// filters out while an outer layer's span is active: otelgin opens no span and
// records nothing, so the chain must record the exception itself rather than
// leave a gin.error event with no exception beside it.
func TestMiddleware_FilteredRequestRecordsExceptions(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(spanStarter(env.tracerProvider))
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{},
		o11ygin.WithFilter(func(*http.Request) bool { return false }),
	)...)
	router.GET("/fail", func(c *ginframework.Context) {
		c.AbortWithError(http.StatusInternalServerError, errors.New("filtered")).SetType(ginframework.ErrorTypePrivate) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))

	spans := env.spanRecorder.Ended()
	require.Len(t, spans, 1, "only the outer span: otelgin filtered the request")
	assert.Equal(t, "manual", spans[0].Name())
	assertSpanStatus(t, spans[0], codes.Error, "filtered")
	assertStandaloneErrorEvents(t, spans[0], typedError{message: "filtered", errorType: "private"})
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
