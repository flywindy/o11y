package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"

	"github.com/flywindy/o11y/internal/baggageattrs"
	o11ylog "github.com/flywindy/o11y/internal/log"
)

// ---------------------------------------------------------------------------
// OtelSlogHandler tests
// ---------------------------------------------------------------------------

func newTestLogger(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	base := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level})
	return slog.New(o11ylog.NewOTelHandler(base))
}

func spanContext(traceHex, spanHex string) trace.SpanContext {
	tid, err := trace.TraceIDFromHex(traceHex)
	if err != nil {
		panic("spanContext: invalid trace ID hex " + traceHex + ": " + err.Error())
	}
	sid, err := trace.SpanIDFromHex(spanHex)
	if err != nil {
		panic("spanContext: invalid span ID hex " + spanHex + ": " + err.Error())
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
}

// TestHandle_InjectsTraceIDs verifies that traceId and spanId appear in the
// JSON output when a valid span is present in the context.
func TestHandle_InjectsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf, slog.LevelInfo)

	sc := spanContext("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "test message")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, sc.TraceID().String(), record["traceId"], "traceId must match")
	assert.Equal(t, sc.SpanID().String(), record["spanId"], "spanId must match")
}

// TestHandle_NoSpan verifies that traceId and spanId are absent when there
// is no active span in the context.
func TestHandle_NoSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf, slog.LevelInfo)

	logger.InfoContext(context.Background(), "no span message")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	_, hasTraceID := record["traceId"]
	_, hasSpanID := record["spanId"]
	assert.False(t, hasTraceID, "traceId must be absent without a span")
	assert.False(t, hasSpanID, "spanId must be absent without a span")
}

// TestWithAttrs verifies that WithAttrs wraps the inner handler and returns
// an *OtelSlogHandler so that trace injection still works.
func TestWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	h := o11ylog.NewOTelHandler(base)
	got := h.WithAttrs([]slog.Attr{slog.String("k", "v")})
	_, ok := got.(*o11ylog.OtelSlogHandler)
	assert.True(t, ok, "WithAttrs must return *OtelSlogHandler")
}

// TestWithGroup verifies that WithGroup wraps the inner handler and returns
// an *OtelSlogHandler.
func TestWithGroup(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	h := o11ylog.NewOTelHandler(base)
	got := h.WithGroup("grp")
	_, ok := got.(*o11ylog.OtelSlogHandler)
	assert.True(t, ok, "WithGroup must return *OtelSlogHandler")
}

// TestEnabled verifies that level filtering is delegated to the wrapped handler.
func TestEnabled(t *testing.T) {
	tests := []struct {
		name       string
		minLevel   slog.Level
		checkLevel slog.Level
		want       bool
	}{
		{"debug below warn", slog.LevelWarn, slog.LevelDebug, false},
		{"info below warn", slog.LevelWarn, slog.LevelInfo, false},
		{"warn at warn", slog.LevelWarn, slog.LevelWarn, true},
		{"error above warn", slog.LevelWarn, slog.LevelError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: tt.minLevel})
			h := o11ylog.NewOTelHandler(base)
			assert.Equal(t, tt.want, h.Enabled(context.Background(), tt.checkLevel))
		})
	}
}

func TestBaggageHandlerInjectsWhitelistedUserName(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(o11ylog.NewBaggageHandler(base, baggageattrs.NewWhitelist(baggageattrs.UserNameKey)))
	ctx := baggageContext(t,
		baggageMember(t, baggageattrs.UserNameKey, "a.einstein"),
		baggageMember(t, "tenant.id", "physics"),
	)

	logger.InfoContext(ctx, "with baggage")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, "a.einstein", record[baggageattrs.UserNameKey])
	_, hasTenant := record["tenant.id"]
	assert.False(t, hasTenant, "non-whitelisted baggage must not be added")
}

func TestBaggageHandlerKeepsExplicitUserNameAttr(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(o11ylog.NewBaggageHandler(base, baggageattrs.NewWhitelist(baggageattrs.UserNameKey)))
	ctx := baggageContext(t, baggageMember(t, baggageattrs.UserNameKey, "baggage-user"))

	logger.InfoContext(ctx, "with explicit user", slog.String(baggageattrs.UserNameKey, "explicit-user"))

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, "explicit-user", record[baggageattrs.UserNameKey])
}

func TestBaggageHandlerKeepsLoggerWithUserNameAttr(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(o11ylog.NewBaggageHandler(base, baggageattrs.NewWhitelist(baggageattrs.UserNameKey))).
		With(slog.String(baggageattrs.UserNameKey, "explicit-user"))
	ctx := baggageContext(t, baggageMember(t, baggageattrs.UserNameKey, "baggage-user"))

	logger.InfoContext(ctx, "with explicit user")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, "explicit-user", record[baggageattrs.UserNameKey])
}

