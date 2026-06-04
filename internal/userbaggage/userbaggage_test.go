package userbaggage_test

import (
	"context"
	"testing"

	"github.com/flywindy/o11y/internal/userbaggage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

func baggageContext(t *testing.T, members ...baggage.Member) context.Context {
	t.Helper()

	bag, err := baggage.New(members...)
	require.NoError(t, err)
	return baggage.ContextWithBaggage(context.Background(), bag)
}

func member(t *testing.T, key, value string) baggage.Member {
	t.Helper()

	m, err := baggage.NewMemberRaw(key, value)
	require.NoError(t, err)
	return m
}

func TestSpanAttributesFromContextReturnsWhitelistedUserNameOnly(t *testing.T) {
	ctx := baggageContext(t,
		member(t, userbaggage.UserNameKey, "a.einstein"),
		member(t, "tenant.id", "physics"),
	)

	attrs := userbaggage.SpanAttributesFromContext(ctx)

	assert.Equal(t, []attribute.KeyValue{semconv.UserName("a.einstein")}, attrs)
}

func TestLogAttrsFromContextReturnsWhitelistedUserNameOnly(t *testing.T) {
	ctx := baggageContext(t,
		member(t, userbaggage.UserNameKey, "a.einstein"),
		member(t, "tenant.id", "physics"),
	)

	attrs := userbaggage.LogAttrsFromContext(ctx)

	require.Len(t, attrs, 1)
	assert.Equal(t, userbaggage.UserNameKey, attrs[0].Key)
	assert.Equal(t, "a.einstein", attrs[0].Value.String())
}

func TestSpanProcessorCopiesUserBaggageOnStart(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(userbaggage.NewSpanProcessor()),
		sdktrace.WithSpanProcessor(sr),
	)
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})
	ctx := baggageContext(t, member(t, userbaggage.UserNameKey, "a.einstein"))

	_, span := tp.Tracer("test").Start(ctx, "operation")
	span.End()

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), semconv.UserName("a.einstein"))
}

func TestSpanProcessorDoesNotOverrideExplicitStartAttribute(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(userbaggage.NewSpanProcessor()),
		sdktrace.WithSpanProcessor(sr),
	)
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})
	ctx := baggageContext(t, member(t, userbaggage.UserNameKey, "baggage-user"))

	_, span := tp.Tracer("test").Start(ctx, "operation", trace.WithAttributes(semconv.UserName("explicit-user")))
	span.End()

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), semconv.UserName("explicit-user"))
	assert.NotContains(t, spans[0].Attributes(), semconv.UserName("baggage-user"))
}
