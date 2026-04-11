package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	o11ylog "github.com/flywindy/o11y/internal/log"
)

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

// TestHandle_InjectsTraceIDs verifies that trace_id and span_id appear in the
// JSON output when a valid span is present in the context.
func TestHandle_InjectsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf, slog.LevelInfo)

	sc := spanContext("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "test message")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, sc.TraceID().String(), record["trace_id"], "trace_id must match")
	assert.Equal(t, sc.SpanID().String(), record["span_id"], "span_id must match")
}

// TestHandle_NoSpan verifies that trace_id and span_id are absent when there
// is no active span in the context.
func TestHandle_NoSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf, slog.LevelInfo)

	logger.InfoContext(context.Background(), "no span message")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	_, hasTraceID := record["trace_id"]
	_, hasSpanID := record["span_id"]
	assert.False(t, hasTraceID, "trace_id must be absent without a span")
	assert.False(t, hasSpanID, "span_id must be absent without a span")
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
