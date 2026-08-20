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

// ---------------------------------------------------------------------------
// OtelSlogHandler — group handling
//
// Log pipelines index traceId as a top-level field, so it must stay at the top
// level no matter how many groups the logger has opened. The handler therefore
// keeps groups to itself rather than delegating them, which makes "does the
// rest of the grouping still behave exactly like slog's own" the property most
// worth pinning.
// ---------------------------------------------------------------------------

// dropTime removes the timestamp so two handlers' output can be compared.
func dropTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

// loggerShapes covers the WithGroup/WithAttrs combinations whose nesting the
// handler has to reproduce itself.
var loggerShapes = []struct {
	name  string
	apply func(*slog.Logger) *slog.Logger
}{
	{"no group", func(l *slog.Logger) *slog.Logger { return l }},
	{"attrs only", func(l *slog.Logger) *slog.Logger { return l.With("top", "a") }},
	{"group only", func(l *slog.Logger) *slog.Logger { return l.WithGroup("g") }},
	{"attrs then group", func(l *slog.Logger) *slog.Logger { return l.With("top", "a").WithGroup("g") }},
	{"group then attrs", func(l *slog.Logger) *slog.Logger { return l.WithGroup("g").With("in", "b") }},
	{"nested groups", func(l *slog.Logger) *slog.Logger { return l.WithGroup("outer").WithGroup("inner") }},
	{"attrs at every level", func(l *slog.Logger) *slog.Logger {
		return l.With("top", "a").WithGroup("outer").With("mid", "b").WithGroup("inner").With("deep", "c")
	}},
	{"repeated attrs in one group", func(l *slog.Logger) *slog.Logger {
		return l.WithGroup("g").With("one", 1).With("two", 2)
	}},
	{"empty group name is a no-op", func(l *slog.Logger) *slog.Logger { return l.WithGroup("") }},
	{"empty group name between groups", func(l *slog.Logger) *slog.Logger {
		return l.WithGroup("outer").WithGroup("").With("in", "b")
	}},
}

// TestOTelHandler_GroupingMatchesStdlib is the equivalence check: with no span
// in context the wrapped handler must produce byte-for-byte what a bare
// slog.JSONHandler produces for the same logger shape. Anything else means the
// handler's own group bookkeeping has diverged from slog's semantics.
func TestOTelHandler_GroupingMatchesStdlib(t *testing.T) {
	for _, shape := range loggerShapes {
		t.Run(shape.name, func(t *testing.T) {
			opts := &slog.HandlerOptions{ReplaceAttr: dropTime}
			var plain, wrapped bytes.Buffer
			plainLogger := shape.apply(slog.New(slog.NewJSONHandler(&plain, opts)))
			wrappedLogger := shape.apply(slog.New(o11ylog.NewOTelHandler(slog.NewJSONHandler(&wrapped, opts))))

			plainLogger.InfoContext(context.Background(), "msg", slog.String("rec", "r"))
			wrappedLogger.InfoContext(context.Background(), "msg", slog.String("rec", "r"))

			assert.JSONEq(t, plain.String(), wrapped.String(),
				"grouping must match slog's own handler when no span is present")
		})
	}
}

// TestOTelHandler_TraceIDsStayTopLevelUnderGroups is the regression test for
// the bug this handler exists to avoid: WithGroup used to bury traceId inside
// the group, so {"req":{"traceId":…}} reached Loki and every query keyed on the
// top-level field silently matched nothing.
func TestOTelHandler_TraceIDsStayTopLevelUnderGroups(t *testing.T) {
	sc := spanContext("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	for _, shape := range loggerShapes {
		t.Run(shape.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := shape.apply(slog.New(o11ylog.NewOTelHandler(
				slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: dropTime}))))

			logger.InfoContext(ctx, "msg", slog.String("rec", "r"))

			var record map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
			assert.Equal(t, sc.TraceID().String(), record[traceIDField],
				"traceId must be a top-level field, got %s", buf.String())
			assert.Equal(t, sc.SpanID().String(), record[spanIDField],
				"spanId must be a top-level field, got %s", buf.String())
		})
	}
}

// TestOTelHandler_GroupedRecordKeepsItsOwnAttrsNested guards the other half:
// hoisting the identifiers must not drag the record's attributes out of the
// group with them.
func TestOTelHandler_GroupedRecordKeepsItsOwnAttrsNested(t *testing.T) {
	var buf bytes.Buffer
	sc := spanContext("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	logger := slog.New(o11ylog.NewOTelHandler(slog.NewJSONHandler(&buf, nil))).
		With("top", "a").
		WithGroup("outer").With("mid", "b").
		WithGroup("inner")

	logger.InfoContext(ctx, "msg", slog.String("rec", "r"))

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, sc.TraceID().String(), record[traceIDField])
	assert.Equal(t, "a", record["top"], "attrs added before any group stay top-level")

	outer, ok := record["outer"].(map[string]any)
	require.True(t, ok, "outer group must be present: %s", buf.String())
	assert.Equal(t, "b", outer["mid"])
	inner, ok := outer["inner"].(map[string]any)
	require.True(t, ok, "inner group must nest inside outer: %s", buf.String())
	assert.Equal(t, "r", inner["rec"], "the record's own attrs belong to the innermost group")
	assert.NotContains(t, inner, traceIDField, "traceId must not also appear inside the group")
}

