package log_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	o11ylog "github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/repeat"
)

func newRecordingLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})), &buf
}

func TestLogr_MapsOTelVerbosityToSlogLevels(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelDebug)
	l := o11ylog.NewLogr(logger, nil)

	l.V(1).Info("warn-ish")
	l.V(4).Info("info-ish")
	l.V(8).Info("debug-ish")
	l.Info("plain")
	l.Error(errors.New("boom"), "errored", "component", "exporter")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 5)
	assert.Contains(t, lines[0], "level=WARN")
	assert.Contains(t, lines[0], `msg=warn-ish`)
	assert.Contains(t, lines[1], "level=INFO")
	assert.Contains(t, lines[2], "level=DEBUG")
	assert.Contains(t, lines[3], "level=INFO")
	assert.Contains(t, lines[4], "level=ERROR")
	assert.Contains(t, lines[4], `error=boom`)
	assert.Contains(t, lines[4], `component=exporter`)
}

func TestLogr_EnabledHonoursTheSlogLevel(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, nil)

	assert.True(t, l.V(1).Enabled(), "warnings pass an INFO gate")
	assert.True(t, l.V(4).Enabled())
	assert.False(t, l.V(8).Enabled(), "debug is gated by the SDK's log level")
	l.V(8).Info("hidden")
	assert.Empty(t, buf.String())
}

func TestLogr_SuppressesRepeatsByMessage(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, repeat.NewSuppressor(time.Minute, 8))

	// The OTel log BatchProcessor reports the same message with a new count on
	// every poll; only the first carries through.
	l.V(1).Info("dropped log records", "dropped", 12)
	l.V(1).Info("dropped log records", "dropped", 30)
	l.V(1).Info("another message")

	out := buf.String()
	assert.Equal(t, 1, strings.Count(out, "dropped log records"))
	assert.Contains(t, out, "dropped=12")
	assert.Contains(t, out, "repeat_suppressed_for=1m0s")
	assert.Equal(t, 1, strings.Count(out, "another message"))
}

func TestLogr_ErrorsWithDifferentTextAreNotCollapsed(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, repeat.NewSuppressor(time.Minute, 8))

	l.Error(errors.New("first"), "export failed")
	l.Error(errors.New("second"), "export failed")
	l.Error(errors.New("first"), "export failed")

	assert.Equal(t, 2, strings.Count(buf.String(), "export failed"))
}

func TestLogr_WithValuesAndWithName(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, nil).WithName("otel").WithName("sdk").WithValues("pipeline", "traces")

	l.Info("hello", "n", 1)

	out := buf.String()
	assert.Contains(t, out, "logger=otel/sdk")
	assert.Contains(t, out, "pipeline=traces")
	assert.Contains(t, out, "n=1")
}

func TestLogr_NilLoggerDiscards(t *testing.T) {
	l := o11ylog.NewLogr(nil, nil)
	assert.NotPanics(t, func() {
		l.Info("ignored")
		l.Error(errors.New("x"), "ignored")
	})
}
