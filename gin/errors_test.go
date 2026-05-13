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
	assertTypedErrorEvents(t, spans[0], typedError{message: "typed", errorType: "bind"})
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
