package log_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	o11ylog "github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/repeat"
)

// newRecordingLogger returns a text-format slog logger writing into a buffer.
func newRecordingLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})), &buf
}

// TestLogr_MapsOTelVerbosityToSlogLevels pins the V-level to slog level mapping.
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

// TestLogr_EnabledHonoursTheSlogLevel checks Enabled follows the slog gate.
func TestLogr_EnabledHonoursTheSlogLevel(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, nil)

	assert.True(t, l.V(1).Enabled(), "warnings pass an INFO gate")
	assert.True(t, l.V(4).Enabled())
	assert.False(t, l.V(8).Enabled(), "debug is gated by the SDK's log level")
	l.V(8).Info("hidden")
	assert.Empty(t, buf.String())
}

// TestLogr_SuppressesRepeatsByMessage checks repeats collapse on message text
// alone, so a changing count does not defeat suppression.
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

// TestLogr_ErrorsWithDifferentTextAreNotCollapsed checks the error text takes
// part in the repeat key.
func TestLogr_ErrorsWithDifferentTextAreNotCollapsed(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, repeat.NewSuppressor(time.Minute, 8))

	l.Error(errors.New("first"), "export failed")
	l.Error(errors.New("second"), "export failed")
	l.Error(errors.New("first"), "export failed")

	assert.Equal(t, 2, strings.Count(buf.String(), "export failed"))
}

// TestLogr_RedactsEndpointCredentials checks an error that quotes a
// configured endpoint back loses the endpoint's userinfo before it is
// logged, on the same terms as the SDK's error handler.
func TestLogr_RedactsEndpointCredentials(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, repeat.NewSuppressor(time.Minute, 8), "http://svc:hunter2@collector:4318")

	l.Error(errors.New(`Post "http://svc:hunter2@collector:4318/v1/traces": EOF`), "export failed")

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.Contains(t, out, "collector:4318")
	assert.Contains(t, out, "export failed")
}

// TestLogr_RedactsEndpointCredentialsInValues covers the shape otlptracehttp
// produces for an endpoint that fails to parse: the URL arrives as a "url"
// key/value beside the error, so redacting the error text alone would leave
// the credential in the structured field. Accumulated WithValues and
// slog.Attr values go through the same redaction.
func TestLogr_RedactsEndpointCredentialsInValues(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const endpoint = "http://svc:hunter2@collector:4318"
	l := o11ylog.NewLogr(logger, nil, endpoint).WithValues("configured", endpoint)

	// The "raw" value is not a configured endpoint and does not even parse;
	// redact.InText's userinfo rule still strips it. The group nests a copy
	// two levels deep and is passed as a bare Attr, the way slog expects one.
	l.Error(errors.New("invalid endpoint"), "otlptrace: parse endpoint",
		"url", endpoint, "attr", slog.String("again", endpoint), "raw", "http://user:secret%zz@host",
		slog.Group("exporter", slog.Group("config", slog.String("endpoint", endpoint))))

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.NotContains(t, out, "secret%zz")
	assert.Equal(t, 4, strings.Count(out, "collector:4318"), "every redacted copy keeps the host")
	assert.Contains(t, out, "exporter.config.endpoint=", "the group structure survives redaction")
	assert.Contains(t, out, "url=")
	assert.Contains(t, out, "configured=")
}

// marshalsToText implements logr.Marshaler the way attribute.Set does, with
// nothing slog could render on its own.
type marshalsToText struct{ hidden string }

// MarshalLog implements logr.Marshaler.
func (m marshalsToText) MarshalLog() any { return "resolved:" + m.hidden }

// TestLogr_ResolvesMarshalerValues checks a logr.Marshaler value reaches
// slog as what MarshalLog returns rather than as an opaque struct; OTel's
// "Tracer created" diagnostic passes an attribute.Set this way.
func TestLogr_ResolvesMarshalerValues(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, nil)

	l.Info("Tracer created", "attributes", marshalsToText{hidden: "scope"})
	l.Info("attribute set", "attributes", attribute.NewSet(attribute.String("k", "v")))

	out := buf.String()
	assert.Contains(t, out, "attributes=resolved:scope")
	assert.NotContains(t, out, "attributes={}")
	assert.Contains(t, out, `k`, "the attribute.Set's contents survive MarshalLog")
}

