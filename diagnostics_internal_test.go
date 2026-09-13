package o11y

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOTelErrorHandler_LogsOncePerWindow checks identical errors collapse to
// one ERROR per window while distinct errors are each logged.
func TestOTelErrorHandler_LogsOncePerWindow(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)))
	now := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return now }

	refused := errors.New(`traces export: Post "http://otel-collector:4318/v1/traces": dial tcp 10.0.0.5:4318: connect: connection refused`)
	h.Handle(refused)
	h.Handle(refused)
	h.Handle(errors.New("logs export: something else"))
	now = now.Add(otelDiagnosticRepeatWindow)
	h.Handle(refused)

	out := buf.String()
	assert.Equal(t, 2, strings.Count(out, "connection refused"), "identical errors collapse to one line per window")
	assert.Equal(t, 1, strings.Count(out, "something else"))
	assert.Equal(t, 3, strings.Count(out, "level=ERROR"), "handled OTel errors must survive an error-only log level")
	assert.Contains(t, out, `msg="otel internal error"`)
	assert.Contains(t, out, "repeat_suppressed_for=1m0s")
}

// TestOTelErrorHandler_RedactsEndpointCredentials checks an endpoint quoted
// back in the error text loses its userinfo before it is logged.
func TestOTelErrorHandler_RedactsEndpointCredentials(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)), "http://svc:hunter2@collector:4318")

	h.Handle(errors.New(`traces export: Post "http://svc:hunter2@collector:4318/v1/traces": EOF`))

	assert.NotContains(t, buf.String(), "hunter2")
	assert.Contains(t, buf.String(), "collector:4318")
}

// TestOTelErrorHandler_RedactsSecrets checks a configured header value is
// replaced in the error text even though nothing about it looks like an
// endpoint.
func TestOTelErrorHandler_RedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)))
	h.secrets = []string{"BearerSecret%zz"}

	h.Handle(errors.New(`escape header value: invalid URL escape "%zz" in BearerSecret%zz`))

	assert.NotContains(t, buf.String(), "BearerSecret")
	assert.Contains(t, buf.String(), "[redacted]")

	buf.Reset()
	h.secrets = diagnosticSecrets(&Config{otlpHeaders: map[string]string{"x-tiny": "a%zz"}})
	h.Handle(errors.New(`escape header value: invalid URL escape "%zz" in a%zz (attempt 12)`))

	assert.NotContains(t, buf.String(), "a%zz", "a short configured value echoed on its own is redacted")
	assert.Contains(t, buf.String(), "attempt 12", "text around it is left alone")
}

// TestDiagnosticSecrets collects the configured header names and values and
// the environment-provided ones: whole variable, each pair, each name and
// value with their unescaped forms, deduplicated, short values included,
// empty ones left out.
func TestDiagnosticSecrets(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20abcdef, x-short=1, api-key=k-1234567890, BearerSecret%zz=oops")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
	cfg := &Config{
		otlpHeaders:          map[string]string{"x-api-key": "configured-secret", "x-tiny": "ab"},
		profilingAuthHeaders: map[string]string{"authorization": "Basic cHJvZmlsZXM="},
	}

	got := diagnosticSecrets(cfg)

	for _, want := range []string{
		"configured-secret", "Basic cHJvZmlsZXM=",
		"authorization=Bearer%20abcdef, x-short=1, api-key=k-1234567890, BearerSecret%zz=oops",
		"authorization=Bearer%20abcdef", "Bearer%20abcdef", "Bearer abcdef",
		"api-key=k-1234567890", "k-1234567890",
		"x-short=1", "1", "ab",
		"BearerSecret%zz=oops", "BearerSecret%zz", "authorization", "api-key",
		"x-api-key", "x-tiny",
	} {
		assert.Contains(t, got, want)
	}
	assert.NotContains(t, got, "", "an empty value is not a secret")
	assert.Len(t, got, len(slices.Compact(slices.Sorted(slices.Values(got)))), "no duplicates")
}

