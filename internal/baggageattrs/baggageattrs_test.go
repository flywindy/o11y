package baggageattrs_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/flywindy/o11y/internal/baggageattrs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
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
		member(t, baggageattrs.UserNameKey, "a.einstein"),
		member(t, "tenant.id", "physics"),
	)

	attrs := baggageattrs.NewWhitelist(baggageattrs.UserNameKey).SpanAttributesFromContext(ctx)

	assert.Equal(t, []attribute.KeyValue{semconv.UserName("a.einstein")}, attrs)
}

func TestSpanAttributesFromContextSkipsEmptyUserName(t *testing.T) {
	ctx := baggageContext(t, member(t, baggageattrs.UserNameKey, ""))

	attrs := baggageattrs.NewWhitelist(baggageattrs.UserNameKey).SpanAttributesFromContext(ctx)

	assert.Empty(t, attrs)
}

func TestLogAttrsFromContextReturnsWhitelistedUserNameOnly(t *testing.T) {
	ctx := baggageContext(t,
		member(t, baggageattrs.UserNameKey, "a.einstein"),
		member(t, "tenant.id", "physics"),
	)

	attrs := baggageattrs.NewWhitelist(baggageattrs.UserNameKey).LogAttrsFromContext(ctx)

	require.Len(t, attrs, 1)
	assert.Equal(t, baggageattrs.UserNameKey, attrs[0].Key)
	assert.Equal(t, "a.einstein", attrs[0].Value.String())
}

func TestLogAttrsFromContextSkipsEmptyUserName(t *testing.T) {
	ctx := baggageContext(t, member(t, baggageattrs.UserNameKey, ""))

	attrs := baggageattrs.NewWhitelist(baggageattrs.UserNameKey).LogAttrsFromContext(ctx)

	assert.Empty(t, attrs)
}

func TestContextWithUserEmptyNameNoops(t *testing.T) {
	ctx := baggageContext(t, member(t, "tenant.id", "physics"))

	got, err := baggageattrs.ContextWithUser(ctx, "")

	require.NoError(t, err)
	assert.Equal(t, ctx, got)
	assert.Empty(t, baggage.FromContext(got).Member(baggageattrs.UserNameKey).Key())
}

func TestSpanProcessorCopiesUserBaggageOnStart(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(baggageattrs.NewWhitelist(baggageattrs.UserNameKey).NewSpanProcessor()),
		sdktrace.WithSpanProcessor(sr),
	)
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})
	ctx := baggageContext(t, member(t, baggageattrs.UserNameKey, "a.einstein"))

	_, span := tp.Tracer("test").Start(ctx, "operation")
	span.End()

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), semconv.UserName("a.einstein"))
}

func TestSpanProcessorDoesNotOverrideExplicitStartAttribute(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(baggageattrs.NewWhitelist(baggageattrs.UserNameKey).NewSpanProcessor()),
		sdktrace.WithSpanProcessor(sr),
	)
	t.Cleanup(func() {
		require.NoError(t, tp.Shutdown(context.Background()))
	})
	ctx := baggageContext(t, member(t, baggageattrs.UserNameKey, "baggage-user"))

	_, span := tp.Tracer("test").Start(ctx, "operation", trace.WithAttributes(semconv.UserName("explicit-user")))
	span.End()

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), semconv.UserName("explicit-user"))
	assert.NotContains(t, spans[0].Attributes(), semconv.UserName("baggage-user"))
}

func TestWhitelistMaterializesApplicationKeysAndIsImmutable(t *testing.T) {
	keys := []string{"app.order.id", "app.site.id", "app.order.id"}
	whitelist := baggageattrs.NewWhitelist(keys...)
	keys[0] = "mutated"
	returned := whitelist.Keys()
	returned[0] = "also-mutated"
	ctx := baggageContext(t,
		member(t, "app.order.id", "order-42"),
		member(t, "app.site.id", "site-7"),
	)

	assert.Equal(t, 2, whitelist.Len())
	assert.Equal(t, []string{"app.order.id", "app.site.id"}, whitelist.Keys())
	assert.Equal(t, []attribute.KeyValue{
		attribute.String("app.order.id", "order-42"),
		attribute.String("app.site.id", "site-7"),
	}, whitelist.SpanAttributesFromContext(ctx))
	assert.Empty(t, baggageattrs.NewWhitelist("app.other.id").SpanAttributesFromContext(ctx),
		"separate whitelist instances must not share keys")
}