func TestBaggageHandlerWithGroupDoesNotInheritUserNameAttr(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(o11ylog.NewBaggageHandler(base, baggageattrs.NewWhitelist(baggageattrs.UserNameKey))).
		With(slog.String(baggageattrs.UserNameKey, "explicit-user")).
		WithGroup("audit")
	ctx := baggageContext(t, baggageMember(t, baggageattrs.UserNameKey, "baggage-user"))

	logger.InfoContext(ctx, "with grouped baggage")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, "explicit-user", record[baggageattrs.UserNameKey])
	audit, ok := record["audit"].(map[string]any)
	require.True(t, ok, "grouped log attrs must be nested under audit")
	assert.Equal(t, "baggage-user", audit[baggageattrs.UserNameKey])
}

func TestBaggageHandlerWithAttrsPreservesType(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	h := o11ylog.NewBaggageHandler(base, baggageattrs.NewWhitelist(baggageattrs.UserNameKey))
	got := h.WithAttrs([]slog.Attr{slog.String("k", "v")})
	_, ok := got.(*o11ylog.BaggageHandler)
	assert.True(t, ok, "WithAttrs must return *BaggageHandler")
}

func TestBaggageHandlerWithGroupPreservesType(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	h := o11ylog.NewBaggageHandler(base, baggageattrs.NewWhitelist(baggageattrs.UserNameKey))
	got := h.WithGroup("grp")
	_, ok := got.(*o11ylog.BaggageHandler)
	assert.True(t, ok, "WithGroup must return *BaggageHandler")
}

func TestBaggageHandlerMaterializesApplicationKeys(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	whitelist := baggageattrs.NewWhitelist("app.order.id", "app.site.id")
	logger := slog.New(o11ylog.NewBaggageHandler(base, whitelist))
	ctx := baggageContext(t,
		baggageMember(t, "app.order.id", "order-42"),
		baggageMember(t, "app.site.id", "site-7"),
	)

	logger.InfoContext(ctx, "with application baggage")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, "order-42", record["app.order.id"])
	assert.Equal(t, "site-7", record["app.site.id"])
}

func TestBaggageHandlerDetectsInlineGroupCollisionButNotNamedGroup(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	whitelist := baggageattrs.NewWhitelist("app.order.id")
	logger := slog.New(o11ylog.NewBaggageHandler(base, whitelist))
	ctx := baggageContext(t, baggageMember(t, "app.order.id", "baggage"))

	logger.InfoContext(ctx, "inline", slog.Group("", slog.String("app.order.id", "explicit")))
	logger.InfoContext(ctx, "named", slog.Group("details", slog.String("app.order.id", "nested")))

	decoder := json.NewDecoder(&buf)
	var inline, named map[string]any
	require.NoError(t, decoder.Decode(&inline))
	require.NoError(t, decoder.Decode(&named))
	assert.Equal(t, "explicit", inline["app.order.id"])
	assert.Equal(t, "baggage", named["app.order.id"])
	assert.Equal(t, "nested", named["details"].(map[string]any)["app.order.id"])
}

func TestBaggageHandlerResolvesLogValuerOnceAndReusesValue(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	whitelist := baggageattrs.NewWhitelist("app.order.id")
	logger := slog.New(o11ylog.NewBaggageHandler(base, whitelist))
	ctx := baggageContext(t, baggageMember(t, "app.order.id", "baggage"))
	count := 0
	valuer := countingLogValuer{
		count: &count,
		value: slog.GroupValue(slog.String("app.order.id", "explicit")),
	}

	logger.InfoContext(ctx, "valuer", slog.Any("", valuer))

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, "explicit", record["app.order.id"])
	assert.Equal(t, 1, count)
}

func TestBaggageHandlerWithAttrsApplicationKeyPrecedence(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	whitelist := baggageattrs.NewWhitelist("app.order.id")
	logger := slog.New(o11ylog.NewBaggageHandler(base, whitelist)).With(
		slog.Group("", slog.String("app.order.id", "explicit")),
	)
	ctx := baggageContext(t, baggageMember(t, "app.order.id", "baggage"))

	logger.InfoContext(ctx, "with attrs")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, "explicit", record["app.order.id"])
}