// TestValidateOTLPExporterEnv mirrors the pinned exporters' parsers and
// their reading order: with traces or push-path metrics enabled every
// ENDPOINT, TIMEOUT and HEADERS variable under the generic and the signal
// prefix is checked; the log exporter reads a variable only where Init
// passes no explicit option, so its endpoint is never checked, its headers
// only without WithOTLPHeaders, and its timeout and compression always. A
// malformed value fails with an error that names the variable (and the
// pair's position) but never its text; an empty variable is ignored.
func TestValidateOTLPExporterEnv(t *testing.T) {
	all := &Config{traceEnabled: true, logEnabled: true, metricsEnabled: true, metricsOTLPEndpoint: "http://collector:4318"}

	t.Run("well-formed", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20abcdef, x-tenant=acme")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "10000")
		t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "gzip")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
		require.NoError(t, validateOTLPExporterEnv(all))
	})

	for _, tc := range []struct{ name, variable, value, reason, secret string }{
		{"missing equals", "OTEL_EXPORTER_OTLP_HEADERS", "x-ok=1, authorization", "pair 2 is missing '='", "authorization"},
		{"bad name", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "x-ok=1, bad name=value", "name of pair 2 is not a valid HTTP header name", "bad name"},
		{"bad value", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "x-ok=1, authorization=BearerSecret%zz", "value of pair 2 is not valid percent-encoding", "BearerSecret"},
		{"bad endpoint", "OTEL_EXPORTER_OTLP_ENDPOINT", "http://user:secret%zz@collector:4318", "not a valid URL", "secret%zz"},
		{"bad metrics endpoint", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://user:secret%zz@collector:4318", "not a valid URL", "secret%zz"},
		{"bad timeout", "OTEL_EXPORTER_OTLP_TIMEOUT", "10s", "not an integer count of milliseconds", "10s"},
		{"bad logs compression", "OTEL_EXPORTER_OTLP_LOGS_COMPRESSION", "brotli", `neither "gzip" nor "none"`, "brotli"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.variable, tc.value)
			err := validateOTLPExporterEnv(all)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.variable+" is malformed")
			assert.Contains(t, err.Error(), tc.reason)
			assert.NotContains(t, err.Error(), tc.secret, "the raw text stays out of the error")
		})
	}

	t.Run("variable no exporter reads is ignored", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "authorization=BearerSecret%zz")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://user:secret%zz@collector:4318")
		pull := &Config{traceEnabled: true, logEnabled: true, metricsEnabled: true}
		require.NoError(t, validateOTLPExporterEnv(pull), "the metrics variables are read only on the OTLP push path")
		require.Error(t, validateOTLPExporterEnv(all))
		none := &Config{}
		require.NoError(t, validateOTLPExporterEnv(none), "no OTLP exporter, nothing to validate")
	})

	t.Run("log exporter reads only what Init leaves to the environment", func(t *testing.T) {
		logsOnly := &Config{logEnabled: true}
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://user:secret%zz@collector:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "http://user:secret%zz@collector:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_INSECURE", "maybe")
		require.NoError(t, validateOTLPExporterEnv(logsOnly), "Init always passes the log endpoint URL, which pins endpoint, path and insecure")

		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=BearerSecret%zz")
		require.Error(t, validateOTLPExporterEnv(logsOnly), "without WithOTLPHeaders the log exporter reads the headers variable")
		withHeaders := &Config{logEnabled: true, otlpHeaders: map[string]string{"x-api-key": "configured"}}
		require.NoError(t, validateOTLPExporterEnv(withHeaders), "explicit headers take precedence and the variable is never parsed")

		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "soon")
		require.Error(t, validateOTLPExporterEnv(withHeaders), "the log exporter always reads the timeout")

		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_TIMEOUT", "10000")
		require.NoError(t, validateOTLPExporterEnv(withHeaders), "a valid signal value shadows the generic one, which the exporter never reads")
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_TIMEOUT", "later")
		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "10000")
		err := validateOTLPExporterEnv(withHeaders)
		require.Error(t, err, "a signal value that fails to parse is echoed before the exporter falls through")
		assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_LOGS_TIMEOUT is malformed")
	})
}

// derefError dereferences its receiver in Error, so a typed nil *derefError
// panics when rendered the ordinary way.
type derefError struct{ msg string }

func (e *derefError) Error() string { return e.msg }

// panicError panics on any receiver.
type panicError struct{}

func (panicError) Error() string { panic("no") }

// TestOTelErrorHandler_SurvivesBrokenErrors pins that a typed nil error and
// an Error method that panics are rendered as placeholders rather than
// taking the process down, and that a live error still renders.
func TestOTelErrorHandler_SurvivesBrokenErrors(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)))

	var typed *derefError
	assert.NotPanics(t, func() { h.Handle(typed) })
	assert.Contains(t, buf.String(), "<nil *o11y.derefError>")

	buf.Reset()
	assert.NotPanics(t, func() { h.Handle(panicError{}) })
	assert.Contains(t, buf.String(), "[omitted: o11y.panicError panicked while rendering]")

	buf.Reset()
	h.Handle(&derefError{msg: "still rendered"})
	assert.Contains(t, buf.String(), "still rendered")
}

