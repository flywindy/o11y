// Package baggageattrs centralizes whitelisted baggage attribute materialization.
package baggageattrs

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// UserNameKey is the OTel semantic convention key for the acting user's login
// name. It is exposed here so root helpers and internal enrichers share the
// same pinned semconv source.
const UserNameKey = string(semconv.UserNameKey)

var baggageWhitelist = map[string]attribute.Key{
	UserNameKey: semconv.UserNameKey,
}

// ContextWithUser returns a context carrying the acting user's login name as a
// whitelisted W3C baggage member.
func ContextWithUser(ctx context.Context, name string) (context.Context, error) {
	member, err := baggage.NewMemberRaw(UserNameKey, name)
	if err != nil {
		return ctx, fmt.Errorf("invalid user.name baggage member: %w", err)
	}
	bag, err := baggage.FromContext(ctx).SetMember(member)
	if err != nil {
		return ctx, fmt.Errorf("set user.name baggage member: %w", err)
	}
	return baggage.ContextWithBaggage(ctx, bag), nil
}

// UserNameSpanAttribute returns the span attribute for the acting user's login
// name.
func UserNameSpanAttribute(name string) attribute.KeyValue {
	return semconv.UserName(name)
}

// UserNameLogAttr returns the slog attribute for the acting user's login name.
func UserNameLogAttr(name string) slog.Attr {
	return slog.String(UserNameKey, name)
}

// SpanAttributesFromContext returns whitelisted baggage members as span
// attributes.
func SpanAttributesFromContext(ctx context.Context) []attribute.KeyValue {
	bag := baggage.FromContext(ctx)
	if bag.Len() == 0 {
		return nil
	}
	var attrs []attribute.KeyValue
	for key, attrKey := range baggageWhitelist {
		member := bag.Member(key)
		if member.Key() == "" {
			continue
		}
		if attrs == nil {
			attrs = make([]attribute.KeyValue, 0, len(baggageWhitelist))
		}
		attrs = append(attrs, attrKey.String(member.Value()))
	}
	return attrs
}

// LogAttrsFromContext returns whitelisted baggage members as slog attributes.
func LogAttrsFromContext(ctx context.Context) []slog.Attr {
	bag := baggage.FromContext(ctx)
	if bag.Len() == 0 {
		return nil
	}
	var attrs []slog.Attr
	for key := range baggageWhitelist {
		member := bag.Member(key)
		if member.Key() == "" {
			continue
		}
		if attrs == nil {
			attrs = make([]slog.Attr, 0, len(baggageWhitelist))
		}
		attrs = append(attrs, slog.String(key, member.Value()))
	}
	return attrs
}

// NewSpanProcessor returns a SpanProcessor that copies whitelisted baggage
// members onto every started span.
func NewSpanProcessor() sdktrace.SpanProcessor {
	return spanProcessor{}
}

type spanProcessor struct{}

func (spanProcessor) OnStart(parent context.Context, span sdktrace.ReadWriteSpan) {
	attrs := SpanAttributesFromContext(parent)
	if len(attrs) == 0 {
		return
	}

	spanAttrs := span.Attributes()
	for _, attr := range attrs {
		if spanHasAttribute(spanAttrs, attr.Key) {
			continue
		}
		span.SetAttributes(attr)
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

func (spanProcessor) OnEnd(sdktrace.ReadOnlySpan) {}

func (spanProcessor) Shutdown(context.Context) error {
	return nil
}

func (spanProcessor) ForceFlush(context.Context) error {
	return nil
}
