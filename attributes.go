package o11y

import (
	"context"
	"log/slog"

	"github.com/flywindy/o11y/internal/baggageattrs"
	"go.opentelemetry.io/otel/trace"
)

// SetUser records the acting user's login name on the current span as the
// OpenTelemetry semantic convention attribute user.name.
//
// SetUser is an explicit, in-process helper: it does not add the field to log
// records, does not store the value in baggage, and does not propagate it across
// service boundaries. Use UserName alongside SetUser when the same username
// should also be present on a slog record. Empty names are ignored.
func SetUser(ctx context.Context, name string) {
	if name == "" {
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(baggageattrs.UserNameSpanAttribute(name))
}

// UserName returns a slog attribute for the acting user's login name using the
// OpenTelemetry semantic convention key user.name.
//
// UserName is an explicit log helper: it only affects the log record where the
// returned attribute is supplied. It does not record the username on spans and
// does not propagate it across service boundaries. Empty names return an empty
// slog attribute.
func UserName(name string) slog.Attr {
	if name == "" {
		return slog.Attr{}
	}
	return baggageattrs.UserNameLogAttr(name)
}

// ContextWithUser returns a child context that carries the acting user's login
// name as the OpenTelemetry baggage member user.name.
//
// This is the source-side opt-in for ADR 0016 automatic user identity
// propagation. Because the SDK propagator already includes W3C Baggage, the
// value can travel across HTTP and NATS boundaries. Use this only after
// authenticating the user, and strip baggage before egress to untrusted third
// parties. Services must also opt in with WithUserBaggage to materialize the
// baggage value onto their own spans and logs. Empty names return the original
// context without adding baggage.
func ContextWithUser(ctx context.Context, name string) (context.Context, error) {
	return baggageattrs.ContextWithUser(ctx, name)
}
