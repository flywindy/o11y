package o11y_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"

	"github.com/flywindy/o11y"
)

func TestSetUserRecordsUserNameOnCurrentSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	ctx, span := tp.Tracer("user-test").Start(context.Background(), "operation")
	o11y.SetUser(ctx, "a.einstein")
	span.End()

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), semconv.UserName("a.einstein"))
}

func TestSetUserEmptyNameDoesNotRecordUserName(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	ctx, span := tp.Tracer("user-test").Start(context.Background(), "operation")
	o11y.SetUser(ctx, "")
	span.End()

	spans := sr.Ended()
	require.Len(t, spans, 1)
	for _, attr := range spans[0].Attributes() {
		assert.NotEqual(t, semconv.UserNameKey, attr.Key)
	}
}

func TestSetUserWithoutCurrentSpanDoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() {
		o11y.SetUser(context.Background(), "a.einstein")
	})
}

func TestUserNameReturnsSlogAttribute(t *testing.T) {
	attr := o11y.UserName("a.einstein")

	assert.Equal(t, slog.KindString, attr.Value.Kind())
	assert.Equal(t, string(semconv.UserNameKey), attr.Key)
	assert.Equal(t, "a.einstein", attr.Value.String())
}

func TestUserNameEmptyReturnsEmptyAttr(t *testing.T) {
	assert.Equal(t, slog.Attr{}, o11y.UserName(""))
}

func TestContextWithUserStoresUserNameBaggage(t *testing.T) {
	tests := []struct {
		name string
		user string
	}{
		{name: "ascii", user: "a.einstein"},
		{name: "spaces", user: "ada lovelace"},
		{name: "unicode", user: "\u5c45\u91cc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, err := o11y.ContextWithUser(context.Background(), tt.user)
			require.NoError(t, err)

			member := baggage.FromContext(ctx).Member(string(semconv.UserNameKey))
			assert.Equal(t, string(semconv.UserNameKey), member.Key())
			assert.Equal(t, tt.user, member.Value())
		})
	}
}

func TestContextWithUserEmptyNameNoops(t *testing.T) {
	ctx := context.Background()

	got, err := o11y.ContextWithUser(ctx, "")

	require.NoError(t, err)
	assert.Equal(t, ctx, got)
	member := baggage.FromContext(got).Member(string(semconv.UserNameKey))
	assert.Empty(t, member.Key())
}

func TestContextWithUserOverridesExistingUserNameBaggage(t *testing.T) {
	ctx, err := o11y.ContextWithUser(context.Background(), "old-user")
	require.NoError(t, err)

	ctx, err = o11y.ContextWithUser(ctx, "new-user")
	require.NoError(t, err)

	member := baggage.FromContext(ctx).Member(string(semconv.UserNameKey))
	assert.Equal(t, "new-user", member.Value())
}

func TestContextWithUserReturnsErrorForInvalidBaggageValue(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})

	ctx, err := o11y.ContextWithUser(context.Background(), invalidUTF8)

	require.Error(t, err)
	assert.Equal(t, context.Background(), ctx)
}
