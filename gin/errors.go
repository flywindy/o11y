package gin

import (
	"net/http"
	"strconv"
	"strings"

	ginframework "github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	ginErrorTypeKey    = "gin.error.type"
	ginErrorMessageKey = "gin.error.message"

	// ginErrorEventName names the span event the canonical Middleware chain
	// adds for each gin.Context.Errors entry. It is deliberately not
	// "exception": otelgin already records one exception event per entry, and
	// a second would double every exception count read from the span.
	ginErrorEventName = "gin.error"
)

// ErrorRecorder records gin.Context.Errors on the active span, for chains that
// do not include Middleware.
//
// Middleware already handles c.Errors; do not add ErrorRecorder after it.
// Inside the canonical chain otelgin records each error as an exception event
// itself, so the chain adds a separate "gin.error" event carrying
// gin.error.type instead of a second exception.
//
// Used on its own — for example on a gin engine served through
// o11yhttp.NewServerHandler, where nothing else reads c.Errors — ErrorRecorder
// is what turns each error into an exception event: it calls span.RecordError
// with the gin.error.type attribute, and sets the span status to Error when the
// response status is 5xx. Entries with a nil Err are skipped. If no valid span
// is active, it is a no-op.
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
			if ge.Err == nil {
				continue // c.Error(&gin.Error{...}) can append one; nothing to record
			}
			span.RecordError(ge.Err,
				trace.WithAttributes(attribute.String(ginErrorTypeKey, ginErrorTypeString(ge.Type))),
			)
		}
		if last := c.Errors.Last(); c.Writer.Status() >= 500 && last.Err != nil {
			span.SetStatus(codes.Error, last.Err.Error())
		}
	}
}

// errorTypeEvents is the canonical chain's error handler. It runs inside
// otelgin, which on unwind records every c.Errors entry as an exception event
// and sets the span status (otelgin v0.68.0 gin.go). What otelgin cannot know
// is gin's bind/render/private/public classification, so this adds only that:
// one "gin.error" event per entry, carrying gin.error.type and, as
// gin.error.message, the same text otelgin's exception event carries as
// exception.message. The message is what ties the two together; their
// positions on the span do not, because application code may record
// exceptions of its own on the server span, and the span's event limit evicts
// the oldest events first. It is an SDK-owned key rather than
// exception.message so that exception.* stays on exception events only and a
// query selecting exceptions by attribute does not count each error twice.
//
// filters are the ones otelgin was given. A request any of them rejects was
// not traced by otelgin, so the span in the context, if any, belongs to some
// other layer; the chain leaves it alone, as otelgin does. The span is read
// before c.Next, while the context is still the one otelgin handed on, since
// a downstream middleware may replace c.Request's context without restoring
// it.
func errorTypeEvents(filters []func(*http.Request) bool) ginframework.HandlerFunc {
	return func(c *ginframework.Context) {
		for _, filter := range filters {
			if !filter(c.Request) {
				c.Next()
				return
			}
		}
		span := trace.SpanFromContext(c.Request.Context())
		c.Next()
		if len(c.Errors) == 0 || !span.SpanContext().IsValid() {
			return
		}
		for _, ge := range c.Errors {
			if ge.Err == nil {
				continue // otelgin's RecordError(nil) records nothing either
			}
			span.AddEvent(ginErrorEventName, trace.WithAttributes(
				attribute.String(ginErrorTypeKey, ginErrorTypeString(ge.Type)),
				attribute.String(ginErrorMessageKey, ge.Err.Error()),
			))
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
