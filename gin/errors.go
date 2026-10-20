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

// chainKey is a gin.Context key private to one Middleware chain. Each chain
// allocates its own, so chains nested on route groups never read each other's
// entries, and no application key can collide with one. It is not zero-sized,
// so distinct keys never compare equal.
type chainKey struct{ name string }

// ErrorRecorder records gin.Context.Errors on the active span, for chains that
// do not include Middleware.
//
// Middleware already records c.Errors; do not add ErrorRecorder after it, or
// every error is recorded twice.
//
// Used on its own — for example on a gin engine served through
// o11yhttp.NewServerHandler, where nothing else reads c.Errors — ErrorRecorder
// turns each error into an exception event: it calls span.RecordError with the
// gin.error.type attribute, and sets the span status to Error when the
// response status is 5xx. It must run after the middleware that starts the
// span. Entries with a nil Err are skipped. If no span is recording, it is a
// no-op.
func ErrorRecorder() ginframework.HandlerFunc {
	return func(c *ginframework.Context) {
		c.Next()
		if len(c.Errors) == 0 {
			return
		}
		span := trace.SpanFromContext(c.Request.Context())
		if !span.IsRecording() {
			return
		}
		recordTypedExceptions(span, c.Errors)
		if last := lastError(c.Errors); c.Writer.Status() >= 500 && last != nil {
			span.SetStatus(codes.Error, last.Error())
		}
	}
}

// chainErrors is the canonical chain's error handler. It runs inside otelgin,
// directly after otelgin's own handler.
//
// otelgin v0.71.0, on unwind, sets the span status to Error with
// c.Errors.String() and sets error.type from the errors — the error's type
// for a single entry, _OTHER for several — but records no exception event,
// and cannot add gin's bind/render/private/public classification. So this
// handler records each entry as the exception event, carrying gin.error.type,
// and leaves the status and error.type to otelgin. It does not touch
// c.Errors. (v0.68.0 recorded every entry itself, which is why the chain
// once kept c.Errors from it; see ADR 0010's 2026-10-20 note.)
//
// tracedKey is nil when the chain has no filters. Otherwise the chain's
// otelgin gin filter sets it on a request it traces; a request it excluded has
// no span of this chain's, so the handler leaves it alone, as otelgin does.
// The span is read before c.Next, while the context is still the one otelgin
// handed on, since a downstream middleware may replace c.Request's context
// without restoring it.
func chainErrors(tracedKey *chainKey) ginframework.HandlerFunc {
	return func(c *ginframework.Context) {
		if tracedKey != nil {
			_, traced := c.Get(tracedKey)
			c.Delete(tracedKey) // read once; leave nothing of the chain's in c.Keys
			if !traced {
				c.Next()
				return
			}
		}
		span := trace.SpanFromContext(c.Request.Context())
		c.Next()
		if len(c.Errors) > 0 && span.IsRecording() {
			recordTypedExceptions(span, c.Errors)
		}
	}
}

// recordTypedExceptions records each entry with an error as an exception event
// carrying gin.error.type. An entry with a nil Err — c.Error(&gin.Error{...})
// appends one — is skipped: there is no error to record.
func recordTypedExceptions(span trace.Span, errs []*ginframework.Error) {
	for _, ge := range errs {
		if ge.Err != nil {
			span.RecordError(ge.Err,
				trace.WithAttributes(attribute.String(ginErrorTypeKey, ginErrorTypeString(ge.Type))),
			)
		}
	}
}

// lastError returns the Err of the last entry that has one, or nil.
func lastError(errs []*ginframework.Error) error {
	for i := len(errs) - 1; i >= 0; i-- {
		if errs[i].Err != nil {
			return errs[i].Err
		}
	}
	return nil
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
