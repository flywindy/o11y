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
	ginErrorTypeKey    = "gin.error.type"
	ginErrorMessageKey = "gin.error.message"

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
		if len(c.Errors) == 0 {
			return
		}
		span := trace.SpanFromContext(c.Request.Context())
		if span.SpanContext().IsValid() {
			recordExceptions(c, span)
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
// The span and whether otelgin opened it are read before c.Next, while the
// request context is still the one otelgin handed on: a downstream middleware
// may replace c.Request's context without restoring it, and a nested
// Middleware overwrites incomingSpanKey. When otelgin filtered the request out
// (WithFilter, WithSkipPaths) it opened no span and records nothing, so any
// span in the context belongs to an outer layer; the handler then records the
// errors as exception events itself, as ErrorRecorder would.
func errorTypeEvents() ginframework.HandlerFunc {
	return func(c *ginframework.Context) {
		span := trace.SpanFromContext(c.Request.Context())
		opened := openedByOtelgin(c, span)
		c.Next()
		if len(c.Errors) == 0 || !span.SpanContext().IsValid() {
			return
		}
		if !opened {
			recordExceptions(c, span)
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

// recordExceptions records each c.Errors entry as an exception event carrying
// gin.error.type, and sets the span status to Error on a 5xx response.
//
// An entry with a nil Err — c.Error(&gin.Error{Type: ...}) appends one — is
// skipped, since there is no error to record, and never dereferenced.
func recordExceptions(c *ginframework.Context, span trace.Span) {
	for _, ge := range c.Errors {
		if ge.Err == nil {
			continue
		}
		span.RecordError(ge.Err,
			trace.WithAttributes(attribute.String(ginErrorTypeKey, ginErrorTypeString(ge.Type))),
		)
	}
	if c.Writer.Status() >= 500 {
		description := ""
		if last := c.Errors.Last(); last.Err != nil {
			description = last.Err.Error()
		}
		span.SetStatus(codes.Error, description)
	}
}

// openedByOtelgin reports whether span is the one otelgin started for this
// request, by comparing it with the span context Middleware stored just
// before otelgin ran. errorTypeEvents is only ever installed by Middleware,
// directly after the handler that stores the entry, so the entry is always
// present; if it were not, otelgin is still in the chain, and assuming it
// opened the span avoids recording every error twice.
func openedByOtelgin(c *ginframework.Context, span trace.Span) bool {
	v, ok := c.Get(incomingSpanKey)
	if !ok {
		return true
	}
	incoming, ok := v.(trace.SpanContext)
	return !ok || !incoming.Equal(span.SpanContext())
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