// TestLogr_RedactsMarshalerAndContainerValues covers the shape the OTel
// tracer provider produces for "Tracer created": an attribute.Set, whose
// MarshalLog returns a map[string]string, so an instrumentation attribute
// carrying the configured endpoint must be redacted inside that map. Maps
// and slices passed directly, nested in each other, and carried as
// slog.Any go through the same path.
func TestLogr_RedactsMarshalerAndContainerValues(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const endpoint = "http://svc:hunter2@collector:4318"
	l := o11ylog.NewLogr(logger, nil, endpoint)

	l.Info("Tracer created",
		"attributes", attribute.NewSet(attribute.String("otlp.endpoint", endpoint)),
		"strings", map[string]string{"a": endpoint},
		"nested", map[string]any{"inner": []any{endpoint, map[string]string{"b": endpoint}}},
		"list", []string{endpoint},
		slog.Any("wrapped", map[string]string{"c": endpoint}),
	)

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.Equal(t, 6, strings.Count(out, "collector:4318"), "every redacted copy keeps the host")
	assert.Contains(t, out, "otlp.endpoint", "the attribute.Set's key survives")
}

// TestLogr_RedactsTypedNestedContainers covers container types that are not
// the plain map[string]string / []any shapes: a slice of maps, a map of
// slices, a named string type, a pointer to a map, and a []byte that must
// be left alone.
func TestLogr_RedactsTypedNestedContainers(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const endpoint = "http://svc:hunter2@collector:4318"
	type named string
	l := o11ylog.NewLogr(logger, nil, endpoint)

	m := map[string]string{"p": endpoint}
	l.Info("typed",
		"maps", []map[string]string{{"a": endpoint}},
		"lists", map[string][]string{"b": {endpoint}},
		"named", named(endpoint),
		"ptr", &m,
		"array", [1]named{named(endpoint)},
		"raw", []byte("not-a-credential"),
	)

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.Equal(t, 5, strings.Count(out, "collector:4318"), "every redacted copy keeps the host")
	assert.Contains(t, out, "not-a-credential", "a []byte is passed through")
}

// TestLogr_BoundsGroupNesting checks a slog.Group nested deeper than the
// resolver follows is cut off with the placeholder rather than descended
// into forever or passed through unredacted.
func TestLogr_BoundsGroupNesting(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const endpoint = "http://svc:hunter2@collector:4318"
	l := o11ylog.NewLogr(logger, nil, endpoint)

	attr := slog.String("endpoint", endpoint)
	for i := range 40 {
		attr = slog.Group("g"+strconv.Itoa(i), attr)
	}
	assert.NotPanics(t, func() { l.Info("deep", attr) })

	out := buf.String()
	assert.Contains(t, out, "msg=deep")
	assert.NotContains(t, out, "hunter2", "a value below the depth bound is dropped, not leaked")
	assert.Contains(t, out, "omitted: nested deeper")
}

// selfMarshaler returns itself from MarshalLog; the depth bound must stop
// the recursion.
type selfMarshaler struct{}

// MarshalLog implements logr.Marshaler.
func (m selfMarshaler) MarshalLog() any { return m }

// TestLogr_BoundsMarshalerRecursion checks a Marshaler that marshals to
// itself is logged rather than recursed on forever.
func TestLogr_BoundsMarshalerRecursion(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, nil)

	assert.NotPanics(t, func() { l.Info("loop", "m", selfMarshaler{}) })
	assert.Contains(t, buf.String(), "msg=loop")
}

// TestLogr_WithValuesAndWithName checks names and values reach the record.
func TestLogr_WithValuesAndWithName(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, nil).WithName("otel").WithName("sdk").WithValues("pipeline", "traces")

	l.Info("hello", "n", 1)

	out := buf.String()
	assert.Contains(t, out, "logger=otel/sdk")
	assert.Contains(t, out, "pipeline=traces")
	assert.Contains(t, out, "n=1")
}

// TestLogr_NilLoggerDiscards checks a nil logger is safe to use.
func TestLogr_NilLoggerDiscards(t *testing.T) {
	l := o11ylog.NewLogr(nil, nil)
	assert.NotPanics(t, func() {
		l.Info("ignored")
		l.Error(errors.New("x"), "ignored")
	})
}
