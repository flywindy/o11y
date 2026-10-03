package log

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// captureHandler records the attributes of each handled record, with the
// attributes added through WithAttrs first.
type captureHandler struct {
	pre     []slog.Attr
	records *[][]slog.Attr
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := append([]slog.Attr(nil), h.pre...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	*h.records = append(*h.records, attrs)
	return nil
}

func (h captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return captureHandler{pre: append(append([]slog.Attr(nil), h.pre...), attrs...), records: h.records}
}

func (h captureHandler) WithGroup(string) slog.Handler { return h }

type wrappedErrValuer struct{ err error }

func (v wrappedErrValuer) LogValue() slog.Value { return slog.AnyValue(v.err) }

type nilPtrErr struct{ msg string }

func (e *nilPtrErr) Error() string { return e.msg }

type nilSafeErr struct{}

func (e *nilSafeErr) Error() string {
	if e == nil {
		return "no-op"
	}
	return "set"
}

type panicErr struct{}

func (panicErr) Error() string { panic("bad error") }

func TestErrorStringHandler(t *testing.T) {
	var records [][]slog.Attr
	logger := slog.New(NewErrorStringHandler(captureHandler{records: &records})).
		With(slog.Any("setup", errors.New("setup failed")))

	var typedNil *nilPtrErr
	valuer := wrappedErrValuer{errors.New("resolved")}
	nested := slog.Group("req", slog.Any("err", errors.New("nested")), slog.Int("n", 1))
	badGroup := slog.Group("bad", slog.Any("err", error(typedNil)))
	logger.Error("publish failed",
		slog.Any("error", errors.New("boom")),
		slog.Any("cause", errors.New("root")),
		slog.Group("", slog.Any("inlined", errors.New("flat"))),
		slog.Any("typed_nil", error(typedNil)),
		slog.Any("nil_safe", error((*nilSafeErr)(nil))),
		slog.Any("panics", panicErr{}),
		nested,
		badGroup,
		slog.Any("valuer", valuer),
		slog.String("plain", "kept"),
	)

	require.Len(t, records, 1)
	got := map[string]slog.Value{}
	var inlined []slog.Attr
	for _, a := range records[0] {
		if a.Key == "" {
			inlined = a.Value.Group()
			continue
		}
		got[a.Key] = a.Value
	}
	for key, want := range map[string]string{
		"setup": "setup failed", "error": "boom", "cause": "root",
		"typed_nil": "<nil>", "nil_safe": "no-op", "panics": "!PANIC: bad error", "plain": "kept",
	} {
		require.Contains(t, got, key)
		assert.Equal(t, slog.KindString, got[key].Kind(), key)
		assert.Equal(t, want, got[key].String(), key)
	}
	assert.Equal(t, []slog.Attr{slog.String("inlined", "flat")}, inlined, "an empty-key group is inlined, so its errors are converted")
	assert.True(t, slog.Group("req", slog.String("err", "nested"), slog.Int("n", 1)).Value.Equal(got["req"]),
		"errors inside a keyed group are converted too")
	assert.True(t, slog.Group("bad", slog.String("err", "<nil>")).Value.Equal(got["bad"]),
		"a panicking Error inside a group is recovered here, not in the bridge")
	assert.Equal(t, slog.KindLogValuer, got["valuer"].Kind(), "a LogValuer is not resolved here")
}

func TestErrorStringHandler_PassesRecordWithoutErrorsUnchanged(t *testing.T) {
	var records [][]slog.Attr
	logger := slog.New(NewErrorStringHandler(captureHandler{records: &records}))
	logger.Info("ok", slog.Int("n", 1))
	require.Len(t, records, 1)
	assert.Equal(t, []slog.Attr{slog.Int("n", 1)}, records[0])
	assert.Nil(t, NewErrorStringHandler(nil))
}

// memoryExporter keeps exported log records.
type memoryExporter struct{ records []sdklog.Record }

func (e *memoryExporter) Export(_ context.Context, records []sdklog.Record) error {
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return nil
}
func (e *memoryExporter) Shutdown(context.Context) error   { return nil }
func (e *memoryExporter) ForceFlush(context.Context) error { return nil }

// TestErrorStringHandler_OverOtelslog runs a record through the same chain as
// the SDK's OTLP path, the wrapper over the otelslog bridge over the log SDK,
// and checks the error stays a string attribute under its own key instead of
// becoming exception.message / exception.type.
func TestErrorStringHandler_OverOtelslog(t *testing.T) {
	exp := &memoryExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })

	for _, tc := range []struct {
		name    string
		handler slog.Handler
		want    map[string]string
	}{
		{
			name:    "wrapped",
			handler: NewErrorStringHandler(otelslog.NewHandler("test", otelslog.WithLoggerProvider(lp))),
			want:    map[string]string{"error": "boom", "cause": "root", "valuer": "resolved"},
		},
		{
			// Pins the bridge behaviour the wrapper exists for: if a bump
			// stops promoting errors, this case fails and the wrapper can go.
			name:    "bare bridge",
			handler: otelslog.NewHandler("test", otelslog.WithLoggerProvider(lp)),
			want:    map[string]string{"exception.message": "root", "valuer": "resolved"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp.records = nil
			attrs := []any{
				slog.Any("error", errors.New("boom")),
				slog.Any("cause", errors.New("root")),
				slog.Group("req", slog.Any("err", errors.New("nested"))),
				slog.Any("valuer", wrappedErrValuer{errors.New("resolved")}),
			}
			if tc.name == "wrapped" {
				var typedNil *nilPtrErr
				attrs = append(attrs, slog.Group("bad", slog.Any("err", error(typedNil))))
			}
			slog.New(tc.handler).Error("publish failed", attrs...)

			require.Len(t, exp.records, 1)
			got := map[string]string{}
			exp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
				got[string(kv.Key)] = kv.Value.AsString()
				return true
			})
			for k, v := range tc.want {
				assert.Equal(t, v, got[k], k)
			}
			if tc.name == "wrapped" {
				assert.NotContains(t, got, "exception.message")
			}
		})
	}
}