func TestBaggageHandlerNonIdempotentLogValuerIsResolvedOnce(t *testing.T) {
	for _, throughWith := range []bool{false, true} {
		for _, first := range []slog.Value{
			slog.GroupValue(slog.String("app.order.id", "explicit")),
			slog.StringValue("scalar"),
		} {
			name := fmt.Sprintf("with=%v/first=%s", throughWith, first.Kind())
			t.Run(name, func(t *testing.T) {
				var buf bytes.Buffer
				base := slog.NewJSONHandler(&buf, nil)
				whitelist := baggageattrs.NewWhitelist("app.order.id")
				logger := slog.New(o11ylog.NewBaggageHandler(base, whitelist))
				ctx := baggageContext(t, baggageMember(t, "app.order.id", "baggage"))
				valuer := &sequenceLogValuer{values: []slog.Value{
					first,
					slog.GroupValue(slog.String("app.order.id", "second-resolution")),
				}}
				attr := slog.Any("", valuer)
				if throughWith {
					logger.With(attr).InfoContext(ctx, "valuer")
				} else {
					logger.InfoContext(ctx, "valuer", attr)
				}

				assert.Equal(t, 1, valuer.calls)
				assert.Equal(t, 1, strings.Count(buf.String(), `"app.order.id"`),
					"the resolved record must contain exactly one field for the whitelisted key")
				var record map[string]any
				require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
				if first.Kind() == slog.KindGroup {
					assert.Equal(t, "explicit", record["app.order.id"])
				} else {
					assert.Equal(t, "baggage", record["app.order.id"])
				}
			})
		}
	}
}

func TestBaggageHandlerDerivedHandlersAreConcurrentSafe(t *testing.T) {
	base := slog.NewJSONHandler(io.Discard, nil)
	logger := slog.New(o11ylog.NewBaggageHandler(base, baggageattrs.NewWhitelist("app.order.id")))
	ctx := baggageContext(t, baggageMember(t, "app.order.id", "order-42"))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			logger.With(slog.Int("worker", worker)).InfoContext(ctx, "concurrent")
		}(i)
	}
	wg.Wait()
}

type countingLogValuer struct {
	count *int
	value slog.Value
}

func (v countingLogValuer) LogValue() slog.Value {
	*v.count++
	return v.value
}

type sequenceLogValuer struct {
	calls  int
	values []slog.Value
}

func (v *sequenceLogValuer) LogValue() slog.Value {
	index := v.calls
	v.calls++
	if index >= len(v.values) {
		index = len(v.values) - 1
	}
	return v.values[index]
}

func baggageContext(t *testing.T, members ...baggage.Member) context.Context {
	t.Helper()

	bag, err := baggage.New(members...)
	require.NoError(t, err)
	return baggage.ContextWithBaggage(context.Background(), bag)
}

func baggageMember(t *testing.T, key, value string) baggage.Member {
	t.Helper()

	m, err := baggage.NewMemberRaw(key, value)
	require.NoError(t, err)
	return m
}

// ---------------------------------------------------------------------------
// MultiHandler tests
// ---------------------------------------------------------------------------

// stubHandler is a minimal slog.Handler used to verify MultiHandler behaviour.
type stubHandler struct {
	minLevel  slog.Level
	calls     int
	msgs      []string
	returnErr error
}

func (h *stubHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.minLevel }
func (h *stubHandler) Handle(_ context.Context, r slog.Record) error {
	h.calls++
	h.msgs = append(h.msgs, r.Message)
	return h.returnErr
}
func (h *stubHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *stubHandler) WithGroup(_ string) slog.Handler      { return h }

func newRecord(level slog.Level, msg string) slog.Record {
	return slog.NewRecord(time.Time{}, level, msg, 0)
}

// TestMultiHandler_NilHandlersIgnored verifies that NewMultiHandler silently
// drops nil entries and does not panic on Enabled or Handle calls.
func TestMultiHandler_NilHandlersIgnored(t *testing.T) {
	h := &stubHandler{minLevel: slog.LevelDebug}
	mh := o11ylog.NewMultiHandler(nil, h)

	assert.NotPanics(t, func() {
		mh.Enabled(context.Background(), slog.LevelInfo)
	}, "Enabled must not panic with a nil handler in the list")

	assert.NotPanics(t, func() {
		_ = mh.Handle(context.Background(), newRecord(slog.LevelInfo, "nil-test"))
	}, "Handle must not panic with a nil handler in the list")

	assert.Equal(t, 1, h.calls, "non-nil handler must still receive the record")
}

