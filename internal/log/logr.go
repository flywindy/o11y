package log

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/flywindy/o11y/internal/redact"
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
//
// endpoints are the configured export endpoints; an error that quotes one
// back (net/url does, verbatim) goes through redact.InText before it is
// logged or used as a repeat key, so embedded credentials never reach the
// log.
func NewLogr(logger *slog.Logger, suppress *repeat.Suppressor, endpoints ...string) logr.Logger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return logr.New(&logrSink{logger: logger, suppress: suppress, now: time.Now, endpoints: endpoints})
}

// logrSink adapts logr's LogSink to slog.
type logrSink struct {
	logger    *slog.Logger
	suppress  *repeat.Suppressor
	now       func() time.Time
	endpoints []string
	name      string
	values    []any
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

// Error implements logr.LogSink. The error text, with any configured
// endpoint's credentials redacted, is carried as the "error" attribute and
// takes part in the repeat key, so two different errors under the same
// message are each logged.
func (s *logrSink) Error(err error, msg string, keysAndValues ...any) {
	key := msg
	if err != nil {
		text := redact.InText(err.Error(), s.endpoints...)
		key = msg + ": " + text
		keysAndValues = append([]any{slog.String("error", text)}, keysAndValues...)
	}
	s.write(slog.LevelError, key, msg, keysAndValues)
}

// write applies suppression keyed by key and emits msg with the sink's
// name, accumulated values and the caller's key/value pairs, each value
// normalized by resolve.
func (s *logrSink) write(level slog.Level, key, msg string, keysAndValues []any) {
	if s.suppress != nil && s.suppress.SuppressedAt(key, s.now()) {
		return
	}
	args := make([]any, 0, len(s.values)+len(keysAndValues)+2)
	if s.name != "" {
		args = append(args, slog.String("logger", s.name))
	}
	for _, v := range s.values {
		args = append(args, s.resolve(v))
	}
	for _, v := range keysAndValues {
		args = append(args, s.resolve(v))
	}
	if s.suppress != nil {
		args = append(args, slog.String("repeat_suppressed_for", s.suppress.Window().String()))
	}
	s.logger.Log(context.Background(), level, msg, args...)
}

// resolve prepares one logr key or value for slog. A logr.Marshaler (OTel
// passes an attribute.Set as the "attributes" value of its "Tracer created"
// diagnostic) is replaced by what MarshalLog returns, since slog would
// otherwise see only unexported fields and render "{}". Strings, errors and
// string-valued slog.Attrs then go through redact.InText with the
// configured endpoints: otlptracehttp reports an endpoint that fails to
// parse as a "url" value beside the error, and the error text alone being
// redacted would leave the credential in that field. Keys are strings too
// and pass through unchanged in practice; redacting them is harmless.
func (s *logrSink) resolve(v any) any {
	if m, ok := v.(logr.Marshaler); ok {
		v = m.MarshalLog()
	}
	switch t := v.(type) {
	case string:
		return redact.InText(t, s.endpoints...)
	case error:
		return redact.InText(t.Error(), s.endpoints...)
	case slog.Attr:
		if t.Value.Kind() == slog.KindString {
			return slog.String(t.Key, redact.InText(t.Value.String(), s.endpoints...))
		}
		return t
	default:
		return v
	}
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
