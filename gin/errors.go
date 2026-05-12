package gin

import (
	"strconv"
	"strings"

	ginframework "github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const ginErrorTypeKey = "gin.error.type"

// ErrorRecorder records gin.Context.Errors on the active server span.
//
// It must run after otelgin.Middleware in the gin chain so that
// c.Request.Context contains the server span. If no valid span is active,
// ErrorRecorder is a no-op.
func ErrorRecorder() ginframework.HandlerFunc {
	return func(c *ginframework.Context) {
		c.Next()
		if len(c.Errors) == 0 {
			return
		}
		span := trace.SpanFromContext(c.Request.Context())
		if !span.SpanContext().IsValid() {
			return
		}
		for _, ge := range c.Errors {
			span.RecordError(ge.Err,
				trace.WithAttributes(attribute.String(ginErrorTypeKey, ginErrorTypeString(ge.Type))),
			)
		}
		if c.Writer.Status() >= 500 {
			span.SetStatus(codes.Error, c.Errors.Last().Error())
		}
	}
}

func ginErrorTypeString(errorType ginframework.ErrorType) string {
	if errorType == ginframework.ErrorTypeAny {
		return "any"
	}

	names := make([]string, 0, 4)
	for _, known := range []struct {
		errorType ginframework.ErrorType
		name      string
	}{
		{ginframework.ErrorTypeBind, "bind"},
		{ginframework.ErrorTypeRender, "render"},
		{ginframework.ErrorTypePrivate, "private"},
		{ginframework.ErrorTypePublic, "public"},
	} {
		if errorType&known.errorType != 0 {
			names = append(names, known.name)
			errorType &^= known.errorType
		}
	}
	if errorType != 0 {
		names = append(names, "unknown:"+strconv.FormatUint(uint64(errorType), 10))
	}
	if len(names) == 0 {
		return "unknown:0"
	}
	return strings.Join(names, "|")
}
