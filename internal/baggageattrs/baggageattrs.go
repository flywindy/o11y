// Package baggageattrs centralizes whitelisted baggage attribute materialization.
package baggageattrs

//go:generate go run ./cmd/gensemconv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

const (
	// UserNameKey is the OTel semantic convention key for the acting user's
	// login name.
	UserNameKey = string(semconv.UserNameKey)

	// MaxBaggageValueBytes is the maximum raw baggage value accepted by SDK
	// setters and materializers.
	MaxBaggageValueBytes = 256

	// MaxBaggageKeyBytes is the maximum baggage key accepted by SDK setters and
	// configuration.
	MaxBaggageKeyBytes = 128

	maxWireMembers = 64
	maxWireBytes   = 8192
)

type whitelistedAttribute struct {
	baggageKey   string
	attributeKey attribute.Key
}

// Whitelist is an immutable, per-instance set of baggage keys.
type Whitelist struct {
	attributes []whitelistedAttribute
}

// NewWhitelist returns an immutable whitelist containing the de-duplicated
// keys in first-seen order. Callers must validate keys before constructing it.
func NewWhitelist(keys ...string) Whitelist {
	seen := make(map[string]struct{}, len(keys))
	attrs := make([]whitelistedAttribute, 0, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		attributeKey := attribute.Key(key)
		if key == UserNameKey {
			attributeKey = semconv.UserNameKey
		}
		attrs = append(attrs, whitelistedAttribute{baggageKey: key, attributeKey: attributeKey})
	}
	return Whitelist{attributes: attrs}
}

// Len returns the number of keys in the whitelist.
func (w Whitelist) Len() int { return len(w.attributes) }

// Keys returns a defensive copy of the whitelist keys.
func (w Whitelist) Keys() []string {
	keys := make([]string, len(w.attributes))
	for i, item := range w.attributes {
		keys[i] = item.baggageKey
	}
	return keys
}

// ValidBaggageKey verifies that key can be propagated safely and cannot
// impersonate SDK, slog, resource, or semantic-convention fields.
func ValidBaggageKey(key string) error {
	if key == UserNameKey {
		return errors.New("user.name is reserved; use ContextWithUser and WithUserBaggage")
	}
	return validBaggageKey(key)
}

func validBaggageKey(key string) error {
	if key == "" {
		return errors.New("baggage key must not be empty")
	}
	if len(key) > MaxBaggageKeyBytes {
		return fmt.Errorf("baggage key exceeds %d bytes", MaxBaggageKeyBytes)
	}
	if _, err := baggage.NewMember(key, "validation"); err != nil {
		return fmt.Errorf("baggage key is not a valid W3C token: %w", err)
	}
	if reason, reserved := reservedKeyReason(key); reserved {
		return fmt.Errorf("baggage key is reserved by %s", reason)
	}
	return nil
}

func reservedKeyReason(key string) (string, bool) {
	if _, ok := sdkReservedKeys[key]; ok {
		return "the SDK or slog record model", true
	}
	if _, ok := semconvReservedKeys[key]; ok {
		return "OpenTelemetry semantic conventions v1.39.0", true
	}
	for _, prefix := range semconvReservedPrefixes {
		if strings.HasPrefix(key, prefix) {
			return "an OpenTelemetry semantic-convention namespace", true
		}
	}
	return "", false
}

var sdkReservedKeys = map[string]struct{}{
	"deployment.environment.name": {},
	"environment":                 {},
	"level":                       {},
	"msg":                         {},
	"service.name":                {},
	"service.namespace":           {},
	"service.version":             {},
	"source":                      {},
	"spanId":                      {},
	"time":                        {},
	"traceId":                     {},
}

// ContextWithValue returns a child context carrying key=value after enforcing
// syntax, reservation, per-member, and serialized W3C baggage limits.
func ContextWithValue(ctx context.Context, key, value string) (context.Context, error) {
	if err := ValidBaggageKey(key); err != nil {
		return ctx, err
	}
	return contextWithValue(ctx, key, value)
}

// ContextWithUser returns a context carrying the acting user's login name as
// the reserved user.name W3C baggage member.
func ContextWithUser(ctx context.Context, name string) (context.Context, error) {
	if name == "" {
		return ctx, nil
	}
	return contextWithValue(ctx, UserNameKey, name)
}