// TestOTelHandler_DerivedHandlersAreIndependent pins that deriving two loggers
// from one grouped parent cannot leak attributes between them — the group
// frames are copied on write rather than shared.
func TestOTelHandler_DerivedHandlersAreIndependent(t *testing.T) {
	var buf bytes.Buffer

	// Both children must come from the *same* parent: two separately built
	// handlers share no backing array, so they would pass without exercising
	// anything. The parent's in-group attrs are added across several calls so
	// its frame slice carries spare capacity — the state in which two children
	// appending their own attr write to the same array unless it is clipped.
	parent := slog.New(o11ylog.NewOTelHandler(
		slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: dropTime}))).
		WithGroup("g").With("a", 1).With("b", 2).With("c", 3)

	parent.With("only", "left").InfoContext(context.Background(), "msg")
	parent.With("only", "right").InfoContext(context.Background(), "msg")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2)

	for i, want := range []string{"left", "right"} {
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(lines[i]), &rec))
		group, ok := rec["g"].(map[string]any)
		require.True(t, ok, "group missing in %s", lines[i])
		assert.Equal(t, map[string]any{"a": 1.0, "b": 2.0, "c": 3.0, "only": want}, group,
			"child %q must hold exactly the parent's attrs plus its own", want)
	}
}

// TestOTelHandler_GroupedWithAttrsResolvesLogValuerOnce pins that opening a
// group does not change when a LogValuer is resolved. Attrs installed inside a
// group are held by this handler and re-attached to every record, so without
// resolving them at derivation time a stateful or expensive value would be
// resolved once per log call — while the same logger without a group resolves
// it once, when the base handler preformats.
func TestOTelHandler_GroupedWithAttrsResolvesLogValuerOnce(t *testing.T) {
	var calls int
	valuer := countingLogValuer{count: &calls, value: slog.StringValue("resolved")}

	var buf bytes.Buffer
	logger := slog.New(o11ylog.NewOTelHandler(slog.NewJSONHandler(&buf, nil))).
		WithGroup("g").With("lazy", valuer)
	require.Equal(t, 1, calls, "deriving the logger must resolve the value once")

	for i := 0; i < 3; i++ {
		logger.InfoContext(context.Background(), "msg")
	}

	assert.Equal(t, 1, calls, "logging must not re-resolve an attr installed at derivation time")
	assert.Contains(t, buf.String(), `"lazy":"resolved"`)
}

// TestOTelHandler_GroupedWithAttrsRunsReplaceAttrPerRecord pins the known cost
// of owning the grouping, so it is a recorded trade-off rather than a surprise.
// Attrs held inside a group are re-attached to every record, so a base handler
// with ReplaceAttr configured sees them once per record; stdlib preformats them
// once when the logger is derived. Preformatting would require opening the
// group on the base handler, which is what buried traceId in the first place.
func TestOTelHandler_GroupedWithAttrsRunsReplaceAttrPerRecord(t *testing.T) {
	countPresetReplacements := func(build func(*bytes.Buffer, *slog.HandlerOptions) slog.Handler) int {
		var calls int
		var buf bytes.Buffer
		opts := &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == "preset" {
				calls++
			}
			return a
		}}
		logger := slog.New(build(&buf, opts)).WithGroup("g").With("preset", "v")
		logger.InfoContext(context.Background(), "one")
		logger.InfoContext(context.Background(), "two")
		return calls
	}

	stdlib := countPresetReplacements(func(b *bytes.Buffer, o *slog.HandlerOptions) slog.Handler {
		return slog.NewJSONHandler(b, o)
	})
	wrapped := countPresetReplacements(func(b *bytes.Buffer, o *slog.HandlerOptions) slog.Handler {
		return o11ylog.NewOTelHandler(slog.NewJSONHandler(b, o))
	})

	assert.Equal(t, 1, stdlib, "stdlib preformats a grouped attr once, at derivation")
	assert.Equal(t, 2, wrapped,
		"the wrapper re-attaches held attrs per record; change this only alongside the doc comment on OtelSlogHandler")
}

func TestOTelHandler_DerivedHandlersAreConcurrentSafe(_ *testing.T) {
	base := slog.NewJSONHandler(io.Discard, nil)
	parent := slog.New(o11ylog.NewOTelHandler(base)).WithGroup("g").With("shared", "v")
	ctx := trace.ContextWithSpanContext(context.Background(),
		spanContext("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"))

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			parent.With(slog.Int("worker", worker)).WithGroup("sub").InfoContext(ctx, "concurrent")
		}(i)
	}
	wg.Wait()
}