func TestWhitelistSkipsOverlongInboundValues(t *testing.T) {
	ctx := baggageContext(t,
		member(t, baggageattrs.UserNameKey, strings.Repeat("u", baggageattrs.MaxBaggageValueBytes+1)),
		member(t, "app.order.id", strings.Repeat("x", baggageattrs.MaxBaggageValueBytes+1)),
	)
	whitelist := baggageattrs.NewWhitelist(baggageattrs.UserNameKey, "app.order.id")

	assert.Empty(t, whitelist.SpanAttributesFromContext(ctx))
	assert.Empty(t, whitelist.LogAttrsFromContext(ctx))
}

func TestValidBaggageKey(t *testing.T) {
	tests := []struct {
		key     string
		wantErr bool
	}{
		{"app.order.id", false},
		{"", true},
		{"not a token", true},
		{strings.Repeat("a", baggageattrs.MaxBaggageKeyBytes+1), true},
		{baggageattrs.UserNameKey, true},
		{"service.name", true},
		{"http.response.body.size", true},
		{"http.response.status_code", true},
		{"http.request.header.x-order", true},
		{"container.label.team", true},
		{"db.operation.parameter.id", true},
		{"db.query.parameter.id", true},
		{"k8s.pod.label.app", true},
		{"rpc.request.metadata.authorization", true},
		{"rpc.response.metadata.result", true},
		{"host.custom", true},
		{"process.custom", true},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			err := baggageattrs.ValidBaggageKey(tt.key)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestContextWithValueValidatesAndPreservesOriginalContextOnError(t *testing.T) {
	ctx := baggageContext(t, member(t, "app.existing", "kept"))

	got, err := baggageattrs.ContextWithValue(ctx, "", "value")
	require.Error(t, err)
	assert.Equal(t, ctx, got)

	got, err = baggageattrs.ContextWithValue(ctx, baggageattrs.UserNameKey, "user")
	require.Error(t, err)
	assert.Equal(t, ctx, got)

	got, err = baggageattrs.ContextWithValue(ctx, "app.order.id", strings.Repeat("x", baggageattrs.MaxBaggageValueBytes+1))
	require.Error(t, err)
	assert.Equal(t, ctx, got)

	got, err = baggageattrs.ContextWithValue(ctx, "app.order.id", "")
	require.NoError(t, err)
	assert.Equal(t, ctx, got)

	got, err = baggageattrs.ContextWithValue(ctx, "", "")
	require.Error(t, err)
	assert.Equal(t, ctx, got)

	got, err = baggageattrs.ContextWithValue(ctx, baggageattrs.UserNameKey, "")
	require.Error(t, err)
	assert.Equal(t, ctx, got)
}

func TestContextWithValuePreservesUnicodeAndEnforcesWireMemberLimit(t *testing.T) {
	ctx, err := baggageattrs.ContextWithValue(context.Background(), "app.order.id", "訂單 42")
	require.NoError(t, err)
	assert.Equal(t, "訂單 42", baggage.FromContext(ctx).Member("app.order.id").Value())

	members := make([]baggage.Member, 0, 64)
	for i := 0; i < 64; i++ {
		members = append(members, member(t, fmt.Sprintf("app.k%d", i), "v"))
	}
	full := baggageContext(t, members...)
	got, err := baggageattrs.ContextWithValue(full, "app.overflow", "v")
	require.Error(t, err)
	assert.Equal(t, full, got)
}

func TestContextWithValueRoundTripsThroughW3CPropagation(t *testing.T) {
	prop := propagation.Baggage{}
	ctx, err := baggageattrs.ContextWithValue(context.Background(), "chat.room.id", "房間 42")
	require.NoError(t, err)
	carrier := propagation.MapCarrier{}
	prop.Inject(ctx, carrier)
	extracted := prop.Extract(context.Background(), carrier)
	assert.Equal(t, "房間 42", baggage.FromContext(extracted).Member("chat.room.id").Value())

	for _, invalid := range []string{"chat room.id", "房間.id", "chat@room"} {
		got, setErr := baggageattrs.ContextWithValue(context.Background(), invalid, "v")
		require.Error(t, setErr)
		assert.Empty(t, baggage.FromContext(got).Member(invalid).Key())
	}
}

func TestContextWithValueCountsOnlyWireSerializableMembers(t *testing.T) {
	members := make([]baggage.Member, 0, 64)
	for i := 0; i < 64; i++ {
		members = append(members, member(t, fmt.Sprintf("invalid key %d", i), "v"))
	}
	ctx := baggageContext(t, members...)

	got, err := baggageattrs.ContextWithValue(ctx, "app.order.id", "order-42")
	require.NoError(t, err)
	assert.Equal(t, "order-42", baggage.FromContext(got).Member("app.order.id").Value())
	carrier := propagation.MapCarrier{}
	propagation.Baggage{}.Inject(got, carrier)
	assert.Equal(t, "app.order.id=order-42", carrier.Get("baggage"))
}

func TestContextWithUserEnforcesEncodedWireBudget(t *testing.T) {
	bag, err := baggage.New()
	require.NoError(t, err)
	for i := 0; i < 32; i++ {
		bag, err = bag.SetMember(member(t, fmt.Sprintf("app.k%02d", i), strings.Repeat("x", 255)))
		require.NoError(t, err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bag)

	got, err := baggageattrs.ContextWithUser(ctx, "user")
	require.Error(t, err)
	assert.Equal(t, ctx, got)
}

func TestContextWithUserEnforcesValueAndWireMemberLimits(t *testing.T) {
	base := context.Background()
	got, err := baggageattrs.ContextWithUser(base, strings.Repeat("u", baggageattrs.MaxBaggageValueBytes+1))
	require.Error(t, err)
	assert.Equal(t, base, got)

	members := make([]baggage.Member, 0, 64)
	for i := 0; i < 64; i++ {
		members = append(members, member(t, fmt.Sprintf("app.member.%d", i), "v"))
	}
	full := baggageContext(t, members...)
	got, err = baggageattrs.ContextWithUser(full, "ordinary-user")
	require.Error(t, err)
	assert.Equal(t, full, got)
}

func TestContextWithoutValuesRemovesOnlyNamedMembers(t *testing.T) {
	ctx := baggageContext(t,
		member(t, baggageattrs.UserNameKey, "a.einstein"),
		member(t, "app.order.id", "order-42"),
		member(t, "app.site.id", "site-7"),
	)

	got := baggageattrs.ContextWithoutValues(ctx, "app.order.id", "missing")
	bag := baggage.FromContext(got)
	assert.Empty(t, bag.Member("app.order.id").Key())
	assert.Equal(t, "a.einstein", bag.Member(baggageattrs.UserNameKey).Value())
	assert.Equal(t, "site-7", bag.Member("app.site.id").Value())

	got = baggageattrs.ContextWithoutValues(got, baggageattrs.UserNameKey)
	assert.Empty(t, baggage.FromContext(got).Member(baggageattrs.UserNameKey).Key())
}

func TestBaggageParseBudgetBehavior(t *testing.T) {
	members := make([]string, 70)
	for i := range members {
		members[i] = fmt.Sprintf("k%d=v", i)
	}
	partial, err := baggage.Parse(strings.Join(members, ","))
	require.Error(t, err)
	assert.Equal(t, 64, partial.Len(), "member overflow should return the first 64 members")

	overBytes, err := baggage.Parse("k=" + strings.Repeat("x", 8192))
	require.Error(t, err)
	assert.Equal(t, 0, overBytes.Len(), "byte overflow should return empty baggage")
}

func TestSpanProcessorAllowsLaterAttributeOverride(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(baggageattrs.NewWhitelist("app.order.id").NewSpanProcessor()),
		sdktrace.WithSpanProcessor(sr),
	)
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	ctx := baggageContext(t, member(t, "app.order.id", "baggage"))

	_, span := tp.Tracer("test").Start(ctx, "operation")
	span.SetAttributes(attribute.String("app.order.id", "explicit-later"))
	span.End()

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Attributes(), attribute.String("app.order.id", "explicit-later"))
	assert.NotContains(t, spans[0].Attributes(), attribute.String("app.order.id", "baggage"))
}