func contextWithValue(ctx context.Context, key, value string) (context.Context, error) {
	if value == "" {
		return ctx, nil
	}
	if len(value) > MaxBaggageValueBytes {
		return ctx, fmt.Errorf("baggage value for %q exceeds %d bytes", key, MaxBaggageValueBytes)
	}
	member, err := baggage.NewMemberRaw(key, value)
	if err != nil {
		return ctx, fmt.Errorf("invalid %s baggage member: %w", key, err)
	}
	candidate, err := baggage.FromContext(ctx).SetMember(member)
	if err != nil {
		return ctx, fmt.Errorf("set %s baggage member: %w", key, err)
	}

	wireMembers := 0
	for _, candidateMember := range candidate.Members() {
		if candidateMember.String() != "" {
			wireMembers++
		}
	}
	if wireMembers > maxWireMembers {
		return ctx, fmt.Errorf("setting baggage member %q would exceed the W3C limit of %d wire members", key, maxWireMembers)
	}
	if encoded := candidate.String(); len(encoded) > maxWireBytes {
		return ctx, fmt.Errorf("setting baggage member %q would exceed the W3C limit of %d encoded bytes", key, maxWireBytes)
	}
	return baggage.ContextWithBaggage(ctx, candidate), nil
}

// ContextWithoutValues returns a child context with the named baggage members
// removed while preserving all other members.
func ContextWithoutValues(ctx context.Context, keys ...string) context.Context {
	bag := baggage.FromContext(ctx)
	for _, key := range keys {
		bag = bag.DeleteMember(key)
	}
	return baggage.ContextWithBaggage(ctx, bag)
}

// UserNameSpanAttribute returns the span attribute for the acting user's login
// name.
func UserNameSpanAttribute(name string) attribute.KeyValue { return semconv.UserName(name) }

// UserNameLogAttr returns the slog attribute for the acting user's login name.
func UserNameLogAttr(name string) slog.Attr { return slog.String(UserNameKey, name) }

// SpanAttributesFromContext returns whitelisted baggage members as span attributes.
func (w Whitelist) SpanAttributesFromContext(ctx context.Context) []attribute.KeyValue {
	bag := baggage.FromContext(ctx)
	if bag.Len() == 0 {
		return nil
	}
	attrs := make([]attribute.KeyValue, 0, len(w.attributes))
	for _, item := range w.attributes {
		member := bag.Member(item.baggageKey)
		if member.Key() == "" || member.Value() == "" || len(member.Value()) > MaxBaggageValueBytes {
			continue
		}
		attrs = append(attrs, item.attributeKey.String(member.Value()))
	}
	return attrs
}

// LogAttrsFromContext returns whitelisted baggage members as slog attributes.
func (w Whitelist) LogAttrsFromContext(ctx context.Context) []slog.Attr {
	bag := baggage.FromContext(ctx)
	if bag.Len() == 0 {
		return nil
	}
	attrs := make([]slog.Attr, 0, len(w.attributes))
	for _, item := range w.attributes {
		member := bag.Member(item.baggageKey)
		if member.Key() == "" || member.Value() == "" || len(member.Value()) > MaxBaggageValueBytes {
			continue
		}
		attrs = append(attrs, slog.String(item.baggageKey, member.Value()))
	}
	return attrs
}

// NewSpanProcessor returns a SpanProcessor that copies whitelisted baggage
// members onto every started span.
func (w Whitelist) NewSpanProcessor() sdktrace.SpanProcessor { return spanProcessor{whitelist: w} }

type spanProcessor struct{ whitelist Whitelist }

func (p spanProcessor) OnStart(parent context.Context, span sdktrace.ReadWriteSpan) {
	attrs := p.whitelist.SpanAttributesFromContext(parent)
	if len(attrs) == 0 {
		return
	}
	spanAttrs := span.Attributes()
	for _, attr := range attrs {
		if !spanHasAttribute(spanAttrs, attr.Key) {
			span.SetAttributes(attr)
		}
	}
}

func spanHasAttribute(attrs []attribute.KeyValue, key attribute.Key) bool {
	for _, attr := range attrs {
		if attr.Key == key {
			return true
		}
	}
	return false
}

func (spanProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (spanProcessor) Shutdown(context.Context) error   { return nil }
func (spanProcessor) ForceFlush(context.Context) error { return nil }