// TestOTelErrorHandler_NilIsIgnored checks nil errors and a nil logger are safe.
func TestOTelErrorHandler_NilIsIgnored(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)))
	assert.NotPanics(t, func() { h.Handle(nil) })
	assert.Empty(t, buf.String())

	assert.NotPanics(t, func() { newOTelErrorHandler(nil).Handle(errors.New("x")) })
}

// TestShutdownBudget_SharesDeadlineAcrossClosers checks the share each
// closer gets: an even split of the time left, the whole remainder for the
// last closer, and ctx itself when there is no deadline or it has passed.
func TestShutdownBudget_SharesDeadlineAcrossClosers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()

	first, cancelFirst := shutdownBudget(ctx, 4)
	defer cancelFirst()
	firstDeadline, ok := first.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, time.Now().Add(250*time.Millisecond), firstDeadline, 50*time.Millisecond)

	last, cancelLast := shutdownBudget(ctx, 1)
	defer cancelLast()
	lastDeadline, _ := last.Deadline()
	assert.Equal(t, deadline, lastDeadline, "the last closer gets everything that is left")

	noDeadline, cancelNone := shutdownBudget(context.Background(), 3)
	defer cancelNone()
	_, ok = noDeadline.Deadline()
	assert.False(t, ok, "no deadline to share")

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	past, cancelPast := shutdownBudget(expired, 3)
	defer cancelPast()
	assert.ErrorIs(t, past.Err(), context.DeadlineExceeded, "an expired deadline is passed through")
}

// TestShutdown_LaterClosersKeepALiveContext is the failure Codex described:
// a closer that blocks until its context is done must not leave the ones
// after it, the meter provider's final collection, with a context that is
// already cancelled.
func TestShutdown_LaterClosersKeepALiveContext(t *testing.T) {
	var sawLive bool
	sdk := &SDK{
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		shutdowns: []func(context.Context) error{
			func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			func(ctx context.Context) error {
				sawLive = ctx.Err() == nil
				return nil
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := sdk.Shutdown(ctx)

	assert.ErrorIs(t, err, context.DeadlineExceeded, "the slow closer's own timeout is reported")
	assert.True(t, sawLive, "the closer after a slow one still ran with a live context")
	assert.NoError(t, ctx.Err(), "the caller's deadline itself was not exhausted")
}

// TestShutdownSequence_DrainsTracesAndLogsBeforeMetrics pins the closer
// order: a batch that fails during the tracer's or logger's final flush is
// counted on the export-failure Recorder, and only a meter provider that
// shuts down afterwards still collects that count.
func TestShutdownSequence_DrainsTracesAndLogsBeforeMetrics(t *testing.T) {
	var order []string
	closer := func(name string) func(context.Context) error {
		return func(context.Context) error {
			order = append(order, name)
			return nil
		}
	}

	seq := shutdownSequence(closer("profiler"), closer("traces"), closer("logs"), closer("metrics-server"), closer("meter"))
	for _, fn := range seq {
		require.NoError(t, fn(context.Background()))
	}
	assert.Equal(t, []string{"profiler", "traces", "logs", "metrics-server", "meter"}, order)

	order = nil
	for _, fn := range shutdownSequence(nil, closer("traces"), closer("logs"), closer("metrics-server"), closer("meter")) {
		require.NoError(t, fn(context.Background()))
	}
	assert.Equal(t, []string{"traces", "logs", "metrics-server", "meter"}, order, "a nil profiler closer is skipped")

	order = nil
	seq = shutdownSequence(nil, closer("traces"), nil, nil, nil)
	require.Len(t, seq, 1, "disabled pillars neither run nor count towards the deadline share")
	for _, fn := range seq {
		require.NoError(t, fn(context.Background()))
	}
	assert.Equal(t, []string{"traces"}, order)
}

// TestShutdown_DisabledPillarsDoNotShareTheDeadline checks the case Codex
// raised: with tracing the only enabled pillar, the tracer's closer must get
// the whole deadline rather than a quarter of it.
func TestShutdown_DisabledPillarsDoNotShareTheDeadline(t *testing.T) {
	var got time.Time
	sdk := &SDK{
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		shutdowns: shutdownSequence(nil, func(ctx context.Context) error {
			got, _ = ctx.Deadline()
			return nil
		}, nil, nil, nil),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want, _ := ctx.Deadline()
	require.NoError(t, sdk.Shutdown(ctx))
	assert.Equal(t, want, got, "the only closer runs under the caller's own deadline")
}
