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

// TestErrorRecorder_MiddlewareRecordsEachErrorOnce pins the event count on the
// canonical chain against a direct count, independent of the matrix helper:
// otelgin v0.68.0 records one exception event per c.Errors entry, so the chain
// must add none of its own.
func TestErrorRecorder_MiddlewareRecordsEachErrorOnce(t *testing.T) {
	env := newTestEnv(t)
	router := ginframework.New()
	router.Use(o11ygin.Middleware(testService, env.tracerProvider, env.meterProvider, propagation.TraceContext{})...)
	router.GET(testRoute, func(c *ginframework.Context) {
		c.Error(errors.New("first")).SetType(ginframework.ErrorTypeBind)                                             //nolint:errcheck
		c.AbortWithError(http.StatusInternalServerError, errors.New("second")).SetType(ginframework.ErrorTypePublic) //nolint:errcheck
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testPath, nil))
	result := env.finish(t, rec)

	var exceptions, typed int
	for _, event := range result.span.Events() {
		switch event.Name {
		case semconv.ExceptionEventName:
			exceptions++
		case "gin.error":
			typed++
		}
	}
	assert.Equal(t, 2, exceptions, "one exception event per error")
	assert.Equal(t, 2, typed, "one gin.error event per error")
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
