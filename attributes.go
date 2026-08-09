package o11y

import (
	"context"
	"log/slog"

	"github.com/flywindy/o11y/internal/baggageattrs"
	"go.opentelemetry.io/otel/trace"
)

const (
	// UserNameKey is the semantic-convention key for the acting user's login name.
	UserNameKey = baggageattrs.UserNameKey

	// MaxBaggageValueBytes is the maximum value size accepted by SDK baggage setters.
	MaxBaggageValueBytes = baggageattrs.MaxBaggageValueBytes

	// MaxBaggageKeyBytes is the maximum key size accepted by SDK baggage setters.
	MaxBaggageKeyBytes = baggageattrs.MaxBaggageKeyBytes
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
// context without adding baggage. Values over MaxBaggageValueBytes or additions
// that would exceed the serialized W3C baggage budget return an error and leave
// the original context unchanged.
func ContextWithUser(ctx context.Context, name string) (context.Context, error) {
	return baggageattrs.ContextWithUser(ctx, name)
}

// ContextWithBaggageValue returns a child context carrying key=value as a W3C
// baggage member. It validates the W3C token, key and value lengths, SDK and
// semantic-convention reservations, and the serialized W3C baggage budget. It
// leaves ctx unchanged on failure. user.name is reserved for ContextWithUser.
// Keys are limited to MaxBaggageKeyBytes, values to MaxBaggageValueBytes, and
// the resulting baggage to 64 wire-serializable members and 8192 encoded bytes.
//
// An empty value is a no-op after key validation; it does not remove an
// existing member. Use ContextWithoutBaggageValues for selective removal, or
// baggage.ContextWithoutBaggage at a public ingress. Set values only after
// authentication/authorization and strip baggage before untrusted egress.
func ContextWithBaggageValue(ctx context.Context, key, value string) (context.Context, error) {
	return baggageattrs.ContextWithValue(ctx, key, value)
}

// ContextWithoutBaggageValues returns a child context with the named baggage
// members removed while preserving every other member. It is intended for
// internal trust boundaries. At public ingress, clear all baggage instead so
// unknown keys cannot pass through to newer downstream services.
func ContextWithoutBaggageValues(ctx context.Context, keys ...string) context.Context {
	return baggageattrs.ContextWithoutValues(ctx, keys...)
}
