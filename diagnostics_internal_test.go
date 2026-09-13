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

// TestDiagnosticSecrets collects the configured and environment-provided
// header values: whole variable, each pair, each value and its unescaped
// form, deduplicated, short values included, empty ones left out.
func TestDiagnosticSecrets(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20abcdef, x-short=1, api-key=k-1234567890")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
	cfg := &Config{
		otlpHeaders:          map[string]string{"x-api-key": "configured-secret", "x-tiny": "ab"},
		profilingAuthHeaders: map[string]string{"authorization": "Basic cHJvZmlsZXM="},
	}

	got := diagnosticSecrets(cfg)

	for _, want := range []string{
		"configured-secret", "Basic cHJvZmlsZXM=",
		"authorization=Bearer%20abcdef, x-short=1, api-key=k-1234567890",
		"authorization=Bearer%20abcdef", "Bearer%20abcdef", "Bearer abcdef",
		"api-key=k-1234567890", "k-1234567890",
		"x-short=1", "1", "ab",
	} {
		assert.Contains(t, got, want)
	}
	assert.NotContains(t, got, "", "an empty value is not a secret")
	assert.Len(t, got, len(slices.Compact(slices.Sorted(slices.Values(got)))), "no duplicates")
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
