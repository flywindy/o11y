package log

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/flywindy/o11y/internal/repeat"
)

// NewLogr returns a logr.Logger that writes OpenTelemetry's internal
// diagnostics to logger as structured records, so a caller can install it
// with otel.SetLogger and stop the OTel SDK from printing plain text to
// stderr.
//
// Verbosity follows the convention the OTel Go SDK uses for its own
// messages (see go.opentelemetry.io/otel/internal/global): V(1) is a
// warning, V(4) is informational, V(8) is debug. Levels in between round to
// the next named level, so V(2) and V(3) are informational and V(5) and
// above are debug; V(0), which the SDK never uses, is informational.
//
// When suppress is non-nil each distinct message is written once per its
// window, keyed by the message text alone: the SDK repeats "dropped log
// records" with a different count on every poll while the collector is
// unreachable, and one line per poll would say nothing new. The first
// occurrence in each window carries the count it saw.
func NewLogr(logger *slog.Logger, suppress *repeat.Suppressor) logr.Logger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return logr.New(&logrSink{logger: logger, suppress: suppress, now: time.Now})
}

// logrSink adapts logr's LogSink to slog.
type logrSink struct {
	logger   *slog.Logger
	suppress *repeat.Suppressor
	now      func() time.Time
	name     string
	values   []any
}

// slogLevel maps a logr verbosity to the slog level OTel's convention gives
// it.
func slogLevel(v int) slog.Level {
	switch {
	case v == 1:
		return slog.LevelWarn
	case v <= 4:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}

// Init implements logr.LogSink.
func (s *logrSink) Init(logr.RuntimeInfo) {}

// Enabled implements logr.LogSink.
func (s *logrSink) Enabled(level int) bool {
	return s.logger.Enabled(context.Background(), slogLevel(level))
}

// Info implements logr.LogSink.
func (s *logrSink) Info(level int, msg string, keysAndValues ...any) {
	s.write(slogLevel(level), msg, msg, keysAndValues)
}

// Error implements logr.LogSink. The error is carried as the "error"
// attribute and takes part in the repeat key, so two different errors under
// the same message are each logged.
func (s *logrSink) Error(err error, msg string, keysAndValues ...any) {
	key := msg
	if err != nil {
		key = msg + ": " + err.Error()
		keysAndValues = append([]any{slog.Any("error", err)}, keysAndValues...)
	}
	s.write(slog.LevelError, key, msg, keysAndValues)
}

func (s *logrSink) write(level slog.Level, key, msg string, keysAndValues []any) {
	if s.suppress != nil && s.suppress.SuppressedAt(key, s.now()) {
		return
	}
	args := make([]any, 0, len(s.values)+len(keysAndValues)+2)
	if s.name != "" {
		args = append(args, slog.String("logger", s.name))
	}
	args = append(args, s.values...)
	args = append(args, keysAndValues...)
	if s.suppress != nil {
		args = append(args, slog.String("repeat_suppressed_for", s.suppress.Window().String()))
	}
	s.logger.Log(context.Background(), level, msg, args...)
}

// WithValues implements logr.LogSink.
func (s *logrSink) WithValues(keysAndValues ...any) logr.LogSink {
	clone := *s
	clone.values = append(append([]any(nil), s.values...), keysAndValues...)
	return &clone
}

// WithName implements logr.LogSink.
func (s *logrSink) WithName(name string) logr.LogSink {
	clone := *s
	clone.name = strings.TrimPrefix(s.name+"/"+name, "/")
	return &clone
}