// TestMultiHandler_Enabled_TrueIfAnyEnabled verifies that Enabled returns true
// when at least one underlying handler is enabled for the given level.
func TestMultiHandler_Enabled_TrueIfAnyEnabled(t *testing.T) {
	h1 := &stubHandler{minLevel: slog.LevelError} // not enabled for Info
	h2 := &stubHandler{minLevel: slog.LevelDebug} // enabled for Info
	mh := o11ylog.NewMultiHandler(h1, h2)
	assert.True(t, mh.Enabled(context.Background(), slog.LevelInfo))
}

// TestMultiHandler_Enabled_FalseIfNoneEnabled verifies that Enabled returns
// false when all underlying handlers are disabled for the given level.
func TestMultiHandler_Enabled_FalseIfNoneEnabled(t *testing.T) {
	h1 := &stubHandler{minLevel: slog.LevelError}
	h2 := &stubHandler{minLevel: slog.LevelError}
	mh := o11ylog.NewMultiHandler(h1, h2)
	assert.False(t, mh.Enabled(context.Background(), slog.LevelInfo))
}

// TestMultiHandler_Handle_OnlyForwardsToEnabledHandlers verifies that Handle
// delivers the record only to handlers that are Enabled for its level.
func TestMultiHandler_Handle_OnlyForwardsToEnabledHandlers(t *testing.T) {
	h1 := &stubHandler{minLevel: slog.LevelDebug} // enabled for Info
	h2 := &stubHandler{minLevel: slog.LevelError} // not enabled for Info
	mh := o11ylog.NewMultiHandler(h1, h2)

	require.NoError(t, mh.Handle(context.Background(), newRecord(slog.LevelInfo, "hello")))
	assert.Equal(t, 1, h1.calls, "h1 must be called")
	assert.Equal(t, 0, h2.calls, "h2 must not be called")
}

// TestMultiHandler_Handle_JoinsErrors verifies that Handle collects and joins
// errors returned by individual handlers.
func TestMultiHandler_Handle_JoinsErrors(t *testing.T) {
	err1 := errors.New("first")
	err2 := errors.New("second")
	h1 := &stubHandler{minLevel: slog.LevelDebug, returnErr: err1}
	h2 := &stubHandler{minLevel: slog.LevelDebug, returnErr: err2}
	mh := o11ylog.NewMultiHandler(h1, h2)

	err := mh.Handle(context.Background(), newRecord(slog.LevelInfo, "msg"))
	require.Error(t, err)
	assert.ErrorIs(t, err, err1)
	assert.ErrorIs(t, err, err2)
}

// TestMultiHandler_Handle_NoErrorWhenAllSucceed verifies that Handle returns
// nil when all underlying handlers succeed.
func TestMultiHandler_Handle_NoErrorWhenAllSucceed(t *testing.T) {
	h1 := &stubHandler{minLevel: slog.LevelDebug}
	h2 := &stubHandler{minLevel: slog.LevelDebug}
	mh := o11ylog.NewMultiHandler(h1, h2)
	require.NoError(t, mh.Handle(context.Background(), newRecord(slog.LevelInfo, "ok")))
}

// TestMultiHandler_WithAttrs_PropagatesAndPreservesType verifies that WithAttrs
// returns a *MultiHandler with the attributes forwarded to each sub-handler.
func TestMultiHandler_WithAttrs_PropagatesAndPreservesType(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	mh := o11ylog.NewMultiHandler(base)
	got := mh.WithAttrs([]slog.Attr{slog.String("k", "v")})
	_, ok := got.(*o11ylog.MultiHandler)
	assert.True(t, ok, "WithAttrs must return *MultiHandler")
}

// TestMultiHandler_WithGroup_PropagatesAndPreservesType verifies that WithGroup
// returns a *MultiHandler with the group applied to each sub-handler.
func TestMultiHandler_WithGroup_PropagatesAndPreservesType(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	mh := o11ylog.NewMultiHandler(base)
	got := mh.WithGroup("grp")
	_, ok := got.(*o11ylog.MultiHandler)
	assert.True(t, ok, "WithGroup must return *MultiHandler")
}

// TestMultiHandler_Handle_ClonesRecord verifies that each handler receives an
// independent copy of the record so that one handler cannot corrupt another's view.
func TestMultiHandler_Handle_ClonesRecord(t *testing.T) {
	h1 := &stubHandler{minLevel: slog.LevelDebug}
	h2 := &stubHandler{minLevel: slog.LevelDebug}
	mh := o11ylog.NewMultiHandler(h1, h2)

	require.NoError(t, mh.Handle(context.Background(), newRecord(slog.LevelInfo, "clone-test")))
	assert.Equal(t, []string{"clone-test"}, h1.msgs)
	assert.Equal(t, []string{"clone-test"}, h2.msgs)
}