// TestHandlers_DoNotWriteThroughSharedRecordArray covers the slog handler
// contract: a handler must not AddAttrs to the record it was given, because the
// caller may hand the same record to a sibling handler and the two share an
// attribute array. Go's slog detects a write through a shared array and appends
// a "!BUG" attr rather than corrupting data, so that sentinel appearing in the
// sibling's output is the observable symptom.
func TestHandlers_DoNotWriteThroughSharedRecordArray(t *testing.T) {
	sc := spanContext("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	whitelist := baggageattrs.NewWhitelist("app.order.id")
	baggageCtx := baggageContext(t, baggageMember(t, "app.order.id", "order-42"))

	handlers := map[string]struct {
		build func(io.Writer) slog.Handler
		ctx   context.Context
	}{
		"OtelSlogHandler": {
			build: func(w io.Writer) slog.Handler {
				return o11ylog.NewOTelHandler(slog.NewJSONHandler(w, nil))
			},
			ctx: ctx,
		},
		"BaggageHandler": {
			build: func(w io.Writer) slog.Handler {
				return o11ylog.NewBaggageHandler(slog.NewJSONHandler(w, nil), whitelist)
			},
			ctx: baggageCtx,
		},
	}

	for name, tc := range handlers {
		t.Run(name, func(t *testing.T) {
			// Sweep attr counts across the boundary where slog.Record spills
			// from its inline array into the heap-allocated one two copies can
			// share. slog only reports the violation once a second copy also
			// appends, which is why both sides here are attr-adding handlers —
			// the shape of a caller fanning one record out to sdk.Logger's
			// handler and another of their own.
			for n := 1; n <= 18; n++ {
				var firstOut, secondOut bytes.Buffer
				first, second := tc.build(&firstOut), tc.build(&secondOut)

				rec := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
				for i := 0; i < n; i++ {
					rec.AddAttrs(slog.Int(fmt.Sprintf("a%d", i), i))
				}

				require.NoError(t, first.Handle(tc.ctx, rec))
				require.NoError(t, second.Handle(tc.ctx, rec))

				for side, out := range map[string]string{"first": firstOut.String(), "second": secondOut.String()} {
					assert.NotContains(t, out, "!BUG",
						"n=%d %s: the handler wrote through the caller's shared attr array", n, side)
				}
			}
		})
	}
}

// TestOTelHandler_UngroupedPathAllocations keeps the common (ungrouped) logger
// on its original cost: cloning a record that still fits its inline attr array
// is free, so injection must not add heap traffic to every log line.
func TestOTelHandler_UngroupedPathAllocations(t *testing.T) {
	handler := o11ylog.NewOTelHandler(discardHandler{})
	sc := spanContext("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "ordinary", 0)
	record.AddAttrs(slog.String("a", "1"), slog.Int("b", 2))

	handled := testing.AllocsPerRun(200, func() {
		_ = handler.Handle(ctx, record)
	})

	// Rendering the two IDs as hex is the irreducible cost of injecting them.
	// Cloning the record to leave the caller's copy alone must not add to it:
	// a record whose attrs still fit slog's inline array clones for free.
	var sink string
	idFormatting := testing.AllocsPerRun(200, func() {
		sink = sc.TraceID().String()
		sink = sc.SpanID().String()
	})
	_ = sink

	assert.LessOrEqual(t, handled-idFormatting, 0.0,
		"ungrouped injection allocated %.0f objects beyond rendering the IDs", handled-idFormatting)
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

// Handle runs for every emitted record, so the ordinary path — no LogValuer to
// resolve and no attr shadowing a whitelisted key — must not pay for the
// record rebuild or for a per-record key set.
func TestBaggageHandlerOrdinaryPathDoesNotAllocatePerRecordBookkeeping(t *testing.T) {
	whitelist := baggageattrs.NewWhitelist("app.order.id")
	handler := o11ylog.NewBaggageHandler(discardHandler{}, whitelist).
		WithAttrs([]slog.Attr{slog.String("preset", "value")})
	ctx := baggageContext(t, baggageMember(t, "app.order.id", "order-42"))
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "ordinary", 0)
	record.AddAttrs(slog.String("a", "1"), slog.Int("b", 2))

	baseline := testing.AllocsPerRun(200, func() {
		_ = handler.Handle(ctx, record.Clone())
	})

	// The whitelist's own attribute slice is the only permitted allocation on
	// top of what cloning the record already costs.
	clonesOnly := testing.AllocsPerRun(200, func() {
		_ = record.Clone()
	})
	assert.LessOrEqual(t, baseline-clonesOnly, 2.0,
		"ordinary path allocated %.0f objects beyond the record clone", baseline-clonesOnly)
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

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

// Field names asserted throughout; they are part of the SDK's log contract.
const (
	traceIDField = "traceId"
	spanIDField  = "spanId"
)
