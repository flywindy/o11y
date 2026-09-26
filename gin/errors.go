package gin

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	ginframework "github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	ginErrorTypeKey = "gin.error.type"

	// ginErrorEventName names the span event the canonical Middleware chain
	// adds for each gin.Context.Errors entry. It is deliberately not
	// "exception": otelgin already records one exception event per entry, and
	// a second would double every exception count read from the span.
	ginErrorEventName = "gin.error"

	// incomingSpanKey is the gin.Context key under which Middleware stores the
	// span context that was active before otelgin ran, so the chain can tell
	// whether otelgin opened a span for this request or filtered it out.
	incomingSpanKey = "o11y.gin.incoming_span_context"
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
// response status is 5xx. If no valid span is active, it is a no-op.
func ErrorRecorder() ginframework.HandlerFunc {
	return func(c *ginframework.Context) {
		c.Next()
		if span, ok := errorSpan(c); ok {
			recordExceptions(c, span)
		}
	}
}

// errorTypeEvents is the canonical chain's error handler. It runs inside
// otelgin, which on unwind records every c.Errors entry as an exception event
// and sets the span status (otelgin v0.68.0 gin.go). What otelgin cannot know
// is gin's bind/render/private/public classification, so this adds only that:
// one "gin.error" event per entry.
//
// Each gin.error event carries the exception.type and exception.message of the
// exception event otelgin records for the same entry. That is what ties the
// two together; their positions on the span do not, because application code
// may record exceptions of its own on the server span, and the span's event
// limit evicts the oldest events first.
//
// When otelgin filtered the request out (WithFilter, WithSkipPaths) it opened
// no span and records nothing, so any span in the context belongs to an outer
// layer. The handler then records exceptions itself, as ErrorRecorder would.
func errorTypeEvents() ginframework.HandlerFunc {
	return func(c *ginframework.Context) {
		c.Next()
		span, ok := errorSpan(c)
		if !ok {
			return
		}
		if !openedByOtelgin(c, span) {
			recordExceptions(c, span)
			return
		}
		for _, ge := range c.Errors {
			span.AddEvent(ginErrorEventName, trace.WithAttributes(
				attribute.String(ginErrorTypeKey, ginErrorTypeString(ge.Type)),
				semconv.ExceptionType(errorTypeName(ge.Err)),
				semconv.ExceptionMessage(ge.Err.Error()),
			))
		}
	}
}

// errorSpan returns the span c's errors are recorded on, and false when c has
// no errors or no valid span is active.
func errorSpan(c *ginframework.Context) (trace.Span, bool) {
	if len(c.Errors) == 0 {
		return nil, false
	}
	span := trace.SpanFromContext(c.Request.Context())
	return span, span.SpanContext().IsValid()
}

// recordExceptions records each c.Errors entry as an exception event carrying
// gin.error.type, and sets the span status to Error on a 5xx response.
func recordExceptions(c *ginframework.Context, span trace.Span) {
	for _, ge := range c.Errors {
		span.RecordError(ge.Err,
			trace.WithAttributes(attribute.String(ginErrorTypeKey, ginErrorTypeString(ge.Type))),
		)
	}
	if c.Writer.Status() >= 500 {
		span.SetStatus(codes.Error, c.Errors.Last().Error())
	}
}

// openedByOtelgin reports whether span is the one otelgin started for this
// request, by comparing it with the span context Middleware stored before
// otelgin ran. A missing entry counts as not opened, so errors are never lost.
func openedByOtelgin(c *ginframework.Context, span trace.Span) bool {
	v, ok := c.Get(incomingSpanKey)
	if !ok {
		return false
	}
	incoming, ok := v.(trace.SpanContext)
	return ok && !incoming.Equal(span.SpanContext())
}

// errorTypeName renders err's type the way the OTel SDK's span.RecordError
// does for exception.type (sdk/trace typeStr), so a gin.error event and the
// exception event otelgin records for the same error carry the same value.
func errorTypeName(err error) string {
	t := reflect.TypeOf(err)
	if t.PkgPath() == "" && t.Name() == "" {
		return t.String()
	}
	return fmt.Sprintf("%s.%s", t.PkgPath(), t.Name())
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
