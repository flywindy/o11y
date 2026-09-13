package log_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/url"
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

// TestLogr_RedactsAttrKeys checks the key of a bare slog.Attr and of a
// group member is redacted like a value, since a caller can put an
// endpoint or a credential there too.
func TestLogr_RedactsAttrKeys(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const endpoint = "http://svc:hunter2@collector:4318"
	l := o11ylog.NewLogrRedacting(logger, nil, o11ylog.Redaction{Endpoints: []string{endpoint}, Secrets: []string{"TopSecretToken"}})

	l.Info("keys", slog.String(endpoint, "v"), slog.Group("exporter", slog.Int(endpoint, 1), slog.String("TopSecretToken", "x")),
		slog.Any("TopSecretToken", []string{"y"}), endpoint, "bare key")

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.NotContains(t, out, "TopSecretToken")
	assert.Equal(t, 3, strings.Count(out, "collector:4318"), "every redacted key keeps the host")
	assert.Contains(t, out, "exporter.", "the group structure survives")
}

// TestLogr_RedactsLoggerName checks a WithName segment is redacted before
// it is emitted as the "logger" attribute.
func TestLogr_RedactsLoggerName(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const endpoint = "http://svc:hunter2@collector:4318"
	l := o11ylog.NewLogrRedacting(logger, nil, o11ylog.Redaction{Endpoints: []string{endpoint}, Secrets: []string{"TopSecretToken"}}).
		WithName(endpoint).WithName("TopSecretToken")

	l.Info("named")

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.NotContains(t, out, "TopSecretToken")
	assert.Contains(t, out, "logger=", "the attribute is still emitted")
	assert.Contains(t, out, "collector:4318", "the host survives")
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
		"intkeys", map[int]string{7: endpoint},
		"keyed", map[string]bool{endpoint: true},
		"structkeys", map[endpointStringer]int{{url: endpoint}: 1},
	)

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.Equal(t, 8, strings.Count(out, "collector:4318"), "every redacted copy keeps the host")
	assert.Contains(t, out, "not-a-credential", "a []byte is passed through")
	assert.Contains(t, out, "intkeys=map[7:", "a map with non-string keys is rebuilt, not passed through")
}

// TestLogr_RedactsMessageText pins that the message text itself goes
// through the redaction rules, for a component that puts an endpoint or a
// configured secret in the message rather than in a key/value pair.
func TestLogr_RedactsMessageText(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const endpoint = "http://svc:hunter2@collector:4318"
	l := o11ylog.NewLogrRedacting(logger, nil, o11ylog.Redaction{Endpoints: []string{endpoint}, Secrets: []string{"TopSecretToken"}})

	l.Info("failed endpoint " + endpoint + " with TopSecretToken")
	l.Error(nil, "bad endpoint "+endpoint)
	l.Error(errors.New("boom"), "token TopSecretToken rejected")

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.NotContains(t, out, "TopSecretToken")
	assert.Contains(t, out, "collector:4318", "the host survives")
	assert.Contains(t, out, "[redacted]")
}

// derefError dereferences its receiver in Error, so a typed nil *derefError
// panics when rendered the ordinary way.
type derefError struct{ msg string }

func (e *derefError) Error() string { return e.msg }

// panicError panics on any receiver.
type panicError struct{}

func (panicError) Error() string { panic("no") }

// TestLogr_SurvivesBrokenErrors pins that Error renders a typed nil error
// and an Error method that panics as placeholders rather than taking the
// process down, and that a live error still renders.
func TestLogr_SurvivesBrokenErrors(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, nil)

	var typed *derefError
	assert.NotPanics(t, func() { l.Error(typed, "typed nil") })
	assert.Contains(t, buf.String(), "<nil *log_test.derefError>")

	buf.Reset()
	assert.NotPanics(t, func() { l.Error(panicError{}, "panicking") })
	assert.Contains(t, buf.String(), "[omitted: log_test.panicError panicked while rendering]")

	buf.Reset()
	l.Error(&derefError{msg: "still rendered"}, "live")
	assert.Contains(t, buf.String(), "still rendered")
}

// endpointStringer renders an endpoint through String(), the way a type with
// a custom text form reaches the slog handler.
type endpointStringer struct{ url string }

// String implements fmt.Stringer.
func (e endpointStringer) String() string { return "endpoint=" + e.url }

// endpointValuer is a bare slog.LogValuer whose value carries an endpoint.
type endpointValuer struct{ url string }

// LogValue implements slog.LogValuer.
func (e endpointValuer) LogValue() slog.Value { return slog.StringValue(e.url) }

