package gin

import (
	"strconv"
	"strings"

	ginframework "github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	ginErrorTypeKey = "gin.error.type"

	// ginErrorEventName names the span event the canonical Middleware chain
	// adds for each gin.Context.Errors entry. It is deliberately not
	// "exception": otelgin already records one exception event per entry, and
	// a second would double every exception count read from the span.
	ginErrorEventName = "gin.error"
)

// ErrorRecorder records gin.Context.Errors on the active span, for chains that
// do not include Middleware.
//
// Middleware already includes the equivalent of ErrorRecorder; do not add it
// again after Middleware. Inside the canonical chain otelgin records each
// error as an exception event itself, so the chain adds a separate
// "gin.error" event carrying gin.error.type instead of a second exception.
//
// Used on its own — for example on a gin engine served through
// o11yhttp.NewServerHandler, where nothing else reads c.Errors — ErrorRecorder
// is what turns each error into an exception event: it calls span.RecordError
// with the gin.error.type attribute, and sets the span status to Error when the
// response status is 5xx. If no valid span is active, it is a no-op.
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

// errorTypeEvents is the canonical chain's error handler. It runs inside
// otelgin, which on unwind records every c.Errors entry as an exception event
// and sets the span status (otelgin v0.68.0 gin.go). What otelgin cannot know
// is gin's bind/render/private/public classification, so this adds only that:
// one "gin.error" event per entry, in c.Errors order. The n-th gin.error event
// therefore describes the same error as the n-th exception event otelgin
// records after it.
func errorTypeEvents() ginframework.HandlerFunc {
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
			span.AddEvent(ginErrorEventName,
				trace.WithAttributes(attribute.String(ginErrorTypeKey, ginErrorTypeString(ge.Type))),
			)
		}
	}
}

func ginErrorTypeString(errorType ginframework.ErrorType) string {
	// ErrorTypeAny is an exact sentinel. A bitmask check would match every
	// non-zero gin error type because ErrorTypeAny contains all bits.
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