// TestLogr_RedactsStructsStringersAndLogValuers covers the value forms the
// handler would otherwise render itself: a struct with an endpoint in an
// exported field (and a nested one), a pointer to a struct, a fmt.Stringer
// whose text carries the endpoint, a bare slog.LogValuer, a struct with no
// exported field (rendered with %+v), and a time.Duration and time.Time
// that must stay what they are.
func TestLogr_RedactsStructsStringersAndLogValuers(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const endpoint = "http://svc:hunter2@collector:4318"
	type inner struct{ Endpoint string }
	type outer struct {
		Name  string
		Inner inner
		note  string
	}
	l := o11ylog.NewLogr(logger, nil, endpoint)

	o := outer{Name: "exporter", Inner: inner{Endpoint: endpoint}, note: "hidden"}
	l.Info("forms",
		"struct", o,
		"ptr", &o,
		"stringer", endpointStringer{url: endpoint},
		"valuer", endpointValuer{url: endpoint},
		"opaque", endpointStringerless{url: endpoint},
		"dur", 1500*time.Millisecond,
		"when", time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC),
	)

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.Equal(t, 4, strings.Count(out, "collector:4318"), "every redacted copy keeps the host")
	assert.Contains(t, out, "Inner", "the struct's exported fields survive")
	assert.NotContains(t, out, "hidden", "unexported fields are not rendered")
	assert.Contains(t, out, `opaque="[omitted: opaque`, "a struct with no exported field is named, not printed; the host count above shows its contents were not")
	assert.Contains(t, out, "dur=1.5s", "a Duration keeps its slog rendering")
	assert.Contains(t, out, "when=2026-09-13T10:00:00.000Z", "a Time keeps its slog rendering")
}

// nilStringer's String dereferences its receiver, so a typed nil must be
// caught before it is called.
type nilStringer struct{ s string }

// String implements fmt.Stringer.
func (n *nilStringer) String() string { return n.s }

// panickingStringer panics on any receiver.
type panickingStringer struct{}

// String implements fmt.Stringer.
func (panickingStringer) String() string { panic("no") }

// TestLogr_SurvivesTypedNilAndPanickingValues checks a typed nil pointer
// with a receiver-dereferencing String is rendered as <nil> without calling
// it, and a String that panics is replaced by a placeholder rather than
// taking the process down over a diagnostic.
func TestLogr_SurvivesTypedNilAndPanickingValues(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	l := o11ylog.NewLogr(logger, nil)

	assert.NotPanics(t, func() {
		l.Info("nils", "typed", (*nilStringer)(nil), "boom", panickingStringer{}, "ok", &nilStringer{s: "fine"})
	})

	out := buf.String()
	assert.Contains(t, out, "typed=<nil>")
	assert.Contains(t, out, `boom="[omitted: log_test.panickingStringer panicked while rendering]"`)
	assert.Contains(t, out, "ok=fine")
}

// TestLogr_RedactsConfiguredSecrets covers the shape the pinned exporters
// produce when OTEL_EXPORTER_OTLP_HEADERS fails to parse: the raw header
// value goes out as a "value" key/value, and a bearer token has no "@" or
// endpoint for the text rules to recognise, so it must be listed as a
// secret and replaced wherever it appears, in strings, errors and nested
// values alike.
func TestLogr_RedactsConfiguredSecrets(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const token = "BearerSecret%zz" // nosemgrep: hardcoded-credential-literal -- test fixture, the value the redaction must remove
	l := o11ylog.NewLogrRedacting(logger, nil, o11ylog.Redaction{Secrets: []string{token, "k-1234567890"}})

	l.Error(errors.New(`invalid URL escape "%zz" in `+token), "escape header value",
		"value", token, "nested", map[string]string{"authorization": "Bearer " + token}, "key", "k-1234567890")

	out := buf.String()
	assert.NotContains(t, out, "BearerSecret")
	assert.NotContains(t, out, "k-1234567890")
	assert.Equal(t, 4, strings.Count(out, "[redacted]"))
}

// endpointStringerless has no exported field and no String method; the
// walk cannot inspect it, so it must be named and omitted rather than
// printed with the credential in its unexported field.
type endpointStringerless struct{ url string }

// TestLogr_RedactsURLValues covers net/url values, which OTel components
// hold parsed endpoints in: a *url.URL and a url.URL value have their
// userinfo replaced the way redact.URL does it, and a *url.Userinfo or
// url.Userinfo on its own (whose String() is "user:password", with nothing
// for the text rules to anchor on) is replaced by the same placeholder.
func TestLogr_RedactsURLValues(t *testing.T) {
	logger, buf := newRecordingLogger(slog.LevelInfo)
	const endpoint = "http://svc:hunter2@collector:4318/v1/traces"
	u, err := url.Parse(endpoint)
	require.NoError(t, err)
	l := o11ylog.NewLogr(logger, nil, endpoint)

	l.Info("urls", "ptr", u, "val", *u, "userptr", u.User, "userval", *u.User, "nilptr", (*url.URL)(nil))

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.NotContains(t, out, "svc", "the username goes with the password")
	assert.Equal(t, 2, strings.Count(out, "redacted@collector:4318/v1/traces"), "both URL forms keep host and path")
	assert.Contains(t, out, "userptr=redacted")
	assert.Contains(t, out, "userval=redacted")
	assert.Contains(t, out, "nilptr=<nil>")
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
